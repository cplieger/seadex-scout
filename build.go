package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/radarr"
	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/scout"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
	"github.com/cplieger/seadex-scout/internal/state"
)

// Outbound HTTP timeouts, sized to each upstream's payload.
const (
	seadexTimeout  = 90 * time.Second  // large paged responses
	mappingTimeout = 180 * time.Second // multi-MB animap file
	anilistTimeout = 30 * time.Second  // small GraphQL replies
	// prowlarrTimeout is a transport BACKSTOP on the shared Prowlarr client,
	// not the per-attempt budget: internal/indexer derives each Torznab
	// attempt's own deadline and sizes its retry tree and HTTP write deadline
	// against it, so this only bounds a request the package's own deadline
	// somehow outlives. It is deliberately generous for that reason - a value
	// tightened here would silently cut the indexer's attempts short.
	prowlarrTimeout = 2 * time.Minute
	// arrMaxAttempts / arrBaseDelay bound arr request retries.
	arrMaxAttempts = 3
	arrBaseDelay   = 5 * time.Second
)

// built holds the assembled compare-cycle runtime and the resources to release
// on shutdown.
type built struct {
	scout   *scout.Scout
	cleanup func()
}

// builtReporter is built's read-only twin: the one-shot report runs a
// *scout.Reporter, which carries neither the comparer, the notifier nor the feed
// writer, so the two entry points cannot be handed each other's runtime.
type builtReporter struct {
	reporter *scout.Reporter
	cleanup  func()
}

// scoutCore is the wiring BOTH entry points need: the persisted store, the arr
// walk, the mapping loader, the SeaDex client and the matcher, plus the
// cleanup releasing the HTTP and arr clients it opened. Each role builder adds
// only its own components on top, so the report never constructs the notifier,
// the comparer, or the feed writer and its Prowlarr client.
type scoutCore struct {
	log     *slog.Logger
	store   scout.StateStore
	library *arrwalk.Walker
	mapping scout.MappingSource
	seadex  scout.SeaDexSource
	matcher *match.Matcher
	anilist *anilist.Client
	cleanup func()
}

// buildCore wires the shared components from config. readOnlyState selects the
// read-only state store for flows documented never to write (or quarantine) the
// state file - the one-shot report - so a corrupt state.json is left in place
// for the daemon's own Load to quarantine and surface.
func buildCore(ctx context.Context, cfg *config.Config, readOnlyState bool) (scoutCore, error) {
	log := slog.Default()

	sc, rc, err := newArrClients(cfg)
	if err != nil {
		return scoutCore{}, err
	}

	seadexHTTP := httpx.NewClient(seadexTimeout)
	mappingHTTP := httpx.NewClient(mappingTimeout)
	anilistHTTP := httpx.NewClient(anilistTimeout)
	reportArrs(ctx, sc, rc)

	anilistClient := anilist.NewClient(anilistHTTP, anilist.DefaultURL,
		anilist.WithRate(anilist.DefaultRate), anilist.WithLogger(log))

	store := state.NewStore(config.DefaultStatePath, log)
	if readOnlyState {
		store = state.NewReadOnlyStore(config.DefaultStatePath, log)
	}

	return scoutCore{
		log:   log,
		store: store,
		library: arrwalk.NewWalker(&arrwalk.Config{
			Sonarr:      sonarrClient(sc),
			Radarr:      radarrClient(rc),
			Logger:      log,
			SonarrURL:   cfg.SonarrWebBase(),
			RadarrURL:   cfg.RadarrWebBase(),
			IncludeTags: cfg.IncludeTags,
			ExcludeTags: cfg.ExcludeTags,
		}),
		mapping: mapping.NewLoader(mappingHTTP, mapread.Source{Repo: mapping.DefaultRepo},
			mapping.WithOverridesPath(config.DefaultMappingOverrides),
			mapping.WithRefresh(mapping.DefaultRefresh), mapping.WithLogger(log)),
		seadex: seadexapi.NewClient(seadexHTTP, seadex.DefaultBaseURL,
			seadexapi.WithPageDelay(seadexapi.DefaultPageDelay), seadexapi.WithLogger(log)),
		matcher: match.New(match.Config{AniList: anilistClient, Logger: log, ExcludeSpecials: cfg.ExcludeSpecials}),
		anilist: anilistClient,
		cleanup: func() {
			seadexHTTP.CloseIdleConnections()
			mappingHTTP.CloseIdleConnections()
			anilistHTTP.CloseIdleConnections()
			if sc != nil {
				sc.Close()
			}
			if rc != nil {
				rc.Close()
			}
		},
	}, nil
}

