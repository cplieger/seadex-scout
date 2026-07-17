package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/slogx/capture"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
			154587: {Titles: []string{"Frieren"}, Format: "TV", Year: 2023},
		}},
		Findings: map[string]report.Alerted{
			"dedupe": {
				AlertedAt: now,
				Finding: compare.Finding{
					Title:     "Frieren",
					Arr:       library.ArrSonarr,
					DedupeKey: "dedupe",
					Status:    compare.StatusBetter,
					AniListID: 154587,
				},
			},
		},
		Baselined: true,
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
	// gates (assertQuarantined's byte-equality is skipped: the body is a
	// 128MB+ sparse file, so existence + the live path renamed away suffice).
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

	huge := &State{Findings: map[string]report.Alerted{
		"huge": {Finding: compare.Finding{Title: strings.Repeat("a", maxStateBytes+1)}},
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
		return &State{Findings: map[string]report.Alerted{
			"huge": {Finding: compare.Finding{Title: strings.Repeat("a", n)}},
		}}
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

// TestStoreSaveLoadPreservesShrunkWalks pins the restart persistence of the
// library-shrink escalation streak through the real Store disk path: a json
// tag drift or a persistence projection omission would silently reset the
// streak after every restart.
func TestStoreSaveLoadPreservesShrunkWalks(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	const want = 7
	if err := store.Save(context.Background(), &State{ShrunkWalks: want}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if got.ShrunkWalks != want {
		t.Errorf("ShrunkWalks after disk round trip = %d, want %d", got.ShrunkWalks, want)
	}
}
