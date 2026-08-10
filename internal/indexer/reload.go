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
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/urlform"
)

// snapshotCache owns the served snapshot's lifecycle: taking it from the
// in-process compare cycle (publish) or from /config/feed.json, memoizing a
// deterministically bad file, tracking the missing/degraded/unavailable states,
// and publishing the current snapshot for the request path to read. It exists as
// its own type because that lifecycle is a second reason to change with its own
// locking contract, and the contract used to be prose only: eleven of Indexer's
// fifteen fields were this cache, under TWO concurrency regimes (mu for the
// published fields a request reads, a token gate for the reload-only flags),
// enforced by per-field comments that nothing stopped a future serving method
// from ignoring (l-f168/l-f174). Behind this type the reload-only flags are
// unreachable from the serving path structurally, and the cache is exercisable
// without constructing an HTTP server.
//
// The server reaches it through three methods only - curation(), feed(scope)
// and unavailable() - so query.go and server.go never name a lock primitive or a
// reload-only flag. ENABLEMENT policy stays outside: the cache answers what is
// loaded, while whether a tracker's feed may be SERVED at all is the server's
// call (feedFor's URL gate).
//
// Nothing on the request path LOADS. A snapshot arrives one of two ways, both
// off the request path: publish, which the in-process compare cycle calls with
// the snapshot it already holds (see FeedWriter.publishSnapshot), and the
// cache's own reload clock (start/watch), which re-stats the file every
// snapshotWatchInterval for restart recovery and for the out-of-process `poll`
// subcommand. A request then does one atomic read of whatever is published - no
// syscall, no gate, no wait - so a wedged /config mount can strand at most the
// one background loader instead of every handler that asked for a feed.
//
// That single background owner is also why there is no coalescing here: refresh
// has exactly one production caller (watch, sequential), so the reload-only
// fields need no lock of their own and there is nothing for concurrent requests
// to queue behind.
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
	// reload; cleared on a successful load. Only deterministic content
	// failures are memoized: a read failure (EIO, EACCES) can recover without
	// changing inode or mtime (a chmod, a transient filesystem repair), so it
	// stays retryable. The same identity test as snapID, for the same reason -
	// a repaired file published at the failed file's timestamp must be retried,
	// not skipped. Owned by the loader goroutine (see below).
	failedID atomicfile.FileIdentity
	// firstLoad is closed when the loader's initial load returns; watchStarted
	// says whether anything will ever close it (a cache nobody started keeps
	// serving whatever publish or a direct refresh installed). Allocated by
	// newSnapshotCache so the request path can always read it.
	firstLoad chan struct{}
	// log, path, and abPasskey are set once by newSnapshotCache and read
	// without a lock, never written afterwards. abPasskey is the one config
	// value the cache genuinely needs: the load path re-derives every AB
	// download link from the persisted GUID (rebuildABDownloadURLs), so the
	// snapshot is never authoritative for fetch targets.
	log       *slog.Logger
	path      string
	abPasskey string
	// cur is the search index PROJECTED from snap.Owners at install time (see
	// curation). It is derived rather than persisted, so it is stored beside the
	// snapshot rather than inside it: nothing can install one without the other.
	cur  curation
	snap snapshot
	// mu guards the published snapshot fields read per request: snap,
	// snapID, installed, installSeq, snapFailed, and snapFailedWarned (see the
	// per-field comments).
	mu sync.RWMutex
	// installSeq is the cache's OWN install ordering, incremented on every
	// accepted install: it is what orders the two producers against each other
	// when neither's snapshot can be compared to the other's by identity or by
	// timestamp. A producer records it BEFORE it derives a snapshot and hands it
	// back at install time (installLoaded); an install whose recorded position
	// is behind the cache's was overtaken while it was being derived, so it is
	// refused rather than moving the served feed backwards. The file's mtime
	// cannot serve as this order - a restored older generation must still
	// install (see loadedSnapshotUnchanged) - and identity INEQUALITY is not an
	// order at all: it accepts any snapshot that merely differs, which is how a
	// loader that began reading generation N-1 used to overwrite a just-published
	// generation N.
	installSeq uint64
	// snapMissing records that the snapshot file disappeared AFTER one was
	// installed (deleted file, incomplete restore, lost volume), so the
	// stale-feed WARN fires once per disappearance instead of once per reload;
	// cleared (with one INFO recovery line) on the first successful stat
	// afterward. A fresh install with no prior snapshot stays silent. Owned by
	// the loader goroutine (see below).
	snapMissing bool
	// reloadDegraded records that reloads are failing (a stat error or a
	// read failure of an unchanged-identity file), so the WARN fires once
	// per degradation onset instead of once per reload; cleared with one
	// INFO recovery line on the next successful snapshot read, and cleared
	// SILENTLY when the file goes absent (openSnapshot's ENOENT arm - the
	// missing state has its own once-per-disappearance WARN) or when the
	// stat lands on the memoized malformed file (skipMemoizedMalformed -
	// access recovered, but nothing was reloaded). The retry itself is NOT
	// suppressed (both faults can recover without an mtime change).
	//
	// It, failedID and snapMissing are the reload-only fields, and they carry
	// no lock because they have exactly ONE writer: refresh, called only by the
	// single loader goroutine watch owns. publish deliberately does not touch
	// them - it is not a file read, and it runs on the compare cycle's
	// goroutine - so there is no second writer to synchronize against.
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
	// failure paths only while nothing is installed (a fault AFTER a
	// successful load keeps serving the last-good snapshot); cleared by the
	// first successful install, and by a genuinely absent file (deleting
	// the bad file returns to fresh-install semantics). Guarded by mu (read
	// per request by unavailable, unlike the loader-owned flags above).
	snapFailed bool
	// snapFailedWarned bounds the snapshot-unavailable WARN to one per onset
	// instead of one per request; re-armed whenever snapFailed clears.
	// Guarded by mu.
	snapFailedWarned bool
	// installed records that SOME snapshot is being served, whichever way it
	// arrived: loaded from the file, or handed over in-process by the compare
	// cycle (publish). It is the "has anything ever loaded" question the
	// startup-window state machine asks - snapID cannot answer it, because it
	// records WHICH FILE GENERATION is loaded and an in-process publish has no
	// file generation to record. It is also the FIRST thing unavailable reads:
	// readiness is whether something is installed, never whether a load is in
	// flight. Guarded by mu.
	installed bool
	// watchStarted records that the reload clock was started, so unavailable
	// can tell a cache still resolving its first load from one nobody started
	// at all (direct publish users and tests).
	watchStarted atomic.Bool
}