// buildScout wires config into the compare-cycle components and returns the
// runnable scout plus a cleanup func that releases the HTTP and arr clients.
// server is the Torznab feed server running in this process, or nil when none does
// (the `poll` subcommand); a completed cycle hands its snapshot straight to it.
func buildScout(ctx context.Context, cfg *config.Config, server *indexer.Indexer) (built, error) {
	c, err := buildCore(ctx, cfg, false)
	if err != nil {
		return built{}, err
	}
	anilistClient := c.anilist
	feed, feedCleanup := feedWriter(cfg, c.log, server)

	sc := scout.New(&scout.Deps{
		Logger:  c.log,
		Store:   c.store,
		Library: c.library,
		Mapping: c.mapping,
		SeaDex:  c.seadex,
		Matcher: c.matcher,
		Comparer: compare.New(compare.Config{
			Logger:     c.log,
			TagFilter:  cfg.TagFilter,
			Filter:     filterOptions(cfg),
			AnimeBytes: cfg.AnimeBytes,
		}),
		Notifier: notify.NewNotifier(c.log, cfg.IgnoreFindings),
		Auditor:  audit.New(auditConfig(cfg)),
		AniListStats: func() scout.AniListStats {
			st := anilistClient.Stats()
			return scout.AniListStats{Calls: st.Calls, RateLimitWaits: st.RateLimitWaits}
		},
		Feed:         feed,
		PollInterval: cfg.PollInterval,
	})

	cleanup := func() {
		feedCleanup()
		c.cleanup()
	}
	return built{scout: sc, cleanup: cleanup}, nil
}

// buildReporter wires config into the read-only one-shot report's components: the
// shared core plus the auditor. It builds no comparer, notifier or feed writer -
// the report cannot reach them - and reads state through the read-only store.
func buildReporter(ctx context.Context, cfg *config.Config) (builtReporter, error) {
	c, err := buildCore(ctx, cfg, true)
	if err != nil {
		return builtReporter{}, err
	}
	sc := scout.NewReporter(&scout.ReportDeps{
		Logger:  c.log,
		Store:   c.store,
		Library: c.library,
		Mapping: c.mapping,
		SeaDex:  c.seadex,
		Matcher: c.matcher,
		Auditor: audit.New(auditConfig(cfg)),
	})
	return builtReporter{reporter: sc, cleanup: c.cleanup}, nil
}

// auditConfig projects the operator config into the audit policy, built in one
// place so the one-shot report and the reconcile's library lines cannot drift.
func auditConfig(cfg *config.Config) audit.Config {
	return audit.Config{
		TagFilter:       cfg.TagFilter,
		ExcludeSpecials: cfg.ExcludeSpecials,
		AnimeBytes:      cfg.AnimeBytes,
	}
}

// upstreamConfig projects the operator config into the indexer's shared Prowlarr
// upstream wiring, built in one place so the feed writer and server cannot drift.
func upstreamConfig(cfg *config.Config) indexer.UpstreamConfig {
	return indexer.UpstreamConfig{
		NyaaTorznabURL: cfg.IndexerNyaaTorznabURL,
		ABTorznabURL:   cfg.IndexerABTorznabURL,
		ProwlarrAPIKey: cfg.IndexerProwlarrAPIKey,
		ABPasskey:      cfg.IndexerABPasskey,
	}
}

// indexerLogger scopes a logger to the Torznab feed, so every feed-owned record
// carries one component label in the shared slog stream.
func indexerLogger(log *slog.Logger) *slog.Logger {
	return log.With("component", "indexer")
}

// Keep X-Api-Key on the configured origin: net/http forwards this custom header
// on followed redirects, and httpx's same-host default admits another port.
func newProwlarrClient() *http.Client {
	c := httpx.NewClient(prowlarrTimeout)
	c.CheckRedirect = httpx.RedirectPolicyFunc(httpx.WithSameOrigin(true))
	return c
}

// feedWriter returns the indexer feed writer the compare cycle drives when the
// Torznab feed is configured - plus the cleanup releasing its Prowlarr HTTP client
// - else a nil writer (the cycle then does no feed work) and a no-op. It persists
// the materialized feed snapshot and hands it to server (nil in the `poll`
// subcommand, whose snapshot reaches the resident daemon through the file), so one
// cycle feeds both the findings and the feed from a single SeaDex fetch.
func feedWriter(cfg *config.Config, log *slog.Logger, server *indexer.Indexer) (fw scout.FeedWriter, cleanup func()) {
	if !cfg.IndexerConfigured() {
		return nil, func() {}
	}
	prowlarrHTTP := newProwlarrClient()
	log = indexerLogger(log)
	writer := indexer.NewFeedWriter(&indexer.FeedWriterConfig{
		Path:           config.DefaultIndexerFeedPath,
		Server:         server,
		TagFilter:      cfg.TagFilter,
		UpstreamConfig: upstreamConfig(cfg),
	}, log, prowlarrHTTP)
	return writer, func() { prowlarrHTTP.CloseIdleConnections() }
}

// builtIndexer holds the assembled Torznab feed server and the resources to
// release. A zero value (nil indexer, no-op cleanup) is the not-configured case.
type builtIndexer struct {
	indexer *indexer.Indexer
	cleanup func()
}

