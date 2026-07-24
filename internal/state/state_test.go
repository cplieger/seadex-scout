package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/slogx/capture"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestStoreLoadMissingReturnsEmptyState(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	st, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load missing state returned error: %v", err)
	}
	if st.Baselined || len(st.Library.Items) != 0 || len(st.Mapping.Records) != 0 || len(st.Memo.Entries) != 0 || len(st.Findings) != 0 {
		t.Errorf("Load missing state = %+v, want zero state", st)
	}
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewStore(filepath.Join(t.TempDir(), "nested", "state.json"), testLogger())
	want := &State{
		Library: library.Snapshot{
			TakenAt: now,
			Items: []library.Item{{
				SeasonGroups: map[int][]string{1: {"subsplease"}},
				Arr:          library.ArrSonarr,
				Title:        "Frieren",
				Groups:       []string{"subsplease"},
				ArrID:        7,
				TvdbID:       123,
				Year:         2023,
				HasFile:      true,
			}},
		},
		Mapping: mapping.Cache{
			FetchedAt:         now,
			ETag:              "etag-1",
			Records:           []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
			RejectedRefreshes: 3,
		},
		Memo: match.Memo{Entries: map[int]match.MemoEntry{
			154587: {Titles: []string{"Frieren"}, Format: "TV", Year: 2023, Expiry: now.Add(300 * time.Hour)},
		}},
		Findings: map[string]notify.Alerted{
			"dedupe": {
				AlertedAt: now,
				Finding: notify.StoredFinding{
					Title:     "Frieren",
					Arr:       library.ArrSonarr,
					Status:    compare.StatusBetter,
					AniListID: 154587,
				},
			},
		},
		Baselined:          true,
		BaselineIncomplete: true,
	}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if !got.Baselined {
		t.Error("Baselined = false, want true")
	}
	if !got.BaselineIncomplete {
		t.Error("BaselineIncomplete = false, want true (the incomplete-baseline window must survive restarts)")
	}
	if len(got.Library.Items) != 1 || got.Library.Items[0].Title != "Frieren" || got.Library.Items[0].SeasonGroups[1][0] != "subsplease" {
		t.Errorf("Library round trip = %+v, want Frieren with season group", got.Library)
	}
	if len(got.Mapping.Records) != 1 || got.Mapping.Records[0].AniListID != 154587 || !got.Mapping.FetchedAt.Equal(now) {
		t.Errorf("Mapping round trip = %+v, want AniList 154587 fetched at %s", got.Mapping, now)
	}
	if got.Mapping.RejectedRefreshes != 3 {
		t.Errorf("Mapping.RejectedRefreshes round trip = %d, want 3 (the rejection streak must survive restarts)", got.Mapping.RejectedRefreshes)
	}
	if got.Memo.Entries[154587].Year != 2023 {
		t.Errorf("Memo year = %d, want 2023", got.Memo.Entries[154587].Year)
	}
	if want := now.Add(300 * time.Hour); !got.Memo.Entries[154587].Expiry.Equal(want) {
		t.Errorf("Memo expiry round trip = %s, want %s (the jittered-TTL stamp must survive restarts)",
			got.Memo.Entries[154587].Expiry, want)
	}
	alert, ok := got.Findings["dedupe"]
	if !ok || alert.Finding.Title != "Frieren" || !alert.AlertedAt.Equal(now) {
		t.Errorf("Findings round trip = %+v, want preserved dedupe alert", got.Findings)
	}
}

// TestStoreSaveSanitizesLibrarySnapshot pins Save's ownership of the
// sanitize-on-persist invariant: a credentialed ArrURL handed to Save never
// lands in state.json (the caller no longer sanitizes), the rest of the item
// survives, and the caller's in-memory State is left untouched (Save works on
// a shallow copy).
func TestStoreSaveSanitizesLibrarySnapshot(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	st := &State{Library: library.Snapshot{Items: []library.Item{{
		Arr:    library.ArrSonarr,
		Title:  "Frieren",
		ArrID:  7,
		ArrURL: "https://user:pass@sonarr.example/series/frieren",
	}}}}

	if err := store.Save(context.Background(), st); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if len(got.Library.Items) != 1 {
		t.Fatalf("loaded library items = %d, want 1", len(got.Library.Items))
	}
	it := got.Library.Items[0]
	if it.ArrURL != "https://sonarr.example/series/frieren" {
		t.Errorf("persisted ArrURL = %q, want the credential stripped by Save", it.ArrURL)
	}
	if it.Title != "Frieren" || it.Arr != library.ArrSonarr || it.ArrID != 7 {
		t.Errorf("persisted item = %+v, want Title/Arr/ArrID untouched by sanitization", it)
	}
	if st.Library.Items[0].ArrURL != "https://user:pass@sonarr.example/series/frieren" {
		t.Errorf("caller's State mutated by Save: ArrURL = %q, want original", st.Library.Items[0].ArrURL)
	}
}

func TestStoreLoadCorruptReturnsDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	_, err := NewStore(path, testLogger()).Load(context.Background())
	if err == nil {
		t.Fatal("Load corrupt state returned nil error, want decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, want decode context", err.Error())
	}
	assertQuarantined(t, path, "{")
}

