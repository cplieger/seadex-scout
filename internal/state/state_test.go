package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/slogx/capture"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// paddedMemo builds an AniList memo whose encoded size grows byte-for-byte with
// n, so a size-bound test can hit an exact on-disk length.
func paddedMemo(n int) match.Memo {
	return match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{strings.Repeat("a", n)}},
	}}
}

// TestStoreLoadDoesNotBlockOnFifoStatePath pins the other half of readState's
// confined-open contract: the open is O_NONBLOCK, so a FIFO at the state path
// with no writer is an error Load classifies instead of an uninterruptible
// open. A blocking open wedges the whole cycle inside Load - a context cannot
// interrupt a blocked open(2) - so the daemon stops polling entirely and only
// the healthcheck max-age deadline eventually restarts it.
func TestStoreLoadDoesNotBlockOnFifoStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this platform: %v", err)
	}
	loaded := make(chan error, 1)
	go func() {
		_, err := NewStore(path, testLogger()).Load(t.Context())
		loaded <- err
	}()
	// The watchdog is only reached if the confined open regresses to a
	// blocking one; against the real code Load returns immediately.
	select {
	case err := <-loaded:
		if !errors.Is(err, atomicfile.ErrNotRegular) {
			t.Errorf("Load of a FIFO state path error = %v, want atomicfile.ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO state path, want the O_NONBLOCK open to fail it immediately")
	}
}

func TestStoreLoadMissingReturnsEmptyState(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	st, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load missing state returned error: %v", err)
	}
	if len(st.ShrunkWalksByArr) != 0 || len(st.Library.Items) != 0 || len(st.Mapping.Records) != 0 || len(st.Memo.Entries) != 0 {
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
		ShrunkWalksByArr: map[string]int{library.ArrSonarr: 2},
		SeadexFailures:   5,
		AniListDegraded:  7,
	}

	if err := store.Save(t.Context(), want); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if got.ShrunkWalksByArr[library.ArrSonarr] != 2 || got.SeadexFailures != 5 || got.AniListDegraded != 7 {
		t.Errorf("degradation streaks round trip = %d/%d/%d, want 2/5/7 (each escalation streak must survive restarts)",
			got.ShrunkWalksByArr[library.ArrSonarr], got.SeadexFailures, got.AniListDegraded)
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
}

func TestStoreLoadCorruptReturnsDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	_, err := NewStore(path, testLogger()).Load(t.Context())
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
	if _, err := store.Load(t.Context()); err == nil {
		t.Fatal("Load returned nil error, want invalid UTF-8 decode error")
	}
	assertQuarantined(t, path, string(body))
	if err := store.Save(t.Context(), &State{}); err != nil {
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
	_, err := NewReadOnlyStore(path, testLogger()).Load(t.Context())
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
	err := NewReadOnlyStore(path, testLogger()).Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})
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
	_, err := store.Load(t.Context())
	if err == nil {
		t.Fatal("Load returned nil error, want error for a negative schema version")
	}
	if !strings.Contains(err.Error(), "negative schema version") {
		t.Errorf("error = %q, want negative-schema-version context", err.Error())
	}
	assertQuarantined(t, path, body)
	if saveErr := store.Save(t.Context(), &State{}); saveErr != nil {
		t.Errorf("Save after quarantining a negative-version file failed: %v", saveErr)
	}
}

