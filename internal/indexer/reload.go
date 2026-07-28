package indexer

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/urlform"
)

// snapshotCache owns the persisted-snapshot lifecycle: loading /config/feed.json,
// memoizing a deterministically bad file, tracking the missing/degraded/
// unavailable states, and publishing the loaded snapshot for the request path to
// read. It exists as its own type because that lifecycle is a second reason to
// change with its own locking contract, and the contract used to be prose only:
// eleven of Indexer's fifteen fields were this cache, under TWO concurrency
// regimes (mu for the published fields a request reads, reloadGate for the
// reload-only flags), enforced by per-field comments that nothing stopped a
// future serving method from ignoring (l-f168/l-f174). Behind this type the
// reload-only flags are unreachable from the serving path structurally, and the
// cache is exercisable without constructing an HTTP server.
//
// The server reaches it through six methods only - curation(), feed(scope),
// refresh(ctx) and unavailable() on the serving side, warm() and warmPending()
// for the initial-load lifecycle - so query.go and server.go never name a lock
// primitive or a reload-only flag. ENABLEMENT policy stays outside: the cache
// answers what is loaded, while whether a tracker's feed may be SERVED at all is
// the server's call (feedFor's URL gate).
type snapshotCache struct {
	// snapID identifies the successfully loaded snapshot file, installed
	// together with snap (guarded by mu). atomicfile.FileIdentity owns the
	// mtime-AND-os.SameFile comparison because the correct form of that test is
	// knowledge about atomicfile's own publish-by-rename barrier: an equal
	// mtime on a DIFFERENT inode is a preserved-timestamp replacement (a backup
	// restore, an rsync -t of an archived generation) that must install rather
	// than be skipped, while an in-place rewrite keeps the inode and only the
	// mtime leg catches it.
	snapID atomicfile.FileIdentity
	// failedID identifies the last snapshot file whose CONTENT failed
	// deterministically: malformed JSON, a structurally invalid document, or a
	// file over the shared maxFeedBytes cap (persist enforces the same cap, so
	// an oversized snapshot is external corruption that never shrinks on its
	// own). An unchanged bad file is then not re-read and re-warned on every
	// request; cleared on a successful load. Only deterministic content
	// failures are memoized: a read failure (EIO, EACCES) can recover without
	// changing inode or mtime (a chmod, a transient filesystem repair), so it
	// stays retryable. The same identity test as snapID, for the same reason -
	// a repaired file published at the failed file's timestamp must be retried,
	// not skipped. Guarded by reloadGate (set/cleared only inside refresh).
	failedID atomicfile.FileIdentity
	// reloadGate coalesces concurrent snapshot refreshes: only one caller runs
	// refresh's stat/read/unmarshal at a time; the rest serve the current
	// immutable snapshot (see refresh). It also guards the reload-only fields
	// failedID / snapMissing / reloadDegraded. It is a capacity-one token
	// channel rather than a sync.Mutex because the pre-first-load wait must be
	// cancellable: a request that has already gone away (client disconnect, arr
	// timeout) must not keep a handler goroutine and its connection parked
	// behind the winner's whole stat/read/decode, which no server write timeout
	// bounds. A send acquires the gate; the matching receive releases it.
	reloadGate chan struct{}
	// warmDone is closed when warm's initial load returns; warmStarted says
	// whether anything will ever close it (a cache nobody warmed keeps the
	// lazy per-request refresh path). Allocated by newSnapshotCache so the
	// request path can always read it.
	warmDone chan struct{}
	// log, path, and abPasskey are set once by newSnapshotCache and read
	// without a lock, never written afterwards. abPasskey is the one config
	// value the cache genuinely needs: the load path re-derives every AB
	// download link from the persisted GUID (rebuildABDownloadURLs), so the
	// snapshot is never authoritative for fetch targets.
	log       *slog.Logger
	path      string
	abPasskey string
	snap      snapshot
	// mu guards the published snapshot fields read per request: snap,
	// snapID, snapFailed, and snapFailedWarned (see the per-field comments).
	mu sync.RWMutex
	// snapMissing records that the snapshot file disappeared AFTER one was
	// loaded (deleted file, incomplete restore, lost volume), so the
	// stale-feed WARN fires once per disappearance instead of on every
	// request; cleared (with one INFO recovery line) on the first successful
	// stat afterward. A fresh install with no prior snapshot stays silent.
	// Guarded by reloadGate (set/cleared only inside refresh).
	snapMissing bool
	// reloadDegraded records that reloads are failing (a stat error or a
	// read failure of an unchanged-identity file), so the WARN fires once
	// per degradation onset instead of on every request; cleared with one
	// INFO recovery line on the next successful snapshot read, and cleared
	// SILENTLY when the file goes absent (openSnapshot's ENOENT arm - the
	// missing state has its own once-per-disappearance WARN) or when the
	// stat lands on the memoized malformed file (skipMemoizedMalformed -
	// access recovered, but nothing was reloaded). The retry itself is NOT
	// suppressed (both faults can recover without an mtime change). Guarded
	// by reloadGate (set/cleared only inside refresh).
	reloadDegraded bool
	// snapFailed records that snapshot loading failed BEFORE any snapshot was
	// installed: a non-ENOENT stat or read fault, or a malformed or
	// structurally invalid file, at startup leaves the zero-value in-memory
	// snapshot indistinguishable from the intentional fresh-install state -
	// query would contact Prowlarr, filter every result against nil curation
	// maps, and serve a successful empty feed, so the arr records a clean
	// no-match during a local fault. While set, query answers with a
	// snapshot-unavailable flag (no Prowlarr query) and serve renders a
	// Torznab <error>, like an unavailable Prowlarr dependency. Set on those
	// failure paths only while no snapshot is recorded (a fault AFTER a
	// successful load keeps serving the last-good snapshot); cleared by the
	// first successful installSnapshot, and by a genuinely absent file (deleting
	// the bad file returns to fresh-install semantics). Guarded by mu (read
	// per request by unavailable, unlike the reloadGate-guarded flags above).
	snapFailed bool
	// snapFailedWarned bounds the snapshot-unavailable WARN to one per onset
	// instead of one per request; re-armed whenever snapFailed clears.
	// Guarded by mu.
	snapFailedWarned bool
	// warmStarted records that warm ran, so warmPending can tell a
	// still-loading cache from one nobody warmed at all (a cache used without
	// Run keeps the lazy per-request refresh).
	warmStarted atomic.Bool
}

