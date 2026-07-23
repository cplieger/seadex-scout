package indexer

import (
	"context"
	"net/url"
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
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "first", GUID: "https://nyaa.si/view/1"}, Key: "nyaa:1"},
		},
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
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "second", GUID: "https://nyaa.si/view/2"}, Key: "nyaa:2"},
		},
	})
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot reappeared"); got != 1 {
		t.Errorf("reappearance logged %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "second" {
		t.Errorf("feed after reappearance = %+v, want the new snapshot served", got)
	}
}

// dirFault returns block/restore funcs that swap sub (the snapshot's
// parent directory) for a regular file - os.Stat on the snapshot then
// fails ENOTDIR (non-ENOENT, root-safe) - and undo it, leaving the
// snapshot file's inode and mtime intact throughout.
func dirFault(t *testing.T, dir, sub string) (block, restore func()) {
	t.Helper()
	aside := filepath.Join(dir, "sub-aside")
	block = func() {
		if err := os.Rename(sub, aside); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sub, []byte("blocker"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restore = func() {
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(aside, sub); err != nil {
			t.Fatal(err)
		}
	}
	return block, restore
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
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "first", GUID: "https://nyaa.si/view/1"}, Key: "nyaa:1"},
		},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// Onset: inject the root-safe ENOTDIR stat fault (see dirFault), then
	// recover — the snapshot file keeps its inode and mtime throughout.
	blockDir, restoreDir := dirFault(t, dir, sub)

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
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "first", GUID: "https://nyaa.si/view/1"}, Key: "nyaa:1"},
		},
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

	// Onset: inject the root-safe ENOTDIR stat fault (see dirFault), then
	// recover — the snapshot file keeps its inode and mtime throughout.
	blockDir, restoreDir := dirFault(t, dir, sub)

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

// TestReloadReassertsFailedStateWhenMalformedSnapshotReappears pins the
// pre-load state machine across a disappear/reappear of the SAME malformed
// snapshot inode (an unmount/remount, a rename away and back): startup over
// malformed bytes answers requests with a Torznab error; the file going
// missing restores fresh-install semantics (an empty feed is intentional, not
// an error); but when the identical bad inode returns, the memo-hit arm must
// re-assert the snapshot-unavailable state - NOT treat the bad snapshot as a
// valid fresh install and serve false-empty success (searches filtering every
// Prowlarr result against nil curation maps) indefinitely - and it must do so
// without rereading the unchanged file (no repeated malformed WARN).
func TestReloadReassertsFailedStateWhenMalformedSnapshotReappears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed snapshot: %v", err)
	}
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)

	rss := url.Values{"t": {"search"}}
	if _, stats := ix.query(context.Background(), rss, upstreamNyaa); !stats.snapshotUnavailable {
		t.Fatalf("startup over a malformed snapshot: stats = %+v, want snapshotUnavailable (a Torznab error)", stats)
	}
	if got := rec.Count("indexer feed snapshot malformed"); got != 1 {
		t.Fatalf("malformed snapshot warned %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}

	// The bad file disappears (unmounted / renamed away): fresh-install
	// semantics return, since deleting the bad file is a valid operator fix.
	aside := filepath.Join(dir, "feed-aside.json")
	if err := os.Rename(path, aside); err != nil {
		t.Fatal(err)
	}
	if _, stats := ix.query(context.Background(), rss, upstreamNyaa); stats.snapshotUnavailable {
		t.Fatalf("missing first snapshot: stats = %+v, want fresh-install semantics (no error)", stats)
	}

	// The SAME malformed inode reappears (remounted / renamed back): the memo
	// hit must re-assert the snapshot-unavailable state without a reread.
	if err := os.Rename(aside, path); err != nil {
		t.Fatal(err)
	}
	if _, stats := ix.query(context.Background(), rss, upstreamNyaa); !stats.snapshotUnavailable {
		t.Errorf("reappeared malformed snapshot: stats = %+v, want snapshotUnavailable (a Torznab error), not false-empty success", stats)
	}
	if got := rec.Count("indexer feed snapshot malformed"); got != 1 {
		t.Errorf("malformed snapshot warned %d times, want still 1 (the memo must hold, no reread); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadMemoizesOversizedItemSnapshot pins readSnapshot's persisted-item
// limit gate: a snapshot whose curation maps are valid but whose feed carries
// an item past maxPersistedFieldBytes is rejected like malformed JSON - the
// last-good feed keeps serving, the WARN fires once, and the deterministic
// bad bytes are memoized so repeated reloads never reread or re-warn.
func TestReloadMemoizesOversizedItemSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "first", GUID: "https://nyaa.si/view/1"}, Key: "nyaa:1"},
		},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: strings.Repeat("a", maxPersistedFieldBytes+1), GUID: "https://nyaa.si/view/2"}},
		},
	})
	distinct := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, distinct, distinct); err != nil {
		t.Fatal(err)
	}
	ix.reload(context.Background())
	ix.reload(context.Background())
	if got := rec.Count("indexer feed snapshot malformed"); got != 1 {
		t.Errorf("over-limit snapshot warned %d times across two reloads, want exactly 1 (deterministic bytes must memoize, no reread); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Errorf("feed after over-limit rewrite = %+v, want the last-good feed kept", got)
	}
}