// TestStoreLoadNullVersionQuarantines pins the wire discriminator's null
// rejection: encoding/json deliberately accepts JSON null into an int without
// an error, so {"version":null} would otherwise load as legacy version zero
// (and could cold-baseline and overwrite the file). Save can never produce
// that payload; it must quarantine as corruption with a subsequent Save
// unblocked.
func TestStoreLoadNullVersionQuarantines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"null version", `{"version":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			store := NewStore(path, testLogger())
			_, err := store.Load(t.Context())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a null version discriminator")
			}
			if strings.Contains(err.Error(), "newer than this binary supports") {
				t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(t.Context(), &State{}); saveErr != nil {
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
			_, err := NewStore(path, testLogger()).Load(t.Context())
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
	store := NewStore(path, testLogger())
	// A canceled read is an UNCLASSIFIED failure (atomicfile rejects on ctx
	// before it stats the file), so it arms the preservation block. The
	// over-cap read below positively classifies the same file, so it must
	// CLEAR that block: quarantining the evidence and then still refusing
	// every Save would strand the daemon cold-starting and never persisting.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, cancelErr := store.Load(canceled); !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("Load with a canceled context error = %v, want context.Canceled to arm the preservation block", cancelErr)
	}
	_, err = store.Load(t.Context())
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
	if saveErr := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); saveErr != nil {
		t.Errorf("Save after quarantining an oversized file failed: %v (the over-cap classification must clear the unclassified-read-failure block)", saveErr)
	}
}

// TestStoreLoadNonRegularPathIsClassifiedCorruption pins the DETERMINISTIC
// half of the loader's self-heal-versus-preserve taxonomy for a non-regular
// inode at the state path (a directory here; a FIFO, device or socket reaches
// the same atomicfile.ErrNotRegular). No retry can turn such a path into valid
// state, so treating it as an unclassified read fault would arm the Save block
// forever and freeze the library snapshot, mapping cache, AniList memo and
// finding dedupe until an operator intervened.
func TestStoreLoadNonRegularPathIsClassifiedCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("create directory at the state path: %v", err)
	}
	store := NewStore(path, testLogger())

	_, err := store.Load(t.Context())
	if err == nil {
		t.Fatal("Load of a non-regular state path returned nil error, want the corruption reported")
	}
	if !errors.Is(err, atomicfile.ErrNotRegular) {
		t.Errorf("error = %v, want it to match atomicfile.ErrNotRegular", err)
	}
	if _, statErr := os.Stat(path + ".corrupt"); statErr != nil {
		t.Errorf("non-regular state path was not quarantined (stat err = %v), want %s.corrupt preserved", statErr, path)
	}
	if saveErr := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); saveErr != nil {
		t.Fatalf("Save after quarantining a non-regular path failed: %v (deterministic corruption must not arm the preservation block)", saveErr)
	}
	if got, loadErr := store.Load(t.Context()); loadErr != nil || got.ShrunkWalksByArr[library.ArrSonarr] != 1 {
		t.Errorf("Load after the recovering Save = (%+v, %v), want the freshly saved state at the live path", got, loadErr)
	}
}

// TestStoreLoadUnpreservableCorruptionBlocksSave pins the fail-closed side of
// the quarantine contract: when preservation cannot happen (a non-empty
// state.json.corrupt directory blocks the rename), the corrupt live file is
// the ONLY forensic copy of the persisted cache, so Save must
// refuse rather than atomically replace it with a cold envelope.
func TestStoreLoadUnpreservableCorruptionBlocksSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	corrupt := []byte("{not json")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}
	// A NON-EMPTY directory at the quarantine destination: os.Rename cannot
	// replace it, so quarantine fails while the live file stays corrupt.
	if err := os.Mkdir(path+".corrupt", 0o750); err != nil {
		t.Fatalf("create quarantine blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path+".corrupt", "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("occupy quarantine blocker: %v", err)
	}
	store := NewStore(path, testLogger())

	if _, err := store.Load(t.Context()); err == nil {
		t.Fatal("Load of a corrupt state returned nil error, want the decode failure reported")
	}
	saveErr := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})
	if !errors.Is(saveErr, ErrSavePreserved) {
		t.Fatalf("Save after a FAILED quarantine = %v, want ErrSavePreserved (the corrupt bytes are still the only copy)", saveErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the live state file: %v", readErr)
	}
	if !bytes.Equal(got, corrupt) {
		t.Errorf("live state file = %q, want the original corrupt bytes %q preserved", got, corrupt)
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
	if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Fatalf("seed valid state: %v", err)
	}

	huge := &State{Memo: paddedMemo(maxStateBytes + 1)}
	err := store.Save(t.Context(), huge)
	if err == nil {
		t.Fatal("Save returned nil error, want over-cap rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want size-cap context", err.Error())
	}

	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load after rejected Save returned error: %v", err)
	}
	if got.ShrunkWalksByArr[library.ArrSonarr] != 1 {
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
	if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Fatalf("seed valid state: %v", err)
	}

	padded := func(n int) *State {
		// Version mirrors the SchemaVersion stamp Save applies to the copy it
		// writes, so the json.Marshal probe below measures the on-disk shape.
		return &State{Memo: paddedMemo(n), Version: SchemaVersion}
	}
	base, err := json.Marshal(padded(0))
	if err != nil {
		t.Fatalf("marshal boundary probe: %v", err)
	}
	exact := padded(maxStateBytes - len(base))

	if err := store.Save(t.Context(), exact); err != nil {
		t.Fatalf("Save of an exactly-maxStateBytes state was rejected: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved state: %v", err)
	}
	if info.Size() != maxStateBytes {
		t.Errorf("saved file is %d bytes, want exactly %d (encoder newline must be truncated away)", info.Size(), maxStateBytes)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load of the boundary-sized state: %v", err)
	}
	if gotLen := len(got.Memo.Entries[1].Titles[0]); gotLen != maxStateBytes-len(base) {
		t.Errorf("round-tripped memo title length = %d, want %d", gotLen, maxStateBytes-len(base))
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

	err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})
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
	st, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load with nil logger returned error: %v", err)
	}
	if len(st.ShrunkWalksByArr) != 0 || len(st.Memo.Entries) != 0 {
		t.Errorf("Load = %+v, want zero state", st)
	}
	if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Fatalf("Save with nil logger returned error: %v", err)
	}
}

// TestStoreQuarantineRenameFailureWarnsAndKeepsFile pins quarantine's
// preservation contract on the rename-failure path: when the corrupt file
// cannot be renamed aside (the .corrupt destination is occupied by a
// directory, a root-safe injection), Load still returns the decode error,
// the corrupt file stays at the live path, and the failure is logged at Warn
// once rather than raised as a Load error. The Save consequence of that
// failure - the preservation block - is pinned by
// TestStoreLoadUnpreservableCorruptionBlocksSave.
func TestStoreQuarantineRenameFailureWarnsAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	if err := os.Mkdir(path+".corrupt", 0o755); err != nil {
		t.Fatalf("create rename blocker: %v", err)
	}
	logger, recorder := capture.New()
	_, err := NewStore(path, logger).Load(t.Context())
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

// TestStoreSaveNilReturnsErrorWithoutWriting pins Save's nil-state guard:
// without it json.Marshal accepts the nil pointer as literal null, writing a
// state file Load immediately treats as corruption (discarding the previous
// cache), so Save(nil) must reject and leave no file behind.
func TestStoreSaveNilReturnsErrorWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	err := NewStore(path, testLogger()).Save(t.Context(), nil)
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
	if err := store.Save(t.Context(), &State{
		ShrunkWalksByArr: map[string]int{library.ArrSonarr: wantShrunk},
		SeadexFailures:   wantSeadex,
		AniListDegraded:  wantAniList,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if got.ShrunkWalksByArr[library.ArrSonarr] != wantShrunk {
		t.Errorf("ShrunkWalksByArr[sonarr] after disk round trip = %d, want %d", got.ShrunkWalksByArr[library.ArrSonarr], wantShrunk)
	}
	if got.SeadexFailures != wantSeadex {
		t.Errorf("SeadexFailures after disk round trip = %d, want %d", got.SeadexFailures, wantSeadex)
	}
	if got.AniListDegraded != wantAniList {
		t.Errorf("AniListDegraded after disk round trip = %d, want %d", got.AniListDegraded, wantAniList)
	}
}

// TestStorePartialWalkStreakPersistsUnderStableWireKey pins the fourth
// persisted degradation streak: internal/scout escalates a permanently partial
// library walk from WARN to ERROR only if the streak survives restarts, so a
// json-tag rename or a Store projection omission must fail here. The raw
// fixture pins the wire key on the load side and the post-Save envelope check
// pins it on the write side.
func TestStorePartialWalkStreakPersistsUnderStableWireKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const body = `{"partial_walks":7}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write persisted state fixture: %v", err)
	}
	store := NewStore(path, testLogger())
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.PartialWalks != 7 {
		t.Errorf("PartialWalks from persisted envelope = %d, want 7", got.PartialWalks)
	}
	if err := store.Save(t.Context(), &got); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(persisted, &envelope); err != nil {
		t.Fatalf("decode persisted envelope: %v", err)
	}
	if value := string(envelope["partial_walks"]); value != "7" {
		t.Errorf("persisted partial_walks = %s, want 7", value)
	}
}