// newSnapshotCache builds the cache for the snapshot file at path. It does not
// load: the caller decides when the first refresh runs (Run warms it eagerly so
// a restart serves the last feed immediately).
func newSnapshotCache(path, abPasskey string, log *slog.Logger) *snapshotCache {
	return &snapshotCache{
		log:        log,
		path:       path,
		abPasskey:  abPasskey,
		reloadGate: make(chan struct{}, 1),
		warmDone:   make(chan struct{}),
	}
}

// warmLoadTimeout bounds how long warm WAITS for the initial load of the
// persisted snapshot - not the load itself. The read is size-bounded
// (maxFeedBytes) but a slow or wedged /config mount has no bound of its own, and
// Run calls warm on the daemon's startup path (main.go's startIndexer, alongside
// the compare loop), so an unbounded WAIT holds the whole daemon down instead of
// one request. A context deadline cannot deliver this bound: refresh stats the
// file before any ctx check, and atomicfile's bounded read only tests ctx around
// its syscalls - it cannot interrupt an os.Open, File.Stat, or io.ReadAll already
// blocked in the filesystem. So the load runs asynchronously and warm stops
// waiting after the deadline; the load may finish in the background, which is
// safe because the cache is synchronized and refresh coalesces through
// reloadGate, so whoever finishes installs and the first request either sees the
// warmed snapshot or reloads itself.
//
// A var, not a const, ONLY so the warm-load test can exercise the wait-expired
// path (see queryGateWait for the pattern) without spending it in real time.
var warmLoadTimeout = 15 * time.Second

// warm loads the served feed from the last persisted snapshot so a restart
// serves immediately rather than empty until the next cycle. Run calls it before
// binding, so the work begins under the explicit lifecycle boundary rather than
// during construction. The load runs asynchronously and only the WAIT is
// bounded, by warmLoadTimeout or by ctx, whichever comes first: a wedged /config
// mount cannot be interrupted mid-syscall, so bounding the wait is the only
// bound that holds (see warmLoadTimeout), and honouring ctx keeps a shutdown
// during a slow load from being reported as a failed request drain. It is
// one-shot: a second call returns immediately, leaving the first load's result
// in place. A request arriving while the load is still running does not park
// behind it (see warmPending).
func (c *snapshotCache) warm(ctx context.Context) {
	// One-shot by construction: warmDone may only be closed once, so a second
	// Run (a supervisor retrying after a bind failure) must not start a second
	// loader - the close would panic in a goroutine outside the daemon's
	// recover shield. The first load's result is still the one being served.
	if !c.warmStarted.CompareAndSwap(false, true) {
		return
	}
	// The load itself is deliberately detached from ctx: a wedged /config mount
	// cannot be interrupted mid-syscall, so only the WAIT below is bounded.
	// WithoutCancel keeps the ctx's values while dropping its cancellation and
	// deadline, which is the same lifetime a bare Background carried.
	loadCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(c.warmDone)
		c.refresh(loadCtx)
	}()
	warmTimer := time.NewTimer(warmLoadTimeout)
	defer warmTimer.Stop()
	select {
	case <-c.warmDone:
	case <-ctx.Done():
		// Shutting down before the load returned: stop waiting so the feed's
		// goroutine returns inside the daemon's drain budget instead of being
		// reported as a failed request drain. The load dies with the process.
		c.log.Debug("feed snapshot warm load abandoned; shutting down",
			"cause", context.Cause(ctx))
	case <-warmTimer.C:
		c.log.Warn("feed snapshot warm load still running; serving requests without it",
			"timeout", warmLoadTimeout)
	}
}

// warmPending reports whether the initial load was started and has not
// finished. While that holds, the initial loader owns the reload gate and a
// request that entered refresh would block on it for as long as the filesystem
// does - net/http's WriteTimeout cannot cancel a handler, so a wedged /config
// mount would pin every request slot. Requests answer the snapshot-unavailable
// fault instead until the loader returns. A cache nobody warmed (direct query
// users and tests) keeps the lazy per-request refresh path.
func (c *snapshotCache) warmPending() bool {
	if !c.warmStarted.Load() {
		return false
	}
	select {
	case <-c.warmDone:
		return false
	default:
		return true
	}
}

// curation returns the three curation maps a search filters against. The maps
// are safe to read after the lock is released: refresh installs a fresh snapshot
// and never mutates the loaded maps in place (the same invariant feed documents
// for the journal slices).
func (c *snapshotCache) curation() curation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return curation{byHash: c.snap.ByHash, byKey: c.snap.ByKey, byPair: c.snap.ByPair}
}

