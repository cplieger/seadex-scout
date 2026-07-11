// Package indexer serves a Torznab feed of SeaDex releases for Sonarr/Radarr.
//
// It answers two request kinds two different ways:
//
//   - A SEARCH (the arr's automatic/interactive search, which carries a query)
//     is proxied to Prowlarr's per-indexer Torznab endpoint for that tracker and
//     filtered to the releases SeaDex curates (matched by info hash / tracker id
//     against a cached SeaDex set), passing their real title/seeders/size/
//     download URL straight through - so a search rides Prowlarr's own tracker
//     parse and credentials, and needs no passkey here.
//
//   - A periodic RSS check (an empty-query "latest releases" fetch, which Sonarr
//     and Radarr issue on their sync interval) is answered from a synthesized
//     per-tracker feed of the whole SeaDex curation set, built in the background
//     from the SeaDex catalogue: one item per curated torrent, its title derived
//     from the release's file names, its size summed from them, and a real
//     download link built directly - a public Nyaa .torrent, or AnimeBytes via
//     the operator's passkey. Synthesis is the only way to serve the SeaDex list
//     on RSS: an empty-query proxy would return the tracker's newest uploads, not
//     what SeaDex curates.
//
// Every item - search or RSS - carries the SeaDex download-volume-factor marker:
// best -> 0.75 (Freeleech25), alt -> 0.25 (Freeleech75), which the operator maps
// to a Custom Format on their anime profile. The AnimeBytes RSS link embeds the
// operator's passkey, so it is a secret; the endpoint is apikey-gated and meant
// to bind LAN-only. The curation set and the two synthesized feeds are cached and
// refreshed together in the background.
//
// The feed is served per-tracker only, addressable by path or by subdomain:
// /nyaa (or a nyaa.* host) serves the Nyaa-sourced curated releases, /ab (or an
// ab.* host) the AnimeBytes ones, and any other path or host is 404 - there is
// no combined feed. Adding the two per-tracker feeds as separate indexers in
// Prowlarr/Sonarr/Radarr lets each carry its own sync profile and gate that
// tracker's RSS/automatic/interactive use independently - the arr is the only
// component that knows the search type (it is never carried in the Torznab
// request), so it owns that policy. The subdomain form lets a reverse proxy map
// per-tracker hostnames to the one port without rewriting paths, for when
// seadex-scout runs apart from the arrs.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

const (
	// refreshInterval is how often the SeaDex curation set is re-fetched.
	refreshInterval = 3 * time.Hour
	// maxItems caps a rendered feed as a safety bound.
	maxItems = 1000
	// dvfBest / dvfAlt are the download-volume-factor markers: 0.75 -> Freeleech25
	// (SeaDex best), 0.25 -> Freeleech75 (SeaDex alt).
	dvfBest = "0.75"
	dvfAlt  = "0.25"

	shutdownGrace     = 10 * time.Second
	readHeaderTimeout = 15 * time.Second
	// listenAddr is the fixed LAN bind address for the Torznab feed server. The
	// port is an internal detail (the container/compose port mapping publishes
	// it), not an operator-tuned setting, so it is hardcoded rather than a key.
	listenAddr = ":9118"
	// upstreamNyaa / upstreamAB name the two proxied Prowlarr indexers. They
	// double as the per-tracker path segments the feed serves (see scopeFromPath)
	// and as the scope values upstreamsForScope matches on.
	upstreamNyaa = "nyaa"
	upstreamAB   = "ab"
)

// Config is the indexer's runtime settings. APIKey (the feed's own gate),
// ProwlarrAPIKey, and ABPasskey are secrets and are never logged. An empty
// Nyaa/AnimeBytes URL disables that upstream. ABPasskey is the operator's
// AnimeBytes passkey, appended to synthesized AB RSS download links (search
// links go through Prowlarr and need no passkey); empty leaves the AB RSS feed
// without grabbable links.
type Config struct {
	APIKey         string
	NyaaTorznabURL string
	ABTorznabURL   string
	ProwlarrAPIKey string
	ABPasskey      string
}