func TestStoreLoadInvalidUTF8Quarantines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := []byte("{\"findings\":{\"bad\xffkey\":{}}}")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write invalid UTF-8 state: %v", err)
	}
	store := NewStore(path, testLogger())
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Load returned nil error, want invalid UTF-8 decode error")
	}
	assertQuarantined(t, path, string(body))
	if err := store.Save(context.Background(), &State{}); err != nil {
		t.Errorf("Save after quarantining invalid UTF-8 remained blocked: %v", err)
	}
}

// TestReadOnlyStoreLoadCorruptLeavesFileInPlace pins the read-only flow's
// quarantine posture (the one-shot report is documented read-only on the
// state file): Load still surfaces the decode error, but the corrupt file
// stays at the live path - never renamed to .corrupt - so the daemon's own
// Load detects and reports the corruption on the container's log stream.
func TestReadOnlyStoreLoadCorruptLeavesFileInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	_, err := NewReadOnlyStore(path, testLogger()).Load(context.Background())
	if err == nil {
		t.Fatal("Load corrupt state returned nil error, want decode error")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("live state path unreadable after read-only Load: %v", readErr)
	}
	if string(got) != "{" {
		t.Errorf("live state bytes = %q, want the original untouched", got)
	}
	if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("read-only Load produced a .corrupt copy (stat err = %v), want none", statErr)
	}
}

// TestReadOnlyStoreSaveRefused pins the read-only store's write guard: the
// one-shot report flow is documented read-only on the state file, so Save
// on a NewReadOnlyStore must refuse and leave no file behind.
func TestReadOnlyStoreSaveRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	err := NewReadOnlyStore(path, testLogger()).Save(context.Background(), &State{Baselined: true})
	if err == nil {
		t.Fatal("Save on a read-only store returned nil error, want refusal")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %q, want read-only refusal context", err.Error())
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("state file after refused Save stat error = %v, want not exist", statErr)
	}
}

// assertQuarantined asserts the decode-failure quarantine contract: the corrupt
// payload is preserved at path+".corrupt" with its original bytes, and the live
// path is gone so the next Save recreates it cleanly.
func assertQuarantined(t *testing.T, path, wantBody string) {
	t.Helper()
	got, err := os.ReadFile(path + ".corrupt")
	if err != nil {
		t.Fatalf("corrupt state was not quarantined: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("quarantined bytes = %q, want original %q", got, wantBody)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("live state path still present after quarantine (stat err = %v), want renamed away", err)
	}
}

// TestStoreLoadDuplicateVersionKeyQuarantines pins that Load never trusts
// st.Version after a decode error: a payload with a duplicate version key
// ({"version":99,"version":"not-a-number"}) leaves the first numeric value in
// the partially-populated State while json.Unmarshal fails on the later
// duplicate. The independently decoded discriminator (schemaVersion)
// reads the wire's effective (last) value, classifies the file as corrupt -
// not newer-schema - so it is quarantined and a following Save is NOT
// blocked (the daemon persists instead of silently re-baselining every run).
func TestStoreLoadDuplicateVersionKeyQuarantines(t *testing.T) {
	const body = `{"version":99,"version":"not-a-number"}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	store := NewStore(path, testLogger())
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("Load returned nil error, want decode error for a duplicate-version-key payload")
	}
	if strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
	}
	assertQuarantined(t, path, body)
	if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
		t.Errorf("Save after quarantining a duplicate-version-key file failed: %v", saveErr)
	}
}

// TestStoreLoadTrailingGarbageAfterValidVersionQuarantines pins the
// trailing-data rejection in schemaVersion: a payload whose leading
// object carries a valid newer version but is followed by trailing bytes
// ({"version":99}x, or a second JSON document) is corruption, never
// newer-schema state. Without the trailing check the poisoned file would be
// preserved at the live path with every subsequent Save blocked; instead it
// must be quarantined and Save left unblocked.
func TestStoreLoadTrailingGarbageAfterValidVersionQuarantines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"raw trailing bytes", `{"version":99}x`},
		{"second JSON document", `{"version":99} {"baselined":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			store := NewStore(path, testLogger())
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for trailing data")
			}
			if strings.Contains(err.Error(), "newer than this binary supports") {
				t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
				t.Errorf("Save after quarantining a trailing-garbage file failed: %v", saveErr)
			}
		})
	}
}

// TestStoreLoadEarlierInvalidDuplicateVersionQuarantines pins the converse
// duplicate ordering: when the INVALID duplicate comes first
// ({"version":"bad","Version":99}), the effective (last, case-insensitive)
// value is a valid 99, so a one-field whole-document unmarshal would classify
// the corrupt payload as newer-schema state - leaving the poisoned bytes at
// the live path and blocking every subsequent Save. schemaVersion must
// validate every occurrence of the discriminator, classify the file as
// corrupt, quarantine it, and leave Save unblocked.
func TestStoreLoadEarlierInvalidDuplicateVersionQuarantines(t *testing.T) {
	const body = `{"version":"bad","Version":99,"findings":{}}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	store := NewStore(path, testLogger())
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("Load returned nil error, want duplicate-version decode error")
	}
	if strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
	}
	assertQuarantined(t, path, body)
	if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
		t.Errorf("Save after quarantining malformed duplicate version remained blocked: %v", saveErr)
	}
}

// TestStoreLoadNegativeVersionQuarantines pins the version-domain check: the
// documented legacy envelope's version is absent or zero and Save only stamps
// SchemaVersion, so a negative decoded version is corruption - quarantined,
// never accepted as valid state.
func TestStoreLoadNegativeVersionQuarantines(t *testing.T) {
	const body = `{"version":-1}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	store := NewStore(path, testLogger())
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("Load returned nil error, want error for a negative schema version")
	}
	if !strings.Contains(err.Error(), "negative schema version") {
		t.Errorf("error = %q, want negative-schema-version context", err.Error())
	}
	assertQuarantined(t, path, body)
	if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
		t.Errorf("Save after quarantining a negative-version file failed: %v", saveErr)
	}
}