// feed returns the loaded journal for a tracker scope, or nil for an unknown
// one. The returned slice is safe to use after the lock is released: refresh
// installs a fresh snapshot with new backing arrays and never mutates the old
// ones, so a slice handed out here stays immutable even across a swap. Callers
// must only read it (never append/write in place). Whether that scope may be
// served at all is the server's enablement decision, not the cache's.
func (c *snapshotCache) feed(scope string) []journalItem {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch scope {
	case upstreamNyaa:
		return c.snap.NyaaFeed
	case upstreamAB:
		return c.snap.ABFeed
	}
	return nil
}

// openSnapshot opens the snapshot file ONCE and applies refresh's
// missing/unreadable policy, returning the open descriptor, its info, and
// whether reload should proceed. The caller owns the returned file and must
// close it. A missing file after one was loaded warns once (the feed is now
// stale); any other open error (EACCES, EIO) warns and freezes the current
// feed. On the recovery path it clears snapMissing and logs one line: INFO when
// a reload will follow, WARN when the file that came back is the same
// deterministically-bad generation refresh already memoized (nothing reloads,
// so "resuming reloads" would be false).
//
// One descriptor is the point. The previous shape observed the pathname three
// separate times - os.Stat, os.Lstat, then atomicfile.ReadBounded's own os.Open
// - and the app's own writer publishes by atomic rename, so a replacement
// landing between them let refresh decode one generation's bytes while
// recording another's FileIdentity (a deterministic failure memoized against
// the wrong inode), and made every non-regular-file rejection a
// check-then-open TOCTOU: a FIFO swapped in after the Lstat still blocked
// ReadBounded's open while this caller held reloadGate, so the asynchronous
// warm loader leaked and every pre-first-load request parked behind it until
// its own context expired. Binding validation, identity and bytes to one
// descriptor closes both: O_NOFOLLOW refuses a final-component symlink at open
// time (matching the writer's ErrSymlinkTarget contract, which
// atomicfile.ReadBounded cannot honor because os.Open follows links) and
// O_NONBLOCK makes a raced FIFO open return immediately so the regular-file
// check can reject it instead of blocking forever. The gate is the full
// regular-file predicate rather than a symlink test: a socket, device, or
// directory left at the path is the same non-regular ingress. Every rejection
// takes the same arm as any other open fault: warn once per onset, keep the
// current feed, and mark the snapshot-unavailable state while nothing has
// loaded.
func (c *snapshotCache) openSnapshot() (*os.File, os.FileInfo, bool) {
	f, err := os.OpenFile(c.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.noteSnapshotAbsent()
			return nil, nil, false
		}
		// Anything else (EACCES, EIO, and ELOOP for a symlink at the final
		// component) silently freezes the served feed, so make it visible -
		// once per onset, not once per request.
		c.noteStatFault("indexer feed snapshot open failed; keeping current feed", "error", err)
		return nil, nil, false
	}
	info, serr := f.Stat()
	if serr != nil {
		f.Close()
		c.noteStatFault("indexer feed snapshot stat failed; keeping current feed", "error", serr)
		return nil, nil, false
	}
	if !info.Mode().IsRegular() {
		f.Close()
		c.noteStatFault("indexer feed snapshot path is not a regular file; refusing to load it", "mode", info.Mode().String())
		return nil, nil, false
	}
	if c.snapMissing {
		c.snapMissing = false
		if c.matchesFailedFile(info) {
			// The file is back but it is the same deterministically-bad
			// generation refresh already memoized, so nothing will reload
			// (skipMemoizedMalformed returns before the read) and the served
			// feed stays frozen on the last-good snapshot with no further
			// signal. Saying "resuming reloads" here would be the last line the
			// operator sees, and it would be false.
			c.log.Warn("indexer feed snapshot reappeared but is the same malformed file; still serving the last loaded feed",
				"path", c.path)
		} else {
			c.log.Info("indexer feed snapshot reappeared; resuming reloads", "path", c.path)
		}
	}
	return f, info, true
}

// noteSnapshotAbsent applies the missing-file policy openSnapshot's ErrNotExist
// arm carries. A missing file is the normal fresh-install case, but after a
// snapshot was loaded it means the materialized view can no longer refresh:
// every request keeps serving the last in-memory feed, so warn once that the
// feed is stale, then stay quiet until the file reappears.
//
// Absence is a successful stat determination, so it ENDS any stat/read
// degradation episode: clear the transient flag (no recovery INFO - nothing was
// reloaded; the missing state has its own once-per-disappearance WARN) so the
// next fault onset warns again instead of being suppressed by a stale flag.
func (c *snapshotCache) noteSnapshotAbsent() {
	c.reloadDegraded = false
	c.mu.RLock()
	loaded := c.snapID.Recorded()
	c.mu.RUnlock()
	if !loaded {
		// A genuinely absent first snapshot IS the fresh-install state -
		// serving the empty feed is intentional there - so an earlier load
		// fault stops blocking requests once the bad file is gone (deleting it
		// returns to fresh-install semantics).
		c.clearSnapshotFailed()
	}
	if loaded && !c.snapMissing {
		c.snapMissing = true
		c.log.Warn("indexer feed snapshot missing; serving last loaded feed until it reappears", "path", c.path)
	}
}

