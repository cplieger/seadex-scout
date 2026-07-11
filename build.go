package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/httpx/v2"
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

	seadexHTTP := httpx.NewClient(seadexTimeout)
	mappingHTTP := httpx.NewClient(mappingTimeout)
	anilistHTTP := httpx.NewClient(anilistTimeout)

	sonarr, radarr, err := newArrClients(cfg)
	if err != nil {
		return built{}, err
	}
	pingArrs(ctx, sonarr, radarr)

	anilistClient := anilist.NewClient(anilistHTTP, config.DefaultAniListURL, config.DefaultAniListRate, log)

	sc := scout.New(&scout.Deps{
		Logger: log,
		Store:  state.NewStore(config.DefaultStatePath, log),
		Library: library.NewWalker(&library.Config{
			Sonarr:      sonarrClient(sonarr),
			Radarr:      radarrClient(radarr),
			Logger:      log,
			RemuxGroups: cfg.RemuxGroups,
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
			RemuxGroups:     cfg.RemuxGroups,
			Filter:          filterOptions(cfg),
			SeasonScoping:   cfg.SeasonScoping,
			IncludeSpecials: cfg.IncludeSpecials,
		}),
		Auditor: audit.NewAuditor(audit.Config{
			Logger:          log,
			RemuxGroups:     cfg.RemuxGroups,
			SeaDexBaseURL:   config.DefaultSeaDexBaseURL,
			IncludeSpecials: cfg.IncludeSpecials,
			AnimeBytes:      cfg.AnimeBytes,
		}),
		Reporter: report.NewReporter(log),
		AniList:  anilistClient,
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

// indexerConfigured reports whether the Torznab feed has an upstream to proxy.
// The daemon starts the feed server only when at least one Prowlarr Torznab URL
// is set, so an alert-only deployment binds no HTTP port (keeping the
// socket-less posture) without needing an explicit on/off knob.
func indexerConfigured(cfg *config.Config) bool {
	return cfg.IndexerNyaaTorznabURL != "" || cfg.IndexerABTorznabURL != ""
}

// builtIndexer holds the assembled Torznab feed and the resources to release.
type builtIndexer struct {
	indexer *indexer.Indexer
	cleanup func()
}

// buildIndexer wires the Torznab feed the daemon runs alongside the compare
// loop: a SeaDex client for the curation set and an HTTP client for Prowlarr's
// per-indexer Torznab endpoints. Its logger carries component=indexer so its
// lines are easy to separate from the compare findings in a shared slog stream.
func buildIndexer(cfg *config.Config) builtIndexer {
	log := slog.Default().With("component", "indexer")
	seadexHTTP := httpx.NewClient(seadexTimeout)
	prowlarrHTTP := httpx.NewClient(indexerUpstreamTimeout)

	ix := indexer.New(&indexer.Config{
		Listen:         cfg.IndexerListen,
		APIKey:         cfg.IndexerAPIKey,
		NyaaTorznabURL: cfg.IndexerNyaaTorznabURL,
		ABTorznabURL:   cfg.IndexerABTorznabURL,
		ProwlarrAPIKey: cfg.IndexerProwlarrAPIKey,
	}, indexer.Deps{
		SeaDex: seadex.NewClient(seadexHTTP, config.DefaultSeaDexBaseURL, config.DefaultSeaDexPageDelay, log),
		HTTP:   prowlarrHTTP,
		Logger: log,
	})
	cleanup := func() {
		httpx.Close(seadexHTTP)
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
		if err := sonarr.Ping(ctx); err != nil {
			slog.Warn("sonarr ping failed at startup", "error", err)
		} else {
			slog.Info("sonarr reachable")
		}
	}
	if radarr != nil {
		if err := radarr.Ping(ctx); err != nil {
			slog.Warn("radarr ping failed at startup", "error", err)
		} else {
			slog.Info("radarr reachable")
		}
	}
}

// filterOptions builds the release filter policy from config.
func filterOptions(cfg *config.Config) filter.Options {
	return filter.Options{
		MinResolution:    cfg.MinResolution,
		AllowRemux:       cfg.AllowRemux,
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
