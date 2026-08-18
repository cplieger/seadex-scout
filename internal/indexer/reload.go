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
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/urlform"
)

// snapshotCache owns the served snapshot's lifecycle: taking it from the in-process
// compare cycle (publish) or from /config/feed.json, memoizing a deterministically bad
// file, tracking the missing/degraded/unavailable states, and publishing the current
// snapshot for the request path to read.
type snapshotCache struct {
	// snapID identifies the successfully loaded snapshot file, installed together with
	// snap (guarded by mu).
	snapID atomicfile.FileIdentity
	// loader owns the reload clock and the reload-only state (see
	// snapshotLoader). Set once by newSnapshotCache and never rewritten.
	loader *snapshotLoader
	// firstLoad is closed when the loader's initial load returns; watchStarted
	// says whether anything will ever close it (a cache nobody started keeps
	// serving whatever publish or a direct refresh installed). Allocated by
	// newSnapshotCache so the request path can always read it.
	firstLoad chan struct{}
	// log, path, and abPasskey are set once by newSnapshotCache and read without a
	// lock, never written afterwards.
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
	// installSeq is the cache's OWN install ordering, incremented on every accepted
	// install: it is what orders the two producers against each other when neither's
	// snapshot can be compared to the other's by identity or by timestamp.
	installSeq uint64
	// snapFailed records that snapshot loading failed BEFORE any snapshot was
	// installed: a non-ENOENT stat or read fault, or a malformed or structurally
	// invalid file, at startup leaves the zero-value in-memory snapshot
	// indistinguishable from the intentional fresh-install state - query would contact
	// Prowlarr, filter every result against nil curation maps, and serve a successful
	// empty feed, so the arr records a clean no-match during a local fault.
	snapFailed bool
	// snapFailedWarned bounds the snapshot-unavailable WARN to one per onset
	// instead of one per request; re-armed whenever snapFailed clears.
	// Guarded by mu.
	snapFailedWarned bool
	// installed records that SOME snapshot is being served, whichever way it arrived:
	// loaded from the file, or handed over in-process by the compare cycle (publish).
	installed bool
	// watchStarted records that the reload clock was started, so unavailable
	// can tell a cache still resolving its first load from one nobody started
	// at all (direct publish users and tests).
	watchStarted atomic.Bool
}

// snapshotLoader is the cache's single background loader and the OWNER of the
// reload-only state.
type snapshotLoader struct {
	// cache is the published state this loader installs into. Set once by
	// newSnapshotCache and never rewritten.
	cache *snapshotCache
	// log is the cache's own logger, copied once at construction and read
	// without a lock, never written afterwards.
	log *slog.Logger
	// failedID identifies the last snapshot file whose CONTENT failed
	// deterministically: malformed JSON, a structurally invalid document, or a file
	// over the shared maxFeedBytes cap (persist enforces the same cap, so an oversized
	// snapshot is external corruption that never shrinks on its own).
	failedID atomicfile.FileIdentity
	// path is the cache's own snapshot path, copied once at construction and
	// read without a lock, never written afterwards.
	path string
	// snapMissing records that the snapshot file disappeared AFTER one was installed
	// (deleted file, incomplete restore, lost volume), so the stale-feed WARN fires
	// once per disappearance instead of once per reload; cleared (with one INFO
	// recovery line) on the first successful stat afterward.
	snapMissing bool
	// reloadDegraded records that reloads are failing (a stat error or a read failure
	// of an unchanged-identity file), so the WARN fires once per degradation onset
	// instead of once per reload; cleared with one INFO recovery line on the next
	// successful snapshot read, and cleared SILENTLY when the file goes absent
	// (openSnapshot's ENOENT arm - the missing state has its own once-per-disappearance
	// WARN) or when the stat lands on the memoized malformed file
	// (skipMemoizedMalformed - access recovered, but nothing was reloaded).
	reloadDegraded bool
}

// newSnapshotCache builds the cache for the snapshot file at path. It does not
// load: the caller decides when the reload clock runs (Run starts it eagerly so
// a restart serves the last feed immediately).
func newSnapshotCache(path, abPasskey string, log *slog.Logger) *snapshotCache {
	c := &snapshotCache{
		log:       log,
		path:      path,
		abPasskey: abPasskey,
		firstLoad: make(chan struct{}),
	}
	c.loader = &snapshotLoader{cache: c, log: log, path: path}
	return c
}