// noteStatFault is the shared onset ladder for every stat-time fault that
// freezes the served feed (an unreadable file, a non-regular path): mark the
// snapshot-unavailable state while nothing has loaded, then WARN once per onset
// rather than once per request.
func (c *snapshotCache) noteStatFault(msg string, attrs ...any) {
	c.markSnapshotFailedIfUnloaded()
	if c.reloadDegraded {
		return
	}
	c.reloadDegraded = true
	c.log.Warn(msg, append([]any{"path", c.path}, attrs...)...)
}

// reloadBlockGate is a test seam (see snapshotUnavailableGate for the
// pattern) marking the moment a pre-first-load coalescing loser commits to
// WAITING on the reload gate instead of returning. A no-op in production.
var reloadBlockGate = func() {}

// tryLockReload acquires the reload gate without waiting, reporting whether
// this caller won the refresh. The sending caller owns the gate until the
// matching unlockReload.
func (c *snapshotCache) tryLockReload() bool {
	select {
	case c.reloadGate <- struct{}{}:
		return true
	default:
		return false
	}
}

// lockReloadOrDone waits for the reload gate, giving up when ctx is done, and
// reports whether it was acquired. This is the cancellable half of the gate: a
// pre-first-load loser must wait for the winner's fresh-install-vs-failed
// verdict, but a request whose client has gone away must be able to abandon
// that wait instead of parking a handler goroutine behind an unbounded
// stat/read/decode.
func (c *snapshotCache) lockReloadOrDone(ctx context.Context) bool {
	select {
	case c.reloadGate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// unlockReload releases the reload gate acquired by tryLockReload or
// lockReloadOrDone.
func (c *snapshotCache) unlockReload() { <-c.reloadGate }

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
// under-mu recheck let only one install it. tryLockReload lets exactly one
// request refresh; once a snapshot has loaded, the rest return immediately and
// serve the current immutable snapshot (the next request picks up the newly
// installed one). Before the FIRST successful load, losers wait for the gate
// instead: the winner has not yet established whether the on-disk snapshot is
// usable, so returning early would have to guess between fresh-install and
// failed state (see the branch below). That wait is CANCELLABLE (ctx): a client
// that disconnected or timed out must not keep its handler goroutine parked
// behind the winner's stat/read/decode, which no server write timeout bounds.
func (c *snapshotCache) refresh(ctx context.Context) {
	if c.path == "" {
		return
	}
	if !c.tryLockReload() {
		c.mu.RLock()
		loaded := c.snapID.Recorded()
		c.mu.RUnlock()
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
		// the gate, making one startup request render a false
		// snapshot-unavailable Torznab error. Initial-load callers instead
		// WAIT until the winning reload has established fresh-install,
		// failed, or loaded state; once acquired, this caller runs the
		// normal stat/read path itself, so a cancelled winner is also
		// retried.
		reloadBlockGate()
		if !c.lockReloadOrDone(ctx) {
			// The caller went away (client disconnect, arr timeout) before
			// the winner established state: abandon the wait rather than
			// accumulate parked goroutines and connections behind it. The
			// snapshot state is left exactly as the winner will set it, and
			// the next request retries.
			return
		}
	}
	defer c.unlockReload()
	f, info, ok := c.openSnapshot()
	if !ok {
		return
	}
	defer f.Close()
	if c.skipMemoizedMalformed(info) {
		return
	}
	// A degraded reload must not take the unchanged-loaded-snapshot fast
	// path: after a stat fault recovers, the file may be the already-loaded
	// inode at the same mtime, so skipping here would leave reloadDegraded
	// set forever — the recovery INFO never emits and the next onset's
	// warning is suppressed by the stale flag. Forcing one bounded read
	// clears the state through the recovery block below; a persistent read
	// fault keeps it degraded without falsely declaring recovery.
	if c.loadedSnapshotUnchanged(info) && !c.reloadDegraded {
		return
	}
	snap, ok, memoize := c.readSnapshot(ctx, f)
	if !ok {
		c.recordSnapshotFailure(info, memoize)
		return
	}
	c.failedID = atomicfile.FileIdentity{}
	if c.reloadDegraded {
		c.reloadDegraded = false
		c.log.Info("indexer feed snapshot reload recovered", "path", c.path)
	}
	if !c.installSnapshot(info, &snap) {
		return
	}
	c.log.Info("indexer feed snapshot loaded",
		"path", c.path, "hashes", len(snap.ByHash), "keys", len(snap.ByKey),
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
// snapFailed to restore fresh-install semantics while keeping failedID, so
// without re-asserting here the pre-load state machine would treat the bad
// snapshot as a valid fresh install and serve false-empty success instead of
// a Torznab error.
func (c *snapshotCache) skipMemoizedMalformed(info os.FileInfo) bool {
	if !c.matchesFailedFile(info) {
		return false
	}
	c.markSnapshotFailedIfUnloaded()
	c.reloadDegraded = false
	return true
}

// recordSnapshotFailure applies reload's failed-read memo policy. Only a
// deterministic content failure is memoized: bytes that decode the same way on
// every read (malformed JSON, a structurally invalid document) or a file whose
// size alone already exceeds the shared cap. Recoverable read failures can
// succeed after a chmod or transient filesystem repair without changing inode
// or mtime, so they must remain retryable - readSnapshot reports those
// (including a cancellation, where the file was never actually read) with
// memoize=false. memoize=true means the failure is reproducible from the same
// inode/mtime, so the memo holds even when the requesting context was cancelled
// after the read completed.
func (c *snapshotCache) recordSnapshotFailure(info os.FileInfo, memoize bool) {
	c.failedID = atomicfile.FileIdentity{}
	if memoize {
		c.failedID = atomicfile.Identify(info)
	}
}

// loadedSnapshotUnchanged reports whether the stat'ed snapshot file is the
// already-loaded snapshot, unchanged. The comparison is
// atomicfile.FileIdentity's (see the snapID field): any mtime CHANGE -
// including an older one - reloads, and a preserved-timestamp replacement on a
// different inode reloads too.
func (c *snapshotCache) loadedSnapshotUnchanged(info os.FileInfo) bool {
	c.mu.RLock()
	loaded := c.snapID
	c.mu.RUnlock()
	return loaded.Matches(info)
}

// matchesFailedFile reports whether the stat'ed snapshot file is the memoized
// malformed file, unchanged by the same identity test as the loaded leg: an
// unchanged malformed file fails deterministically, so it is never re-read
// (reload clears only the transient reloadDegraded flag and returns), while any
// mtime or identity change means new bytes worth retrying.
func (c *snapshotCache) matchesFailedFile(info os.FileInfo) bool {
	return c.failedID.Matches(info)
}

// installSnapshot publishes snap as the served feed under mu, recording the
// file's identity for the next reload's skip check, and reports whether it
// installed. The re-check under the write lock is defense in depth: reloadGate
// already serializes the whole stat/read/install sequence, so no concurrent
// reload can install in between today, but never re-installing a copy of what
// is already loaded holds even if the gate coalescing changes. Same identity
// test as loadedSnapshotUnchanged.
func (c *snapshotCache) installSnapshot(info os.FileInfo, snap *snapshot) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapID.Matches(info) {
		return false
	}
	c.snap = *snap
	c.snapID = atomicfile.Identify(info)
	// A successful install ends any startup snapshot-unavailable state and
	// re-arms its per-onset WARN (see snapFailed).
	c.snapFailed = false
	c.snapFailedWarned = false
	return true
}

// markSnapshotFailedIfUnloaded flags the snapshot-unavailable state (see the
// snapFailed field) after a load fault, but only while no snapshot has ever
// been installed: after a successful load the last-good snapshot keeps being
// served instead.
func (c *snapshotCache) markSnapshotFailedIfUnloaded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.snapID.Recorded() {
		c.snapFailed = true
	}
}

// clearSnapshotFailed resets the snapshot-unavailable state and re-arms its
// per-onset WARN (see the snapFailed field).
func (c *snapshotCache) clearSnapshotFailed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapFailed = false
	c.snapFailedWarned = false
}

// readSnapshot is reload's read/decode error policy: it bounded-reads f - the
// descriptor openSnapshot already validated, so the bytes decoded here are
// exactly the ones the recorded FileIdentity describes - and decodes the
// persisted feed snapshot, reporting ok=false on any failure so
// the caller keeps the current feed. A shutdown cancellation is silent; an
// unreadable or malformed file is logged (a bad write must never blank a live
// feed). The third result means "memoize unchanged bytes": true for every
// failure that is deterministic for an unchanged file - malformed JSON, a
// structurally invalid document, and an over-cap file (persist enforces the
// same maxFeedBytes cap, so an oversized snapshot is external corruption that
// never shrinks on its own; the writer's classifyPreviousReadError classifies
// it the same way). A recoverable read failure (EIO, a fixable EACCES) can
// succeed without changing inode or mtime, so it stays retryable.
func (c *snapshotCache) readSnapshot(ctx context.Context, f *os.File) (snapshot, bool, bool) {
	data, err := atomicfile.ReadBoundedFile(ctx, f, maxFeedBytes)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// A shutdown cancellation is silent and never marks the
			// snapshot-unavailable state (the file was never actually read;
			// a retry could succeed).
			c.markSnapshotFailedIfUnloaded()
			if !c.reloadDegraded {
				c.reloadDegraded = true
				c.log.Warn("indexer feed snapshot unreadable; keeping current feed", "path", c.path, "error", err)
			}
		}
		// An over-cap file is the one pre-decode failure that is deterministic
		// for an unchanged inode, so it memoizes like malformed JSON: without
		// that, sustained requests reopen and size-check the same oversized
		// file on every reload attempt while it remains unchanged.
		return snapshot{}, false, errors.Is(err, atomicfile.ErrFileTooLarge)
	}
	snap, scrub, reason, decodeErr := decodeSnapshot(data)
	if decodeErr != nil {
		c.markSnapshotFailedIfUnloaded()
		// Bounded through the shared emit-boundary policy: a decoder error can
		// embed the offending document text (encoding/json quotes an over-range
		// numeric literal verbatim), and feed.json is a tamperable boundary.
		c.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", c.path, "error", capLogText(decodeErr.Error(), 256))
		return snapshot{}, false, true
	}
	// `null` or `{}` decodes cleanly into a zero value; nil curation maps
	// and over-limit items identify a structurally invalid snapshot (see
	// decodeSnapshot). Both are deterministic for unchanged bytes, so they
	// memoize like malformed JSON; the offending value itself is never
	// logged (it can be attacker-shaped multi-megabyte text).
	if reason != "" {
		c.markSnapshotFailedIfUnloaded()
		c.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", c.path, "reason", reason)
		return snapshot{}, false, true
	}
	reportTransitionalSchema(c.log, c.path, &snap)
	// A persisted FirstSeen ahead of the wall clock is repaired on the writer's
	// carry path (prepareCarriedItem), but the reader installs the decoded feed
	// directly: without the same correction a restored future-skewed or
	// hand-edited snapshot is served with a future <pubDate> until the next
	// rebuild - indefinitely in resident-idle mode - where an arr's delay
	// profile sees a negative release age and can hold the release instead of
	// honoring the bounded journal window (h-f15).
	now := time.Now().UTC()
	if rebased := rebaseFutureFeed(snap.NyaaFeed, now) + rebaseFutureFeed(snap.ABFeed, now); rebased > 0 {
		// Counts only; the rejected timestamp comes from a tamperable file.
		c.log.Warn("indexer feed snapshot: future item timestamps rebased to load time",
			"path", c.path, "rebased", rebased)
	}
	snap.ABFeed = c.rebuildABDownloadURLs(snap.ABFeed)
	snap.NyaaFeed = c.rebuildNyaaDownloadURLs(snap.NyaaFeed)
	c.warnBlankedInfoURLs(scrub)
	return snap, true, false
}

