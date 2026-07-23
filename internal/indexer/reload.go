package indexer

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/release"
)

// statSnapshot stats the snapshot file and applies reload's missing/unreadable
// policy, returning the file info and whether reload should proceed. A missing
// file after one was loaded warns once (the feed is now stale); any other stat
// error (EACCES, EIO) warns and freezes the current feed. On the recovery path
// it clears snapMissing and logs one INFO line.
func (ix *Indexer) statSnapshot() (os.FileInfo, bool) {
	info, err := os.Stat(ix.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A missing file is the normal fresh-install case, but after a
			// snapshot was loaded it means the materialized view can no
			// longer refresh: every request keeps serving the last in-memory
			// feed, so warn once that the feed is stale, then stay quiet
			// until the file reappears.
			//
			// Absence is a successful stat determination, so it ENDS any
			// stat/read degradation episode: clear the transient flag (no
			// recovery INFO - nothing was reloaded; the missing state has
			// its own once-per-disappearance WARN) so the next fault onset
			// warns again instead of being suppressed by a stale flag.
			ix.reloadDegraded = false
			ix.mu.RLock()
			loaded := ix.snapInfo != nil
			ix.mu.RUnlock()
			if !loaded {
				// A genuinely absent first snapshot IS the fresh-install
				// state - serving the empty feed is intentional there - so
				// an earlier load fault stops blocking requests once the bad
				// file is gone (deleting it returns to fresh-install
				// semantics).
				ix.clearSnapshotFailed()
			}
			if loaded && !ix.snapMissing {
				ix.snapMissing = true
				ix.log.Warn("indexer feed snapshot missing; serving last loaded feed until it reappears", "path", ix.path)
			}
			return nil, false
		}
		// Anything else (EACCES, EIO) silently freezes the served feed, so
		// make it visible - once per onset, not once per request.
		ix.markSnapshotFailedIfUnloaded()
		if !ix.reloadDegraded {
			ix.reloadDegraded = true
			ix.log.Warn("indexer feed snapshot stat failed; keeping current feed", "path", ix.path, "error", err)
		}
		return nil, false
	}
	if ix.snapMissing {
		ix.snapMissing = false
		ix.log.Info("indexer feed snapshot reappeared; resuming reloads", "path", ix.path)
	}
	return info, true
}