// warmLoadTimeout bounds how long start WAITS for the initial load of the persisted
// snapshot - not the load itself.
var warmLoadTimeout = 15 * time.Second

// snapshotWatchInterval is the cache's OWN reload clock: how often watch
// re-stats the persisted snapshot after the initial load. It exists because the
// request path no longer loads anything, so something else has to notice a
// snapshot this process did not publish itself - the two cases being restart
// recovery (covered by the initial load) and a `poll` subcommand cycle, which
// runs in ANOTHER process and can only hand its feed over through the file.
var snapshotWatchInterval = time.Minute

// start begins the cache's own reload clock - the initial load of the persisted
// snapshot, then one re-stat per snapshotWatchInterval until ctx is done - and
// waits, briefly, for that first load so a restart serves the last persisted
// feed immediately rather than empty until the next cycle. Run calls it before
// binding, so the work begins under the explicit lifecycle boundary rather than
// during construction.
func (c *snapshotCache) start(ctx context.Context) {
	// One-shot by construction: firstLoad may only be closed once, so a second
	// Run (a supervisor retrying after a bind failure) must not start a second
	// loader - the close would panic in a goroutine outside the daemon's
	// recover shield. The first loader is still the one serving.
	if !c.watchStarted.CompareAndSwap(false, true) {
		return
	}
	go c.loader.watch(ctx, snapshotWatchInterval)
	c.awaitFirstLoad(ctx)
}

// watch is the cache's loader goroutine and the ONLY production caller of
// refresh: the initial load, then one re-stat every `every` until ctx is done.
func (l *snapshotLoader) watch(ctx context.Context, every time.Duration) {
	l.refresh(ctx)
	close(l.cache.firstLoad)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.watchedRefresh(ctx)
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
func (l *snapshotLoader) watchedRefresh(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)
	go l.warnOnStalledLoad(done, warmLoadTimeout)
	l.refresh(ctx)
}

// warnOnStalledLoad emits the loader's stall WARN when a load has not returned
// within timeout, then retires. It fires at most once per load, which is at most
// once per wedge: a wedged load holds the sole loader goroutine, so no further
// tick - and no further watchdog - can arm behind it.
func (l *snapshotLoader) warnOnStalledLoad(done <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		l.log.Warn("indexer feed snapshot reload still running; this process will not pick up a new snapshot generation until it returns",
			"path", l.path, "timeout", timeout)
	}
}

// awaitFirstLoad waits for the loader's first load to resolve, bounded by
// warmLoadTimeout and by ctx. Split from start so the bounded wait is
// exercisable without a filesystem that can actually wedge. It is also the first
// load's LIVENESS observable, the role watchedRefresh plays for every later one.
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

// loadPending reports whether the reload clock was started and its first load has not
// resolved yet.
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

