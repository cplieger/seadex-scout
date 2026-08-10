package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/httpx/v4"
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
	mappingTimeout = 180 * time.Second // multi-MB Fribb file
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
// walk, the Fribb mapping loader, the SeaDex client and the matcher, plus the
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

	sonarr, radarr, err := newArrClients(cfg)
	if err != nil {
		return scoutCore{}, err
	}

	seadexHTTP := httpx.NewClient(seadexTimeout)
	mappingHTTP := httpx.NewClient(mappingTimeout)
	anilistHTTP := httpx.NewClient(anilistTimeout)
	pingArrs(ctx, sonarr, radarr)

	anilistClient := anilist.NewClient(anilistHTTP, anilist.DefaultURL, anilist.DefaultRate, log)

	store := state.NewStore(config.DefaultStatePath, log)
	if readOnlyState {
		store = state.NewReadOnlyStore(config.DefaultStatePath, log)
	}

	return scoutCore{
		log:   log,
		store: store,
		library: arrwalk.NewWalker(&arrwalk.Config{
			Sonarr:      sonarrClient(sonarr),
			Radarr:      radarrClient(radarr),
			Logger:      log,
			SonarrURL:   cfg.SonarrWebBase(),
			RadarrURL:   cfg.RadarrWebBase(),
			IncludeTags: cfg.IncludeTags,
			ExcludeTags: cfg.ExcludeTags,
		}),
		mapping: mapping.NewLoader(mappingHTTP, mapping.DefaultURL, config.DefaultMappingOverrides, mapping.DefaultRefresh, log),
		seadex:  seadexapi.NewClient(seadexHTTP, seadex.DefaultBaseURL, seadexapi.DefaultPageDelay, log),
		matcher: match.NewMatcher(anilistClient, log),
		anilist: anilistClient,
		cleanup: func() {
			seadexHTTP.CloseIdleConnections()
			mappingHTTP.CloseIdleConnections()
			anilistHTTP.CloseIdleConnections()
			if sonarr != nil {
				sonarr.Close()
			}
			if radarr != nil {
				radarr.Close()
			}
		},
	}, nil
}

// buildScout wires config into the compare-cycle components and returns the
// runnable scout plus a cleanup func that releases the HTTP and arr clients -
// including the feed writer's Prowlarr client when a Torznab feed is configured.
//
// server is the Torznab feed server running in this process, or nil when none
// does (the `poll` subcommand). It is threaded through to the feed writer so a
// completed cycle hands its snapshot straight to that server instead of leaving
// it to be re-read from the file (see indexer.FeedWriterConfig.Server), which is
// why the daemon builds the server BEFORE the scout.
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
		Comparer: compare.NewComparer(compare.Config{
			TagFilter:       cfg.TagFilter,
			Filter:          filterOptions(cfg),
			ExcludeSpecials: cfg.ExcludeSpecials,
			AnimeBytes:      cfg.AnimeBytes,
		}),
		Notifier: notify.NewNotifier(c.log, cfg.IgnoreFindings),
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

// buildReporter wires config into the read-only one-shot report's components:
// the shared core plus the auditor. It deliberately builds no comparer, no
// notifier and no feed writer - the report cannot reach them - so a report run
// also opens no Prowlarr connection, and it reads state through the read-only
// store.
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
		Auditor: audit.NewAuditor(audit.Config{
			TagFilter:       cfg.TagFilter,
			ExcludeSpecials: cfg.ExcludeSpecials,
			AnimeBytes:      cfg.AnimeBytes,
		}),
	})
	return builtReporter{reporter: sc, cleanup: c.cleanup}, nil
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

// indexerLogger scopes a logger to the Torznab feed, so every feed-owned
// record (the writer's title harvest, the server's requests, the daemon
// goroutine's terminal lines) carries one component label and stays
// queryable together in the shared slog stream.
func indexerLogger(log *slog.Logger) *slog.Logger {
	return log.With("component", "indexer")
}