// reload refreshes the served feed from the persisted snapshot when the file
// on disk differs from the loaded copy by mtime or file identity (or nothing
// is loaded yet). A compare cycle - in this process (the daemon loop) or
// another (the `poll` subcommand) - rewrites the snapshot atomically, so a
// cheap stat check per request picks up a new feed without the server ever
// fetching SeaDex itself. Any mtime change triggers a reload, including an
// older restored timestamp. When the mtime is equal, os.SameFile
// distinguishes the unchanged file (skip) from a replacement inode whose
// timestamp was preserved (reload), preventing an atomic rename or backup
// restore from wedging the server on stale in-memory data. A missing file
// leaves the current (possibly empty) feed in place; a malformed or
// unreadable file is logged and ignored, so a bad write never blanks a live
// feed.
//
// Concurrent calls coalesce: after a cycle rewrites the snapshot, every
// in-flight request observes the newer mtime at once, and without coalescing
// each would independently read and unmarshal up to maxFeedBytes before the
// under-mu recheck let only one install it. reloadMu.TryLock lets exactly one
// request refresh; once a snapshot has loaded, the rest return immediately and
// serve the current immutable snapshot (the next request picks up the newly
// installed one). Before the FIRST successful load, losers block on the lock
// instead: the winner has not yet established whether the on-disk snapshot is
// usable, so returning early would have to guess between fresh-install and
// failed state (see the branch below).
func (ix *Indexer) reload(ctx context.Context) {
	if ix.path == "" {
		return
	}
	if !ix.reloadMu.TryLock() {
		ix.mu.RLock()
		loaded := ix.snapInfo != nil
		ix.mu.RUnlock()
		if loaded {
			// After a successful load, losers coalesce non-blocking and
			// keep serving the current immutable snapshot; the next request
			// picks up whatever the winner installs.
			return
		}
		// Before the first successful load, an in-flight reload has not yet
		// established whether the on-disk snapshot is usable, and marking
		// the snapshot failed here would race the winner: it can confirm
		// the healthy fresh-install ENOENT case and clear snapFailed, then
		// this loser would set it again before the winner releases
		// reloadMu, making one startup request render a false
		// snapshot-unavailable Torznab error. Initial-load callers instead
		// BLOCK until the winning reload has established fresh-install,
		// failed, or loaded state; once acquired, this caller runs the
		// normal stat/read path itself, so a cancelled winner is also
		// retried.
		ix.reloadMu.Lock()
	}
	defer ix.reloadMu.Unlock()
	info, ok := ix.statSnapshot()
	if !ok {
		return
	}
	if ix.skipMemoizedMalformed(info) {
		return
	}
	// A degraded reload must not take the unchanged-loaded-snapshot fast
	// path: after a stat fault recovers, the file may be the already-loaded
	// inode at the same mtime, so skipping here would leave reloadDegraded
	// set forever — the recovery INFO never emits and the next onset's
	// warning is suppressed by the stale flag. Forcing one bounded read
	// clears the state through the recovery block below; a persistent read
	// fault keeps it degraded without falsely declaring recovery.
	if ix.loadedSnapshotUnchanged(info) && !ix.reloadDegraded {
		return
	}
	snap, ok, memoize := ix.readSnapshot(ctx)
	if !ok {
		ix.recordSnapshotFailure(ctx, info, memoize)
		return
	}
	ix.failedFile = nil
	if ix.reloadDegraded {
		ix.reloadDegraded = false
		ix.log.Info("indexer feed snapshot reload recovered", "path", ix.path)
	}
	if !ix.installSnapshot(info, &snap) {
		return
	}
	ix.log.Info("indexer feed snapshot loaded",
		"path", ix.path, "hashes", len(snap.ByHash), "keys", len(snap.ByKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed))
}

// skipMemoizedMalformed applies reload's memoized-malformed-file arm: it
// reports whether the stat'ed file is the memoized malformed snapshot,
// unchanged, and if so re-asserts the snapshot-unavailable state and clears
// the transient degradation flag. The memoized malformed snapshot fails
// deterministically: unchanged bytes decode the same way on every read, so
// rereading it would only repeat the per-request I/O/JSON work and the
// malformed WARN. The successful stat that reached this point already proves
// file access recovered from any transient stat/read fault, so clear the
// degradation flag directly - re-arming the next onset's warning - without a
// reread and without the "reload recovered" INFO (nothing was successfully
// reloaded; the file is still bad). markSnapshotFailedIfUnloaded is a no-op
// after a last-good snapshot, but it restores the startup error state when
// the same memoized bad inode REAPPEARS after an ENOENT interval (an
// unmount/remount, a rename away and back): the missing-file arm cleared
// snapFailed to restore fresh-install semantics while keeping failedFile, so
// without re-asserting here the pre-load state machine would treat the bad
// snapshot as a valid fresh install and serve false-empty success instead of
// a Torznab error.
func (ix *Indexer) skipMemoizedMalformed(info os.FileInfo) bool {
	if !ix.matchesFailedFile(info) {
		return false
	}
	ix.markSnapshotFailedIfUnloaded()
	ix.reloadDegraded = false
	return true
}

// recordSnapshotFailure applies reload's failed-read memo policy. Only
// malformed bytes are deterministic for an unchanged file. Read failures can
// recover after chmod or transient filesystem repair without changing inode
// or mtime, so they must remain retryable - and a shutdown cancellation never
// memoizes (the file was never actually read; a retry could succeed).
func (ix *Indexer) recordSnapshotFailure(ctx context.Context, info os.FileInfo, memoize bool) {
	ix.failedFile = nil
	if ctx.Err() == nil && memoize {
		ix.failedFile = info
	}
}

