package indexer

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
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
	setMtime(t, path, distinct)
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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})

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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
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
	setMtime(t, path, distinct)
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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: "http://prowlarr/1/api",
		ABTorznabURL:   "http://prowlarr/2/api",
		ABPasskey:      "PASSKEY",
	}}, Deps{Logger: log})
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
	ix := New(&Config{SnapshotPath: filepath.Join(t.TempDir(), "feed.json"), UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})

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
			ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{
				NyaaTorznabURL: "http://prowlarr/1/api",
				ABTorznabURL:   "http://prowlarr/2/api",
				ABPasskey:      "PASSKEY",
			}}, Deps{Logger: log})

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
			ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{
				NyaaTorznabURL: "http://prowlarr/1/api",
				ABTorznabURL:   "http://prowlarr/2/api",
				ABPasskey:      "PASSKEY",
			}}, Deps{Logger: log})

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
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})

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

// TestSnapshotInfoURLAllowedRejectsMalformedAndUserinfoURLs pins the
// fail-closed arms of the persisted-InfoURL display gate: a userinfo-bearing
// URL on the canonical host, an unparseable URL, a scheme-relative form, and
// a non-http(s) scheme must all be rejected, while the canonical http/https
// forms (case-insensitive host) stay allowed.
func TestSnapshotInfoURLAllowedRejectsMalformedAndUserinfoURLs(t *testing.T) {
	host := seadexInfoHost()
	if host == "" {
		t.Fatal("seadexInfoHost() = empty; the canonical constant must parse")
	}
	tests := map[string]struct {
		raw  string
		want bool
	}{
		"canonical https accepted":             {"https://releases.moe/154587", true},
		"canonical http accepted":              {"http://releases.moe/154587", true},
		"uppercase host accepted":              {"https://RELEASES.MOE/154587", true},
		"userinfo on canonical host rejected":  {"https://evil@releases.moe/154587", false},
		"user:pass on canonical host rejected": {"https://u:p@releases.moe/154587", false},
		"unparseable URL rejected":             {"https://releases.moe/%zz", false},
		"scheme-relative rejected":             {"//releases.moe/154587", false},
		"ftp scheme rejected":                  {"ftp://releases.moe/154587", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := snapshotInfoURLAllowed(tc.raw, host); got != tc.want {
				t.Errorf("snapshotInfoURLAllowed(%q, %q) = %v, want %v", tc.raw, host, got, tc.want)
			}
		})
	}
}

// TestReloadDropsCrossTrackerSnapshotItems pins rebuildDownloadURLs' second
// drop gate: a SELF-CONSISTENT item (Key matches its GUID, so the journal
// identity check passes) planted in the WRONG tracker's feed must be dropped
// by downloadURL's tracker-ownership gate, never served - a tampered
// feed.json could otherwise route a Nyaa torrent through the AB feed (or
// vice versa) and have the load boundary mint it a download link on the
// wrong tracker's endpoint shape.
func TestReloadDropsCrossTrackerSnapshotItems(t *testing.T) {
	tests := map[string]struct {
		scope    string
		planted  journalItem
		wantWarn string
	}{
		"nyaa item planted in the ab feed dropped": {
			scope:    upstreamAB,
			planted:  journalItem{item: item{Title: "planted", GUID: "https://nyaa.si/view/42"}, Key: "nyaa:42"},
			wantWarn: "indexer feed snapshot: AnimeBytes items dropped; no download URL derivable from tracker page URL",
		},
		"ab item planted in the nyaa feed dropped": {
			scope:    upstreamNyaa,
			planted:  journalItem{item: item{Title: "planted", GUID: "https://animebytes.tv/torrents.php?id=1&torrentid=777"}, Key: "ab:777"},
			wantWarn: "indexer feed snapshot: Nyaa items dropped; no download URL derivable from tracker page URL",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			snap := &snapshot{ByHash: map[string]bool{}, ByKey: map[string]bool{}, Seen: map[string]bool{}}
			if tc.scope == upstreamNyaa {
				snap.NyaaFeed = []journalItem{tc.planted}
			} else {
				snap.ABFeed = []journalItem{tc.planted}
			}
			writeSnapshotFile(t, path, snap)
			log, rec := capture.New()
			ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{
				NyaaTorznabURL: "http://prowlarr/1/api",
				ABTorznabURL:   "http://prowlarr/2/api",
				ABPasskey:      "PASSKEY",
			}}, Deps{Logger: log})

			if got := ix.feedFor(tc.scope); len(got) != 0 {
				t.Errorf("%s feed = %d items (%+v), want 0: a cross-tracker item must never serve from the wrong feed", tc.scope, len(got), got)
			}
			if count := rec.Count(tc.wantWarn); count != 1 {
				t.Errorf("cross-tracker drop warnings = %d, want 1", count)
			}
		})
	}
}