// warnBlankedInfoURLs reports the info-URL scrub PER TRACKER, one line per
// affected feed. The attribution is the point: a tampered or hand-edited
// feed.json is the only way these URLs get there, and an operator seeing a
// single summed count cannot tell which journal was touched (l-f176). Scopes are
// iterated in a fixed order so the lines are deterministic for a test to pin and
// for a human to diff across reloads.
func (c *snapshotCache) warnBlankedInfoURLs(scrub snapshotScrub) {
	for _, scope := range []string{upstreamNyaa, upstreamAB} {
		if n := scrub.blankedInfoURLs[scope]; n > 0 {
			// Counts only; the rejected value can be attacker-shaped text.
			c.log.Warn("indexer feed snapshot: non-SeaDex info URLs blanked",
				"path", c.path, "tracker", scope, "blanked", n)
		}
	}
}

// reportTransitionalSchema reports (and where needed neutralizes) a loaded
// snapshot written by an older binary. Both arms are one INFO per load: the
// state is real but transitional, it clears on the next cycle's snapshot
// rewrite, and neither is an operator fault.
//
//   - Seen == nil is the retired pre-journal schema. The journal contract (see
//     loadPrevious) treats its feeds as absent, so the curation maps are kept
//     (searches work) while the RSS feeds are dropped: an upgrade must never
//     re-broadcast the whole legacy catalogue as newly curated releases.
//   - ByPair == nil beside a non-empty ByHash is the pre-relation schema, and a
//     REACHABLE upgrade window: a released binary persisted no by_pair key, so
//     the first server start after this branch ships loads a snapshot whose
//     ByPair decodes nil. lookup then fails closed for every dual-signal item -
//     and a healthy Prowlarr Nyaa result carries BOTH an info hash and a
//     nyaa.si/view/{id} guid, so EVERY Nyaa search answers an empty 200 feed,
//     indistinguishable to the arr from "SeaDex curates nothing for this show".
//     Fail-closed is correct (absence of the co-membership relation is not
//     permission to fall back to the weaker per-signal checks), so the serving
//     decision is unchanged; what was missing is any way for an operator to tell
//     this local schema fault from a genuine no-match, since the only evidence
//     was curated=0 on the request line (l-f170). An EMPTY curation set carries
//     no signal (a fresh install curates nothing yet) and stays silent.
func reportTransitionalSchema(log *slog.Logger, path string, snap *snapshot) {
	if snap.Seen == nil {
		if len(snap.NyaaFeed) > 0 || len(snap.ABFeed) > 0 {
			log.Info("indexer feed snapshot is pre-journal schema; serving empty RSS feeds until the next cycle re-baselines",
				"path", path)
		}
		snap.NyaaFeed, snap.ABFeed = nil, nil
	}
	if len(snap.ByHash) > 0 && snap.ByPair == nil {
		log.Info("indexer feed snapshot predates the pair relation; searches match single-signal items only until the next cycle rewrites it",
			"path", path, "curated_hashes", len(snap.ByHash))
	}
}