// newSnapshotCache builds the cache for the snapshot file at path. It does not
// load: the caller decides when the reload clock runs (Run starts it eagerly so
// a restart serves the last feed immediately).
func newSnapshotCache(path, abPasskey string, log *slog.Logger) *snapshotCache {
	return &snapshotCache{
		log:       log,
		path:      path,
		abPasskey: abPasskey,
		firstLoad: make(chan struct{}),
	}
}

// warmLoadTimeout bounds how long start WAITS for the initial load of the
// persisted snapshot - not the load itself. The read is size-bounded
// (maxFeedBytes) but a slow or wedged /config mount has no bound of its own, and
// Run calls start on the daemon's startup path (main.go's startIndexer, alongside
// the compare loop), so an unbounded WAIT holds the whole daemon down instead of
// one goroutine. A context deadline cannot deliver this bound: refresh opens and
// stats the file before any ctx check, and atomicfile's bounded read only tests
// ctx around its syscalls - it cannot interrupt an os.OpenFile, File.Stat, or
// io.ReadAll already blocked in the filesystem. So the load runs on the cache's
// own goroutine and start stops waiting after the deadline; the load may finish
// in the background, which is safe because the cache is synchronized and the
// loader is the only caller of refresh.
//
// A var, not a const, ONLY so the warm-load test can exercise the wait-expired
// path (see queryGateWait for the pattern) without spending it in real time.
//
// It is ALSO the bound the loader's stall watchdog uses for every later load
// (see watchedRefresh): the two observations measure the same thing - a load
// that has not returned in the time startup was willing to wait for one is
// stalled - and a second tunable would be a second number to keep in step for
// no distinction.
var warmLoadTimeout = 15 * time.Second

// snapshotWatchInterval is the cache's OWN reload clock: how often watch
// re-stats the persisted snapshot after the initial load. It exists because the
// request path no longer loads anything, so something else has to notice a
// snapshot this process did not publish itself - the two cases being restart
// recovery (covered by the initial load) and a `poll` subcommand cycle, which
// runs in ANOTHER process and can only hand its feed over through the file.
//
// A minute is chosen against the consumer, not the producer: an arr's RSS sync
// interval is tens of minutes, so a minute of cross-process lag is invisible to
// it, while the cost is one stat per minute (the read happens only when the
// identity actually changed). The daemon's own cycle does not wait for this
// clock at all - it publishes in-process on completion - so this interval bounds
// only the out-of-process poll mode.
//
// A var, not a const, ONLY so the watch test can drive several ticks without
// spending them in real time (the warmLoadTimeout pattern).
var snapshotWatchInterval = time.Minute