// TestStoreSaveEnvelopeNestedShape pins the wire shape of the persisted
// members other packages own (match.Memo, library.Snapshot).
// Their json tags define state.json's schema while SchemaVersion, the
// discriminator that governs a rename, lives here - so a tag moved on the
// domain side must fail in this package rather than silently zero-load out of
// every existing state file at the next deploy. mapping.Cache's validator keys
// are already pinned by the raw fixture in
// TestStoreLoadReadsPersistedValidatorsAndPartialWalk. If this test fails
// because a member was renamed or moved deliberately, bump SchemaVersion in
// the same commit (see its doc) and update the expectations below. The
// assertions are on the KEY SET only, never the values, so ordinary value
// changes do not churn them.
func TestStoreSaveEnvelopeNestedShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path, testLogger())
	st := &State{
		Memo: match.Memo{Entries: map[int]match.MemoEntry{1: {
			Expiry:   time.Now().UTC().Add(time.Hour),
			Format:   "TV",
			Titles:   []string{"t"},
			Year:     2020,
			NotFound: true,
		}}},
		Library: library.Snapshot{
			TakenAt: time.Now().UTC(),
			Items: []library.Item{{
				SeasonGroups: map[int][]string{1: {"g"}},
				Arr:          "sonarr",
				ImdbID:       "tt1",
				Title:        "t",
				ArrURL:       "http://sonarr.local/series/t",
				// Every release fingerprint field is populated: they are all
				// omitempty, so a zero Current would write an empty object and
				// a renamed nested tag would stay invisible to the key-set
				// assertion below.
				Current: release.Release{
					Group:       "g",
					Tracker:     "Nyaa",
					Resolution:  "1080p",
					Codec:       "x265",
					Kind:        release.KindRemux,
					TrackerType: tracker.Public,
					Reason:      "name/notes marker: remux",
					DualAudio:   true,
				},
				AltTitles: []string{"alt"},
				Groups:    []string{"g"},
				ArrID:     1,
				TvdbID:    2,
				TmdbID:    3,
				Year:      2020,
				HasFile:   true,
				Failed:    true,
			}},
			Partial: true,
		},
	}
	if err := store.Save(t.Context(), st); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	var envelope struct {
		Memo struct {
			Entries map[string]map[string]json.RawMessage `json:"entries"`
		} `json:"anilist_memo"`
		Library map[string]json.RawMessage `json:"library"`
	}
	if err := json.Unmarshal(persisted, &envelope); err != nil {
		t.Fatalf("decode persisted envelope: %v", err)
	}
	// The snapshot's own members are pinned too, not just its items: a renamed
	// taken_at would otherwise be invisible here (the older raw fixture in
	// TestStoreLoadReadsPersistedValidatorsAndPartialWalk carries a zero
	// timestamp) and a renamed partial would zero-load a complete-looking
	// library out of every existing state file.
	wantSnapshotKeys := []string{"items", "partial", "taken_at"}
	if keys := slices.Sorted(maps.Keys(envelope.Library)); !slices.Equal(keys, wantSnapshotKeys) {
		t.Errorf("persisted library snapshot keys = %v, want %v (a deliberate rename needs a SchemaVersion bump)", keys, wantSnapshotKeys)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Library["items"], &items); err != nil {
		t.Fatalf("decode persisted library items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("persisted library.items = %d, want 1", len(items))
	}
	wantItemKeys := []string{
		"alt_titles", "arr", "arr_id", "arr_url", "current", "failed", "groups",
		"has_file", "imdb_id", "season_groups", "title", "tmdb_id", "tvdb_id", "year",
	}
	if keys := slices.Sorted(maps.Keys(items[0])); !slices.Equal(keys, wantItemKeys) {
		t.Errorf("persisted library item keys = %v, want %v (a deliberate rename needs a SchemaVersion bump)", keys, wantItemKeys)
	}
	// The nested release fingerprint is the state file's comparison baseline:
	// a renamed field there silently zero-loads a group or tracker and
	// re-alerts every finding, so its keys are pinned like the outer members.
	var current map[string]json.RawMessage
	if err := json.Unmarshal(items[0]["current"], &current); err != nil {
		t.Fatalf("decode persisted library item current: %v", err)
	}
	wantCurrentKeys := []string{
		"codec", "dual_audio", "group", "kind", "reason", "resolution", "tracker", "tracker_type",
	}
	if keys := slices.Sorted(maps.Keys(current)); !slices.Equal(keys, wantCurrentKeys) {
		t.Errorf("persisted library item current keys = %v, want %v (a deliberate rename needs a SchemaVersion bump)", keys, wantCurrentKeys)
	}
	entry, ok := envelope.Memo.Entries["1"]
	if !ok {
		t.Fatalf("persisted anilist_memo.entries missing id 1 (got %v)", slices.Sorted(maps.Keys(envelope.Memo.Entries)))
	}
	wantEntryKeys := []string{
		"expiry", "format", "not_found", "titles", "year",
	}
	if keys := slices.Sorted(maps.Keys(entry)); !slices.Equal(keys, wantEntryKeys) {
		t.Errorf("persisted anilist_memo entry keys = %v, want %v (a deliberate rename needs a SchemaVersion bump)", keys, wantEntryKeys)
	}
}