// TestReloadDropsUserinfoBearingSnapshotGUID pins the userinfo arm of the
// persisted-GUID gate after the dedicated userinfoFreeURL check was removed
// as redundant: rebuildDownloadURLs now rejects a userinfo-bearing GUID only
// as a consequence of journalIdentityMatches -> trackerKeyFromURL ->
// httpNoUserinfoURL, so a future change to that shared key derivation must
// fail here rather than silently republishing an attacker-shaped credential
// URL as the RSS <guid>.
func TestReloadDropsUserinfoBearingSnapshotGUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{}, ByKey: map[string]bool{}, Seen: map[string]bool{},
		NyaaFeed: []journalItem{{
			item: item{Title: "planted", GUID: "https://evil@nyaa.si/view/42"},
			Key:  "nyaa:42",
		}},
	})
	log, rec := capture.New()
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: "http://prowlarr/1/api",
	}}, Deps{Logger: log})

	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		t.Errorf("nyaa feed = %d items (%+v), want 0: a userinfo-bearing persisted GUID must never serve", len(got), got)
	}
	const wantWarn = "indexer feed snapshot: Nyaa items dropped; no download URL derivable from tracker page URL"
	if count := rec.Count(wantWarn); count != 1 {
		t.Errorf("userinfo drop warnings = %d, want 1", count)
	}
}

// TestReloadCoalescingLoserBlocksWithoutMarkingFailure deterministically pins
// the pre-first-load coalescing arm: while a
// winning reload holds the reload gate over a missing first snapshot, a loser that
// reaches the pre-first-load arm must commit to BLOCKING (the
// reloadBlockGate seam marks that commitment) without latching snapFailed -
// the historic bug this arm exists to prevent - and, once the winner
// releases the lock, must run the stat path itself and confirm the healthy
// fresh-install state.
func TestReloadCoalescingLoserBlocksWithoutMarkingFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json") // never written: fresh install
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})

	// The winning reload is in flight over the missing first snapshot.
	if !ix.tryLockReload() {
		t.Fatal("reload gate already held; want it free before simulating the winning reload")
	}
	atGate := make(chan struct{})
	prev := reloadBlockGate
	reloadBlockGate = func() { close(atGate) }
	t.Cleanup(func() { reloadBlockGate = prev })

	done := make(chan struct{})
	go func() { defer close(done); ix.reload(context.Background()) }()

	<-atGate // the loser took the pre-first-load arm and committed to blocking
	ix.mu.RLock()
	failed := ix.snapFailed
	ix.mu.RUnlock()
	if failed {
		t.Error("snapFailed = true while the winner still holds the reload gate; a blocked loser must not latch a failure it never observed")
	}
	ix.unlockReload()
	<-done

	if ix.snapshotUnavailable() {
		t.Fatal("snapshotUnavailable() = true after the loser re-ran the stat path on a fresh install; absence of a first snapshot is the documented healthy state")
	}
}