// loadedSnapshotUnchanged reports whether the stat'ed snapshot file is the
// already-loaded snapshot, unchanged - an equal mtime AND os.SameFile
// identity. Identity is required, not just the timestamp (see reload's doc
// comment): an equal mtime on a DIFFERENT inode is a preserved-timestamp
// replacement (an atomic rename, a backup restore) and must install, while
// any mtime CHANGE - including an older one - always reloads.
func (ix *Indexer) loadedSnapshotUnchanged(info os.FileInfo) bool {
	ix.mu.RLock()
	loadedMod, loadedInfo := ix.snapMod, ix.snapInfo
	ix.mu.RUnlock()
	return info.ModTime().Equal(loadedMod) && loadedInfo != nil && os.SameFile(info, loadedInfo)
}

// matchesFailedFile reports whether the stat'ed snapshot file is the memoized
// malformed file, unchanged by the same equal-mtime AND os.SameFile identity
// test as the loaded leg: an unchanged malformed file fails deterministically,
// so it is never re-read (reload clears only the transient reloadDegraded
// flag and returns), while any mtime or identity change means new bytes worth
// retrying.
func (ix *Indexer) matchesFailedFile(info os.FileInfo) bool {
	return ix.failedFile != nil && info.ModTime().Equal(ix.failedFile.ModTime()) && os.SameFile(info, ix.failedFile)
}

// installSnapshot publishes snap as the served feed under mu, recording the
// file's mtime + identity for the next reload's skip check, and reports
// whether it installed. The re-check under the write lock is defense in depth:
// reloadMu already serializes the whole stat/read/install sequence, so no
// concurrent reload can install in between today, but never re-installing a
// copy of what is already loaded holds even if the TryLock coalescing changes.
// Same test as loadedSnapshotUnchanged: only an equal mtime on the
// SAME file (os.SameFile identity) skips.
func (ix *Indexer) installSnapshot(info os.FileInfo, snap *snapshot) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if info.ModTime().Equal(ix.snapMod) && ix.snapInfo != nil && os.SameFile(info, ix.snapInfo) {
		return false
	}
	ix.snap = *snap
	ix.snapMod = info.ModTime()
	ix.snapInfo = info
	// A successful install ends any startup snapshot-unavailable state and
	// re-arms its per-onset WARN (see snapFailed).
	ix.snapFailed = false
	ix.snapFailedWarned = false
	return true
}

// markSnapshotFailedIfUnloaded flags the snapshot-unavailable state (see the
// snapFailed field) after a load fault, but only while no snapshot has ever
// been installed: after a successful load the last-good snapshot keeps being
// served instead.
func (ix *Indexer) markSnapshotFailedIfUnloaded() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.snapInfo == nil {
		ix.snapFailed = true
	}
}

// clearSnapshotFailed resets the snapshot-unavailable state and re-arms its
// per-onset WARN (see the snapFailed field).
func (ix *Indexer) clearSnapshotFailed() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.snapFailed = false
	ix.snapFailedWarned = false
}