// Deps are the assembled clients the indexer needs: SeaDex for the curation set,
// an HTTP client for the Prowlarr Torznab endpoints, and the Fribb mapping
// loader used to tag each synthesized RSS item's category (movie vs series) by
// the entry's real media type, since a file name cannot tell a film from a
// single-file OVA/special. Mapping may be nil (every item is then treated as
// anime/series, the safe default that never mis-routes a special to Radarr).
type Deps struct {
	SeaDex  *seadex.Client
	HTTP    *http.Client
	Mapping *mapping.Loader
	Logger  *slog.Logger
}

// curation is the set of SeaDex-tracked releases, keyed by info hash and by
// tracker key, each mapping to whether SeaDex marks that release best.
type curation struct {
	byHash map[string]bool
	byKey  map[string]bool
}

// lookup reports whether a release (by its info hash and page URLs) is SeaDex-
// curated, and if so whether it is the best release.
func (c *curation) lookup(hash, infoURL, guid string) (isBest, matched bool) {
	if hash != "" {
		if b, ok := c.byHash[hash]; ok {
			return b, true
		}
	}
	for _, u := range []string{infoURL, guid} {
		if k := trackerKeyFromURL(u); k != "" {
			if b, ok := c.byKey[k]; ok {
				return b, true
			}
		}
	}
	return false, false
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation,
// and periodic RSS checks from the two synthesized per-tracker feeds. set (the
// search match index), nyaaFeed, and abFeed are rebuilt together on refresh and
// read under mu.
type Indexer struct {
	set       curation
	seadex    *seadex.Client
	mapping   *mapping.Loader
	mapCache  mapping.Cache
	log       *slog.Logger
	cfg       Config
	upstreams []*upstream
	nyaaFeed  []Item
	abFeed    []Item
	mu        sync.RWMutex
}

// New builds an Indexer from cfg and deps, wiring one upstream per configured
// Prowlarr Torznab URL.
func New(cfg *Config, deps Deps) *Indexer {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		seadex:  deps.SeaDex,
		mapping: deps.Mapping,
		log:     log,
		cfg:     *cfg,
	}
	// One upstream per configured Prowlarr Torznab URL. An empty URL means that
	// tracker is off: it is simply not wired, so the feed never queries it. (The
	// daemon only starts the feed at all when at least one URL is set.)
	if cfg.NyaaTorznabURL != "" {
		ix.upstreams = append(ix.upstreams, &upstream{
			http: deps.HTTP, log: log, name: upstreamNyaa, feed: cfg.NyaaTorznabURL, apiKey: cfg.ProwlarrAPIKey,
		})
	}
	if cfg.ABTorznabURL != "" {
		ix.upstreams = append(ix.upstreams, &upstream{
			http: deps.HTTP, log: log, name: upstreamAB, feed: cfg.ABTorznabURL, apiKey: cfg.ProwlarrAPIKey,
		})
	}
	return ix
}

// Run starts the background curation refresh and serves the Torznab endpoint
// until ctx is cancelled. The endpoint listens immediately (so an arr's caps
// Test succeeds right away) while the first SeaDex fetch warms up; the feed is
// empty until the curation set is populated. It owns no health marker - the
// daemon that runs it does - so a feed failure never flips container health.
func (ix *Indexer) Run(ctx context.Context) error {
	go ix.refreshLoop(ctx)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           ix.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	ix.log.Info("seadex-scout indexer listening",
		"addr", listenAddr, "apikey_set", ix.cfg.APIKey != "", "upstreams", len(ix.upstreams))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("indexer server: %w", err)
	}
	ix.log.Info("indexer shutdown complete", "cause", context.Cause(ctx))
	return nil
}

