package indexer

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
)

// warmedIndexer builds a server and warms its snapshot cache, the pairing Run
// makes at the lifecycle boundary (New itself is pure assembly and loads
// nothing). Tests that assert on the served feed immediately after construction
// use it; tests that exercise the pre-first-load paths call New directly.
//
// There is no wiring helper beside it any more: New and NewFeedWriter wire
// their own upstreams from the cfg they are given, so a test that exercises the
// Prowlarr proxy or the title harvest just passes its httptest client, and one
// that does not passes nil.
func warmedIndexer(cfg *Config, log *slog.Logger, client *http.Client) *Indexer {
	ix := New(cfg, log, client)
	ix.cache.loader.refresh(context.Background())
	return ix
}

// tick runs one iteration of the cache's own reload clock, which is what watch
// does per snapshotWatchInterval. Requests never load (see snapshotCache), so a
// test that changes the snapshot FILE and then asserts on the served feed says
// so with this rather than relying on a request to pick the change up.
func tick(ix *Indexer) { ix.cache.loader.refresh(context.Background()) }

// loadingIndexer builds a server whose reload clock is RUNNING with its first
// load unresolved - the state start leaves behind while the loader is still
// working, and the one state in which readiness and the two producers' ORDER are
// observable at all.
//
// warmedIndexer cannot produce it: it loads synchronously and never marks the
// clock started, so every test built on it has loadPending() == false. That gap
// is why the readiness inversion (a published snapshot answering a fault because
// a load was in flight) and the install ordering (an older read landing after a
// newer publish) were both unreachable from the suite - the publishing tests
// never had a load in flight and the watch tests never published.
func loadingIndexer(cfg *Config, log *slog.Logger, client *http.Client) *Indexer {
	ix := New(cfg, log, client)
	ix.cache.watchStarted.Store(true)
	return ix
}

// heldLoad installs the snapshotLoadGate seam so the NEXT load blocks on entry
// until the returned release func is called, and returns a channel closed once
// that load has actually entered. It is how a test puts a load IN FLIGHT at a
// chosen instant, which no filesystem can be made to do on demand.
//
// Only the first load through the gate is held; every later one passes straight
// through, so a test can hold one tick of the reload clock without stalling the
// rest.
func heldLoad(t *testing.T) (entered <-chan struct{}, release func()) {
	t.Helper()
	in := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	prev := snapshotLoadGate
	snapshotLoadGate = func() {
		held := false
		once.Do(func() { held = true })
		if !held {
			return
		}
		close(in)
		<-rel
	}
	var relOnce sync.Once
	t.Cleanup(func() {
		relOnce.Do(func() { close(rel) })
		snapshotLoadGate = prev
	})
	return in, func() { relOnce.Do(func() { close(rel) }) }
}