// start begins the cache's own reload clock - the initial load of the persisted
// snapshot, then one re-stat per snapshotWatchInterval until ctx is done - and
// waits, briefly, for that first load so a restart serves the last persisted
// feed immediately rather than empty until the next cycle. Run calls it before
// binding, so the work begins under the explicit lifecycle boundary rather than
// during construction.
//
// The load runs on the cache's own goroutine and only the WAIT is bounded, by
// warmLoadTimeout or by ctx, whichever comes first: a wedged /config mount
// cannot be interrupted mid-syscall, so bounding the wait is the only bound that
// holds (see warmLoadTimeout), and honouring ctx keeps a shutdown during a slow
// load from being reported as a failed request drain. It is one-shot: a second
// call returns immediately, leaving the first loader in place. A request
// arriving while that load is still running is answered with the
// snapshot-unavailable fault rather than a false-empty feed (see unavailable).
func (c *snapshotCache) start(ctx context.Context) {
	// One-shot by construction: firstLoad may only be closed once, so a second
	// Run (a supervisor retrying after a bind failure) must not start a second
	// loader - the close would panic in a goroutine outside the daemon's
	// recover shield. The first loader is still the one serving.
	if !c.watchStarted.CompareAndSwap(false, true) {
		return
	}
	go c.watch(ctx, snapshotWatchInterval)
	c.awaitFirstLoad(ctx)
}

// watch is the cache's loader goroutine and the ONLY production caller of
// refresh: the initial load, then one re-stat every `every` until ctx is done.
// Being the sole caller is what makes the reload-only fields lock-free and
// leaves nothing to coalesce - the whole reason the request path can be a
// lock-free read of the published snapshot.
//
// The period is a parameter rather than a read of snapshotWatchInterval here, so
// the only read of that var happens on the caller's goroutine: a test that
// shortens it must not race a loader some earlier test left running.
//
// ctx is passed to the load rather than detached: the loader has no request
// riding on it, so a shutdown may as well abandon a read between syscalls
// (readSnapshot treats a cancellation as silent, never as a fault). A read
// already blocked inside the filesystem is uninterruptible either way and dies
// with the process.
func (c *snapshotCache) watch(ctx context.Context, every time.Duration) {
	c.refresh(ctx)
	close(c.firstLoad)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.watchedRefresh(ctx)
		}
	}
}

// watchedRefresh runs one reload under a liveness watchdog. Being the SOLE
// loader is what makes the reload-only fields lock-free, and it is also what
// makes a stall total: a load wedged in the filesystem cannot be interrupted, so
// the ticker never arms again, every later snapshot generation is ignored for the
// life of the process, and noteStatFault cannot report any of it because the
// blocked syscall never returns. Nothing about the cache is observably broken in
// that state - requests keep serving the last snapshot - which is exactly why it
// needs an observable of its own.
//
// The watchdog is that observable: one bounded WARN per stalled load, from a
// goroutine that retires the moment the load returns. It cannot end the stall
// (nothing can, short of the process), so the operator signal IS the whole
// mechanism.
//
// The first load is deliberately NOT wrapped: start's awaitFirstLoad already
// observes it on the same bound, and doubling that would emit two lines for one
// wedge.
func (c *snapshotCache) watchedRefresh(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)
	go c.warnOnStalledLoad(done, warmLoadTimeout)
	c.refresh(ctx)
}

// warnOnStalledLoad emits the loader's stall WARN when a load has not returned
// within timeout, then retires. It fires at most once per load, which is at most
// once per wedge: a wedged load holds the sole loader goroutine, so no further
// tick - and no further watchdog - can arm behind it.
func (c *snapshotCache) warnOnStalledLoad(done <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.log.Warn("indexer feed snapshot reload still running; this process will not pick up a new snapshot generation until it returns",
			"path", c.path, "timeout", timeout)
	}
}