// TestStoreLoadNegativeDuplicateVersionQuarantines pins the version-domain
// check against duplicate-key reduction: schemaVersion validates EVERY
// case-insensitive occurrence of the discriminator, so a payload whose
// invalid negative occurrence precedes a newer-schema value must classify as
// corruption (quarantined, Save subsequently unblocked) - not as preserved
// newer-schema state, which would leave the daemon cold-starting every cycle
// with every end-of-cycle Save refused.
func TestStoreLoadNegativeDuplicateVersionQuarantines(t *testing.T) {
	const body = `{"version":-1,"version":99}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	store := NewStore(path, testLogger())
	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("Load returned nil error, want error for a negative duplicate schema version")
	}
	if !strings.Contains(err.Error(), "negative schema version") {
		t.Errorf("error = %q, want negative-schema-version context", err.Error())
	}
	if strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %q, want corruption classification, not the newer-schema preservation", err.Error())
	}
	assertQuarantined(t, path, body)
	if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
		t.Errorf("Save after quarantining a negative-duplicate-version file remained blocked: %v", saveErr)
	}
}

// TestStoreLoadNullVersionQuarantines pins the wire discriminator's null
// rejection: encoding/json deliberately accepts JSON null into an int without
// an error, so {"version":null} would otherwise load as legacy version zero
// (and could cold-baseline and overwrite the file), while
// {"version":99,"version":null} would leave the stale earlier 99 in
// State.Version and be preserved forever as newer-schema state. Save can
// never produce either payload; both must quarantine as corruption with a
// subsequent Save unblocked.
func TestStoreLoadNullVersionQuarantines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"null version", `{"version":null}`},
		{"stale numeric before null duplicate", `{"version":99,"version":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			store := NewStore(path, testLogger())
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a null version discriminator")
			}
			if strings.Contains(err.Error(), "newer than this binary supports") {
				t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
				t.Errorf("Save after quarantining a null-version file failed: %v", saveErr)
			}
		})
	}
}

// TestStoreLoadNullReturnsDecodeError pins the envelope check: a state file
// holding literal JSON null is syntactically valid (json.Unmarshal accepts
// null into a struct) but can never be produced by Save, so loading it must
// surface the corruption as a decode error rather than a silently-empty state
// that fake-cold-starts and re-baselines every finding.
func TestStoreLoadNullReturnsDecodeError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"literal null", "null"},
		{"null with whitespace", "  null\n"},
		{"non-object array", "[]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o644); err != nil {
				t.Fatalf("write state: %v", err)
			}
			_, err := NewStore(path, testLogger()).Load(context.Background())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a non-object state file")
			}
			if !strings.Contains(err.Error(), "decode") {
				t.Errorf("error = %q, want decode context", err.Error())
			}
			assertQuarantined(t, path, tt.body)
		})
	}
}

func TestStoreLoadOversizedReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create oversized state: %v", err)
	}
	if err := f.Truncate(maxStateBytes + 1); err != nil {
		t.Fatalf("truncate oversized state: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close oversized state: %v", err)
	}
	_, err = NewStore(path, testLogger()).Load(context.Background())
	if err == nil {
		t.Fatal("Load oversized state returned nil error, want bounded-read error")
	}
	// Save enforces the same maxStateBytes cap, so an oversized file is
	// definitionally foreign/corrupt and must be quarantined like the decode
	// gates (assertQuarantined's byte-equality is skipped: the body is an
	// over-cap sparse file, so existence + the live path renamed away suffice).
	if _, statErr := os.Stat(path + ".corrupt"); statErr != nil {
		t.Errorf("oversized state was not quarantined (stat err = %v), want %s.corrupt preserved", statErr, path)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("live state path still present after quarantine (stat err = %v), want renamed away", statErr)
	}
}

// TestStoreSaveOverCapReturnsErrorAndKeepsPreviousFile pins the writer side of
// the shared maxStateBytes invariant: a state whose encoding exceeds what Load
// is contractually able to read must be rejected BEFORE the atomic replacement
// starts, leaving the last readable state file unchanged and loadable (writing
// it would silently discard the whole cache next cycle).
func TestStoreSaveOverCapReturnsErrorAndKeepsPreviousFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, testLogger())
	if err := store.Save(context.Background(), &State{Baselined: true}); err != nil {
		t.Fatalf("seed valid state: %v", err)
	}

	huge := &State{Findings: map[string]notify.Alerted{
		"huge": {Finding: notify.StoredFinding{Title: strings.Repeat("a", maxStateBytes+1)}},
	}}
	err := store.Save(context.Background(), huge)
	if err == nil {
		t.Fatal("Save returned nil error, want over-cap rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want size-cap context", err.Error())
	}

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after rejected Save returned error: %v", err)
	}
	if !got.Baselined {
		t.Error("previous state was not preserved after the rejected over-cap Save")
	}
}