// buildIndexer wires the Torznab feed server the daemon runs alongside the compare
// loop, or the zero builtIndexer when no Prowlarr Torznab URL is configured (the
// daemon then binds no HTTP port). It needs only an HTTP client for Prowlarr's
// per-indexer Torznab endpoints; the curation set and RSS feeds it serves come from
// the compare cycle, in-process once wired and from the feed path on restart.
func buildIndexer(cfg *config.Config) builtIndexer {
	if !cfg.IndexerConfigured() {
		return builtIndexer{cleanup: func() {}}
	}
	log := indexerLogger(slog.Default())
	prowlarrHTTP := newProwlarrClient()

	ix := indexer.New(&indexer.Config{
		APIKey:         cfg.IndexerAPIKey,
		SnapshotPath:   config.DefaultIndexerFeedPath,
		UpstreamConfig: upstreamConfig(cfg),
	}, log, prowlarrHTTP)
	cleanup := func() {
		prowlarrHTTP.CloseIdleConnections()
	}
	return builtIndexer{indexer: ix, cleanup: cleanup}
}

// newArrClients constructs the enabled arr clients from config.
func newArrClients(cfg *config.Config) (sc *sonarr.Client, rc *radarr.Client, err error) {
	if cfg.SonarrEnabled() {
		sc, err = sonarr.New(cfg.SonarrURL, arrapi.APIKey(cfg.SonarrAPIKey),
			sonarr.WithMaxAttempts(arrMaxAttempts), sonarr.WithBaseDelay(arrBaseDelay))
		if err != nil {
			return nil, nil, errors.New("sonarr client: sonarr.url or sonarr.api_key rejected by the arr client")
		}
	}
	if cfg.RadarrEnabled() {
		rc, err = radarr.New(cfg.RadarrURL, arrapi.APIKey(cfg.RadarrAPIKey),
			radarr.WithMaxAttempts(arrMaxAttempts), radarr.WithBaseDelay(arrBaseDelay))
		if err != nil {
			if sc != nil {
				sc.Close()
			}
			return nil, nil, errors.New("radarr client: radarr.url or radarr.api_key rejected by the arr client")
		}
	}
	return sc, rc, nil
}

// reportArrs detects each enabled arr at startup and logs its version, API and
// features. A failure is not fatal: the first cycle's walk reports the live
// state, and a temporarily-down arr should not stop the daemon from starting.
func reportArrs(ctx context.Context, sc *sonarr.Client, rc *radarr.Client) {
	if sc != nil {
		info, err := sc.Info(ctx)
		if err != nil {
			logArrStartupFailure("sonarr", err)
		} else {
			slog.Info("sonarr reachable", sonarrReachableAttrs(&info)...)
		}
	}
	if rc != nil {
		info, err := rc.Info(ctx)
		if err != nil {
			logArrStartupFailure("radarr", err)
		} else {
			slog.Info("radarr reachable", "version", info.Version, "api", string(info.API),
				"deprecated", info.Deprecated)
		}
	}
}

// sonarrReachableAttrs is the "sonarr reachable" line's attributes: the
// detected version, API generation and every feature flag.
func sonarrReachableAttrs(info *sonarr.Info) []any {
	f := info.Features
	return []any{
		"version", info.Version, "api", string(info.API), "deprecated", info.Deprecated,
		"series_orders", f.SeriesOrders, "season_titles", f.SeasonTitles,
		"translations", f.Translations, "anime_ids", f.AnimeIDs,
	}
}

// logArrStartupFailure logs one arr's failed startup detection. A shutdown is
// routine (Debug); an unsupported API is an ERROR because it does not clear
// without an operator; anything else may be transient (Warn). arrapi's errors
// carry no request URL; LogSafeError is defence in depth.
func logArrStartupFailure(arr string, err error) {
	safeErr := httpx.LogSafeError(err)
	if errors.Is(err, context.Canceled) {
		slog.Debug(arr+" startup ping cancelled by shutdown", "error", safeErr)
		return
	}
	if _, ok := errors.AsType[*arrapi.UnsupportedAPIError](err); ok {
		slog.Error(arr+" API unsupported", "error", safeErr)
		return
	}
	slog.Warn(arr+" ping failed at startup", "error", safeErr)
}

// filterOptions builds the content-filter policy from config. The AnimeBytes
// tracker toggle is not part of filter.Options; it rides compare.Config and
// audit.Config directly. The filters.exclude_tags policy rides the same three
// component configs as cfg.TagFilter (one tagfilter.Filter, parsed once in
// internal/config), so the findings, the report and the feed ask ONE policy
// instead of each hardcoding a tag vocabulary.
func filterOptions(cfg *config.Config) filter.Options {
	return filter.Options{
		ExcludeRemux:     cfg.ExcludeRemux,
		RequireDualAudio: cfg.RequireDualAudio,
	}
}

// sonarrClient returns s as a arrwalk.SonarrClient, or a nil interface when
// Sonarr is disabled (so the walker skips it rather than calling a nil pointer).
func sonarrClient(s *sonarr.Client) arrwalk.SonarrClient {
	if s == nil {
		return nil
	}
	return arrwalk.FromSonarr(s)
}

// radarrClient returns r as a arrwalk.RadarrClient, or a nil interface when
// Radarr is disabled.
func radarrClient(r *radarr.Client) arrwalk.RadarrClient {
	if r == nil {
		return nil
	}
	return r
}