// readSnapshot is reload's read/decode error policy: it bounded-reads and
// decodes the persisted feed snapshot, reporting ok=false on any failure so
// the caller keeps the current feed. A shutdown cancellation is silent; an
// unreadable or malformed file is logged (a bad write must never blank a live
// feed). The third result means "memoize unchanged bytes": true only for
// malformed JSON, the one failure that is deterministic for an unchanged
// file - a read failure (EIO, a fixable EACCES) can recover without changing
// inode or mtime, so it must stay retryable.
func (ix *Indexer) readSnapshot(ctx context.Context) (snapshot, bool, bool) {
	data, err := atomicfile.ReadBounded(ctx, ix.path, maxFeedBytes)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// A shutdown cancellation is silent and never marks the
			// snapshot-unavailable state (the file was never actually read;
			// a retry could succeed).
			ix.markSnapshotFailedIfUnloaded()
			if !ix.reloadDegraded {
				ix.reloadDegraded = true
				ix.log.Warn("indexer feed snapshot unreadable; keeping current feed", "path", ix.path, "error", err)
			}
		}
		return snapshot{}, false, false
	}
	snap, reason, decodeErr := decodeSnapshot(data)
	if decodeErr != nil {
		ix.markSnapshotFailedIfUnloaded()
		ix.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", ix.path, "error", decodeErr)
		return snapshot{}, false, true
	}
	// `null` or `{}` decodes cleanly into a zero value; nil curation maps
	// and over-limit items identify a structurally invalid snapshot (see
	// decodeSnapshot). Both are deterministic for unchanged bytes, so they
	// memoize like malformed JSON; the offending value itself is never
	// logged (it can be attacker-shaped multi-megabyte text).
	if reason != "" {
		ix.markSnapshotFailedIfUnloaded()
		ix.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", ix.path, "reason", reason)
		return snapshot{}, false, true
	}
	if snap.Seen == nil {
		// The retired pre-journal schema: the journal contract (see
		// loadPrevious) treats its feeds as absent. Keep the curation maps
		// so searches still work, but serve empty RSS feeds until the next
		// cycle re-baselines - an upgrade must never re-broadcast the whole
		// legacy catalogue as newly curated releases.
		if len(snap.NyaaFeed) > 0 || len(snap.ABFeed) > 0 {
			ix.log.Info("indexer feed snapshot is pre-journal schema; serving empty RSS feeds until the next cycle re-baselines",
				"path", ix.path)
		}
		snap.NyaaFeed, snap.ABFeed = nil, nil
	}
	snap.ABFeed = ix.rebuildABDownloadURLs(snap.ABFeed)
	snap.NyaaFeed = ix.rebuildNyaaDownloadURLs(snap.NyaaFeed)
	snap.ABFeed = ix.sanitizeSnapshotInfoURLs(snap.ABFeed)
	snap.NyaaFeed = ix.sanitizeSnapshotInfoURLs(snap.NyaaFeed)
	return snap, true, false
}

// rebuildDownloadURLs is the shared derivation mechanics behind
// rebuildABDownloadURLs and rebuildNyaaDownloadURLs: it re-derives each feed
// item's download URL from its non-secret tracker page URL (the GUID) via
// downloadURL, which enforces the tracker-ownership gate internally
// (trackerOwnURL, the same fail-closed check writer-side journal admission
// runs through trackerKey). Persisted data crosses a separate trust boundary
// from writer admission: a tampered but structurally valid feed.json could
// otherwise carry a foreign (https://evil.example/view/123) or
// independent-subdomain (sukebei.nyaa.si/view/123) GUID whose numeric id
// would be minted into the apex tracker's download URL for an unrelated
// torrent; the gate drops such items exactly like an undecodable GUID. Any
// item whose URL cannot be derived is dropped, collecting
// the drop count plus up to three bounded sample GUIDs for the wrappers'
// tracker-specific warnings. The wrappers own the policy (the AB passkey
// gate) and the exact log contract.
func rebuildDownloadURLs(feed []journalItem, tracker, passkey string) (out []journalItem, dropped int, samples []string) {
	out = make([]journalItem, 0, len(feed))
	for i := range feed {
		it := feed[i]
		dl, ok := downloadURL(tracker, it.GUID, passkey)
		if !ok {
			dropped++
			if len(samples) < 3 {
				// The GUID is a non-secret tracker page URL; bound it through
				// the shared emit-boundary policy before it reaches the log.
				samples = append(samples, capLogText(it.GUID, 256))
			}
			continue
		}
		it.DownloadURL = dl
		out = append(out, it)
	}
	return out, dropped, samples
}