// TestStoreSaveExactCapBoundaryAccepted pins the accepted-size boundary of the
// shared maxStateBytes invariant: a state whose json.Marshal encoding is
// EXACTLY maxStateBytes must save (json.Encoder's appended trailing newline is
// the encoder's artifact, not part of the persisted encoding, and must not tip
// the boundary), the persisted file must be exactly maxStateBytes bytes (no
// newline, so Load's bound reads it), and Load must round-trip it.
func TestStoreSaveExactCapBoundaryAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, testLogger())
	if err := store.Save(context.Background(), &State{Baselined: true}); err != nil {
		t.Fatalf("seed valid state: %v", err)
	}

	padded := func(n int) *State {
		// Version mirrors the SchemaVersion stamp Save applies to the copy it
		// writes, so the json.Marshal probe below measures the on-disk shape.
		return &State{
			Findings: map[string]notify.Alerted{
				"huge": {Finding: notify.StoredFinding{Title: strings.Repeat("a", n)}},
			},
			Version: SchemaVersion,
		}
	}
	base, err := json.Marshal(padded(0))
	if err != nil {
		t.Fatalf("marshal boundary probe: %v", err)
	}
	exact := padded(maxStateBytes - len(base))

	if err := store.Save(context.Background(), exact); err != nil {
		t.Fatalf("Save of an exactly-maxStateBytes state was rejected: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved state: %v", err)
	}
	if info.Size() != maxStateBytes {
		t.Errorf("saved file is %d bytes, want exactly %d (encoder newline must be truncated away)", info.Size(), maxStateBytes)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load of the boundary-sized state: %v", err)
	}
	if gotLen := len(got.Findings["huge"].Finding.Title); gotLen != maxStateBytes-len(base) {
		t.Errorf("round-tripped title length = %d, want %d", gotLen, maxStateBytes-len(base))
	}
}

// TestStoreSaveWriteFailureReturnsError pins Save's write-error contract: when
// the atomic write cannot reach disk (here the parent "directory" is a regular
// file, a root-safe injection), Save must return a wrapped error naming the
// path so the caller (scout.save) can log it, never swallow the failure.
func TestStoreSaveWriteFailureReturnsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}
	store := NewStore(filepath.Join(blocker, "state.json"), testLogger())

	err := store.Save(context.Background(), &State{Baselined: true})
	if err == nil {
		t.Fatal("Save returned nil error, want write failure")
	}
	if !strings.Contains(err.Error(), "state: write") {
		t.Errorf("error = %q, want 'state: write' context", err.Error())
	}
}

// TestNewStoreNilLoggerDefaults pins NewStore's documented "logger may be
// nil" contract: a nil logger must fall back to slog.Default, so Load (which
// logs the cold start) and Save work without panicking on a nil *slog.Logger.
func TestNewStoreNilLoggerDefaults(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), nil)
	st, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load with nil logger returned error: %v", err)
	}
	if st.Baselined || len(st.Findings) != 0 {
		t.Errorf("Load = %+v, want zero state", st)
	}
	if err := store.Save(context.Background(), &State{Baselined: true}); err != nil {
		t.Fatalf("Save with nil logger returned error: %v", err)
	}
}

// TestStoreQuarantineRenameFailureWarnsAndKeepsFile pins quarantine's
// best-effort contract: when the corrupt file cannot be renamed aside (the
// .corrupt destination is occupied by a directory, a root-safe injection),
// Load still returns the decode error, the corrupt file stays at the live
// path, and the failure is logged at Warn once - never escalated.
func TestStoreQuarantineRenameFailureWarnsAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	if err := os.Mkdir(path+".corrupt", 0o755); err != nil {
		t.Fatalf("create rename blocker: %v", err)
	}
	logger, recorder := capture.New()
	_, err := NewStore(path, logger).Load(context.Background())
	if err == nil {
		t.Fatal("Load corrupt state returned nil error, want decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, want decode context", err.Error())
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("corrupt file missing from live path after failed quarantine (stat err = %v), want kept in place", statErr)
	}
	if got := recorder.CountExact("could not preserve corrupt state file"); got != 1 {
		t.Errorf("rename-failure WARN count = %d, want 1", got)
	}
}