// awaitFirstLoad waits for the loader's first load to resolve, bounded by
// warmLoadTimeout and by ctx. Split from start so the bounded wait is
// exercisable without a filesystem that can actually wedge. It is also the first
// load's LIVENESS observable, the role watchedRefresh plays for every later one.
//
// The WARN is the only signal that startup stopped waiting and began serving
// without the persisted snapshot, so the line names that consequence: until
// something is installed - by this load or by an in-process publish from the
// compare cycle - every request answers a Torznab error.
func (c *snapshotCache) awaitFirstLoad(ctx context.Context) {
	warmTimer := time.NewTimer(warmLoadTimeout)
	defer warmTimer.Stop()
	select {
	case <-c.firstLoad:
	case <-ctx.Done():
		// Shutting down before the load returned: stop waiting so the feed's
		// goroutine returns inside the daemon's drain budget instead of being
		// reported as a failed request drain. The load dies with the process.
		c.log.Debug("feed snapshot warm load abandoned; shutting down",
			"cause", context.Cause(ctx))
	case <-warmTimer.C:
		c.log.Warn("feed snapshot warm load still running; answering search and RSS requests with a Torznab error until a snapshot is installed",
			"timeout", warmLoadTimeout)
	}
}

// loadPending reports whether the reload clock was started and its first load
// has not resolved yet. It is the LAST question unavailable asks and it never
// outranks what is installed: the compare cycle can publish a complete snapshot
// in-process while this load is still blocked, and a load in flight says nothing
// about whether the served feed has something behind it (see unavailable, its
// only caller). While nothing IS installed, a pending first load is the state
// that must answer the snapshot-unavailable fault rather than a false-empty
// success, because the empty in-memory snapshot is indistinguishable from a
// fresh install. A cache nobody started - a direct publish consumer, or a test -
// is never pending.
func (c *snapshotCache) loadPending() bool {
	if !c.watchStarted.Load() {
		return false
	}
	select {
	case <-c.firstLoad:
		return false
	default:
		return true
	}
}

