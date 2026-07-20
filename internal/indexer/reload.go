package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"

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
// request refresh; the rest return immediately and serve the current immutable
// snapshot (the next request picks up the newly installed one).
func (ix *Indexer) reload(ctx context.Context) {
	if ix.path == "" {
		return
	}
	if !ix.reloadMu.TryLock() {
		return
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
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		ix.markSnapshotFailedIfUnloaded()
		ix.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", ix.path, "error", err)
		return snapshot{}, false, true
	}
	// Syntactically valid JSON is not yet a usable snapshot: `null` or `{}`
	// decodes cleanly into a zero value, and installing it would blank both
	// synthesized feeds and both curation maps. The writer always emits
	// non-nil by_hash/by_key maps - even for an honestly empty catalogue - so
	// nil curation maps identify a structurally invalid snapshot without
	// rejecting a valid empty feed.
	if snap.ByHash == nil || snap.ByKey == nil {
		ix.markSnapshotFailedIfUnloaded()
		ix.log.Warn("indexer feed snapshot malformed; keeping current feed",
			"path", ix.path, "reason", "missing required curation maps")
		return snapshot{}, false, true
	}
	snap.ABFeed = ix.rebuildABDownloadURLs(snap.ABFeed)
	return snap, true, false
}

// rebuildABDownloadURLs derives each persisted AnimeBytes feed item's download
// URL from its non-secret tracker page URL (the GUID) and the CURRENTLY
// configured passkey. FeedWriter persists AB items GUID-only - never a
// passkey-bearing download URL (see stripABDownloadURLs) - so this derivation
// is what makes the loaded AB feed servable at all; it also means a rotated
// indexer.ab_passkey takes effect on the next load, and a LEGACY snapshot
// that still embeds a (possibly rotated) passkey URL is overwritten rather
// than served verbatim. An empty configured passkey clears the AB feed (serve
// already answers the /ab RSS check with a Torznab <error> then); an item
// whose current URL cannot be derived (no parseable AB id in its GUID) is
// dropped rather than served link-less.
func (ix *Indexer) rebuildABDownloadURLs(feed []item) []item {
	if len(feed) == 0 {
		return feed
	}
	if ix.cfg.ABPasskey == "" {
		return nil
	}
	out := make([]item, 0, len(feed))
	dropped := 0
	var samples []string
	for i := range feed {
		it := feed[i]
		dl, ok := downloadURL(release.TrackerNameAnimeBytes, it.GUID, ix.cfg.ABPasskey)
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
	if dropped > 0 {
		// The GUID (a tracker page URL) is not a secret and names the
		// undecodable items; the download URL (which embeds the passkey) is
		// never logged.
		ix.log.Warn("indexer feed snapshot: AnimeBytes items dropped; no download URL derivable from tracker page URL",
			"path", ix.path, "dropped", dropped, "kept", len(out), "sample_guids", samples)
	}
	return out
}
