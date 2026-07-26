package indexer

import (
	"log/slog"
	"net/http"
)

// wiredIndexer builds a server whose upstreams are wired from cfg's own
// UpstreamConfig onto client - the pairing build.go makes explicitly at the
// composition root. Tests that do not exercise the Prowlarr search proxy pass
// Upstreams{} to New directly instead: the persisted snapshot still serves RSS
// for every tracker cfg enables, and a search for a scope with no wired upstream
// returns empty with a WARN.
func wiredIndexer(cfg *Config, log *slog.Logger, client *http.Client) *Indexer {
	return New(cfg, log, WireUpstreams(client, log, cfg.UpstreamConfig))
}

// wiredWriter is wiredIndexer's feed-writer twin, for tests that exercise the
// Prowlarr title harvest.
func wiredWriter(cfg *FeedWriterConfig, log *slog.Logger, client *http.Client) *FeedWriter {
	return NewFeedWriter(cfg, log, WireUpstreams(client, log, cfg.UpstreamConfig))
}