// TestReloadCoalescingLoserWaitAbandonsOnCancelledContext pins the
// cancellability of the pre-first-load wait: a loser whose request context is
// already done (client disconnect, arr timeout) must return BEFORE the winner
// releases the gate, instead of parking its handler goroutine and connection
// behind the winner's unbounded stat/read/decode (no server write timeout
// bounds a mutex wait). It must also leave the snapshot state untouched, exactly
// like the blocking loser: the verdict is the winner's to establish.
func TestReloadCoalescingLoserWaitAbandonsOnCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json") // never written: fresh install
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})

	// The winning reload is in flight over the missing first snapshot.
	if !ix.tryLockReload() {
		t.Fatal("reload gate already held; want it free before simulating the winning reload")
	}
	t.Cleanup(ix.unlockReload)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); ix.reload(ctx) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled loser still waiting while the winner holds the reload gate; want an abandoned wait")
	}
	ix.mu.RLock()
	failed := ix.snapFailed
	ix.mu.RUnlock()
	if failed {
		t.Error("snapFailed = true after an abandoned wait; the verdict is the winner's to establish")
	}
}

// TestReloadKeepsFeedOnMalformedSnapshot verifies reload's resilience contract: once a
// good feed is loaded, a later malformed snapshot write (a partial/corrupt cycle write) is
// logged and ignored, never blanking the live feed. A cross-process poll writes the file
// non-atomically only in the failure case; the server must not serve an empty feed then.
func TestReloadKeepsFeedOnMalformedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}}, Deps{})
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	bumpMtime(t, path)
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Errorf("after malformed rewrite feed = %d items, want 1 (a bad write must not blank a live feed)", len(got))
	}
}

// TestReloadKeepsFeedOnZeroSnapshot extends the malformed-snapshot contract to
// syntactically valid but structurally empty JSON: `null` and `{}` decode
// cleanly into a zero snapshot, and installing one would blank both synthesized
// feeds and both curation maps. The writer always emits non-nil by_hash/by_key
// maps (even for an empty catalogue), so nil curation maps identify a
// structurally invalid snapshot the reload must reject, preserving the
// last-good feed.
func TestReloadKeepsFeedOnZeroSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"null document", "null"},
		{"empty object", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			seedEmptyLedger(t, path)
			if err := newTestWriter(path, "", false).Rebuild(context.Background(), nyaaTestEntries(1), nil); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
			if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
				t.Fatalf("initial feed = %d items, want 1", len(got))
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("zero-snapshot write: %v", err)
			}
			bumpMtime(t, path)
			ix.reload(context.Background())
			if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
				t.Errorf("after %s rewrite feed = %d items, want 1 (a zero snapshot must not blank a live feed)", tc.name, len(got))
			}
		})
	}
}