// Refresh fetches the SeaDex catalogue and rebuilds both what searches match
// against (the curation set: info hashes and tracker keys of every tracked
// release, and whether each is best) and what periodic RSS checks serve (the two
// synthesized per-tracker feeds). The Fribb mapping is loaded alongside to tag
// each synthesized item's category by the entry's real media type (movie vs
// series). A SeaDex fetch failure returns the error and leaves the previous set
// and feeds in place; a mapping failure only degrades categorization (items fall
// back to anime/series), never the whole refresh.
func (ix *Indexer) Refresh(ctx context.Context) error {
	entries, err := ix.seadex.FetchEntries(ctx)
	if err != nil {
		return fmt.Errorf("fetch seadex entries: %w", err)
	}
	set := curation{byHash: make(map[string]bool), byKey: make(map[string]bool)}
	torrents := 0
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			torrents++
			if h := strings.ToLower(strings.TrimSpace(t.InfoHash)); h != "" {
				set.byHash[h] = set.byHash[h] || t.IsBest
			}
			if k := trackerKey(t.Tracker, t.URL); k != "" {
				set.byKey[k] = set.byKey[k] || t.IsBest
			}
		}
	}
	idx := ix.loadMapping(ctx)
	nyaaFeed, abFeed, abSkippedNoPasskey := buildFeeds(entries, ix.cfg.ABPasskey, movieClassifier(idx.Lookup))

	ix.mu.Lock()
	ix.set = set
	ix.nyaaFeed = nyaaFeed
	ix.abFeed = abFeed
	ix.mu.Unlock()

	ix.log.Info("indexer curation set refreshed",
		"entries", len(entries), "torrents", torrents, "hashes", len(set.byHash), "keys", len(set.byKey),
		"nyaa_feed", len(nyaaFeed), "ab_feed", len(abFeed))
	if abSkippedNoPasskey > 0 {
		ix.log.Warn("ab RSS feed empty of grabbable links: set indexer.ab_passkey to serve AnimeBytes releases",
			"ab_releases_skipped", abSkippedNoPasskey)
	}
	return nil
}

// loadMapping returns the Fribb mapping index used to categorize synthesized
// feed items, reusing the in-memory cache across refreshes (a conditional GET on
// the slow Fribb cadence). It is safe to call with no mapping configured (nil
// loader -> nil index -> every item categorized as anime/series). A load failure
// is logged and the (stale or empty) index is used, so categorization degrades
// without failing the refresh. Called only from the single refresh goroutine.
func (ix *Indexer) loadMapping(ctx context.Context) *mapping.Index {
	if ix.mapping == nil {
		return nil
	}
	cache, idx, err := ix.mapping.Load(ctx, &ix.mapCache)
	ix.mapCache = cache
	if err != nil {
		ix.log.Warn("indexer mapping load degraded; categorizing with available data", "error", err)
	}
	return idx
}

// movieClassifier returns the category function buildFeeds stamps onto each
// entry's items. It routes a Fribb-typed movie to the Movies category (Radarr)
// and everything else - TV, OVA, ONA, SPECIAL, or an unmapped entry - to Anime
// (Sonarr). Defaulting the unknown/unmapped case to anime is deliberate: a
// single-file OVA/special looks just like a movie by file name, so the failure
// that matters (a special mis-routed to Radarr, where it can never match) is
// avoided at the cost of a rare unmapped film not surfacing on Radarr's RSS. The
// lookup is mapping.Index.Lookup, which is nil-safe.
func movieClassifier(lookup func(alID int) (mapping.Record, bool)) func(alID int) []int {
	return func(alID int) []int {
		if rec, ok := lookup(alID); ok && rec.IsMovie() {
			return []int{catMovies}
		}
		return []int{catAnime}
	}
}

// refreshLoop refreshes the curation set once immediately, then on
// refreshInterval until ctx is done.
func (ix *Indexer) refreshLoop(ctx context.Context) {
	if err := ix.Refresh(ctx); err != nil {
		ix.log.Warn("curation refresh failed; keeping previous set", "error", err)
	}
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ix.Refresh(ctx); err != nil {
				ix.log.Warn("curation refresh failed; keeping previous set", "error", err)
			}
		}
	}
}

// handler builds the HTTP mux (a single Torznab endpoint).
func (ix *Indexer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ix.serve)
	return mux
}

