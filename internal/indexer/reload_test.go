package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestReloadWarnsOnceOnMissingSnapshotAndRecovers pins the disappeared-snapshot
// state machine: once a feed was loaded, a deleted snapshot file warns exactly
// once (not per request) while the last loaded feed keeps serving, and the
// file's reappearance logs the recovery and resumes reloads with the new feed.
func TestReloadWarnsOnceOnMissingSnapshotAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash:   map[string]bool{},
		ByKey:    map[string]bool{},
		NyaaFeed: []item{{Title: "first", GUID: "https://nyaa.si/view/1"}},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	ix.reload(context.Background())
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot missing"); got != 1 {
		t.Errorf("missing-snapshot warned %d times across two reloads, want exactly 1 (warn once, then stay quiet); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Errorf("feed after disappearance = %+v, want the last loaded feed kept", got)
	}

	writeSnapshotFile(t, path, &snapshot{
		ByHash:   map[string]bool{},
		ByKey:    map[string]bool{},
		NyaaFeed: []item{{Title: "second", GUID: "https://nyaa.si/view/2"}},
	})
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot reappeared"); got != 1 {
		t.Errorf("reappearance logged %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "second" {
		t.Errorf("feed after reappearance = %+v, want the new snapshot served", got)
	}
}

// TestReloadRecoversDegradationOnUnchangedSnapshot pins the reloadDegraded
// state machine across a stat fault whose recovery leaves the snapshot
// untouched: the file is still the already-loaded inode at the same mtime, so
// the unchanged-snapshot fast path would skip the read that clears the flag —
// recovery would never log and the next degradation onset's warning would be
// suppressed by the stale flag. A degraded reload forces one real read:
// exactly one recovery INFO on the recovered pass, and a fresh WARN on the
// next onset.
func TestReloadRecoversDegradationOnUnchangedSnapshot(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash:   map[string]bool{},
		ByKey:    map[string]bool{},
		NyaaFeed: []item{{Title: "first", GUID: "https://nyaa.si/view/1"}},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// Onset: swap the parent directory for a regular file so os.Stat fails
	// with ENOTDIR (non-ENOENT, root-safe), then recover by restoring the
	// directory — the snapshot file keeps its inode and mtime throughout.
	aside := filepath.Join(dir, "sub-aside")
	blockDir := func() {
		if err := os.Rename(sub, aside); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sub, []byte("blocker"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restoreDir := func() {
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(aside, sub); err != nil {
			t.Fatal(err)
		}
	}

	blockDir()
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot stat failed"); got != 1 {
		t.Fatalf("stat-failure warned %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}

	restoreDir()
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot reload recovered"); got != 1 {
		t.Errorf("recovery logged %d times after the stat fault cleared, want exactly 1; log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}

	blockDir()
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot stat failed"); got != 2 {
		t.Errorf("stat-failure warned %d times across two onsets, want 2 (a cleared flag must re-arm the warning); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadMemoizedMalformedSnapshotClearsDegradation pins the interaction
// of the malformed-file memo with the reloadDegraded state machine: once a
// deterministic malformed snapshot is memoized (failedFile), a transient stat
// fault and its recovery must NOT defeat the memo — the recovered stat clears
// only the degradation flag, without rereading the unchanged bad file,
// without repeating the malformed WARN per request, and without a false
// "reload recovered" INFO (nothing was reloaded) — while the next stat-fault
// onset still warns afresh.
func TestReloadMemoizedMalformedSnapshotClearsDegradation(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash:   map[string]bool{},
		ByKey:    map[string]bool{},
		NyaaFeed: []item{{Title: "first", GUID: "https://nyaa.si/view/1"}},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// Replace the good snapshot with malformed JSON at a distinct mtime so
	// the next reload reads and memoizes it (equal-second mtimes must not
	// accidentally take the unchanged-loaded fast path).
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed snapshot: %v", err)
	}
	distinct := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, distinct, distinct); err != nil {
		t.Fatal(err)
	}
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot malformed"); got != 1 {
		t.Fatalf("malformed snapshot warned %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}

	// Onset: swap the parent directory for a regular file so os.Stat fails
	// with ENOTDIR (non-ENOENT, root-safe), then recover by restoring the
	// directory — the snapshot file keeps its inode and mtime throughout.
	aside := filepath.Join(dir, "sub-aside")
	blockDir := func() {
		if err := os.Rename(sub, aside); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sub, []byte("blocker"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restoreDir := func() {
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(aside, sub); err != nil {
			t.Fatal(err)
		}
	}

	blockDir()
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot stat failed"); got != 1 {
		t.Fatalf("stat-failure warned %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}

	// Recovery over the memoized bad file: repeated reloads must neither
	// reread it (no repeated malformed WARN) nor claim a false recovery.
	restoreDir()
	ix.reload(context.Background())
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot malformed"); got != 1 {
		t.Errorf("malformed snapshot warned %d times after the stat fault cleared, want still 1 (the memo must hold, no reread); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
	if got := rec.Count("indexer feed snapshot reload recovered"); got != 0 {
		t.Errorf("reload recovery logged %d times, want 0 (nothing was successfully reloaded); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Errorf("feed = %+v, want the last good snapshot kept", got)
	}

	// The cleared flag must re-arm the next onset's warning.
	blockDir()
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot stat failed"); got != 2 {
		t.Errorf("stat-failure warned %d times across two onsets, want 2 (the recovered stat over the memoized file must re-arm the warning); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
}