// TestReloadRebuildsABDownloadURLsFromCurrentPasskey pins the credential
// policy for the persisted AB feed: FeedWriter persists AB items GUID-only
// (no passkey-bearing download URL lands in feed.json), so the reload MUST
// derive every AB download URL from the item's non-secret tracker page URL
// (GUID) and the CURRENT passkey or the feed has no grabbable links at all.
// The same derivation makes an ab_passkey rotation take effect on the next
// load, never serves a legacy snapshot's persisted credential verbatim, drops
// an item whose URL cannot be derived, and clears the AB feed entirely when
// no passkey is configured.
func TestReloadRebuildsABDownloadURLsFromCurrentPasskey(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	if err := newTestWriter(path, "OLD_PASSKEY", true).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// A restart after rotating the passkey: the loaded AB feed must carry only
	// the NEW credential.
	ix := New(&Config{APIKey: "k", SnapshotPath: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api", ABPasskey: "NEW_PASSKEY"}}, Deps{})
	got := ix.feedFor(upstreamAB)
	if len(got) != 1 {
		t.Fatalf("ab feed = %d items, want 1", len(got))
	}
	if want := "https://animebytes.tv/torrent/1167293/download/NEW_PASSKEY"; got[0].DownloadURL != want {
		t.Errorf("ab download = %q, want %q (rebuilt from the current passkey)", got[0].DownloadURL, want)
	}
	if strings.Contains(got[0].DownloadURL, "OLD_PASSKEY") {
		t.Errorf("ab download still carries the rotated passkey: %q", got[0].DownloadURL)
	}

	// With NO passkey configured the persisted credential-bearing links must
	// not be served at all: the AB feed clears (serve answers the /ab RSS
	// check with a Torznab <error> in that state); Nyaa is untouched.
	none := New(&Config{APIKey: "k", SnapshotPath: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, Deps{})
	if got := none.feedFor(upstreamAB); len(got) != 0 {
		t.Errorf("ab feed without a configured passkey = %d items, want 0", len(got))
	}

	// An AB item whose page URL yields no torrent id cannot have its URL
	// re-derived: it is dropped rather than served with the stale credential.
	noID := `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[],"ab_feed":[{"Title":"no id","GUID":"https://animebytes.tv/torrents.php?id=1","DownloadURL":"https://animebytes.tv/torrent/1/download/OLD_PASSKEY"}]}`
	noIDPath := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(noIDPath, []byte(noID), 0o600); err != nil {
		t.Fatalf("write no-id snapshot: %v", err)
	}
	dropper := New(&Config{APIKey: "k", SnapshotPath: noIDPath, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api", ABPasskey: "NEW_PASSKEY"}}, Deps{})
	if got := dropper.feedFor(upstreamAB); len(got) != 0 {
		t.Errorf("ab feed with an underivable item = %d items, want 0 (dropped, never served with the persisted credential)", len(got))
	}
}

// TestReloadRetriesPreservedMtimeReplacementAfterFailure pins the failed-file
// memo to file IDENTITY, not just mtime: after a malformed snapshot fails to
// load at mtime T, a repaired valid snapshot installed on a NEW inode whose
// mtime is reset to the same T (an atomic rename or backup restore preserving
// timestamps) must be retried and installed - a mtime-only watermark would skip
// it and wedge the server on the old feed until restart. Only the unchanged bad
// inode itself stays memoized.
func TestReloadRetriesPreservedMtimeReplacementAfterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("malformed write: %v", err)
	}
	failedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	setMtime(t, path, failedAt)
	// New's warm-up reload reads the malformed file and memoizes it as failed.
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}}, Deps{})
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 0 {
		t.Fatalf("initial feed = %d items, want 0 (malformed snapshot must not load)", len(got))
	}
	// Repair: a valid snapshot on a NEW inode, renamed over the bad file with
	// the failed mtime preserved.
	repaired := filepath.Join(dir, "feed-repaired.json")
	if err := seedRebuild(repaired, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := os.Rename(repaired, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	setMtime(t, path, failedAt)
	ix.reload(context.Background())
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Errorf("after preserved-mtime repair feed = %d items, want 1 (a new inode at the failed mtime must be retried)", len(got))
	}
}

// TestReloadInstallsPreservedMtimeReplacementAfterSuccess pins the last-good
// gate to file IDENTITY, not just mtime: after a snapshot loads successfully at
// mtime T, a DIFFERENT valid snapshot installed on a new inode with its mtime
// reset to the same T (an atomic rename or backup restore preserving
// timestamps) must still install - a mtime-only last-good check would return
// early and leave the old feed served until an unrelated write or a restart.
func TestReloadInstallsPreservedMtimeReplacementAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := seedRebuild(path, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	loadedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	setMtime(t, path, loadedAt)
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// A different snapshot on a NEW inode, renamed over the loaded file with
	// the loaded mtime preserved.
	replacement := filepath.Join(dir, "feed-replacement.json")
	if err := seedRebuild(replacement, nyaaTestEntries(2)); err != nil {
		t.Fatalf("Rebuild replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	setMtime(t, path, loadedAt)
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 2 {
		t.Errorf("after preserved-mtime replacement feed = %d items, want 2 (a new inode at the loaded mtime must install)", len(got))
	}
}

// TestReloadRetriesTransientReadFailureOnSameInode pins the failed-file memo to
// DETERMINISTIC failures only: a snapshot whose read fails (here an oversized
// file the bounded read rejects - a root-safe stand-in for a transient EIO or
// a later-chmodded EACCES) must NOT be memoized, so a subsequent in-place
// repair that changes neither inode nor mtime is still retried and installs.
// Memoizing the read failure would skip the unchanged-identity file forever.
func TestReloadRetriesTransientReadFailureOnSameInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	// A sparse file one byte over the bound: os.Stat succeeds, the bounded
	// read fails.
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxFeedBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	failedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	setMtime(t, path, failedAt)
	// New's warm-up reload hits the read failure; it must stay retryable.
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		t.Fatalf("initial feed = %d items, want 0 (oversized snapshot must not load)", len(got))
	}

	// Repair IN PLACE (same inode: build a valid snapshot beside it, then
	// rewrite the original file's bytes) and restore the failed mtime.
	repaired := filepath.Join(dir, "feed-repaired.json")
	if err := seedRebuild(repaired, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	valid, err := os.ReadFile(repaired)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatalf("in-place repair: %v", err)
	}
	setMtime(t, path, failedAt)
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("after same-inode repair feed = %d items, want 1 (a read failure must stay retryable)", len(got))
	}
}