// serve handles the Torznab endpoint. Every request must address a specific
// tracker feed - /nyaa or /ab by path, or a nyaa.*/ab.* host; an unscoped
// request is 404 (there is no combined feed). t=caps returns capabilities,
// everything else proxies that tracker's Prowlarr endpoint filtered to SeaDex's
// curation.
func (ix *Indexer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ix.cfg.APIKey != "" && q.Get("apikey") != ix.cfg.APIKey {
		ix.log.Info("indexer request rejected", "reason", "bad apikey", "path", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	scope := scopeFor(r.Host, r.URL.Path)
	if scope == "" {
		ix.log.Info("indexer request rejected", "reason", "no tracker scope", "path", r.URL.Path, "host", r.Host)
		http.Error(w, "not found: address a tracker feed at /nyaa or /ab", http.StatusNotFound)
		return
	}
	if q.Get("t") == "caps" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = io.WriteString(w, renderCaps())
		ix.log.Info("indexer request", "scope", scope, "t", "caps")
		return
	}
	// The AnimeBytes RSS feed needs the operator's passkey to build grabbable
	// links, so without it the /ab feed has nothing to serve a periodic RSS check
	// (an empty-q request). Answer that with a Torznab error rather than an empty
	// feed, so Prowlarr's save-test fails with a clear reason and the operator
	// sets the passkey. An AB search (non-empty q) is unaffected: it proxies
	// Prowlarr, whose own link needs no passkey.
	if scope == upstreamAB && ix.cfg.ABPasskey == "" && strings.TrimSpace(q.Get("q")) == "" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = io.WriteString(w, renderError(errCodeIncorrectCredentials,
			"AnimeBytes passkey not configured: set indexer.ab_passkey in seadex-scout to serve the AnimeBytes feed"))
		ix.log.Info("indexer request rejected", "scope", scope, "reason", "ab passkey not configured")
		return
	}
	items, stats := ix.query(r.Context(), q, scope)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = io.WriteString(w, renderFeed(items))
	// One INFO line per request: the incoming Torznab params plus a result
	// summary. `answered` is false when the feed deliberately skips a per-episode
	// query (so an empty result reads as a skip, not a no-match); `feed` is true
	// for an empty-q RSS check served from the synthesized SeaDex feed; `upstream`
	// is how many results the tracker returned via Prowlarr for a search,
	// `curated` how many items were returned after curation/synthesis, `returned`
	// the final count after the category filter.
	ix.log.Info("indexer request",
		"scope", scope,
		"t", q.Get("t"),
		"q", q.Get("q"),
		"season", q.Get("season"),
		"ep", q.Get("ep"),
		"cat", q.Get("cat"),
		"answered", stats.answered,
		"feed", stats.feed,
		"upstream", stats.upstream,
		"curated", stats.curated,
		"returned", len(items))
}

// queryStats summarizes one request for the per-request log line: whether the
// feed answered it (answered), whether it was served from the synthesized RSS
// feed (feed - an empty-q periodic check) rather than a proxied search, how many
// raw results the tracker(s) returned via Prowlarr (search only), and how many
// items were returned after curation or synthesis (curated).
type queryStats struct {
	answered bool
	feed     bool
	upstream int
	curated  int
}

// query returns the feed items for a request (restricted to scope's tracker)
// plus a queryStats summary for logging.
//
// An empty-q request (Prowlarr's caps/save test, or an RSS "latest" fetch) is
// served from the synthesized per-tracker SeaDex feed - the whole curation set
// rendered as grabbable items - without contacting a tracker. This is the
// periodic new-release check: the arr parses each synthesized title and grabs
// what matches its library.
//
// A search (non-empty q) is proxied to that tracker's Prowlarr endpoint and
// filtered to SeaDex's curation, passing real titles/seeders/links through. A
// per-episode query is deliberately answered with nothing (without contacting a
// tracker): Sonarr searches an anime season episode by episode AND as a whole
// season (see NewznabRequestGenerator), so answering only the season search
// still delivers the pack while sparing the trackers a query per episode.
func (ix *Indexer) query(ctx context.Context, q url.Values, scope string) ([]Item, queryStats) {
	if !servesQuery(q) {
		return nil, queryStats{}
	}

	var (
		items []Item
		stats queryStats
	)
	if strings.TrimSpace(q.Get("q")) == "" {
		items = ix.feedFor(scope)
		stats = queryStats{answered: true, feed: true, curated: len(items)}
	} else {
		raw := ix.fetchRaw(ctx, upstreamParams(q), scope)
		ix.mu.RLock()
		set := ix.set
		ix.mu.RUnlock()
		items = markAndDedupe(raw, &set)
		stats = queryStats{answered: true, upstream: len(raw), curated: len(items)}
	}

	items = filterByCats(items, parseCats(q.Get("cat")))
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	return items, stats
}

