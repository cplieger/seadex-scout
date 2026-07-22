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
