// Package indexer serves a Torznab feed of SeaDex releases for Sonarr/Radarr.
//
// It does not synthesize releases or talk to the trackers: it proxies Prowlarr's
// per-indexer Torznab endpoints for Nyaa and AnimeBytes, keeps only the results
// SeaDex curates (matched by tracker id / info hash against a cached SeaDex set),
// passes their real title/seeders/size/download URL straight through, and adds
// one SeaDex-specific signal - the download-volume-factor marker: best -> 0.75
// (Freeleech25), alt -> 0.25 (Freeleech75) - which the operator maps to a Custom
// Format on their anime profile. Because the download URLs are Prowlarr's own
// proxy links, no tracker credentials live here; the endpoint is apikey-gated
// and meant to bind LAN-only. The catalogue of what SeaDex curates is cached and
// refreshed in the background; per-request upstream results are briefly cached
// to soften the extra Prowlarr/AnimeBytes query load.
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
)

// Config is the indexer's runtime settings. APIKey (the feed's own gate) and
// ProwlarrAPIKey are secrets and are never logged. An empty Nyaa/AnimeBytes URL
// disables that upstream.
type Config struct {
	APIKey         string
	NyaaTorznabURL string
	ABTorznabURL   string
	ProwlarrAPIKey string
}

// Deps are the assembled clients the indexer needs: SeaDex for the curation set,
// and an HTTP client for the Prowlarr Torznab endpoints.
type Deps struct {
	SeaDex *seadex.Client
	HTTP   *http.Client
	Logger *slog.Logger
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

// empty reports whether the curation set has not been populated yet.
func (c *curation) empty() bool { return len(c.byHash) == 0 && len(c.byKey) == 0 }

// Indexer proxies Prowlarr, filtered to SeaDex's curation, over a Torznab feed.
type Indexer struct {
	set       curation
	seadex    *seadex.Client
	log       *slog.Logger
	cfg       Config
	upstreams []*upstream
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
		seadex: deps.SeaDex,
		log:    log,
		cfg:    *cfg,
	}
	// One upstream per configured Prowlarr Torznab URL. An empty URL means that
	// tracker is off: it is simply not wired, so the feed never queries it. (The
	// daemon only starts the feed at all when at least one URL is set.)
	if cfg.NyaaTorznabURL != "" {
		ix.upstreams = append(ix.upstreams, &upstream{
			http: deps.HTTP, log: log, name: "nyaa", feed: cfg.NyaaTorznabURL, apiKey: cfg.ProwlarrAPIKey,
		})
	}
	if cfg.ABTorznabURL != "" {
		ix.upstreams = append(ix.upstreams, &upstream{
			http: deps.HTTP, log: log, name: "ab", feed: cfg.ABTorznabURL, apiKey: cfg.ProwlarrAPIKey,
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

// Refresh fetches the SeaDex catalogue and rebuilds the curation set (info
// hashes and tracker keys of every tracked release, and whether each is best).
// A SeaDex fetch failure returns the error and leaves the previous set in place.
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
	ix.mu.Lock()
	ix.set = set
	ix.mu.Unlock()
	ix.log.Info("indexer curation set refreshed",
		"entries", len(entries), "torrents", torrents, "hashes", len(set.byHash), "keys", len(set.byKey))
	return nil
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

// serve handles the Torznab endpoint: t=caps returns capabilities, everything
// else proxies Prowlarr filtered to SeaDex's curation.
func (ix *Indexer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ix.cfg.APIKey != "" && q.Get("apikey") != ix.cfg.APIKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if q.Get("t") == "caps" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = io.WriteString(w, renderCaps())
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = io.WriteString(w, renderFeed(ix.query(r.Context(), q)))
}

// query returns the feed items for a request. It answers season, movie, special,
// and RSS queries by proxying the upstreams filtered to SeaDex's curation, and
// deliberately returns nothing (without contacting a tracker) for a per-episode
// query: Sonarr searches an anime season episode by episode AND as a whole season
// (see NewznabRequestGenerator), so answering only the season search still
// delivers the pack while sparing the trackers a query per episode - a manual
// single-episode search then costs nothing.
func (ix *Indexer) query(ctx context.Context, q url.Values) []Item {
	if !servesQuery(q) {
		return nil
	}
	items := ix.fetchAndFilter(ctx, upstreamParams(q))
	items = filterByCats(items, parseCats(q.Get("cat")))
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	return items
}

// fetchAndFilter queries every upstream in parallel, then keeps and marks the
// results SeaDex curates. It returns nil before the curation set is warm.
func (ix *Indexer) fetchAndFilter(ctx context.Context, params url.Values) []Item {
	ix.mu.RLock()
	set := ix.set
	ups := ix.upstreams
	ix.mu.RUnlock()

	if set.empty() || len(ups) == 0 {
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

	return markAndDedupe(raw, &set)
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
