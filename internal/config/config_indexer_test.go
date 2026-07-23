package config

import (
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

func TestValidateIndexerWarnsOnIdenticalTorznabURLs(t *testing.T) {
	rec := capture.Default(t)
	const upstream = "http://prowlarr:9696/22/api"
	cfg := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		IndexerAPIKey: strings.Repeat("a", 16), IndexerProwlarrAPIKey: "pk", IndexerABPasskey: "passkey",
		IndexerNyaaTorznabURL: upstream, IndexerABTorznabURL: upstream,
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want identical endpoints to remain warn-only", err)
	}
	if !rec.Contains("indexer.nyaa_torznab_url and indexer.ab_torznab_url are identical") {
		t.Errorf("Validate() log = %v, want the identical-endpoint warning", rec.Messages())
	}
}

// TestValidateIndexerDistinctTorznabURLsStaySilent pins the absence side of
// the identical-torznab-endpoints warning: two distinct per-indexer Prowlarr
// endpoints (the correct configuration) must not fire it.
func TestValidateIndexerDistinctTorznabURLsStaySilent(t *testing.T) {
	rec := capture.Default(t)
	cfg := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		IndexerAPIKey: strings.Repeat("a", 16), IndexerProwlarrAPIKey: "pk", IndexerABPasskey: "passkey",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerABTorznabURL:   "http://prowlarr:9696/2/api",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want distinct per-indexer endpoints to validate", err)
	}
	if rec.Contains("are identical") {
		t.Errorf("Validate() log = %v, want no identical-endpoint warning for distinct per-indexer URLs", rec.Messages())
	}
}
