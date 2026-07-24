package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/seadex-scout/internal/scout"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
)

// Outbound HTTP timeouts, sized to each upstream's payload.
const (
	seadexTimeout  = 90 * time.Second  // large paged responses
	mappingTimeout = 180 * time.Second // multi-MB Fribb file
	anilistTimeout = 30 * time.Second  // small GraphQL replies
	// indexerUpstreamTimeout bounds a Prowlarr Torznab query (which searches the
	// trackers live), used by the daemon's Torznab feed.
	indexerUpstreamTimeout = 60 * time.Second
	// arrMaxAttempts / arrBaseDelay bound arr request retries.
	arrMaxAttempts = 3
	arrBaseDelay   = 5 * time.Second
)

// built holds the assembled runtime and the resources to release on shutdown.
type built struct {
	scout   *scout.Scout
	cleanup func()
}

// buildScout wires config into every component and returns the runnable scout
// plus a cleanup func that releases the HTTP and arr clients.
func buildScout(ctx context.Context, cfg *config.Config) (built, error) {
	log := slog.Default()

	sonarr, radarr, err := newArrClients(cfg)
	if err != nil {
		return built{}, err
	}

	seadexHTTP := httpx.NewClient(seadexTimeout)
	mappingHTTP := httpx.NewClient(mappingTimeout)
	anilistHTTP := httpx.NewClient(anilistTimeout)
	pingArrs(ctx, sonarr, radarr)

	anilistClient := anilist.NewClient(anilistHTTP, config.DefaultAniListURL, config.DefaultAniListRate, log)

	sc := scout.New(&scout.Deps{
		Logger: log,
		Store:  state.NewStore(config.DefaultStatePath, log),
		Library: library.NewWalker(&library.Config{
			Sonarr:      sonarrClient(sonarr),
			Radarr:      radarrClient(radarr),
			Logger:      log,
			SonarrURL:   cfg.SonarrWebBase(),
			RadarrURL:   cfg.RadarrWebBase(),
			IncludeTags: cfg.IncludeTags,
			ExcludeTags: cfg.ExcludeTags,
		}),
		Mapping: mapping.NewLoader(mappingHTTP, config.DefaultMappingURL, config.DefaultMappingOverrides, config.DefaultMappingRefresh, log),
		SeaDex:  seadex.NewClient(seadexHTTP, config.DefaultSeaDexBaseURL, config.DefaultSeaDexPageDelay, log),
		Matcher: match.NewMatcher(anilistClient, log),
		Comparer: compare.NewComparer(compare.Config{
			Logger:          log,
			Filter:          filterOptions(cfg),
			ExcludeSpecials: cfg.ExcludeSpecials,
		}),
		Auditor: audit.NewAuditor(audit.Config{
			Logger:          log,
			SeaDexBaseURL:   config.DefaultSeaDexBaseURL,
			ExcludeSpecials: cfg.ExcludeSpecials,
			AnimeBytes:      cfg.AnimeBytes,
		}),
		Reporter: report.NewReporter(log),
		AniList:  anilistClient,
		Feed:     feedWriter(cfg, log),
	})

	cleanup := func() {
		httpx.Close(seadexHTTP)
		httpx.Close(mappingHTTP)
		httpx.Close(anilistHTTP)
		if sonarr != nil {
			sonarr.Close()
		}
		if radarr != nil {
			radarr.Close()
		}
	}
	return built{scout: sc, cleanup: cleanup}, nil
}

// feedWriter returns the indexer feed writer the compare cycle drives when the
// Torznab feed is configured, else nil (the cycle then does no feed work). It
// persists the materialized feed snapshot (curation set + synthesized RSS feeds)
// the indexer HTTP server reads, so one cycle feeds both the findings and the
// feed from a single SeaDex fetch. It holds no clients: the cycle owns the
// shared SeaDex + Fribb fetch and hands the results to Rebuild.
func feedWriter(cfg *config.Config, log *slog.Logger) scout.FeedWriter {
	if !cfg.IndexerConfigured() {
		return nil
	}
	abConfigured := cfg.IndexerABTorznabURL != ""
	return indexer.NewFeedWriter(cfg.IndexerABPasskey, abConfigured, config.DefaultIndexerFeedPath, log.With("component", "indexer"))
}