// TestStoreLoadCanceledReturnsErrorWithoutQuarantine pins Load's generic
// bounded-read error path: a pre-canceled context propagates context.Canceled
// without quarantining or deleting the valid state file.
func TestStoreLoadCanceledReturnsErrorWithoutQuarantine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"baselined":true}`), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewStore(path, testLogger()).Load(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Load canceled context error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("live state file after cancellation: %v, want preserved", statErr)
	}
	if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("corrupt quarantine after cancellation stat error = %v, want not exist", statErr)
	}
}

// TestStoreSaveNilReturnsErrorWithoutWriting pins Save's nil-state guard:
// without it json.Marshal accepts the nil pointer as literal null, writing a
// state file Load immediately treats as corruption (discarding the previous
// cache), so Save(nil) must reject and leave no file behind.
func TestStoreSaveNilReturnsErrorWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	err := NewStore(path, testLogger()).Save(context.Background(), nil)
	if err == nil {
		t.Fatal("Save(nil) returned nil error, want rejection")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("state file after Save(nil) stat error = %v, want not exist", statErr)
	}
}

// TestStoreSaveLoadPreservesEscalationStreaks pins the restart persistence of
// the scout's three escalation streaks (the library-shrink walk streak, the
// consecutive SeaDex-failure streak, and the consecutive AniList-degraded
// streak) through the real Store disk path: a json tag drift or a
// persistence projection omission would silently reset a streak after every
// restart, deferring its WARN-to-ERROR escalation forever.
func TestStoreSaveLoadPreservesEscalationStreaks(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	const wantShrunk, wantSeadex, wantAniList = 7, 5, 6
	if err := store.Save(context.Background(), &State{ShrunkWalks: wantShrunk, SeadexFailures: wantSeadex, AniListDegraded: wantAniList}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if got.ShrunkWalks != wantShrunk {
		t.Errorf("ShrunkWalks after disk round trip = %d, want %d", got.ShrunkWalks, wantShrunk)
	}
	if got.SeadexFailures != wantSeadex {
		t.Errorf("SeadexFailures after disk round trip = %d, want %d", got.SeadexFailures, wantSeadex)
	}
	if got.AniListDegraded != wantAniList {
		t.Errorf("AniListDegraded after disk round trip = %d, want %d", got.AniListDegraded, wantAniList)
	}
}

// TestStoreSaveStampsSchemaVersion pins the envelope versioning contract:
// Save stamps SchemaVersion into every file it writes (round-tripping through
// Load), the stamp lands on the copy Save writes - never the caller's State -
// a legacy pre-version file (no version field) loads without error as
// version zero, and a file stamped by a newer binary is refused, preserved at
// the live path, and shielded from every subsequent Save instead of silently
// zero-loading moved members or overwriting the newer state.
func TestStoreSaveStampsSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, testLogger())
	st := &State{Baselined: true}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if got.Version != SchemaVersion {
		t.Errorf("Version after disk round trip = %d, want the stamped SchemaVersion %d", got.Version, SchemaVersion)
	}
	if st.Version != 0 {
		t.Errorf("caller's State mutated by Save: Version = %d, want 0 (the stamp belongs on the written copy)", st.Version)
	}

	// A legacy envelope written before versioning carries no version field:
	// it must load cleanly as version zero (tolerated, no migration today).
	if err := os.WriteFile(path, []byte(`{"baselined":true}`), 0o644); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	legacy, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load of a legacy pre-version file returned error: %v", err)
	}
	if legacy.Version != 0 || !legacy.Baselined {
		t.Errorf("legacy load = Version %d Baselined %v, want 0/true (absent version tolerated)", legacy.Version, legacy.Baselined)
	}

	// A file stamped by a NEWER binary (an image rollback) must be refused,
	// not field-by-field zero-loaded: its members may have moved. It is valid
	// state, not corruption, so it stays at the live path (no .corrupt copy)
	// and every subsequent Save on this Store is refused — otherwise this
	// binary would overwrite the newer-schema file with a cold envelope and
	// rolling forward would silently lose the newer state.
	newer := fmt.Sprintf(`{"version":%d,"baselined":true}`, SchemaVersion+1)
	if err := os.WriteFile(path, []byte(newer), 0o644); err != nil {
		t.Fatalf("write newer-version state: %v", err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Load of a newer-schema file returned nil error, want refusal")
	} else {
		wantFile := fmt.Sprintf("schema version %d", SchemaVersion+1)
		wantSupported := fmt.Sprintf("(%d)", SchemaVersion)
		if !strings.Contains(err.Error(), wantFile) || !strings.Contains(err.Error(), wantSupported) {
			t.Errorf("error = %q, want both the file's version (%q) and the supported version (%q) named",
				err.Error(), wantFile, wantSupported)
		}
	}
	if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("newer-schema file quarantined (stat err = %v), want it preserved at the live path", statErr)
	}
	live, readErr := os.ReadFile(path)
	if readErr != nil || string(live) != newer {
		t.Errorf("live state file after newer-schema load = %q (err %v), want the original bytes preserved", live, readErr)
	}
	if saveErr := store.Save(context.Background(), &State{}); saveErr == nil {
		t.Error("Save after loading a newer-schema file returned nil error, want refusal")
	}
	live, readErr = os.ReadFile(path)
	if readErr != nil || string(live) != newer {
		t.Errorf("live state file after blocked Save = %q (err %v), want the newer-schema bytes untouched", live, readErr)
	}
}

// TestStoreLoadLogsLibrarySnapshotAge pins the snapshot-age diagnostic on the
// "state loaded" line: the persisted snapshot's TakenAt is read back at load
// and surfaced as a library_age attribute (the indexer feed's title synthesis
// runs over this snapshot, so stale-title diagnostics need its age), while a
// snapshot that never recorded a walk (zero TakenAt) omits the attribute
// instead of logging a nonsensical epoch-sized age.
func TestStoreLoadLogsLibrarySnapshotAge(t *testing.T) {
	libraryAge := func(recorder *capture.Recorder) (string, bool) {
		for _, r := range recorder.Records() {
			if r.Message != "state loaded" {
				continue
			}
			age, found := "", false
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "library_age" {
					age, _ = a.Value.Any().(string)
					found = true
					return false
				}
				return true
			})
			return age, found
		}
		t.Fatal("no \"state loaded\" record captured")
		return "", false
	}

	logger, recorder := capture.New()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	taken := time.Now().Add(-90 * time.Minute).UTC()
	if err := store.Save(context.Background(), &State{Library: library.Snapshot{TakenAt: taken}}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	age, found := libraryAge(recorder)
	if !found {
		t.Fatal("\"state loaded\" carries no library_age attribute for a walked snapshot")
	}
	d, err := time.ParseDuration(age)
	if err != nil {
		t.Fatalf("library_age = %q, want a parseable duration: %v", age, err)
	}
	if d < 89*time.Minute || d > 92*time.Minute {
		t.Errorf("library_age = %s, want ~90m (the persisted TakenAt's age)", d)
	}

	// A snapshot with the zero TakenAt (legacy state, or one persisted before
	// any walk succeeded) must omit the attribute.
	zeroLogger, zeroRecorder := capture.New()
	zeroStore := NewStore(filepath.Join(t.TempDir(), "state.json"), zeroLogger)
	if err := zeroStore.Save(context.Background(), &State{Baselined: true}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if _, err := zeroStore.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, found := libraryAge(zeroRecorder); found {
		t.Error("\"state loaded\" carries a library_age attribute for a zero TakenAt, want it omitted")
	}
}

// TestStoreSaveCanceledFailsFastWithoutWriting pins Save's documented
// fail-fast contract: a context already cancelled on entry returns before the
// sanitize and encode work (so scout.save's detached shutdown retry runs
// immediately), wrapped as "state: save" - distinct from the late
// "state: write" wrap - and no file is written.
func TestStoreSaveCanceledFailsFastWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Save(ctx, &State{Baselined: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Save with pre-canceled context error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "state: save") {
		t.Errorf("error = %q, want the fast-fail 'state: save' wrap (not the late 'state: write')", err.Error())
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("state file after canceled Save stat error = %v, want not exist", statErr)
	}
}

// TestStoreSaveCommitFailureReturnsError pins Save's commit-error contract:
// when the atomic rename cannot land (the target path is occupied by a
// directory, a root-safe injection), Save must return a wrapped "state: write"
// error naming the path, and the failed Commit must leave no orphaned temp in
// the parent directory (atomicfile removes its temp on a failed Commit).
func TestStoreSaveCommitFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("create rename blocker dir: %v", err)
	}
	store := NewStore(target, testLogger())

	err := store.Save(context.Background(), &State{Baselined: true})
	if err == nil {
		t.Fatal("Save returned nil error, want commit failure")
	}
	if !strings.Contains(err.Error(), "state: write") {
		t.Errorf("error = %q, want 'state: write' context", err.Error())
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read parent dir: %v", readErr)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("unexpected leftover entry %q after failed Commit, want temp removed", e.Name())
		}
	}
}

func TestStoreLoadReadsPersistedValidatorsAndPartialWalk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"mapping":{"fetched_at":"2026-07-01T00:00:00Z","etag":"W/\"fribb-v7\"","last_modified":"Wed, 01 Jul 2026 12:00:00 GMT"},"library":{"taken_at":"0001-01-01T00:00:00Z","partial":true},"anilist_memo":{}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}
	got, err := NewStore(path, testLogger()).Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.Mapping.ETag != `W/"fribb-v7"` {
		t.Errorf("Mapping.ETag from persisted envelope = %q, want %q (a json-tag drift silently drops the conditional-GET validator on restart)", got.Mapping.ETag, `W/"fribb-v7"`)
	}
	if got.Mapping.LastModified != "Wed, 01 Jul 2026 12:00:00 GMT" {
		t.Errorf("Mapping.LastModified from persisted envelope = %q, want the fixture's validator", got.Mapping.LastModified)
	}
	if !got.Library.Partial {
		t.Error("Library.Partial from persisted envelope = false, want true (an incomplete walk must not read as complete after a restart)")
	}
}

func TestStoreSaveAppliesOwnerOnlyFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed permissive state file: %v", err)
	}
	store := NewStore(path, testLogger())
	if err := store.Save(context.Background(), &State{Baselined: true}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want -rw------- (owner-only: the file holds the operator's library inventory and finding history, and Save must tighten a permissive pre-upgrade file)",
			info.Mode().Perm())
	}
}

func TestStoreLoadRecoveryClearsNewerSchemaSaveBlock(t *testing.T) {
	tests := []struct {
		name    string
		recover func(t *testing.T, path string)
	}{
		{"replaced with supported envelope", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"version":1,"baselined":true}`), 0o600); err != nil {
				t.Fatalf("write supported state: %v", err)
			}
		}},
		{"file removed", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove state: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			newer := fmt.Sprintf(`{"version":%d}`, SchemaVersion+1)
			if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
				t.Fatalf("write newer-schema state: %v", err)
			}
			store := NewStore(path, testLogger())
			if _, err := store.Load(context.Background()); err == nil {
				t.Fatal("Load of newer-schema state returned nil error, want refusal")
			}
			if err := store.Save(context.Background(), &State{}); err == nil {
				t.Fatal("Save while blocked returned nil error, want refusal")
			}
			tt.recover(t, path)
			if _, err := store.Load(context.Background()); err != nil {
				t.Fatalf("Load after recovery returned error: %v", err)
			}
			if err := store.Save(context.Background(), &State{Baselined: true}); err != nil {
				t.Errorf("Save after a recovered Load still blocked: %v (the block must clear once a supported or missing state loads)", err)
			}
		})
	}
}