// TestReloadConcurrentCallers exercises reload's coalescing under concurrency
// (run with -race): many requests observing a rewritten snapshot at once must
// never race on the published snapshot fields, and the new feed must be
// installed once the dust settles.
func TestReloadConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := seedRebuild(path, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}
	// A second cycle over a grown catalogue: entry 1 carries, entry 2 is new.
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), nyaaTestEntries(2), nil); err != nil {
		t.Fatalf("Rebuild newer: %v", err)
	}
	bumpMtime(t, path)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ix.reload(context.Background())
			_ = ix.feedFor(upstreamNyaa)
		})
	}
	wg.Wait()
	// TryLock losers return without installing; one more serial reload
	// guarantees the newer snapshot is in.
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 2 {
		t.Errorf("after concurrent reloads feed = %d items, want 2", len(got))
	}
}

// TestReloadInstallsOlderMtimeSnapshot pins reload's inequality freshness
// guard: an on-disk snapshot whose mtime is OLDER than the loaded copy's still
// installs. A /config volume restored from backup, or a file replaced by an
// atomic rename preserving an older mtime, is the current truth on disk; the
// former strictly-After guard never installed it and wedged the server on the
// stale in-memory snapshot until restart. Any mtime CHANGE reloads; only
// equality skips (TestReloadSkipsUnchangedMtime). Driven single-threaded: the
// pre-install holds the write lock exactly as a real cycle would, and the lone
// reload runs after it, so there is no shared-state access outside the lock.
func TestReloadInstallsOlderMtimeSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	oldTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newerTime := oldTime.Add(time.Hour)
	restoredJSON := `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Key":"nyaa:7","Title":"restored","GUID":"https://nyaa.si/view/7","DownloadURL":"restored"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(restoredJSON), 0o600); err != nil {
		t.Fatalf("write restored snapshot: %v", err)
	}
	setMtime(t, path, oldTime)

	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})

	// Pre-install a newer-mtime snapshot the way a pre-restore cycle would,
	// holding the write lock exactly as reload's install path does.
	ix.mu.Lock()
	ix.snap = snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "stale", GUID: "stale", DownloadURL: "stale"}},
		},
	}
	ix.snapMod = newerTime
	ix.mu.Unlock()

	// Reloading against the older-mtime on-disk file must install it: the
	// mtime differs from the loaded snapshot's, and the file is the truth.
	ix.reload(context.Background())

	got := ix.feedFor(upstreamNyaa)
	if len(got) != 1 || got[0].Title != "restored" {
		t.Fatalf("feed after reloading an older-mtime snapshot = %#v, want the restored on-disk snapshot", got)
	}
	ix.mu.RLock()
	reloadedMod := ix.snapMod
	ix.mu.RUnlock()
	if reloadedMod.Equal(newerTime) {
		t.Fatalf("snapMod after reloading an older-mtime snapshot = %v, want the on-disk mtime, not the stale %v", reloadedMod, newerTime)
	}
}

// TestReloadSkipsUnchangedMtime pins the equality leg of reload's freshness
// guard: when the on-disk mtime equals the loaded snapshot's, reload leaves the
// served feed untouched - even if the bytes changed - so the per-request mtime
// check stays a cheap stat, never a read/unmarshal.
func TestReloadSkipsUnchangedMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	when := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	firstJSON := `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Key":"nyaa:1","Title":"first","GUID":"https://nyaa.si/view/1","DownloadURL":"first"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(firstJSON), 0o600); err != nil {
		t.Fatalf("write first snapshot: %v", err)
	}
	setMtime(t, path, when)
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("initial feed = %#v, want the first snapshot", got)
	}

	// Rewrite the content but restore the identical mtime: reload must skip.
	secondJSON := `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Key":"nyaa:2","Title":"second","GUID":"https://nyaa.si/view/2","DownloadURL":"second"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(secondJSON), 0o600); err != nil {
		t.Fatalf("write second snapshot: %v", err)
	}
	setMtime(t, path, when)
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("feed after unchanged-mtime rewrite = %#v, want the loaded first snapshot (equality skips)", got)
	}
}

// TestReloadCoalescesConcurrentRefreshes pins reload's coalescing contract
// AFTER a first successful load: while one request holds the refresh
// (the reload gate, as a winning reload does for its whole stat/read/unmarshal), a
// sibling reload returns immediately without duplicating the read - it does
// not block and does not install the on-disk snapshot itself - and feedFor
// keeps serving the current snapshot unblocked. Once the refresh is released,
// the next reload installs the new snapshot. (Before the first load, losers
// block on the winner's verdict instead - see
// TestReloadCoalescingLoserDefersToWinnerOnFreshInstall.)
func TestReloadCoalescesConcurrentRefreshes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	firstJSON := `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Key":"nyaa:1","Title":"first","GUID":"https://nyaa.si/view/1","DownloadURL":"first"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(firstJSON), 0o600); err != nil {
		t.Fatalf("write first snapshot: %v", err)
	}
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("initial feed = %#v, want the first snapshot loaded", got)
	}

	// A newer on-disk snapshot the in-progress refresh has not installed yet.
	// The in-place rewrite lands on the same inode within the filesystem's
	// mtime granularity, so bump the mtime past the loaded snapshot's or
	// loadedSnapshotUnchanged would skip the reload (production writes are
	// atomic renames, which install a new inode instead).
	newJSON := `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Key":"nyaa:3","Title":"new","GUID":"https://nyaa.si/view/3","DownloadURL":"new"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(newJSON), 0o600); err != nil {
		t.Fatalf("write new snapshot: %v", err)
	}
	bumpMtime(t, path)

	// Simulate a refresh in progress: hold the reload gate exactly as the
	// winning request does across its stat/read/unmarshal.
	if !ix.tryLockReload() {
		t.Fatal("reload gate already held; want it free before simulating a refresh")
	}

	// A sibling reload must return immediately rather than queue behind the
	// in-progress refresh or perform a duplicate read.
	done := make(chan struct{})
	go func() {
		ix.reload(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		ix.unlockReload()
		t.Fatal("sibling reload blocked behind an in-progress refresh; want an immediate return once a snapshot is loaded")
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		ix.unlockReload()
		t.Fatalf("sibling reload = %#v; want the current snapshot kept and the install left to the refresh holder", got)
	}

	// Once the winning request releases the refresh, the next reload installs
	// the new snapshot as usual.
	ix.unlockReload()
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "new" {
		t.Fatalf("reload after the refresh released = %#v, want the new snapshot installed", got)
	}
}

// TestInstallSnapshotSkipsAlreadyInstalledFile pins installSnapshot's
// under-lock re-check: re-installing the same unchanged file (equal mtime AND
// os.SameFile identity) returns false and leaves the published snapshot
// untouched. The reload gate already serializes reloads today, but the comment
// declares this defense-in-depth invariant must hold even if the TryLock
// coalescing changes, so it is pinned by direct call.
func TestInstallSnapshotSkipsAlreadyInstalledFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{})
	if !ix.installSnapshot(info1, &snapshot{NyaaFeed: []journalItem{
		{item: item{Title: "first"}},
	}}) {
		t.Fatal("first installSnapshot = false, want true")
	}
	if ix.installSnapshot(info2, &snapshot{NyaaFeed: []journalItem{
		{item: item{Title: "second"}},
	}}) {
		t.Fatal("second installSnapshot with same unchanged file = true, want false")
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("served feed = %+v, want the originally installed snapshot", got)
	}
}