// curation returns the three curation maps a search filters against. They are a
// PROJECTION of the persisted per-entry ownership fact, derived once per install
// (installSnapshot) rather than read out of the file - so a tampered snapshot
// cannot carry a search index that disagrees with the ownership it claims to
// derive from, and there is no separate index for a rebuild to forget to update.
// The maps are safe to read after the lock is released: installSnapshot installs
// a freshly derived set and never mutates a published one in place (the same
// invariant feed documents for the journal slices).
func (c *snapshotCache) curation() curation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur
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
// ReadBounded's open, wedging the loader on a file it had already decided to
// refuse - permanently, since nothing can interrupt an open blocked in the
// kernel, so the served feed would freeze on whatever was loaded and never
// reload again. Binding validation, identity and bytes to one descriptor closes
// both: O_NOFOLLOW refuses a final-component symlink at open time (matching the
// writer's ErrSymlinkTarget contract, which atomicfile.ReadBounded cannot honor
// because os.Open follows links) and O_NONBLOCK makes a raced FIFO open return
// immediately so the regular-file check can reject it instead of blocking
// forever. The gate is the full regular-file predicate rather than a symlink
// test: a socket, device, or directory left at the path is the same non-regular
// ingress. Every rejection takes the same arm as any other open fault: warn
// once per onset, keep the current feed, and mark the snapshot-unavailable
// state while nothing has loaded.
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
// snapshot was installed it means the materialized view can no longer refresh
// FROM THE FILE: requests keep serving the last in-memory feed (and an
// in-process cycle can still publish over it), so warn once that the file-borne
// feed is stale, then stay quiet until the file reappears.
//
// Absence is a successful stat determination, so it ENDS any stat/read
// degradation episode: clear the transient flag (no recovery INFO - nothing was
// reloaded; the missing state has its own once-per-disappearance WARN) so the
// next fault onset warns again instead of being suppressed by a stale flag.
func (c *snapshotCache) noteSnapshotAbsent() {
	c.reloadDegraded = false
	c.mu.RLock()
	loaded := c.installed
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

// snapshotLoadGate is a test seam (see snapshotUnavailableGate for the pattern)
// marking the entry to a load, held open so the loader's stall watchdog and the
// two producers' install ORDER are exercisable at all: both are about a load that
// is in flight while something else happens, and a filesystem cannot be made to
// wedge - or to be slow at a chosen instant - on demand. A no-op in production.
var snapshotLoadGate = func() {}

// reload refreshes the served feed from the persisted snapshot when the file
// on disk differs from the loaded copy by mtime or file identity (or nothing
// is loaded yet). A compare cycle in ANOTHER process (the `poll` subcommand)
// rewrites the snapshot atomically, so a cheap stat per tick picks up its feed
// without the server ever fetching SeaDex itself; this process's own cycle does
// not go through the file at all - it publishes in-process on completion (see
// publish). Any mtime change triggers a reload, including an older restored
// timestamp. When the mtime is equal, os.SameFile distinguishes the unchanged
// file (skip) from a replacement inode whose timestamp was preserved (reload),
// preventing an atomic rename or backup restore from wedging the server on
// stale in-memory data. A missing file leaves the current (possibly empty) feed
// in place; a malformed or unreadable file is logged and ignored, so a bad
// write never blanks a live feed.
//
// It has ONE production caller - watch, the cache's loader goroutine - and that
// is load-bearing rather than incidental: being sequential is what lets the
// reload-only fields go unlocked and removes any need to coalesce. It is not
// safe to call concurrently with the loader.
func (c *snapshotCache) refresh(ctx context.Context) {
	if c.path == "" {
		return
	}
	// Recorded BEFORE the file is opened: everything below derives from the
	// generation on disk AS OF HERE, so this is the position installLoaded
	// orders the result against (see the installSeq field). Reading it after
	// the load would compare the read against its own outcome and order
	// nothing.
	readFrom := c.installedSeq()
	snapshotLoadGate()
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
	if !c.installLoaded(info, &snap, readFrom) {
		// Either the generation is already installed (the ordinary skip) or a
		// newer snapshot was installed while this one was being read, in which
		// case discarding it is the point (see installLoaded). Neither is a
		// fault; the next tick reloads if the file's identity says to.
		c.log.Debug("indexer feed snapshot reload discarded; nothing newer to install",
			"path", c.path)
		return
	}
	c.log.Info("indexer feed snapshot loaded",
		"path", c.path, "owners", len(snap.Owners),
		"hashes", len(c.curation().byHash), "keys", len(c.curation().byKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed))
}

// skipMemoizedMalformed applies reload's memoized-malformed-file arm: it
// reports whether the stat'ed file is the memoized malformed snapshot,
// unchanged, and if so re-asserts the snapshot-unavailable state and clears
// the transient degradation flag. The memoized malformed snapshot fails
// deterministically: unchanged bytes decode the same way on every read, so
// rereading it would only repeat the I/O and JSON work on every tick and the
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
// installed. A nil info records no identity - the in-process publish path, whose
// snapshot came from memory rather than from a generation of the file - which
// makes the next stat reload, the safe fail direction (a spurious reload costs
// one read; a missed one serves stale data). Same identity test as
// loadedSnapshotUnchanged.
//
// It is the shared body of the two producers' entry points, installLoaded and
// installPublished, which own the ORDER between them; the caller holds mu.
func (c *snapshotCache) installSnapshot(info os.FileInfo, snap *snapshot) bool {
	if info != nil && c.snapID.Matches(info) {
		return false
	}
	c.snap = *snap
	c.cur = projectCuration(snap.Owners)
	c.snapID = atomicfile.Identify(info)
	c.installed = true
	c.installSeq++
	// A successful install ends any startup snapshot-unavailable state and
	// re-arms its per-onset WARN (see snapFailed).
	c.snapFailed = false
	c.snapFailedWarned = false
	return true
}

// installedSeq reads the cache's current install ordering position, which a
// producer records BEFORE it derives a snapshot (see the installSeq field).
func (c *snapshotCache) installedSeq() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.installSeq
}

// installLoaded installs a snapshot the loader READ FROM the file, ordered
// against readFrom - the install position the loader recorded before it opened
// that file. When the cache has moved on since, this read was overtaken while it
// was in flight and its bytes are older than what is installed, so it is refused:
// a load must never move the served feed backwards, whatever the interleaving.
//
// That is the ordering test identity inequality could not give. The two producers
// derive their snapshots from different places - one from a generation of the
// file, one from the compare cycle's memory - so neither the recorded identity
// nor the file's mtime relates them (an older restored timestamp is a legitimate
// reload), while the cache's own install sequence relates every install to every
// other one by construction.
func (c *snapshotCache) installLoaded(info os.FileInfo, snap *snapshot, readFrom uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if readFrom != c.installSeq {
		return false
	}
	return c.installSnapshot(info, snap)
}

// installPublished installs the snapshot the in-process compare cycle just
// built. It carries no ordering position because it needs none: the pass that
// produced it ran to completion before publish was called, and it is the only
// producer with a snapshot that is not a generation of the file, so it is newer
// than every install that precedes the call. Only the identity re-check can
// refuse it (the generation it just persisted is already installed).
func (c *snapshotCache) installPublished(info os.FileInfo, snap *snapshot) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.installSnapshot(info, snap)
}