// TestStoreLoadIgnoresRetiredScalarShrunkWalks pins the one compatibility
// property the per-arr shrink streak's field RENAME rests on: a state.json
// written by a build that persisted the retired scalar `shrunk_walks` must load
// CLEANLY - the unknown key is ignored, the file is not quarantined, and every
// other member (the expensive AniList memo above all) survives.
//
// It is pinned because the alternative shape was a trap. Re-typing the existing
// key from int to object would have made json.Unmarshal fail on such a file,
// and decode treats an unmarshal failure as corruption: the operator's state
// would be renamed aside and the memo rebuilt over a measured ~25-minute cold
// reconcile. Losing the streak instead is the cheap half of that trade (a
// transient counter, at most one extra cycle of tolerance), and the app's
// no-rollback-no-migration decision covers exactly it.
func TestStoreLoadIgnoresRetiredScalarShrunkWalks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"version":1,"shrunk_walks":4,"seadex_failures":2,"anilist_memo":{"entries":{"154587":{"titles":["Frieren"],"format":"TV","year":2023}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	store := NewStore(path, testLogger())
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load of a file carrying the retired scalar shrunk_walks returned error: %v (an old file must never be quarantined over a retired key)", err)
	}
	if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("quarantine stat = %v, want not exist (a retired key is not corruption)", statErr)
	}
	if len(got.ShrunkWalksByArr) != 0 {
		t.Errorf("ShrunkWalksByArr = %v, want empty (the retired scalar carries no per-arr attribution, so it is dropped)", got.ShrunkWalksByArr)
	}
	if got.SeadexFailures != 2 {
		t.Errorf("SeadexFailures = %d, want 2 (the sibling streaks are unaffected by the rename)", got.SeadexFailures)
	}
	if len(got.Memo.Entries) != 1 {
		t.Errorf("memo entries = %d, want 1 (the memo is the member a quarantine would cost ~25 minutes to rebuild)", len(got.Memo.Entries))
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
	st := &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}
	if err := store.Save(t.Context(), st); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(t.Context())
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
	if err := os.WriteFile(path, []byte(`{"shrunk_walks_by_arr":{"sonarr":3}}`), 0o644); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	legacy, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load of a legacy pre-version file returned error: %v", err)
	}
	if legacy.Version != 0 || legacy.ShrunkWalksByArr[library.ArrSonarr] != 3 {
		t.Errorf("legacy load = Version %d ShrunkWalksByArr[sonarr] %d, want 0/3 (absent version tolerated)", legacy.Version, legacy.ShrunkWalksByArr[library.ArrSonarr])
	}

	// The same envelope with the version stamped EXPLICITLY zero: the contract
	// treats absent and zero alike, and zero is the one value that sits between
	// the negative stamps quarantined as corruption and the versions this binary
	// supports, so refusing it would quarantine a legitimate legacy file.
	if err := os.WriteFile(path, []byte(`{"version":0,"shrunk_walks_by_arr":{"sonarr":4}}`), 0o644); err != nil {
		t.Fatalf("write zero-version state: %v", err)
	}
	zeroStamped, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load of an explicitly zero-version file returned error: %v", err)
	}
	if zeroStamped.Version != 0 || zeroStamped.ShrunkWalksByArr[library.ArrSonarr] != 4 {
		t.Errorf("zero-version load = Version %d ShrunkWalksByArr[sonarr] %d, want 0/4 (an explicit zero is the legacy shape)",
			zeroStamped.Version, zeroStamped.ShrunkWalksByArr[library.ArrSonarr])
	}

	// A file stamped by a NEWER binary (an image rollback) must be refused,
	// not field-by-field zero-loaded: its members may have moved. It is valid
	// state, not corruption, so it stays at the live path (no .corrupt copy)
	// and every subsequent Save on this Store is refused — otherwise this
	// binary would overwrite the newer-schema file with a cold envelope and
	// rolling forward would silently lose the newer state.
	newer := fmt.Sprintf(`{"version":%d,"shrunk_walks_by_arr":{"sonarr":1}}`, SchemaVersion+1)
	if err := os.WriteFile(path, []byte(newer), 0o644); err != nil {
		t.Fatalf("write newer-version state: %v", err)
	}
	_, loadErr := store.Load(t.Context())
	if loadErr == nil {
		t.Fatal("Load of a newer-schema file returned nil error, want refusal")
	}
	wantFile := fmt.Sprintf("schema version %d", SchemaVersion+1)
	wantSupported := fmt.Sprintf("(%d)", SchemaVersion)
	if !strings.Contains(loadErr.Error(), wantFile) || !strings.Contains(loadErr.Error(), wantSupported) {
		t.Errorf("error = %q, want both the file's version (%q) and the supported version (%q) named",
			loadErr.Error(), wantFile, wantSupported)
	}
	if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("newer-schema file quarantined (stat err = %v), want it preserved at the live path", statErr)
	}
	live, readErr := os.ReadFile(path)
	if readErr != nil || string(live) != newer {
		t.Errorf("live state file after newer-schema load = %q (err %v), want the original bytes preserved", live, readErr)
	}
	if saveErr := store.Save(t.Context(), &State{}); saveErr == nil {
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
	if err := store.Save(t.Context(), &State{Library: library.Snapshot{TakenAt: taken}}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if _, err := store.Load(t.Context()); err != nil {
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
	if err := zeroStore.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if _, err := zeroStore.Load(t.Context()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, found := libraryAge(zeroRecorder); found {
		t.Error("\"state loaded\" carries a library_age attribute for a zero TakenAt, want it omitted")
	}

	// A snapshot whose TakenAt lies in the FUTURE (a backward host clock step,
	// or a hand-edited state file) is clamped to zero rather than logged as a
	// misleading negative age.
	futureLogger, futureRecorder := capture.New()
	futureStore := NewStore(filepath.Join(t.TempDir(), "state.json"), futureLogger)
	future := library.Snapshot{TakenAt: time.Now().Add(time.Hour).UTC()}
	if err := futureStore.Save(t.Context(), &State{Library: future}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if _, err := futureStore.Load(t.Context()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	futureAge, found := libraryAge(futureRecorder)
	if !found {
		t.Fatal("\"state loaded\" carries no library_age attribute for a future TakenAt, want the clamped age")
	}
	if futureAge != "0s" {
		t.Errorf("library_age for a future TakenAt = %q, want \"0s\" (clamped; a negative age is never logged)", futureAge)
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

	err := store.Save(ctx, &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})
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

	err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})
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
	got, err := NewStore(path, testLogger()).Load(t.Context())
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
	if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
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
			if err := os.WriteFile(path, []byte(`{"version":1,"seadex_failures":1}`), 0o600); err != nil {
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
			if _, err := store.Load(t.Context()); err == nil {
				t.Fatal("Load of newer-schema state returned nil error, want refusal")
			}
			if err := store.Save(t.Context(), &State{}); err == nil {
				t.Fatal("Save while blocked returned nil error, want refusal")
			}
			tt.recover(t, path)
			if _, err := store.Load(t.Context()); err != nil {
				t.Fatalf("Load after recovery returned error: %v", err)
			}
			if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
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
	huge := &State{Memo: paddedMemo(maxStateBytes + 1)}
	stamped := *huge
	stamped.Version = SchemaVersion
	encoded, err := json.Marshal(&stamped)
	if err != nil {
		t.Fatalf("marshal size probe: %v", err)
	}
	saveErr := store.Save(t.Context(), huge)
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
	if _, err := store.Load(t.Context()); err == nil {
		t.Fatal("Load of newer-schema state returned nil error, want refusal")
	}
	if err := store.Save(t.Context(), &State{}); err == nil {
		t.Fatal("Save while blocked returned nil error, want refusal")
	}

	// The live file is later replaced by corruption: Load must quarantine it
	// AND clear the remembered newer-schema block (the block is documented as
	// describing what the LAST Load found at the live path), so the daemon
	// resumes persisting instead of silently re-baselining every run.
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	if _, err := store.Load(t.Context()); err == nil {
		t.Fatal("Load of corrupt state returned nil error, want decode error")
	}
	assertQuarantined(t, path, "null")
	if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Errorf("Save after a corrupt Load still blocked: %v (maybeQuarantine must clear the newer-schema block once the live file is positively classified corrupt)", err)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load after unblocked Save returned error: %v", err)
	}
	if got.ShrunkWalksByArr[library.ArrSonarr] != 1 {
		t.Error("re-loaded state lost ShrunkWalksByArr, want the unblocked Save persisted")
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
		if _, err := NewStore(filepath.Join(dir, "state.json"), testLogger()).Load(t.Context()); err != nil {
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
		if _, err := NewReadOnlyStore(filepath.Join(dir, "state.json"), testLogger()).Load(t.Context()); err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if _, err := os.Stat(stale); err != nil {
			t.Errorf("stale temp after read-only Load: stat err = %v, want left in place (the report flow is documented read-only on the state dir)", err)
		}
	})
}

// TestStoreLoadNonDirectoryStateDirSurfacesAsReadError pins Load's handling of
// a state directory that is not a directory at all (the "directory" is a
// regular file, a root-safe injection): the confined open fails, so Load
// reports it as a classified read error rather than a cold start, and the
// stale-temp sweep - which now runs THROUGH that root - never runs at all.
//
// It no longer asserts a cleanup-failure WARN: the ambient sweep this test was
// written against ran BEFORE the root was opened and could fail its own readdir
// on a non-directory. The sweep is now pinned to the same root the
// read/classify/preserve decision uses, so an unopenable state directory leaves
// nothing for it to attempt and there is no maintenance failure to report.
func TestStoreLoadNonDirectoryStateDirSurfacesAsReadError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}
	logger, recorder := capture.New()
	_, err := NewStore(filepath.Join(blocker, "state.json"), logger).Load(t.Context())
	if err == nil {
		t.Fatal("Load with an unreadable state dir returned nil error, want read error")
	}
	if !strings.Contains(err.Error(), "state: read") {
		t.Errorf("error = %q, want 'state: read' context (an unopenable state directory is a read failure, not a cold start)", err.Error())
	}
	if got := recorder.CountExact("could not clean stale atomic-write temp files"); got != 0 {
		t.Errorf("cleanup-failure WARN count = %d, want 0 (the sweep is pinned to a root that could not be opened, so it never ran)", got)
	}
}

// TestEncodeStateWriteErrorWrapped pins encodeState's generic (non-size)
// error path: an I/O failure from the pending temp (here the fd is already
// closed via Cleanup, standing in for a disk error) surfaces wrapped as
// "state: encode <path>", distinct from the over-cap rejection message.
func TestEncodeStateWriteErrorWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	pf, err := atomicfile.NewPendingFile(t.Context(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	encErr := encodeState(pf, &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}, path)
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
			_, err := store.Load(t.Context())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a malformed version value")
			}
			if strings.Contains(err.Error(), "newer than this binary supports") {
				t.Errorf("error = %q, want plain decode error, not the newer-schema classification", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(t.Context(), &State{}); saveErr != nil {
				t.Errorf("Save after quarantining a malformed-version-value file failed: %v", saveErr)
			}
		})
	}
}