// TestStoreSaveOverCapErrorReportsSizes pins the over-cap error's numbers:
// the app wrap names the limit Load actually enforces (maxStateBytes, NOT
// the internal +1 the encoder's newline rides on), and the wrapped
// atomicfile rejection quotes the staged size the encoder attempted - the
// JSON size plus that trailing newline - so an operator can read both the
// contract and the overshoot from one line.
func TestStoreSaveOverCapErrorReportsSizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, testLogger())
	huge := &State{
		Findings: map[string]notify.Alerted{
			"huge": {Finding: notify.StoredFinding{Title: strings.Repeat("a", maxStateBytes+1)}},
		},
	}
	stamped := *huge
	stamped.Version = SchemaVersion
	encoded, err := json.Marshal(&stamped)
	if err != nil {
		t.Fatalf("marshal size probe: %v", err)
	}
	saveErr := store.Save(context.Background(), huge)
	if saveErr == nil {
		t.Fatal("Save returned nil error, want over-cap rejection")
	}
	if !errors.Is(saveErr, atomicfile.ErrFileTooLarge) {
		t.Errorf("error = %v, want it to match atomicfile.ErrFileTooLarge", saveErr)
	}
	wantLimit := fmt.Sprintf("exceeds the %d-byte load limit", maxStateBytes)
	if !strings.Contains(saveErr.Error(), wantLimit) {
		t.Errorf("error = %q, want the load limit named (%q)", saveErr.Error(), wantLimit)
	}
	wantAttempted := fmt.Sprintf("to %d bytes", len(encoded)+1)
	if !strings.Contains(saveErr.Error(), wantAttempted) {
		t.Errorf("error = %q, want the attempted staged size named (%q: the JSON size plus the encoder's trailing newline)", saveErr.Error(), wantAttempted)
	}
}