// feedWriter returns the indexer feed writer the compare cycle drives when the
// Torznab feed is configured - plus the cleanup releasing its Prowlarr HTTP
// client - else a nil writer (the cycle then does no feed work) and a no-op.
// It persists the materialized feed snapshot (curation set + the synthesized
// RSS journal) and hands it to server, the feed server running in this process
// (nil in the `poll` subcommand, whose snapshot reaches the resident daemon
// through the file), so one cycle feeds both the findings and the feed from a
// single SeaDex fetch. The cycle owns the shared SeaDex + Fribb fetch and hands
// the results to Rebuild; the writer's own client only serves the title harvest,
// which queries the same per-indexer Prowlarr Torznab endpoints the server
// proxies searches through.
func feedWriter(cfg *config.Config, log *slog.Logger, server *indexer.Indexer) (fw scout.FeedWriter, cleanup func()) {
	if !cfg.IndexerConfigured() {
		return nil, func() {}
	}
	prowlarrHTTP := httpx.NewClient(prowlarrTimeout)
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

// buildIndexer wires the Torznab feed server the daemon runs alongside the
// compare loop, or the zero builtIndexer when no Prowlarr Torznab URL is
// configured (the daemon then binds no HTTP port). It needs only an HTTP client
// for Prowlarr's per-indexer Torznab endpoints (a search proxies them); the
// curation set and RSS feeds it serves come from the compare cycle, in-process
// once the cycle is wired to it (see feedWriter) and from
// config.DefaultIndexerFeedPath on restart or when a `poll` cycle wrote it. Its
// logger carries component=indexer so its lines separate cleanly from the compare
// findings in a shared slog stream.
func buildIndexer(cfg *config.Config) builtIndexer {
	if !cfg.IndexerConfigured() {
		return builtIndexer{cleanup: func() {}}
	}
	log := indexerLogger(slog.Default())
	prowlarrHTTP := httpx.NewClient(prowlarrTimeout)

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
func newArrClients(cfg *config.Config) (*arrapi.Sonarr, *arrapi.Radarr, error) {
	var sonarr *arrapi.Sonarr
	var radarr *arrapi.Radarr
	if cfg.SonarrEnabled() {
		s, err := arrapi.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey,
			arrapi.WithMaxAttempts(arrMaxAttempts), arrapi.WithBaseDelay(arrBaseDelay))
		if err != nil {
			// arrapi's constructor error echoes the full baseURL with %q
			// (validateClientParams: `arrapi: invalid baseURL %q`), and an arr
			// url may carry configured userinfo - config.Validate only WARNS on
			// that shape. main logs a dispatch error at ERROR, so the message
			// must stay field-name-only, like reportSnapshot's LogSafeError
			// reduction and logPing below. config.validateArrPair already
			// rejects every shape arrapi rejects, so this arm only fires if the
			// two validators drift apart.
			return nil, nil, errors.New("sonarr client: sonarr.url or sonarr.api_key rejected by the arr client")
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
			// Field-name-only for the same reason as the sonarr arm above.
			return nil, nil, errors.New("radarr client: radarr.url or radarr.api_key rejected by the arr client")
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
// The logged error rides httpx.LogSafeError like the cycle and per-series
// walk boundaries: arrapi wraps transport failures around *url.Error, whose
// text carries the full request URL, so an accepted arr URL with embedded
// reverse-proxy credentials would otherwise leak them to this WARN/DEBUG.
func logPing(arr string, err error) {
	if err == nil {
		slog.Info(arr + " reachable")
		return
	}
	safeErr := httpx.LogSafeError(err)
	if errors.Is(err, context.Canceled) {
		slog.Debug(arr+" startup ping cancelled by shutdown", "error", safeErr)
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
func sonarrClient(s *arrapi.Sonarr) arrwalk.SonarrClient {
	if s == nil {
		return nil
	}
	return s
}

// radarrClient returns r as a arrwalk.RadarrClient, or a nil interface when
// Radarr is disabled.
func radarrClient(r *arrapi.Radarr) arrwalk.RadarrClient {
	if r == nil {
		return nil
	}
	return r
}