// builtIndexer holds the assembled Torznab feed server and the resources to
// release.
type builtIndexer struct {
	indexer *indexer.Indexer
	cleanup func()
}

// buildIndexer wires the Torznab feed server the daemon runs alongside the
// compare loop. It needs only an HTTP client for Prowlarr's per-indexer Torznab
// endpoints (a search proxies them); the curation set and RSS feeds it serves
// come from the snapshot the compare cycle persists (see feedWriter), which it
// reads from config.DefaultIndexerFeedPath. Its logger carries component=indexer
// so its lines separate cleanly from the compare findings in a shared slog stream.
func buildIndexer(cfg *config.Config) builtIndexer {
	log := slog.Default().With("component", "indexer")
	prowlarrHTTP := httpx.NewClient(indexerUpstreamTimeout)

	ix := indexer.New(&indexer.Config{
		APIKey:         cfg.IndexerAPIKey,
		NyaaTorznabURL: cfg.IndexerNyaaTorznabURL,
		ABTorznabURL:   cfg.IndexerABTorznabURL,
		ProwlarrAPIKey: cfg.IndexerProwlarrAPIKey,
		ABPasskey:      cfg.IndexerABPasskey,
	}, indexer.Deps{
		HTTP:   prowlarrHTTP,
		Logger: log,
	}, config.DefaultIndexerFeedPath)
	cleanup := func() {
		httpx.Close(prowlarrHTTP)
	}
	return builtIndexer{indexer: ix, cleanup: cleanup}
}

// newArrClients constructs the enabled arr clients from config.
func newArrClients(cfg *config.Config) (*arrapi.Sonarr, *arrapi.Radarr, error) {
	var sonarr *arrapi.Sonarr
	var radarr *arrapi.Radarr
	if cfg.SonarrEnabled() {
		s, err := arrapi.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey,
			arrapi.WithMaxAttempts(arrMaxAttempts), arrapi.WithBaseDelay(arrBaseDelay))
		if err != nil {
			return nil, nil, fmt.Errorf("sonarr client: %w", err)
		}
		sonarr = s
	}
	if cfg.RadarrEnabled() {
		r, err := arrapi.NewRadarr(cfg.RadarrURL, cfg.RadarrAPIKey,
			arrapi.WithMaxAttempts(arrMaxAttempts), arrapi.WithBaseDelay(arrBaseDelay))
		if err != nil {
			return nil, nil, fmt.Errorf("radarr client: %w", err)
		}
		radarr = r
	}
	return sonarr, radarr, nil
}

// pingArrs checks arr reachability at startup, logging the outcome. A failure
// is not fatal: the first cycle's health reflects the live state, and a
// temporarily-down arr should not stop the daemon from starting.
func pingArrs(ctx context.Context, sonarr *arrapi.Sonarr, radarr *arrapi.Radarr) {
	if sonarr != nil {
		logPing("sonarr", sonarr.Ping(ctx))
	}
	if radarr != nil {
		logPing("radarr", radarr.Ping(ctx))
	}
}

// logPing logs one arr's startup reachability, classifying a context
// cancellation (shutdown mid-startup) as routine rather than an arr fault.
func logPing(arr string, err error) {
	switch {
	case err == nil:
		slog.Info(arr + " reachable")
	case errors.Is(err, context.Canceled):
		slog.Debug(arr+" startup ping cancelled by shutdown", "error", err)
	default:
		slog.Warn(arr+" ping failed at startup", "error", err)
	}
}

// filterOptions builds the release filter policy from config.
func filterOptions(cfg *config.Config) filter.Options {
	return filter.Options{
		ExcludeRemux:     cfg.ExcludeRemux,
		RequireDualAudio: cfg.RequireDualAudio,
		AnimeBytes:       cfg.AnimeBytes,
	}
}

// sonarrClient returns s as a library.SonarrClient, or a nil interface when
// Sonarr is disabled (so the walker skips it rather than calling a nil pointer).
func sonarrClient(s *arrapi.Sonarr) library.SonarrClient {
	if s == nil {
		return nil
	}
	return s
}

// radarrClient returns r as a library.RadarrClient, or a nil interface when
// Radarr is disabled.
func radarrClient(r *arrapi.Radarr) library.RadarrClient {
	if r == nil {
		return nil
	}
	return r
}