func TestStoreLoadCorruptClearsNewerSchemaSaveBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	newer := fmt.Sprintf(`{"version":%d}`, SchemaVersion+1)
	if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
		t.Fatalf("write newer-schema state: %v", err)
	}
	store := NewStore(path, testLogger())
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Load of newer-schema state returned nil error, want refusal")
	}
	if err := store.Save(context.Background(), &State{}); err == nil {
		t.Fatal("Save while blocked returned nil error, want refusal")
	}

	// The live file is later replaced by corruption: Load must quarantine it
	// AND clear the remembered newer-schema block (the block is documented as
	// describing what the LAST Load found at the live path), so the daemon
	// resumes persisting instead of silently re-baselining every run.
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Load of corrupt state returned nil error, want decode error")
	}
	assertQuarantined(t, path, "null")
	if err := store.Save(context.Background(), &State{Baselined: true}); err != nil {
		t.Errorf("Save after a corrupt Load still blocked: %v (maybeQuarantine must clear the newer-schema block once the live file is positively classified corrupt)", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after unblocked Save returned error: %v", err)
	}
	if !got.Baselined {
		t.Error("re-loaded state lost Baselined, want the unblocked Save persisted")
	}
}

func TestStoreLoadReapsStaleTempsAndReadOnlySkips(t *testing.T) {
	writeTemp := func(t *testing.T, dir, name string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write temp fixture: %v", err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("age temp fixture: %v", err)
		}
		return p
	}

	t.Run("normal store reaps stale, keeps fresh", func(t *testing.T) {
		dir := t.TempDir()
		stale := writeTemp(t, dir, ".atomicfile-11111.tmp", time.Now().Add(-2*time.Hour))
		fresh := writeTemp(t, dir, ".atomicfile-22222.tmp", time.Now())
		if _, err := NewStore(filepath.Join(dir, "state.json"), testLogger()).Load(context.Background()); err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stale temp after Load: stat err = %v, want reaped (hour-old orphan)", err)
		}
		if _, err := os.Stat(fresh); err != nil {
			t.Errorf("fresh temp after Load: stat err = %v, want kept (could be a live concurrent Save)", err)
		}
	})

	t.Run("read-only store leaves even a stale temp", func(t *testing.T) {
		dir := t.TempDir()
		stale := writeTemp(t, dir, ".atomicfile-33333.tmp", time.Now().Add(-2*time.Hour))
		if _, err := NewReadOnlyStore(filepath.Join(dir, "state.json"), testLogger()).Load(context.Background()); err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if _, err := os.Stat(stale); err != nil {
			t.Errorf("stale temp after read-only Load: stat err = %v, want left in place (the report flow is documented read-only on the state dir)", err)
		}
	})
}

// TestStoreLoadStaleTempCleanupFailureWarnsAndContinues pins Load's
// degraded-maintenance contract: when the stale-temp sweep cannot read the
// state directory (the "directory" is a regular file, a root-safe injection),
// the failure is logged at Warn exactly once and Load still proceeds to the
// read (surfacing the read error), never aborting on the maintenance failure.
func TestStoreLoadStaleTempCleanupFailureWarnsAndContinues(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}
	logger, recorder := capture.New()
	_, err := NewStore(filepath.Join(blocker, "state.json"), logger).Load(context.Background())
	if err == nil {
		t.Fatal("Load with an unreadable state dir returned nil error, want read error")
	}
	if !strings.Contains(err.Error(), "state: read") {
		t.Errorf("error = %q, want 'state: read' context (cleanup failure must not become the returned error)", err.Error())
	}
	if got := recorder.CountExact("could not clean stale atomic-write temp files"); got != 1 {
		t.Errorf("cleanup-failure WARN count = %d, want 1", got)
	}
}

