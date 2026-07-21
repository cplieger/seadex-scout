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
// plus a cleanup func that releases the HTTP and arr clients. readOnlyState
// selects the read-only state store for flows documented never to write (or
// quarantine) the state file - the one-shot report - so a corrupt state.json
// is left in place for the daemon's own Load to quarantine and surface.
func buildScout(ctx context.Context, cfg *config.Config, readOnlyState bool) (built, error) {
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
	feed, feedCleanup := feedWriter(cfg, log)

	store := state.NewStore(config.DefaultStatePath, log)
	if readOnlyState {
		store = state.NewReadOnlyStore(config.DefaultStatePath, log)
	}

	sc := scout.New(&scout.Deps{
		Logger: log,
		Store:  store,
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
		SeaDex:  seadex.NewClient(seadexHTTP, seadex.DefaultBaseURL, config.DefaultSeaDexPageDelay, log),
		Matcher: match.NewMatcher(anilistClient, log),
		Comparer: compare.NewComparer(compare.Config{
			Filter:          filterOptions(cfg),
			ExcludeSpecials: cfg.ExcludeSpecials,
			AnimeBytes:      cfg.AnimeBytes,
		}),
		Auditor: audit.NewAuditor(audit.Config{
			SeaDexBaseURL:   seadex.DefaultBaseURL,
			ExcludeSpecials: cfg.ExcludeSpecials,
			AnimeBytes:      cfg.AnimeBytes,
		}),
		Reporter: report.NewReporter(log),
		AniListStats: func() (calls, rateLimitWaits int64) {
			st := anilistClient.Stats()
			return st.Calls, st.RateLimitWaits
		},
		Feed: feed,
	})

	cleanup := func() {
		seadexHTTP.CloseIdleConnections()
		mappingHTTP.CloseIdleConnections()
		anilistHTTP.CloseIdleConnections()
		feedCleanup()
		if sonarr != nil {
			sonarr.Close()
		}
		if radarr != nil {
			radarr.Close()
		}
	}
	return built{scout: sc, cleanup: cleanup}, nil
}

// upstreamConfig projects the operator config into the indexer's shared
// Prowlarr upstream wiring - built in one place so the feed writer (title
// harvest) and the feed server (search proxying) cannot drift apart.
func upstreamConfig(cfg *config.Config) indexer.UpstreamConfig {
	return indexer.UpstreamConfig{
		NyaaTorznabURL: cfg.IndexerNyaaTorznabURL,
		ABTorznabURL:   cfg.IndexerABTorznabURL,
		ProwlarrAPIKey: cfg.IndexerProwlarrAPIKey,
		ABPasskey:      cfg.IndexerABPasskey,
	}
}

// feedWriter returns the indexer feed writer the compare cycle drives when the
// Torznab feed is configured - plus the cleanup releasing its Prowlarr HTTP
// client - else a nil writer (the cycle then does no feed work) and a no-op.
// It persists the materialized feed snapshot (curation set + the synthesized
// RSS journal) the indexer HTTP server reads, so one cycle feeds both the
// findings and the feed from a single SeaDex fetch. The cycle owns the shared
// SeaDex + Fribb fetch and hands the results to Rebuild; the writer's own
// client only serves the title harvest, which queries the same per-indexer
// Prowlarr Torznab endpoints the server proxies searches through.
func feedWriter(cfg *config.Config, log *slog.Logger) (fw scout.FeedWriter, cleanup func()) {
	if !cfg.IndexerConfigured() {
		return nil, func() {}
	}
	prowlarrHTTP := httpx.NewClient(indexer.UpstreamAttemptTimeout)
	writer := indexer.NewFeedWriter(&indexer.FeedWriterConfig{
		Path:           config.DefaultIndexerFeedPath,
		SeaDexBaseURL:  seadex.DefaultBaseURL,
		UpstreamConfig: upstreamConfig(cfg),
	}, indexer.Deps{HTTP: prowlarrHTTP, Logger: log.With("component", "indexer")})
	return writer, func() { prowlarrHTTP.CloseIdleConnections() }
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
	prowlarrHTTP := httpx.NewClient(indexer.UpstreamAttemptTimeout)

	ix := indexer.New(&indexer.Config{
		APIKey:         cfg.IndexerAPIKey,
		UpstreamConfig: upstreamConfig(cfg),
	}, indexer.Deps{
		HTTP:   prowlarrHTTP,
		Logger: log,
	}, config.DefaultIndexerFeedPath)
	cleanup := func() {
		prowlarrHTTP.CloseIdleConnections()
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
			if sonarr != nil {
				sonarr.Close()
			}
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

// filterOptions builds the content-filter policy from config. The AnimeBytes
// tracker toggle is not part of filter.Options; it rides compare.Config and
// audit.Config directly.
func filterOptions(cfg *config.Config) filter.Options {
	return filter.Options{
		ExcludeRemux:     cfg.ExcludeRemux,
		RequireDualAudio: cfg.RequireDualAudio,
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