// publish installs a snapshot the in-process compare cycle just built, so the
// process that PRODUCED a feed never re-reads its own file to serve it. It is
// the primary way a snapshot reaches the served feed in the daemon: the cycle
// holds the fresh snapshot in memory already, so a completed pass makes it
// servable immediately instead of waiting for a reload clock to notice the file
// changed. The file write still happens (persist), and stays the only channel
// for restart recovery and for a `poll` cycle in another process.
//
// info is the freshly written file's stat, when the producer could take one:
// recording that generation is what stops the loader from re-reading the bytes
// this process already holds. A nil info costs exactly one redundant background
// read.
//
// The download URLs are re-derived here, not carried: persist strips both feeds
// to GUID-only before writing (the snapshot must never hold the AB passkey at
// rest), and re-deriving through the same helpers the load path uses is what
// makes a published feed byte-identical to a loaded one - the alternative,
// publishing the pre-strip render, would make the served feed depend on WHICH
// producer installed it.
//
// Two producers means one interleaving is possible and it is REFUSED rather than
// accepted: a loader that began reading generation N-1 and finishes after this
// publish installs generation N would install N-1 over it, serving stale feed and
// curation data to every RSS and search request until the next tick - so an arr
// asking in that window could miss a newly curated release, or accept one the
// completed pass removed. The ordering that refuses it is the cache's own install
// sequence (see installLoaded and the installSeq field), not this method's
// business: publish holds the newest snapshot in the process by construction and
// says so by taking the unordered entry point.
func (c *snapshotCache) publish(snap *snapshot, info os.FileInfo) {
	// The maps and slices are shared with the caller's snapshot rather than
	// deep-copied, which is sound because the producer never mutates a snapshot
	// it has persisted: the next pass rebuilds from loadPrevious, which decodes
	// the FILE into fresh values. The two feed slices are replaced outright by
	// the rebuild below, so the published render is this cache's own.
	pub := *snap
	pub.ABFeed = c.rebuildABDownloadURLs(pub.ABFeed)
	pub.NyaaFeed = c.rebuildNyaaDownloadURLs(pub.NyaaFeed)
	if !c.installPublished(info, &pub) {
		return
	}
	cur := c.curation()
	c.log.Info("indexer feed snapshot published in-process",
		"owners", len(pub.Owners),
		"hashes", len(cur.byHash), "keys", len(cur.byKey),
		"nyaa_feed", len(pub.NyaaFeed), "ab_feed", len(pub.ABFeed))
}

// markSnapshotFailedIfUnloaded flags the snapshot-unavailable state (see the
// snapFailed field) after a load fault, but only while NOTHING has ever been
// installed - by a load or by an in-process publish: after either, that
// snapshot keeps being served instead.
func (c *snapshotCache) markSnapshotFailedIfUnloaded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.installed {
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
	// identify a structurally invalid snapshot (see decodeSnapshot). That is
	// deterministic for unchanged bytes, so it memoizes like malformed JSON;
	// the offending value itself is never logged (it can be attacker-shaped
	// multi-megabyte text). A defect in ONE journal item is not structural -
	// decodeSnapshot drops that item and reports the count per feed (see
	// warnDroppedItems).
	if reason != "" {
		c.markSnapshotFailedIfUnloaded()
		c.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", c.path, "reason", reason)
		return snapshot{}, false, true
	}
	reportUnsupportedVersion(c.log, c.path, &snap)
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
	c.warnDroppedItems(scrub)
	return snap, true, false
}

// warnDroppedItems reports the per-item prune PER TRACKER, one line per
// affected feed - the same attribution warnBlankedInfoURLs gives the info-URL
// scrub, and for the same reason: a hand-edited or partially corrupted
// feed.json is the only way such an item gets there, and the operator needs to
// know WHICH journal lost items. The count is the whole message: the dropped
// item's fields come from a tamperable file (l-f45).
func (c *snapshotCache) warnDroppedItems(scrub snapshotScrub) {
	for _, scope := range feedScopes {
		if n := scrub.droppedItems[scope]; n > 0 {
			c.log.Warn("indexer feed snapshot: invalid journal items dropped",
				"path", c.path, "tracker", scope, "dropped", n)
		}
	}
}