// normalizeSnapshotItems re-canonicalizes each persisted item's non-derived
// wire fields, so a hand-edited, tampered, or legacy snapshot cannot put a
// value in the served feed that no producer could have written:
//
//   - InfoHash goes through validInfoHash (the writer's own gate), blanking
//     anything not a 40-char hex hash - writeItem renders it as the torznab
//     infohash attr, a field consumers treat as torrent identity.
//   - DownloadVolumeFactor is blanked unless it is exactly one of the two
//     markers the feed emits (dvfBest / dvfAlt). writeItem renders any
//     non-empty value as the downloadvolumefactor attr, the arr's freeleech
//     accounting input, so an out-of-vocabulary value ("0", arbitrary text)
//     would either present a curated release as fully freeleech or feed the
//     arr an unparseable factor; blanking falls back to the normal-item
//     (factor 1) shape writeItem already documents.
//   - Categories goes through validCategories, dropping any id outside this
//     feed's own vocabulary (catTV / catAnime / catMovies) - the list is the
//     arr's ROUTING input, so an at-rest value could route a series pack to
//     Radarr or hide a movie from it.
//
// validPersistedItem bounds only these fields' LENGTH, and carryStoredItem
// carries a non-curated item's stored render verbatim, so an at-rest value
// would otherwise survive both the serve path and the next rebuild.
// decodeSnapshot invokes this for BOTH consumers (reader and writer), so a
// non-canonical at-rest value can neither be served nor compared as identity
// by the writer's carry gates.
func normalizeSnapshotItems(feed []journalItem) {
	for i := range feed {
		feed[i].InfoHash = validInfoHash(feed[i].InfoHash)
		feed[i].DownloadVolumeFactor = validMarker(feed[i].DownloadVolumeFactor)
		feed[i].Categories = validCategories(feed[i].Categories)
	}
}