// feedFor returns the synthesized RSS feed for a tracker scope (nyaa or ab),
// read under the lock since Refresh replaces the slices in the background. The
// returned slice is read-only for callers (query only sub-slices/filters a copy
// of the header via filterByCats, which allocates).
func (ix *Indexer) feedFor(scope string) []Item {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	switch scope {
	case upstreamNyaa:
		return ix.nyaaFeed
	case upstreamAB:
		return ix.abFeed
	default:
		return nil
	}
}

// fetchRaw queries the scope's upstream(s) in parallel and returns the raw
// results, before any curation filtering. Returns nil when no upstream is
// configured for the scope.
func (ix *Indexer) fetchRaw(ctx context.Context, params url.Values, scope string) []Item {
	ix.mu.RLock()
	ups := upstreamsForScope(ix.upstreams, scope)
	ix.mu.RUnlock()

	if len(ups) == 0 {
		return nil
	}

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		raw []Item
	)
	for _, u := range ups {
		wg.Add(1)
		go func(u *upstream) {
			defer wg.Done()
			items, err := u.search(ctx, params)
			if err != nil {
				ix.log.Warn("upstream query failed", "upstream", u.name, "error", err)
				return
			}
			mu.Lock()
			raw = append(raw, items...)
			mu.Unlock()
		}(u)
	}
	wg.Wait()

	return raw
}

// markAndDedupe keeps the curated releases, stamps each with the best/alt
// marker, and drops duplicates (same release from two upstreams) by guid.
func markAndDedupe(raw []Item, set *curation) []Item {
	seen := make(map[string]struct{}, len(raw))
	out := make([]Item, 0, len(raw))
	for i := range raw {
		it := raw[i]
		isBest, matched := set.lookup(it.InfoHash, it.InfoURL, it.GUID)
		if !matched {
			continue
		}
		it.DownloadVolumeFactor = dvfAlt
		if isBest {
			it.DownloadVolumeFactor = dvfBest
		}
		id := it.guid()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, it)
	}
	return out
}