// warnBlankedInfoURLs reports the info-URL scrub PER TRACKER, one line per
// affected feed. The attribution is the point: a tampered or hand-edited
// feed.json is the only way these URLs get there, and an operator seeing a
// single summed count cannot tell which journal was touched (l-f176). Scopes are
// iterated in a fixed order so the lines are deterministic for a test to pin and
// for a human to diff across reloads.
func (c *snapshotCache) warnBlankedInfoURLs(scrub snapshotScrub) {
	for _, scope := range feedScopes {
		if n := scrub.blankedInfoURLs[scope]; n > 0 {
			// Counts only; the rejected value can be attacker-shaped text.
			c.log.Warn("indexer feed snapshot: non-SeaDex info URLs blanked",
				"path", c.path, "tracker", scope, "blanked", n)
		}
	}
}

// reportUnsupportedVersion reports a loaded snapshot written at a schema version
// this binary does not read, and NEUTRALIZES it: every member is zeroed, so
// searches answer no-match and both RSS feeds serve empty until the next cycle
// rewrites the file. One INFO per load - the state is real but transitional, it
// clears on the next cycle's snapshot rewrite, and it is not an operator fault.
//
// Re-baselining is the right answer rather than refusing (which would answer a
// Torznab error) because this file is a MATERIALIZED VIEW: the cost is one
// empty-RSS window, and the next pass re-derives the catalogue half of it from
// SeaDex. Reading it instead would be worse than either: the members that cannot
// be re-derived - the permanent publication log, the journals' FirstSeen and
// harvested titles - are exactly the ones a differently-versioned binary may
// have written in a shape this one misreads.
func reportUnsupportedVersion(log *slog.Logger, path string, snap *snapshot) {
	if snap.supportedVersion() {
		return
	}
	// The version is a small integer from our own schema field, so it is safe
	// to log verbatim.
	log.Info("indexer feed snapshot has an unsupported schema version; serving an empty feed until the next cycle rewrites it",
		"path", path, "version", snap.Version, "supported", currentFeedVersion)
	*snap = snapshot{}
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
// (trackerOwnForm, the same fail-closed check writer-side journal admission
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
	// urlform.FoldHostASCII IS the fold urlform applies to Form.Host, so the
	// expected host and the value it is compared against cannot disagree.
	return urlform.FoldHostASCII(u.Hostname())
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
//
// A vouched item KEEPS its link, in the cleaned spelling the gate actually
// judged rather than its persisted one (h-f8).
func sanitizeSnapshotInfoURLs(feed []journalItem) int {
	host := seadexInfoHost()
	blanked := 0
	for i := range feed {
		if feed[i].InfoURL == "" {
			continue
		}
		if cleaned, ok := snapshotInfoURLAllowed(feed[i].InfoURL, host); ok {
			// Store the VOUCHED spelling, never the original. The gate reads
			// urlform's WHATWG-preprocessed form, so an edge-padded persisted
			// value ("\thttps://releases.moe/123") is vouched on the BROWSER's
			// reading of it - and renderFeed would then hand the arr UI the
			// padded original as its clickable <comments> link. Classify once,
			// emit the cleaned form (h-f8). The set of values that pass is
			// unchanged, so no blanked count moves.
			feed[i].InfoURL = cleaned
			continue
		}
		feed[i].InfoURL = ""
		blanked++
	}
	return blanked
}

// snapshotInfoURLAllowed reports whether raw is a userinfo-free absolute
// http(s) URL, free of smuggling forms, on the canonical SeaDex host, and
// returns the VOUCHED spelling of it for the caller to store: urlform's
// WHATWG-preprocessed reading (Form.Trimmed), the string the gate actually
// judged. Returning it is the point - the gate vouches the browser's reading of
// an edge-padded value while the caller used to keep the padded original, which
// is the same vouch-one-reading / emit-another split h-f8 closed at the search
// path's id extraction (prowlarr.go's httpDisplayForm, whose shape this
// mirrors). It changes only WHAT IS STORED on the success path: every
// structural and host leg below is unchanged, so the set of values that pass is
// identical.
//
// This is a PUBLISH gate on a tamperable boundary - a persisted feed.json
// InfoURL is vouched here and handed to the arr web UI as a clickable link,
// where the BROWSER is the parser of record - so its structural legs are
// internal/displaylink's, the app's one home for that vouch step (h-f13),
// shared with the sibling publish gate trackerlink.Publish and with the search
// path's httpDisplayHost. Those legs read their facts from urlform (the app's
// own classifier of record for exactly that browser-vs-net/url divergence)
// rather than from incidental net/url behavior; this gate adds only its own
// host policy, matched behind urlform.IsASCIIHost.
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
//
// host is the EXPECTED canonical hostname and must already be ASCII-lowercased;
// seadexInfoHost, its only producer, owns that fold. The gate deliberately does
// not re-apply it (Meyer's Non-Redundancy Principle: one side owns a
// precondition), and it never reaches for strings.EqualFold to compensate -
// that full-Unicode fold is the l-f114 bug itself.
func snapshotInfoURLAllowed(raw, host string) (cleaned string, ok bool) {
	// An unresolved expected host vouches NOTHING, and this is the leg that says
	// so: displaylink.VouchForm has no non-empty-host requirement of its own (its
	// package doc assigns that leg to httpDisplayHost), so an absolute
	// "http:///path" would otherwise byte-equal an empty expectation. Pinned by
	// FuzzSnapshotInfoURL's empty-host case.
	if host == "" {
		return "", false
	}
	// The shared structural legs: absolute, http(s), no userinfo, no smuggling
	// bytes - publish-or-drop, the same stance trackerlink.Publish takes.
	// Classified here rather than through displaylink.Vouch because the vouched
	// FORM is what carries the cleaned string this gate returns.
	f := urlform.Classify(raw)
	if !displaylink.VouchForm(&f) {
		return "", false
	}
	// IsASCIIHost refuses a non-ASCII host, so no homograph can fold into the
	// allowlisted name - and it is why the EXPECTED host needs no ASCII check of
	// its own: an ASCII f.Host can never byte-equal a non-ASCII expectation, so a
	// non-ASCII `host` refuses everything here rather than admitting anything.
	if !urlform.IsASCIIHost(f.Host) || f.Host != host {
		return "", false
	}
	return f.Trimmed, true
}

// snapshotUnavailableGate is a test seam (see harvestWait for the pattern)
// marking the window between snapshotUnavailable's read-unlock and its
// write-lock, where a concurrent install/clear can race the escalation. A
// no-op in production.
var snapshotUnavailableGate = func() {}

// unavailable reports whether the served feed has NO snapshot behind it. It
// derives readiness from what is INSTALLED - whichever producer installed it -
// and never from whether a load is in flight: a load is the cache's business,
// not the request's. That ordering is the whole point. loadPending() stays true
// from start() until the loader's first refresh returns, that load is an
// uninterruptible os.OpenFile + File.Stat on /config/feed.json (warmLoadTimeout
// bounds the WAIT, never the load), so on a wedged /config mount it is true for
// the process's whole life - and consulting it first meant every Torznab request
// answered a fault even after the compare cycle had published a complete
// snapshot in-process. Serving must not depend on the snapshot's load: neither
// performing it nor being failed by it.
//
// So the questions are asked in that order: something installed is served; else
// a load fault that happened before anything was installed (the snapFailed
// field) answers the fault; else a first load still resolving answers it too,
// because until it does the empty in-memory snapshot is indistinguishable from a
// fresh install. A genuinely absent file is NOT unavailable - that is the
// intentional fresh-install state.
//
// It is deliberately startup-window-only: markSnapshotFailedIfUnloaded sets the
// flag only while nothing is installed, so once a snapshot is being served a
// later load fault keeps serving it instead of failing requests.
//
// The once-per-onset WARN fires on the first report of the failed state so the
// local fault is visible without a per-request log storm (the still-loading
// state has its own line, from awaitFirstLoad). The write-locked re-check is
// authoritative: a request that saw the failed state under the read lock but
// loses the race to an install/clear before acquiring the write lock answers
// from the fresh snapshot instead of rendering a stale Torznab error.
func (c *snapshotCache) unavailable() bool {
	c.mu.RLock()
	installed, failed, warned := c.installed, c.snapFailed, c.snapFailedWarned
	c.mu.RUnlock()
	if installed {
		return false
	}
	if !failed {
		return c.loadPending()
	}
	if warned {
		return true
	}

	snapshotUnavailableGate()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the write lock, authoritatively: concurrent requests
	// racing the onset must still emit the WARN exactly once, and an install
	// that landed between the read-unlock and here (a load returning, or an
	// in-process publish) must make THIS request answer from the fresh snapshot
	// rather than render a stale error.
	if c.installed || !c.snapFailed {
		return false
	}
	if !c.snapFailedWarned {
		c.snapFailedWarned = true
		c.log.Warn("indexer feed snapshot unavailable; answering Torznab requests with an error until a snapshot loads",
			"path", c.path)
	}
	return true
}