// TestReloadPreJournalSnapshotServesEmptyFeeds pins readSnapshot's pre-journal
// schema gate: a legacy snapshot with NO "seen" key (the retired
// whole-catalogue schema; loadPrevious re-baselines on the same sentinel) must
// not serve its persisted feeds as the RSS journal - an upgrade must never
// re-broadcast the whole legacy catalogue as newly curated releases - while
// the curation maps are kept so searches still match.
func TestReloadPreJournalSnapshotServesEmptyFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	legacy := `{"by_hash":{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":true},"by_key":{"nyaa:1":true},` +
		`"nyaa_feed":[{"Title":"legacy nyaa","GUID":"https://nyaa.si/view/1"}],` +
		`"ab_feed":[{"Title":"legacy ab","GUID":"https://animebytes.tv/torrents.php?id=1&torrentid=2"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: "http://prowlarr/1/api",
		ABTorznabURL:   "http://prowlarr/2/api",
		ABPasskey:      "PASSKEY",
	}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		t.Errorf("nyaa feed from a pre-journal snapshot = %d items, want 0 (the legacy catalogue must not re-broadcast)", len(got))
	}
	if got := ix.feedFor(upstreamAB); len(got) != 0 {
		t.Errorf("ab feed from a pre-journal snapshot = %d items, want 0 (the legacy catalogue must not re-broadcast)", len(got))
	}
	if got := rec.Count("indexer feed snapshot is pre-journal schema; serving empty RSS feeds until the next cycle re-baselines"); got != 1 {
		t.Errorf("pre-journal INFO logged %d times, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	ix.mu.RLock()
	curated := ix.snap.ByHash["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] && ix.snap.ByKey["nyaa:1"]
	ix.mu.RUnlock()
	if !curated {
		t.Error("curation maps dropped from a pre-journal snapshot; searches must still match against them")
	}
}

// TestSnapshotUnavailableRecoveredBetweenLocksAnswersFresh pins the
// read-fast-path escalation window deterministically: a request that
// observes the failed state under the read lock, then loses the race to an
// install/clear before it acquires the write lock, must answer from the
// fresh snapshot (snapshotUnavailable = false, no Torznab error) and emit no
// stale snapshot-unavailable WARN. The snapshotUnavailableGate seam pauses
// the request exactly between the read unlock and the write lock.
func TestSnapshotUnavailableRecoveredBetweenLocksAnswersFresh(t *testing.T) {
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, filepath.Join(t.TempDir(), "feed.json"))
	ix.mu.Lock()
	ix.snapFailed = true
	ix.mu.Unlock()

	prev := snapshotUnavailableGate
	snapshotUnavailableGate = func() {
		// A concurrent installSnapshot/clearSnapshotFailed wins the race and
		// clears the failure before this request obtains the write lock.
		ix.mu.Lock()
		ix.snapFailed = false
		ix.mu.Unlock()
	}
	t.Cleanup(func() { snapshotUnavailableGate = prev })

	if ix.snapshotUnavailable() {
		t.Error("snapshotUnavailable = true after the failure cleared between the read unlock and the write lock, want false (answer from the fresh snapshot)")
	}
	if got := rec.Count("indexer feed snapshot unavailable; answering Torznab requests with an error until a snapshot loads"); got != 0 {
		t.Errorf("stale snapshot-unavailable WARN emitted %d times after recovery, want 0; log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadCoalescingLoserDefersToWinnerOnFreshInstall pins the pre-first-
// load coalescing handoff: while a winning reload holds reloadMu over a
// MISSING snapshot (the healthy fresh-install case), a concurrent reload must
// not mark the snapshot unavailable and return - it blocks until the winner's
// verdict and then runs the stat path itself - so no startup request can
// render a false snapshot-unavailable Torznab error that the winner's ENOENT
// confirmation contradicts. Synchronization is by holding the real lock and
// a done channel; no sleeps.
func TestReloadCoalescingLoserDefersToWinnerOnFreshInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json") // never written: fresh install
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{}, path)

	// Simulate the winning reload in flight: hold the coalescing lock the
	// way the winner does for its whole stat/read/install sequence.
	ix.reloadMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ix.reload(context.Background())
	}()
	// Release the winner's lock; the loser (blocked on it, or arriving
	// after) then runs the normal stat path and confirms the fresh-install
	// ENOENT state instead of latching a failure it never observed.
	ix.reloadMu.Unlock()
	<-done

	ix.mu.RLock()
	failed := ix.snapFailed
	ix.mu.RUnlock()
	if failed {
		t.Fatal("snapFailed = true after a fresh-install reload; a coalescing loser must defer to the winning reload's verdict, not mark the snapshot unavailable")
	}
	if ix.snapshotUnavailable() {
		t.Fatal("snapshotUnavailable() = true on a fresh install; absence of a first snapshot is the documented healthy state")
	}
}

// TestReloadRebuildsNyaaDownloadURLsFromGUID pins the Nyaa load-boundary
// guarantees (rebuildNyaaDownloadURLs): a persisted DownloadURL is never
// authoritative - an attacker-planted fetch target is overwritten from the
// non-secret GUID - and an item whose GUID carries no parseable numeric Nyaa
// id is dropped with the bounded warning rather than served link-less.
func TestReloadRebuildsNyaaDownloadURLsFromGUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "valid", GUID: "https://nyaa.si/view/42", DownloadURL: "https://attacker.example/poison.torrent"}, Key: "nyaa:42"},
			{item: item{Title: "invalid", GUID: "https://nyaa.si/view/not-a-number", DownloadURL: "https://attacker.example/invalid.torrent"}, Key: "nyaa:invalid"},
		},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)

	got := ix.feedFor(upstreamNyaa)
	if len(got) != 1 {
		t.Fatalf("nyaa feed = %d items, want 1 valid item after dropping the underivable GUID", len(got))
	}
	if got[0].Title != "valid" {
		t.Errorf("kept item title = %q, want valid", got[0].Title)
	}
	if want := "https://nyaa.si/download/42.torrent"; got[0].DownloadURL != want {
		t.Errorf("nyaa download = %q, want %q rebuilt from the GUID", got[0].DownloadURL, want)
	}
	if count := rec.Count("indexer feed snapshot: Nyaa items dropped; no download URL derivable from tracker page URL"); count != 1 {
		t.Errorf("underivable-item warnings = %d, want 1", count)
	}
}

// TestReloadDropsForeignHostSnapshotGUIDs pins the load-boundary trust gate
// (downloadURL's internal tracker-ownership check): a tampered but
// structurally valid feed.json cannot
// mint an apex-tracker download URL from a foreign or independent-subdomain
// GUID - trackerID's shape-only extraction would otherwise read the numeric
// id out of https://evil.example/view/123 or sukebei.nyaa.si/view/123 - so
// only items whose GUID passes the same trackerOwnURL gate writer-side
// journal admission applies survive the reload, with their served URLs
// derived on the expected apex tracker.
func TestReloadDropsForeignHostSnapshotGUIDs(t *testing.T) {
	tests := map[string]struct {
		scope     string
		feed      []journalItem
		wantTitle string
		wantURL   string
	}{
		"nyaa keeps only the canonical-host GUID": {
			scope: upstreamNyaa,
			feed: []journalItem{
				{item: item{Title: "canonical", GUID: "https://nyaa.si/view/42"}, Key: "nyaa:42"},
				{item: item{Title: "foreign", GUID: "https://evil.example/view/123"}, Key: "nyaa:123"},
				{item: item{Title: "subdomain", GUID: "https://sukebei.nyaa.si/view/123"}, Key: "nyaa:123"},
			},
			wantTitle: "canonical",
			wantURL:   "https://nyaa.si/download/42.torrent",
		},
		"ab keeps only the canonical-host GUID": {
			scope: upstreamAB,
			feed: []journalItem{
				{item: item{Title: "canonical", GUID: "https://animebytes.tv/torrents.php?id=1&torrentid=777"}, Key: "ab:777"},
				{item: item{Title: "foreign", GUID: "https://evil.example/torrents.php?id=1&torrentid=888"}, Key: "ab:888"},
			},
			wantTitle: "canonical",
			wantURL:   "https://animebytes.tv/torrent/777/download/PASSKEY",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			snap := &snapshot{ByHash: map[string]bool{}, ByKey: map[string]bool{}, Seen: map[string]bool{}}
			if tc.scope == upstreamNyaa {
				snap.NyaaFeed = tc.feed
			} else {
				snap.ABFeed = tc.feed
			}
			writeSnapshotFile(t, path, snap)
			log, _ := capture.New()
			ix := New(&Config{UpstreamConfig: UpstreamConfig{
				NyaaTorznabURL: "http://prowlarr/1/api",
				ABTorznabURL:   "http://prowlarr/2/api",
				ABPasskey:      "PASSKEY",
			}}, Deps{Logger: log}, path)

			got := ix.feedFor(tc.scope)
			if len(got) != 1 {
				t.Fatalf("%s feed = %d items (%+v), want only the canonical-host item after the trust gate", tc.scope, len(got), got)
			}
			if got[0].Title != tc.wantTitle {
				t.Errorf("kept item = %q, want %q", got[0].Title, tc.wantTitle)
			}
			if got[0].DownloadURL != tc.wantURL {
				t.Errorf("derived download = %q, want %q on the apex tracker", got[0].DownloadURL, tc.wantURL)
			}
		})
	}
}

// TestReloadDropsCrossKeySnapshotGUIDs pins the reader half of the journal's
// GUID-to-Key invariant (journalIdentityMatches in rebuildDownloadURLs): a
// structurally valid snapshot whose stored GUID resolves to a DIFFERENT
// torrent than its persisted Key names must be dropped at load - the writer's
// carry gates enforce the same invariant, and without the reader-side check a
// tampered feed.json with Key nyaa:42 and GUID .../view/666 would rebuild and
// serve torrent 666 as the journaled curated item until a later writer
// rebuild self-heals. Same gap for AnimeBytes.
func TestReloadDropsCrossKeySnapshotGUIDs(t *testing.T) {
	tests := map[string]struct {
		scope    string
		feed     []journalItem
		wantWarn string
	}{
		"nyaa cross-key GUID dropped": {
			scope: upstreamNyaa,
			feed: []journalItem{
				{item: item{Title: "cross", GUID: "https://nyaa.si/view/666"}, Key: "nyaa:42"},
			},
			wantWarn: "indexer feed snapshot: Nyaa items dropped; no download URL derivable from tracker page URL",
		},
		"ab cross-key GUID dropped": {
			scope: upstreamAB,
			feed: []journalItem{
				{item: item{Title: "cross", GUID: "https://animebytes.tv/torrents.php?id=1&torrentid=666"}, Key: "ab:42"},
			},
			wantWarn: "indexer feed snapshot: AnimeBytes items dropped; no download URL derivable from tracker page URL",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			snap := &snapshot{ByHash: map[string]bool{}, ByKey: map[string]bool{}, Seen: map[string]bool{}}
			if tc.scope == upstreamNyaa {
				snap.NyaaFeed = tc.feed
			} else {
				snap.ABFeed = tc.feed
			}
			writeSnapshotFile(t, path, snap)
			log, rec := capture.New()
			ix := New(&Config{UpstreamConfig: UpstreamConfig{
				NyaaTorznabURL: "http://prowlarr/1/api",
				ABTorznabURL:   "http://prowlarr/2/api",
				ABPasskey:      "PASSKEY",
			}}, Deps{Logger: log}, path)

			if got := ix.feedFor(tc.scope); len(got) != 0 {
				t.Errorf("%s feed = %d items (%+v), want 0: a cross-key GUID must never serve under the persisted curation binding", tc.scope, len(got), got)
			}
			if count := rec.Count(tc.wantWarn); count != 1 {
				t.Errorf("cross-key drop warnings = %d, want 1", count)
			}
		})
	}
}

// TestReloadSanitizesSnapshotInfoURLs pins the load-boundary display-URL gate
// (sanitizeSnapshotInfoURLs): a tampered but structurally valid feed.json
// cannot plant a javascript:/data: or foreign-host clickable info link that
// renderFeed would hand the arr UI as <comments> - only the canonical
// releases.moe entry URL the writer persists (entryURL) survives; anything
// else is blanked (never dropped), mirroring the search path's
// sanitizeDisplayURL.
func TestReloadSanitizesSnapshotInfoURLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "canonical", GUID: "https://nyaa.si/view/42", InfoURL: "https://releases.moe/154587"}, Key: "nyaa:42"},
			{item: item{Title: "scheme", GUID: "https://nyaa.si/view/43", InfoURL: "javascript:alert(1)"}, Key: "nyaa:43"},
			{item: item{Title: "foreign", GUID: "https://nyaa.si/view/44", InfoURL: "https://evil.example/phish"}, Key: "nyaa:44"},
		},
	})
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)

	got := ix.feedFor(upstreamNyaa)
	if len(got) != 3 {
		t.Fatalf("nyaa feed = %d items (%+v), want 3: the gate blanks InfoURL, never drops the item", len(got), got)
	}
	want := map[string]string{
		"canonical": "https://releases.moe/154587",
		"scheme":    "",
		"foreign":   "",
	}
	for _, it := range got {
		w, ok := want[it.Title]
		if !ok {
			t.Errorf("unexpected item %q in the served feed", it.Title)
			continue
		}
		if it.InfoURL != w {
			t.Errorf("item %q InfoURL = %q, want %q", it.Title, it.InfoURL, w)
		}
	}
	if count := rec.Count("indexer feed snapshot: non-SeaDex info URLs blanked"); count != 1 {
		t.Errorf("blanked-InfoURL warnings = %d, want 1", count)
	}
}