// upstreamParams selects the Torznab query params to forward to Prowlarr,
// dropping our own apikey. It defaults the search type to a basic search.
func upstreamParams(q url.Values) url.Values {
	out := url.Values{}
	for _, k := range []string{"t", "q", "cat", "season", "ep", "limit", "offset"} {
		if v := q.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	if out.Get("t") == "" {
		out.Set("t", "search")
	}
	return out
}

// scopeFor resolves which tracker's results a request targets: the URL path
// first (scopeFromPath), the Host subdomain as a fallback (scopeFromHost), or ""
// when neither names a tracker - which serve treats as 404, since there is no
// combined feed. Serving per-tracker lets an arr treat the feed as two indexers
// and gate each tracker's RSS/automatic/interactive use with that indexer's own
// flags - the arr is the only component that knows the search type (it is never
// carried in the Torznab request), so it owns that decision. Two
// addressing styles are supported so it works whether seadex-scout shares a host
// with the arrs or sits behind a reverse proxy: a path (.../nyaa, .../ab) for
// direct use, or a subdomain (nyaa.example.com, ab.example.com) a proxy can map
// to the single port without rewriting the path.
func scopeFor(host, path string) string {
	if s := scopeFromPath(path); s != "" {
		return s
	}
	return scopeFromHost(host)
}

// scopeFromPath maps the URL path to a tracker via its first segment: "/nyaa..."
// -> nyaa, "/ab..." -> ab, anything else (including "/" and a bare "/api") -> ""
// (no tracker; serve 404s it).
func scopeFromPath(p string) string {
	switch firstSegment(p) {
	case upstreamNyaa:
		return upstreamNyaa
	case upstreamAB:
		return upstreamAB
	default:
		return ""
	}
}

// scopeFromHost maps a request Host to a tracker via its leading DNS label:
// nyaa.example.com -> nyaa, ab.example.com -> ab, anything else (a bare internal
// name like seadex-scout:9118, or any non-tracker host) -> "". This lets a
// reverse proxy route per-tracker subdomains to the one port with no path
// rewrite; the Host must reach the app unmodified (the default for a Caddy/nginx
// reverse proxy).
func scopeFromHost(host string) string {
	label, _, _ := strings.Cut(host, ".")
	switch strings.ToLower(label) {
	case upstreamNyaa:
		return upstreamNyaa
	case upstreamAB:
		return upstreamAB
	default:
		return ""
	}
}

// firstSegment returns the first non-empty path segment, lowercased.
func firstSegment(p string) string {
	p = strings.TrimLeft(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return strings.ToLower(p)
}

// upstreamsForScope returns the single upstream a scope targets (nyaa or ab),
// or none when no configured upstream matches. Scope is always a specific
// tracker here (serve rejects an unscoped request), so there is no
// all-trackers case.
func upstreamsForScope(all []*upstream, scope string) []*upstream {
	out := make([]*upstream, 0, 1)
	for _, u := range all {
		if u.name == scope {
			out = append(out, u)
		}
	}
	return out
}

// servesQuery reports whether the feed answers a request by querying the
// trackers, or returns empty without contacting them. It answers movie searches,
// season searches (`tvsearch` with no `ep`) and bare/RSS searches, and
// special/generic text searches - but NOT a per-episode query: a `tvsearch` with
// an `ep`, or a `t=search` whose `q` ends in the absolute episode number Sonarr
// appends (e.g. "Frieren 01"). Sonarr issues a season search too, which returns
// the pack, so dropping the per-episode queries loses nothing for a series while
// sparing the trackers one query per episode per scene-title alias. Specials and
// movies are single releases (not packs), so they are always answered.
//
// NOTE: this relies on Sonarr issuing the season search. For an Anime-type series
// that requires the indexer's "Anime Standard Format Search" option to be on (it
// gates AnimeSeasonSearchCriteria); see the README.
func servesQuery(q url.Values) bool {
	switch strings.ToLower(strings.TrimSpace(q.Get("t"))) {
	case "movie", "movie-search", "moviesearch":
		return true
	case "tvsearch", "tv-search":
		return strings.TrimSpace(q.Get("ep")) == ""
	default: // "search", "", specials, generic, RSS
		return !trailingEpisode.MatchString(strings.TrimSpace(q.Get("q")))
	}
}

// trailingEpisode matches the absolute episode number Sonarr appends to an anime
// title query (a space then a 2-4 digit, zero-padded number, e.g. "Frieren 01"),
// which marks a per-episode search the feed does not answer. A title that ends in
// a number ("Mob Psycho 100") is unaffected unless an episode is also appended.
var trailingEpisode = regexp.MustCompile(`\s+\d{2,4}$`)

// filterByCats keeps items whose category is requested (an anime item satisfies
// a request for its TV parent). An empty request keeps everything; an item with
// no categories is kept (Prowlarr already applied the forwarded cat filter).
func filterByCats(items []Item, cats map[int]bool) []Item {
	if len(cats) == 0 {
		return items
	}
	out := make([]Item, 0, len(items))
	for i := range items {
		if categoryMatch(items[i].Categories, cats) {
			out = append(out, items[i])
		}
	}
	return out
}

// categoryMatch reports whether an item's categories satisfy the requested set.
func categoryMatch(itemCats []int, want map[int]bool) bool {
	if len(itemCats) == 0 {
		return true
	}
	for _, c := range itemCats {
		if want[c] || (c == catAnime && want[catTV]) {
			return true
		}
	}
	return false
}

// parseCats parses a comma-separated torznab category list into a set.
func parseCats(s string) map[int]bool {
	out := make(map[int]bool)
	for part := range strings.SplitSeq(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n != 0 {
			out[n] = true
		}
	}
	return out
}
