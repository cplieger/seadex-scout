package indexer

import (
	"context"
	"log/slog"
	"net/http"
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
	ix.cache.refresh(context.Background())
	return ix
}

// tick runs one iteration of the cache's own reload clock, which is what watch
// does per snapshotWatchInterval. Requests never load (see snapshotCache), so a
// test that changes the snapshot FILE and then asserts on the served feed says
// so with this rather than relying on a request to pick the change up.
func tick(ix *Indexer) { ix.cache.refresh(context.Background()) }