// validMarker returns m when it is one of the two download-volume-factor
// markers the feed emits (dvfBest / dvfAlt), else "" - writeItem then omits
// both factor attrs and the arr treats the item as a normal release.
func validMarker(m string) string {
	if m == dvfBest || m == dvfAlt {
		return m
	}
	return ""
}

// validCategories keeps only the Torznab category ids this feed's own
// vocabulary defines (catTV / catAnime / catMovies - categoriesFor emits the
// latter two, and catTV is the parent the caps document advertises), dropping
// anything else. An at-rest category is an arr ROUTING input exactly as the
// download-volume-factor marker is an arr accounting input: filterByCats
// matches on it and writeItem renders every positive id as a torznab category
// attr, so a hand-edited or legacy value could route a series pack to Radarr
// (2000) or hide a movie from it. Emptying the list falls back to writeItem's
// documented catAnime default, the same degradation validMarker relies on.
func validCategories(cats []int) []int {
	if len(cats) == 0 {
		return cats
	}
	out := cats[:0]
	for _, c := range cats {
		if c == catTV || c == catAnime || c == catMovies {
			out = append(out, c)
		}
	}
	return out
}

// rebuildDownloadURLs is the shared derivation mechanics behind
// rebuildABDownloadURLs and rebuildNyaaDownloadURLs: it re-derives each feed
// item's download URL from its non-secret tracker page URL (the GUID) via
// downloadURLForScope, which enforces the tracker-ownership gate internally
// (trackerOwnURL, the same fail-closed check writer-side journal admission
// runs through trackerKey). Persisted data crosses a separate trust boundary
// from writer admission: a tampered but structurally valid feed.json could
// otherwise carry a foreign (https://evil.example/view/123) or
// independent-subdomain (sukebei.nyaa.si/view/123) GUID whose numeric id
// would be minted into the apex tracker's download URL for an unrelated
// torrent; the gate drops such items exactly like an undecodable GUID. Each
// item must also pass the journal's GUID-to-Key invariant
// (journalIdentityMatches, the same check the writer's carry gates apply): a
// structurally valid snapshot whose GUID resolves to a DIFFERENT torrent
// than its persisted Key names (Key nyaa:42, GUID .../view/666) would
// otherwise rebuild and serve torrent 666 as the journaled curated item
// until a later writer rebuild self-heals. Any item failing either gate is
// dropped, collecting the drop count plus up to three bounded sample GUIDs
// for the wrappers' tracker-specific warnings. The wrappers own the policy
// (the AB passkey gate) and the exact log contract.
func rebuildDownloadURLs(feed []journalItem, scope, passkey string) (out []journalItem, dropped int, samples []string) {
	out = make([]journalItem, 0, len(feed))
	// drop accounts for one rejected item, keeping up to three sample GUIDs for
	// the wrappers' warnings. The GUID is a non-secret tracker page URL; bound
	// it through the shared emit-boundary policy before it reaches the log.
	drop := func(guid string) {
		dropped++
		if len(samples) < 3 {
			samples = append(samples, capLogText(guid, 256))
		}
	}
	for i := range feed {
		it := feed[i]
		if !journalIdentityMatches(&it) {
			drop(it.GUID)
			continue
		}
		dl, ok := downloadURLForScope(scope, it.GUID, passkey)
		if !ok {
			drop(it.GUID)
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
func (c *snapshotCache) rebuildABDownloadURLs(feed []journalItem) []journalItem {
	if len(feed) == 0 {
		return feed
	}
	if unusableABPasskey(c.abPasskey) {
		return nil
	}
	out, dropped, samples := rebuildDownloadURLs(feed, upstreamAB, c.abPasskey)
	if dropped > 0 {
		// The GUID (a tracker page URL) is not a secret and names the
		// undecodable items; the download URL (which embeds the passkey) is
		// never logged.
		c.log.Warn("indexer feed snapshot: AnimeBytes items dropped; no download URL derivable from tracker page URL",
			"path", c.path, "dropped", dropped, "kept", len(out), "sample_guids", samples)
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
func (c *snapshotCache) rebuildNyaaDownloadURLs(feed []journalItem) []journalItem {
	if len(feed) == 0 {
		return feed
	}
	out, dropped, samples := rebuildDownloadURLs(feed, upstreamNyaa, "")
	if dropped > 0 {
		c.log.Warn("indexer feed snapshot: Nyaa items dropped; no download URL derivable from tracker page URL",
			"path", c.path, "dropped", dropped, "kept", len(out), "sample_guids", samples)
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
	// ASCII-only fold, like every other host comparison in this file: a
	// strings.ToLower here is the full-Unicode fold snapshotInfoURLAllowed
	// exists to keep out of the expected host, and IsASCIIHost is the only
	// thing that currently stops a folded rune from reaching the allowlist.
	return asciiLowerHost(u.Hostname())
})

// sanitizeSnapshotInfoURLs blanks any persisted item's InfoURL that is not a
// userinfo-free absolute http(s) URL on the canonical SeaDex host - the only
// shape the writer ever persists (entryURL). The persisted snapshot crosses
// the same trust boundary rebuildDownloadURLs defends for fetch targets:
// renderFeed hands InfoURL to the arr UI as the item's clickable info link,
// so a tampered feed.json must not plant a javascript:/data:/foreign-host
// link there. Blanking (never dropping) mirrors the search path's
// sanitizeDisplayURL: writeItem omits an empty <comments>. It returns the
// number of blanked items and logs nothing itself: decodeSnapshot invokes it
// for BOTH consumers (reader and writer), and the operator-facing WARN is the
// caller's to keep - the server's readSnapshot logs the count, while the
// writer's loadPrevious sanitizes silently (its rebuild re-derives every
// curated item's InfoURL anyway).
func sanitizeSnapshotInfoURLs(feed []journalItem) int {
	host := seadexInfoHost()
	blanked := 0
	for i := range feed {
		if feed[i].InfoURL == "" || snapshotInfoURLAllowed(feed[i].InfoURL, host) {
			continue
		}
		feed[i].InfoURL = ""
		blanked++
	}
	return blanked
}

// snapshotInfoURLAllowed reports whether raw is a userinfo-free absolute
// http(s) URL, free of smuggling forms, on the canonical SeaDex host.
//
// This is a PUBLISH gate on a tamperable boundary - a persisted feed.json
// InfoURL is vouched here and handed to the arr web UI as a clickable link,
// where the BROWSER is the parser of record - so it reads its structural facts
// from urlform (the app's own classifier of record for exactly that
// browser-vs-net/url divergence, already applied at the sibling publish gate
// trackerlink.Publish) rather than from incidental net/url behavior, and matches
// host evidence behind urlform.IsASCIIHost.
//
// The host gate is the substantive change. It used to be a bare
// strings.EqualFold, which is the FULL-UNICODE simple fold urlform's ASCII-only
// fold plus IsASCIIHost were extracted to close: U+017F (long s) folds to 's',
// so a byte-wise foreign host could EqualFold-match the canonical
// releases.moe allowlist. For this particular hostname UTS46 happens to map
// that rune back to 's', so the old gate was not demonstrably exploitable -
// but the safety was incidental to releases.moe's letters, not designed, and a
// hostname change would have reopened it silently (l-f114). Host comparison
// stays ASCII-fold via urlform.Host with a non-ASCII host refused outright.
func snapshotInfoURLAllowed(raw, host string) bool {
	// Lower the expected host ONCE, ASCII-only, before any comparison. Keeping
	// the fold out of the comparison expression is deliberate: a
	// strings.EqualFold(f.Host, host) form reads equivalent and is what a linter
	// suggests, but EqualFold is the full-Unicode simple fold this gate exists to
	// avoid (U+017F folds to 's', so a byte-wise foreign host could match the
	// allowlist). urlform.Host is already ASCII-lowercased, so an ASCII-lowered
	// expectation makes the comparison a plain byte equality.
	want := asciiLowerHost(host)
	if want == "" || !urlform.IsASCIIHost(want) {
		return false
	}
	f := urlform.Classify(raw)
	if f.Class != urlform.ClassAbsolute {
		return false
	}
	if f.HasUserInfo || f.HasBackslash || f.HasTabOrNewline {
		// Publish-or-drop, the same stance trackerlink.Publish takes: a userinfo
		// authority is visual spoofing, and a de-smuggled backslash or
		// tab/newline form is not vouchable - a browser reads it differently
		// from net/url.
		return false
	}
	if !isHTTPScheme(f.Scheme) {
		return false
	}
	// IsASCIIHost refuses a non-ASCII host, so no homograph can fold into the
	// allowlisted name.
	return urlform.IsASCIIHost(f.Host) && f.Host == want
}

// asciiLowerHost lowercases ASCII A-Z only, leaving every other byte alone -
// the same fold urlform applies to Form.Host, and deliberately NOT a Unicode
// fold (which would launder homograph runes into ASCII).
func asciiLowerHost(host string) string {
	b := []byte(host)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// snapshotUnavailableGate is a test seam (see harvestWait for the pattern)
// marking the window between snapshotUnavailable's read-unlock and its
// write-lock, where a concurrent install/clear can race the escalation. A
// no-op in production.
var snapshotUnavailableGate = func() {}

// unavailable reports whether the startup snapshot-unavailable state
// (see the snapFailed field) is active, emitting its once-per-onset WARN on
// the first report so the local fault is visible without a per-request log
// storm. The state is set/cleared by reload's load paths; requests only read
// it here. The write-locked re-check is authoritative: a request that saw the
// failed state under the read lock but loses the race to an install/clear
// before acquiring the write lock answers from the fresh snapshot instead of
// rendering a stale Torznab error.
func (c *snapshotCache) unavailable() bool {
	c.mu.RLock()
	failed, warned := c.snapFailed, c.snapFailedWarned
	c.mu.RUnlock()
	if !failed {
		return false
	}
	if warned {
		return true
	}

	snapshotUnavailableGate()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the write lock, authoritatively: concurrent requests
	// racing the onset must still emit the WARN exactly once, and an install
	// that cleared snapFailed between the read-unlock and here must make THIS
	// request answer from the fresh snapshot rather than render a stale error.
	if !c.snapFailed {
		return false
	}
	if !c.snapFailedWarned {
		c.snapFailedWarned = true
		c.log.Warn("indexer feed snapshot unavailable; answering Torznab requests with an error until a snapshot loads",
			"path", c.path)
	}
	return true
}