// TestEncodeStateWriteErrorWrapped pins encodeState's generic (non-size)
// error path: an I/O failure from the pending temp (here the fd is already
// closed via Cleanup, standing in for a disk error) surfaces wrapped as
// "state: encode <path>", distinct from the over-cap rejection message.
func TestEncodeStateWriteErrorWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	pf, err := atomicfile.NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	encErr := encodeState(pf, &State{Baselined: true}, path)
	if encErr == nil {
		t.Fatal("encodeState on a closed pending temp returned nil error, want write failure")
	}
	if !strings.Contains(encErr.Error(), "state: encode") {
		t.Errorf("error = %q, want the generic 'state: encode' wrap", encErr.Error())
	}
	if errors.Is(encErr, atomicfile.ErrFileTooLarge) {
		t.Errorf("error = %v classified as the over-cap rejection, want the generic I/O wrap", encErr)
	}
}

// TestStoreLoadUnclassifiedReadErrorBlocksSaveUntilClassified pins the
// preservation posture's last gap: a present-but-unreadable state file (here
// EACCES via chmod 0o000 - not absence, not over-cap, not a decode failure)
// must block Save, or the cycle that started cold after the failed read would
// overwrite the possibly-recoverable bytes at its end. Every CLASSIFIED
// failure already preserves its evidence (quarantine / the newer-schema Save
// block); the block here clears as soon as a later Load succeeds - the scout
// loads at the start of every cycle, so a transient fault self-heals.
func TestStoreLoadUnclassifiedReadErrorBlocksSaveUntilClassified(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0o000 does not produce EACCES")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewStore(path, testLogger())
	if err := store.Save(context.Background(), &State{}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Load on an unreadable file = nil error, want a read error")
	}
	if err := store.Save(context.Background(), &State{}); err == nil {
		t.Fatal("Save after an unclassified read failure = nil error, want the preservation block")
	} else if !strings.Contains(err.Error(), "unclassified read failure") {
		t.Errorf("blocked Save error = %v, want it to name the unclassified read failure", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("Load after repair: %v", err)
	}
	if err := store.Save(context.Background(), &State{}); err != nil {
		t.Errorf("Save after a classifying Load = %v, want the block cleared", err)
	}
}

// TestStoreLoadMalformedVersionValueQuarantines pins schemaVersion's raw-value
// decode error branch: a version member whose VALUE is syntactically invalid
// JSON ({"version":} / a truncated {"version":) fails the raw decode before
// the int unmarshal runs. The payload is corruption Save can never produce, so
// it must quarantine (original bytes preserved at .corrupt, live path renamed
// away) with the following Save unblocked - never classified newer-schema.
func TestStoreLoadMalformedVersionValueQuarantines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing value", `{"version":}`},
		{"truncated after key", `{"version":`},
		{"bare invalid token", `{"version":nul}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			store := NewStore(path, testLogger())
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a malformed version value")
			}
			if strings.Contains(err.Error(), "newer than this binary supports") {
				t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
				t.Errorf("Save after quarantining a malformed-version-value file failed: %v", saveErr)
			}
		})
	}
}

// TestStoreLoadStateFieldTypeMismatchQuarantines pins decode's final gate: a
// payload that passes the UTF-8, object-envelope, and version-discriminator
// checks but fails the State unmarshal on a member type mismatch
// ({"baselined":"yes"} / {"findings":[]}) is corruption Save can never
// produce. It must quarantine with the original bytes preserved and the
// following Save unblocked - the daemon persists a fresh envelope instead of
// silently re-baselining behind a poisoned file forever.
func TestStoreLoadStateFieldTypeMismatchQuarantines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bool member holds a string", `{"baselined":"yes"}`},
		{"map member holds an array", `{"findings":[]}`},
		{"int member holds an object", `{"shrunk_walks":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			store := NewStore(path, testLogger())
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a State field type mismatch")
			}
			if !strings.Contains(err.Error(), "decode") {
				t.Errorf("error = %q, want decode context", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(context.Background(), &State{}); saveErr != nil {
				t.Errorf("Save after quarantining a type-mismatched file failed: %v", saveErr)
			}
		})
	}
}

// TestStoreLoadCanceledReadBlocksSaveUntilClassified pins the root-safe leg
// of the unclassified-read-failure preservation posture: a canceled read is
// an UNCLASSIFIED failure (like EACCES/EIO), so after a Load under a
// pre-canceled context the on-disk bytes must be preserved by refusing Save
// until a later Load classifies the file. Unlike the EACCES variant (which
// skips under root), this injection works in any environment.
func TestStoreLoadCanceledReadBlocksSaveUntilClassified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"baselined":true}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	store := NewStore(path, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load canceled context error = %v, want context.Canceled", err)
	}
	err := store.Save(context.Background(), &State{})
	if err == nil {
		t.Fatal("Save after a canceled (unclassified) read = nil error, want the preservation block")
	}
	if !strings.Contains(err.Error(), "unclassified read failure") {
		t.Errorf("blocked Save error = %v, want it to name the unclassified read failure", err)
	}
	live, readErr := os.ReadFile(path)
	if readErr != nil || string(live) != `{"baselined":true}` {
		t.Errorf("live state after blocked Save = %q (err %v), want the original bytes preserved", live, readErr)
	}
	got, loadErr := store.Load(context.Background())
	if loadErr != nil {
		t.Fatalf("Load after recovery returned error: %v", loadErr)
	}
	if !got.Baselined {
		t.Error("re-loaded state lost Baselined, want the preserved file read back")
	}
	if saveErr := store.Save(context.Background(), &got); saveErr != nil {
		t.Errorf("Save after a classifying Load = %v, want the block cleared", saveErr)
	}
}