// rebuildABDownloadURLs derives each persisted AnimeBytes feed item's download
// URL from its non-secret tracker page URL (the GUID) and the CURRENTLY
// configured passkey. FeedWriter persists AB items GUID-only - never a
// passkey-bearing download URL (see stripDownloadURLs) - so this derivation
// is what makes the loaded AB feed servable at all; it also means a rotated
// indexer.ab_passkey takes effect on the next load, and a LEGACY snapshot
// that still embeds a (possibly rotated) passkey URL is overwritten rather
// than served verbatim. An empty configured passkey clears the AB feed (serve
// already answers the /ab RSS check with a Torznab <error> then); an item
// whose current URL cannot be derived (no parseable AB id in its GUID) is
// dropped rather than served link-less.
func (ix *Indexer) rebuildABDownloadURLs(feed []journalItem) []journalItem {
	if len(feed) == 0 {
		return feed
	}
	if ix.cfg.ABPasskey == "" {
		return nil
	}
	out, dropped, samples := rebuildDownloadURLs(feed, release.TrackerNameAnimeBytes, ix.cfg.ABPasskey)
	if dropped > 0 {
		// The GUID (a tracker page URL) is not a secret and names the
		// undecodable items; the download URL (which embeds the passkey) is
		// never logged.
		ix.log.Warn("indexer feed snapshot: AnimeBytes items dropped; no download URL derivable from tracker page URL",
			"path", ix.path, "dropped", dropped, "kept", len(out), "sample_guids", samples)
	}
	return out
}

// rebuildNyaaDownloadURLs derives each persisted Nyaa feed item's download
// URL from its non-secret tracker page URL (the GUID), mirroring
// rebuildABDownloadURLs. The Nyaa link carries no credential, but re-deriving
// it at the load boundary keeps the persisted snapshot non-authoritative for
// fetch targets on BOTH feeds: a tampered /config/feed.json cannot plant an
// arbitrary URL that renderFeed would then hand the arrs as a curated
// release's enclosure. FeedWriter only ever produces Nyaa links of the fixed
// nyaa.BaseURL/download/{id}.torrent shape, so the derivation is lossless for
// every writer-produced snapshot; an item whose URL cannot be derived (no
// parseable Nyaa id in its GUID) is dropped rather than served link-less.
func (ix *Indexer) rebuildNyaaDownloadURLs(feed []journalItem) []journalItem {
	if len(feed) == 0 {
		return feed
	}
	out, dropped, samples := rebuildDownloadURLs(feed, release.TrackerNameNyaa, "")
	if dropped > 0 {
		ix.log.Warn("indexer feed snapshot: Nyaa items dropped; no download URL derivable from tracker page URL",
			"path", ix.path, "dropped", dropped, "kept", len(out), "sample_guids", samples)
	}
	return out
}

// seadexInfoHost is the canonical releases.moe hostname persisted InfoURLs
// must live on, derived once from the same constant the writer builds them
// from (feed.go's defaultSeaDexBaseURL) so the two ends cannot drift.
var seadexInfoHost = sync.OnceValue(func() string {
	u, err := url.Parse(defaultSeaDexBaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
})

// sanitizeSnapshotInfoURLs blanks any persisted item's InfoURL that is not a
// userinfo-free absolute http(s) URL on the canonical SeaDex host - the only
// shape the writer ever persists (entryURL). The persisted snapshot crosses
// the same trust boundary rebuildDownloadURLs defends for fetch targets:
// renderFeed hands InfoURL to the arr UI as the item's clickable info link,
// so a tampered feed.json must not plant a javascript:/data:/foreign-host
// link there. Blanking (never dropping) mirrors the search path's
// sanitizeDisplayURL: writeItem omits an empty <comments>.
func (ix *Indexer) sanitizeSnapshotInfoURLs(feed []journalItem) []journalItem {
	host := seadexInfoHost()
	blanked := 0
	for i := range feed {
		if feed[i].InfoURL == "" || snapshotInfoURLAllowed(feed[i].InfoURL, host) {
			continue
		}
		feed[i].InfoURL = ""
		blanked++
	}
	if blanked > 0 {
		// Counts only; the rejected value can be attacker-shaped text.
		ix.log.Warn("indexer feed snapshot: non-SeaDex info URLs blanked",
			"path", ix.path, "blanked", blanked)
	}
	return feed
}

// snapshotInfoURLAllowed reports whether raw is a userinfo-free absolute
// http(s) URL on the canonical SeaDex host.
func snapshotInfoURLAllowed(raw, host string) bool {
	if host == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	if s != "http" && s != "https" {
		return false
	}
	return strings.EqualFold(u.Hostname(), host)
}