// openSnapshot opens the snapshot file ONCE and applies refresh's missing/unreadable
// policy, returning the open descriptor, its info, and whether reload should proceed.
func (l *snapshotLoader) openSnapshot() (*os.File, os.FileInfo, bool) {
	// atomicfile.OpenRegular is the library's own form of this sequence and it owns the
	// three non-obvious details its godoc says every hand-rolling consumer re-derives:
	// O_NOFOLLOW so the KERNEL refuses a final-component symlink (reported as
	// ErrSymlinkTarget, the writer's own vocabulary), O_NONBLOCK so a planted FIFO is
	// an immediate rejection rather than an uninterruptible open, and a stat of the
	// OPEN HANDLE so the returned FileInfo describes the very inode whose bytes
	// readSnapshot reads and whose FileIdentity is recorded.
	f, info, err := atomicfile.OpenRegular(l.path)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			l.noteSnapshotAbsent()
		case errors.Is(err, atomicfile.ErrNotRegular):
			// A directory, FIFO, socket, or device node left at the snapshot
			// path; the error names the actual mode type.
			l.noteStatFault("indexer feed snapshot path is not a regular file; refusing to load it", "error", err)
		default:
			// Anything else (EACCES, EIO, and ELOOP/ErrSymlinkTarget for a
			// symlink at the final component) silently freezes the served
			// feed, so make it visible - once per onset, not once per request.
			l.noteStatFault("indexer feed snapshot open failed; keeping current feed", "error", err)
		}
		return nil, nil, false
	}
	if l.snapMissing {
		l.snapMissing = false
		if l.matchesFailedFile(info) {
			// The file is back but it is the same deterministically-bad generation
			// refresh already memoized, so nothing will reload (skipMemoizedMalformed
			// returns before the read) and the served feed stays frozen on the
			// last-good snapshot with no further signal.
			l.log.Warn("indexer feed snapshot reappeared but is the same malformed file; still serving the last loaded feed",
				"path", l.path)
		} else {
			l.log.Info("indexer feed snapshot reappeared; resuming reloads", "path", l.path)
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
func (l *snapshotLoader) noteSnapshotAbsent() {
	l.reloadDegraded = false
	loaded := l.cache.hasInstalled()
	if !loaded {
		// A genuinely absent first snapshot IS the fresh-install state -
		// serving the empty feed is intentional there - so an earlier load
		// fault stops blocking requests once the bad file is gone (deleting it
		// returns to fresh-install semantics).
		l.cache.clearSnapshotFailed()
	}
	if loaded && !l.snapMissing {
		l.snapMissing = true
		l.log.Warn("indexer feed snapshot missing; serving last loaded feed until it reappears", "path", l.path)
	}
}

// noteStatFault is the shared onset ladder for every stat-time fault that
// freezes the served feed (an unreadable file, a non-regular path): mark the
// snapshot-unavailable state while nothing has loaded, then WARN once per onset
// rather than once per request.
func (l *snapshotLoader) noteStatFault(msg string, attrs ...any) {
	l.cache.markSnapshotFailedIfUnloaded()
	if l.reloadDegraded {
		return
	}
	l.reloadDegraded = true
	l.log.Warn(msg, append([]any{"path", l.path}, attrs...)...)
}

// snapshotLoadGate is a test seam (see snapshotUnavailableGate for the pattern)
// marking the entry to a load, held open so the loader's stall watchdog and the
// two producers' install ORDER are exercisable at all: both are about a load that
// is in flight while something else happens, and a filesystem cannot be made to
// wedge - or to be slow at a chosen instant - on demand. A no-op in production.
var snapshotLoadGate = func() {}

// refresh reloads the served feed from the persisted snapshot when the file on disk
// differs from the loaded copy by mtime or file identity (or nothing is loaded yet).
func (l *snapshotLoader) refresh(ctx context.Context) {
	if l.path == "" {
		return
	}
	// Recorded BEFORE the file is opened: everything below derives from the generation
	// on disk AS OF HERE, so this is the position installLoaded orders the result
	// against (see the installSeq field).
	readFrom := l.cache.installedSeq()
	snapshotLoadGate()
	f, info, ok := l.openSnapshot()
	if !ok {
		return
	}
	defer f.Close()
	if l.skipMemoizedMalformed(info) {
		return
	}
	// A degraded reload must not take the unchanged-loaded-snapshot fast path: after a
	// stat fault recovers, the file may be the already-loaded inode at the same mtime,
	// so skipping here would leave reloadDegraded set forever — the recovery INFO never
	// emits and the next onset's warning is suppressed by the stale flag.
	if l.cache.loadedSnapshotUnchanged(info) && !l.reloadDegraded {
		return
	}
	snap, ok, memoize := l.readSnapshot(ctx, f)
	if !ok {
		l.recordSnapshotFailure(info, memoize)
		return
	}
	l.failedID = atomicfile.FileIdentity{}
	if l.reloadDegraded {
		l.reloadDegraded = false
		l.log.Info("indexer feed snapshot reload recovered", "path", l.path)
	}
	if !l.cache.installLoaded(info, &snap, readFrom) {
		// Either the generation is already installed (the ordinary skip) or a
		// newer snapshot was installed while this one was being read, in which
		// case discarding it is the point (see installLoaded). Neither is a
		// fault; the next tick reloads if the file's identity says to.
		l.log.Debug("indexer feed snapshot reload discarded; nothing newer to install",
			"path", l.path)
		return
	}
	cur := l.cache.curation()
	l.log.Info("indexer feed snapshot loaded",
		"path", l.path, "owners", len(snap.Owners),
		"hashes", len(cur.byHash), "keys", len(cur.byKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed))
}

// skipMemoizedMalformed applies refresh's memoized-malformed-file arm: it reports
// whether the stat'ed file is the memoized malformed snapshot, unchanged, and if so
// re-asserts the snapshot-unavailable state and clears the transient degradation flag.
func (l *snapshotLoader) skipMemoizedMalformed(info os.FileInfo) bool {
	if !l.matchesFailedFile(info) {
		return false
	}
	l.cache.markSnapshotFailedIfUnloaded()
	l.reloadDegraded = false
	return true
}

// recordSnapshotFailure applies refresh's failed-read memo policy.
func (l *snapshotLoader) recordSnapshotFailure(info os.FileInfo, memoize bool) {
	l.failedID = atomicfile.FileIdentity{}
	if memoize {
		l.failedID = atomicfile.Identify(info)
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
// (refresh clears only the transient reloadDegraded flag and returns), while any
// mtime or identity change means new bytes worth retrying.
func (l *snapshotLoader) matchesFailedFile(info os.FileInfo) bool {
	return l.failedID.Matches(info)
}

// installSnapshot publishes snap as the served feed under mu, recording the
// file's identity for the next reload's skip check, and reports whether it
// installed. A nil info records no identity - the in-process publish path, whose
// snapshot came from memory rather than from a generation of the file - which
// makes the next stat reload, the safe fail direction (a spurious reload costs
// one read; a missed one serves stale data). Same identity test as
// loadedSnapshotUnchanged.
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

// hasInstalled reports whether SOME snapshot is being served (see the installed
// field). It is the loader's mu-guarded read of that fact: the loader owns no
// lock primitive of its own, so the missing-file policy asks through here
// instead of taking mu directly.
func (c *snapshotCache) hasInstalled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.installed
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
func (c *snapshotCache) publish(snap *snapshot, info os.FileInfo) {
	// The maps and slices are shared with the caller's snapshot rather than
	// deep-copied, which is sound because the producer never mutates a snapshot it has
	// persisted: the next pass rebuilds from loadPrevious, which decodes the FILE into
	// fresh values.
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

// readSnapshot is refresh's read/decode error policy: it bounded-reads f - the
// descriptor openSnapshot already validated, so the bytes decoded here are exactly the
// ones the recorded FileIdentity describes - and decodes the persisted feed snapshot,
// reporting ok=false on any failure so the caller keeps the current feed.
func (l *snapshotLoader) readSnapshot(ctx context.Context, f *os.File) (snapshot, bool, bool) {
	data, err := atomicfile.ReadBoundedFile(ctx, f, maxFeedBytes)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// A shutdown cancellation is silent and never marks the
			// snapshot-unavailable state (the file was never actually read;
			// a retry could succeed).
			l.cache.markSnapshotFailedIfUnloaded()
			if !l.reloadDegraded {
				l.reloadDegraded = true
				l.log.Warn("indexer feed snapshot unreadable; keeping current feed", "path", l.path, "error", err)
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
		l.cache.markSnapshotFailedIfUnloaded()
		// Bounded through the shared emit-boundary policy: a decoder error can
		// embed the offending document text (encoding/json quotes an over-range
		// numeric literal verbatim), and feed.json is a tamperable boundary.
		l.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", l.path, "error", capLogText(decodeErr.Error(), 256))
		return snapshot{}, false, true
	}
	// `null` or `{}` decodes cleanly into a zero value; nil curation maps identify a
	// structurally invalid snapshot (see decodeSnapshot).
	if reason != "" {
		l.cache.markSnapshotFailedIfUnloaded()
		l.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", l.path, "reason", reason)
		return snapshot{}, false, true
	}
	reportUnsupportedVersion(l.log, l.path, &snap)
	// A persisted FirstSeen ahead of the wall clock is repaired on the writer's carry
	// path (prepareCarriedItem), but the reader installs the decoded feed directly:
	// without the same correction a restored future-skewed or hand-edited snapshot is
	// served with a future <pubDate> until the next rebuild - indefinitely in
	// resident-idle mode - where an arr's delay profile sees a negative release age and
	// can hold the release instead of honoring the bounded journal window (h-f15).
	now := time.Now().UTC()
	if rebased := rebaseFutureFeed(snap.NyaaFeed, now) + rebaseFutureFeed(snap.ABFeed, now); rebased > 0 {
		// Counts only; the rejected timestamp comes from a tamperable file.
		l.log.Warn("indexer feed snapshot: future item timestamps rebased to load time",
			"path", l.path, "rebased", rebased)
	}
	snap.ABFeed = l.cache.rebuildABDownloadURLs(snap.ABFeed)
	snap.NyaaFeed = l.cache.rebuildNyaaDownloadURLs(snap.NyaaFeed)
	l.cache.warnBlankedInfoURLs(scrub)
	l.cache.warnDroppedItems(scrub)
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

// normalizeSnapshotItems re-canonicalizes each persisted item's non-derived wire
// fields, so a hand-edited, tampered, or legacy snapshot cannot put a value in the
// served feed that no producer could have written: - InfoHash goes through
// validInfoHash (the writer's own gate), blanking anything not a 40-char hex hash -
// writeItem renders it as the torznab infohash attr, a field consumers treat as torrent
// identity.
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

// validCategories keeps only the Torznab category ids this feed's own vocabulary
// defines (catTV / catAnime / catMovies - categoriesFor emits the latter two, and catTV
// is the parent the caps document advertises), dropping anything else.
func validCategories(cats []int) []int {
	out := cats[:0]
	for _, c := range cats {
		if c == catTV || c == catAnime || c == catMovies {
			out = append(out, c)
		}
	}
	return out
}

// rebuildDownloadURLs is the shared derivation mechanics behind rebuildABDownloadURLs
// and rebuildNyaaDownloadURLs: it re-derives each feed item's download URL from its
// non-secret tracker page URL (the GUID) via downloadURLForScope, which enforces the
// tracker-ownership gate internally (trackerOwnForm, the same fail-closed check
// writer-side journal admission runs through trackerKey).
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

// rebuildABDownloadURLs derives each persisted AnimeBytes feed item's download URL from
// its non-secret tracker page URL (the GUID) and the CURRENTLY configured passkey.
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

// rebuildNyaaDownloadURLs derives each persisted Nyaa feed item's download URL from its
// non-secret tracker page URL (the GUID), mirroring rebuildABDownloadURLs.
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
	return urlform.FoldHostASCII(u.Hostname())
})

// sanitizeSnapshotInfoURLs blanks any persisted item's InfoURL that is not a
// userinfo-free absolute http(s) URL on the canonical SeaDex host - the only shape the
// writer ever persists (entryURL).
func sanitizeSnapshotInfoURLs(feed []journalItem) int {
	host := seadexInfoHost()
	blanked := 0
	for i := range feed {
		if feed[i].InfoURL == "" {
			continue
		}
		if cleaned, ok := snapshotInfoURLAllowed(feed[i].InfoURL, host); ok {
			// Store the VOUCHED spelling, never the original. The gate reads urlform's
			// WHATWG-preprocessed form, so an edge-padded persisted value
			// ("\thttps://releases.moe/123") is vouched on the BROWSER's reading of it
			// - and renderFeed would then hand the arr UI the padded original as its
			// clickable <comments> link.
			feed[i].InfoURL = cleaned
			continue
		}
		feed[i].InfoURL = ""
		blanked++
	}
	return blanked
}

// snapshotInfoURLAllowed reports whether raw is a userinfo-free absolute http(s) URL,
// free of smuggling forms, on the canonical SeaDex host, and returns the VOUCHED
// spelling of it for the caller to store: urlform's WHATWG-preprocessed reading
// (Form.Trimmed), the string the gate actually judged.
func snapshotInfoURLAllowed(raw, host string) (cleaned string, ok bool) {
	// An unresolved expected host vouches NOTHING, and this is the leg that says so:
	// displaylink.VouchForm has no non-empty-host requirement of its own (its package
	// doc assigns that leg to httpDisplayHost), so an absolute "http:///path" would
	// otherwise byte-equal an empty expectation.
	if host == "" {
		return "", false
	}
	// The shared structural legs: absolute, http(s), no userinfo, no smuggling
	// bytes - publish-or-drop, the same stance trackerlink.Publish takes.
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

// unavailable reports whether the served feed has NO snapshot behind it.
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
	// Re-check under the write lock, authoritatively: concurrent requests racing the
	// onset must still emit the WARN exactly once, and an install that landed between
	// the read-unlock and here (a load returning, or an in-process publish) must make
	// THIS request answer from the fresh snapshot rather than render a stale error.
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