// TestStoreLoadStateFieldTypeMismatchQuarantines pins decode's final gate: a
// payload that passes the UTF-8, object-envelope, and version-discriminator
// checks but fails the State unmarshal on a member type mismatch
// ({"library":"x"} / {"anilist_memo":[]}) is corruption Save can never
// produce. It must quarantine with the original bytes preserved and the
// following Save unblocked - the daemon persists a fresh envelope instead of
// reading a poisoned file forever.
func TestStoreLoadStateFieldTypeMismatchQuarantines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bool member holds a string", `{"library":{"partial":"yes"}}`},
		{"map member holds an array", `{"anilist_memo":[]}`},
		{"struct member holds a string", `{"library":"not-an-object"}`},
		{"map member holds a number", `{"shrunk_walks_by_arr":3}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			store := NewStore(path, testLogger())
			_, err := store.Load(t.Context())
			if err == nil {
				t.Fatal("Load returned nil error, want decode error for a State field type mismatch")
			}
			if !strings.Contains(err.Error(), "decode") {
				t.Errorf("error = %q, want decode context", err.Error())
			}
			assertQuarantined(t, path, tt.body)
			if saveErr := store.Save(t.Context(), &State{}); saveErr != nil {
				t.Errorf("Save after quarantining a type-mismatched file failed: %v", saveErr)
			}
		})
	}
}

// TestStoreLoadCanceledReadBlocksSaveUntilClassified pins the
// unclassified-read-failure preservation posture: a canceled read is an
// UNCLASSIFIED failure (like EACCES/EIO), so after a Load under a
// pre-canceled context the on-disk bytes must be preserved by refusing Save
// until a later Load classifies the file. Cancellation is the injection this
// posture is pinned with because it needs no permission trickery and so works
// in any environment, root included.
func TestStoreLoadCanceledReadBlocksSaveUntilClassified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"shrunk_walks_by_arr":{"sonarr":1}}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	store := NewStore(path, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load canceled context error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("corrupt quarantine after an unclassified read stat error = %v, want not exist (cancellation is not corruption)", statErr)
	}
	err := store.Save(t.Context(), &State{})
	if err == nil {
		t.Fatal("Save after a canceled (unclassified) read = nil error, want the preservation block")
	}
	if !strings.Contains(err.Error(), "unclassified read failure") {
		t.Errorf("blocked Save error = %v, want it to name the unclassified read failure", err)
	}
	live, readErr := os.ReadFile(path)
	if readErr != nil || string(live) != `{"shrunk_walks_by_arr":{"sonarr":1}}` {
		t.Errorf("live state after blocked Save = %q (err %v), want the original bytes preserved", live, readErr)
	}
	got, loadErr := store.Load(t.Context())
	if loadErr != nil {
		t.Fatalf("Load after recovery returned error: %v", loadErr)
	}
	if got.ShrunkWalksByArr[library.ArrSonarr] != 1 {
		t.Error("re-loaded state lost ShrunkWalksByArr, want the preserved file read back")
	}
	if saveErr := store.Save(t.Context(), &got); saveErr != nil {
		t.Errorf("Save after a classifying Load = %v, want the block cleared", saveErr)
	}
}

// TestStoreSavePreservationRefusalsMatchErrSavePreserved pins the
// preservation sentinel itself, not just the refusal text: scout.save
// classifies a refused Save with errors.Is(err, state.ErrSavePreserved) to
// log it at WARN instead of ERROR, so an unwrapped refusal turns a routine
// redeploy (a SIGTERM landing in Load's read window arms the block) into a
// cycle-error alert. Both documented blocks must carry the sentinel, and a
// genuine write fault must NOT, or the same classification would silence a
// real persistence failure.
func TestStoreSavePreservationRefusalsMatchErrSavePreserved(t *testing.T) {
	t.Run("newer-schema block", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		newer := fmt.Sprintf(`{"version":%d,"shrunk_walks_by_arr":{"sonarr":1}}`, SchemaVersion+1)
		if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
			t.Fatalf("write newer-schema state: %v", err)
		}
		store := NewStore(path, testLogger())
		if _, err := store.Load(t.Context()); err == nil {
			t.Fatal("Load of a newer-schema file returned nil error, want refusal")
		}
		err := store.Save(t.Context(), &State{})
		if !errors.Is(err, ErrSavePreserved) {
			t.Errorf("Save error = %v, want errors.Is(err, ErrSavePreserved) (scout.save logs an unmatched refusal at ERROR and fires the cycle-error alert)", err)
		}
	})

	t.Run("unclassified-read-failure block", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(`{"seadex_failures":1}`), 0o600); err != nil {
			t.Fatalf("write state: %v", err)
		}
		store := NewStore(path, testLogger())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load with a canceled context error = %v, want context.Canceled", err)
		}
		err := store.Save(t.Context(), &State{})
		if !errors.Is(err, ErrSavePreserved) {
			t.Errorf("Save error = %v, want errors.Is(err, ErrSavePreserved) (this is the redeploy path that must not page the operator)", err)
		}
	})

	t.Run("a genuine write fault is not a preservation refusal", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatalf("create blocker file: %v", err)
		}
		store := NewStore(filepath.Join(blocker, "state.json"), testLogger())
		err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})
		if err == nil {
			t.Fatal("Save returned nil error, want a write failure")
		}
		if errors.Is(err, ErrSavePreserved) {
			t.Errorf("write fault %v matches ErrSavePreserved, want a fault classification (a real persistence failure must stay at ERROR)", err)
		}
	})
}

// TestStoreSaveWarnsApproachingSizeLimit pins the pre-cliff size warning,
// the operator's only notice before the shared maxStateBytes bound starts
// refusing every Save and freezes the persisted cache: a staged encoding past
// stateSizeWarnBytes (80% of the cap) warns exactly once, and an ordinary
// state stays silent so the warning keeps meaning something.
func TestStoreSaveWarnsApproachingSizeLimit(t *testing.T) {
	const wantMsg = "state file approaching the size limit; a Save that exceeds it is refused and the persisted cache freezes"
	padded := func(n int) *State {
		// Version mirrors the SchemaVersion stamp Save applies to the copy it
		// writes, so the json.Marshal probe below measures the staged shape.
		return &State{Memo: paddedMemo(n), Version: SchemaVersion}
	}
	base, err := json.Marshal(padded(0))
	if err != nil {
		t.Fatalf("marshal size probe: %v", err)
	}
	sized := func(total int) *State { return padded(total - len(base)) }

	t.Run("crossing the threshold warns once", func(t *testing.T) {
		logger, recorder := capture.New()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
		if err := store.Save(t.Context(), sized(stateSizeWarnBytes+1024)); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		if got := recorder.CountExact(wantMsg); got != 1 {
			t.Errorf("pre-cliff WARN count = %d, want 1 (without it the cache freezes at the cap with no prior signal)", got)
		}
	})

	t.Run("an ordinary state stays quiet", func(t *testing.T) {
		logger, recorder := capture.New()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
		if err := store.Save(t.Context(), sized(stateSizeWarnBytes/2)); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		if got := recorder.CountExact(wantMsg); got != 0 {
			t.Errorf("pre-cliff WARN count = %d for a half-sized state, want 0 (a warning on every save is noise)", got)
		}
	})

	t.Run("a state exactly at the threshold stays quiet", func(t *testing.T) {
		// encodeState truncates the encoder's newline away and the staged count
		// re-syncs with it, so the guard reads exactly the marshalled length.
		// The threshold is the point the warning starts being warranted PAST,
		// and only this row can say which side of it the equal case falls on.
		logger, recorder := capture.New()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
		if err := store.Save(t.Context(), sized(stateSizeWarnBytes)); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		if got := recorder.CountExact(wantMsg); got != 0 {
			t.Errorf("pre-cliff WARN count = %d for a state exactly at the threshold, want 0 (the warning is for crossing it)", got)
		}
	})
}

// TestStoreSaveStaysQuietOnASuccessfulWrite pins the other side of Save's temp
// lifecycle: Cleanup is a no-op after a successful Commit, so a healthy Save
// warns about nothing. The deferred cleanup runs on EVERY save, so a warning
// misread from its nil error would fire once per cycle and read as a permanent
// temp-file leak in the state directory - the same alert-fatigue failure the
// pre-cliff warning above is careful to avoid.
func TestStoreSaveStaysQuietOnASuccessfulWrite(t *testing.T) {
	logger, recorder := capture.New()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), logger)

	if err := store.Save(t.Context(), &State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	if got := recorder.CountLevel(slog.LevelWarn, ""); got != 0 {
		t.Errorf("WARN count after a successful Save = %d, want 0; captured messages: %q", got, recorder.Messages())
	}
}

// TestStoreLoadStaysQuietOnAHealthyRead pins that a healthy Load narrates
// itself and nothing else: it warns about neither the directory handle it
// closes nor the stale-temp sweep it runs, and it reports a reclaim only when
// one happened. Load runs once per cold start and on every report, so a line
// that fires unconditionally is the one that buries the real one - the sweep's
// Info exists precisely to name a reclaim an operator can act on.
func TestStoreLoadStaysQuietOnAHealthyRead(t *testing.T) {
	const reclaimMsg = "reclaimed stale atomic-write temp files"
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"shrunk_walks_by_arr":{"sonarr":3}}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	logger, recorder := capture.New()
	store := NewStore(path, logger)

	if _, err := store.Load(t.Context()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := recorder.CountLevel(slog.LevelWarn, ""); got != 0 {
		t.Errorf("WARN count after a healthy Load = %d, want 0; captured messages: %q", got, recorder.Messages())
	}
	if got := recorder.CountExact(reclaimMsg); got != 0 {
		t.Errorf("reclaim INFO count = %d with no stale temp to reclaim, want 0", got)
	}
	if got := recorder.CountExact("state loaded"); got != 1 {
		t.Errorf("state-loaded INFO count = %d, want 1; captured messages: %q", got, recorder.Messages())
	}
}

// TestStoreLoadRefusesStatePathEscapingItsDirectory pins readState's
// confinement contract: the state file is opened through an os.Root rooted at
// its parent directory, so a state path that resolves OUTSIDE that directory
// (a symlink planted at /config/state.json) is a read error Load classifies,
// never a silent load of foreign bytes as the library snapshot, mapping cache,
// AniList memo and degradation streaks. The escaping path is a
// DETERMINISTIC failure (no retry makes a foreign inode readable), so Load
// classifies it as corruption: the link itself is quarantined, its target is
// left untouched, and Save resumes on the fresh regular file rather than being
// blocked forever by the recoverable-fault gate.
func TestStoreLoadRefusesStatePathEscapingItsDirectory(t *testing.T) {
	const foreign = `{"seadex_failures":9}`
	target := filepath.Join(t.TempDir(), "foreign.json")
	if err := os.WriteFile(target, []byte(foreign), 0o600); err != nil {
		t.Fatalf("write foreign state: %v", err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}

	store := NewStore(path, testLogger())
	got, err := store.Load(t.Context())
	if err == nil {
		t.Fatalf("Load through an escaping symlink returned nil error and state %+v, want the confinement refusal", got)
	}
	if len(got.ShrunkWalksByArr) != 0 {
		t.Error("Load returned the foreign file's state, want the confined open to refuse it")
	}
	if !strings.Contains(err.Error(), "state: read") {
		t.Errorf("error = %q, want the 'state: read' classification", err.Error())
	}
	// The link itself is renamed aside (os.Rename never follows it), so the
	// foreign target keeps its bytes while the live path is freed for Save.
	info, lstatErr := os.Lstat(path + ".corrupt")
	if lstatErr != nil {
		t.Fatalf("escaping path not quarantined (lstat err = %v), want the foreign inode preserved aside", lstatErr)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("quarantined inode mode = %v, want the symlink itself preserved", info.Mode())
	}
	live, readErr := os.ReadFile(target)
	if readErr != nil || string(live) != foreign {
		t.Errorf("symlink target after Load = %q (err %v), want the foreign file untouched", live, readErr)
	}
	if saveErr := store.Save(t.Context(), &State{}); saveErr != nil {
		t.Errorf("Save after a quarantined escaping path = %v, want persistence to resume", saveErr)
	}
	after, readErr := os.ReadFile(target)
	if readErr != nil || string(after) != foreign {
		t.Errorf("symlink target after Save = %q (err %v), want Save's temp+rename to replace the link, not write through it", after, readErr)
	}
}

// TestStoreLoadTransientFailureKeepsInDirectorySymlink pins that a symlink
// which stays inside the state directory is NOT classified as corruption: the
// confined read follows it, so a cancelled read over one is the recoverable
// fault it always was - quarantining it would destroy an operator's
// deliberate redirection on the next redeploy.
func TestStoreLoadTransientFailureKeepsInDirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state-real.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("write real state: %v", err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.Symlink("state-real.json", path); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	store := NewStore(path, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load with a canceled context error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Lstat(path + ".corrupt"); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("in-directory symlink quarantined (lstat err = %v), want it left in place", statErr)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("state path after a canceled read = %v (err %v), want the symlink intact", info, err)
	}
	if saveErr := store.Save(t.Context(), &State{}); !errors.Is(saveErr, ErrSavePreserved) {
		t.Errorf("Save after a transient read failure = %v, want ErrSavePreserved", saveErr)
	}
}
