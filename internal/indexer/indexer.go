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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/webhttp"
)

const (
	// maxItems caps a rendered feed as a safety bound.
	maxItems = 1000
	// defaultCapsLimit is the default result count advertised in t=caps.
	defaultCapsLimit = 100
	// dvfBest / dvfAlt are the download-volume-factor markers: 0.75 -> Freeleech25
	// (SeaDex best), 0.25 -> Freeleech75 (SeaDex alt).
	dvfBest = "0.75"
	dvfAlt  = "0.25"

	shutdownGrace     = 10 * time.Second
	readHeaderTimeout = 15 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	// listenAddr is the fixed LAN bind address for the Torznab feed server. The
	// port is an internal detail (the container/compose port mapping publishes
	// it), not an operator-tuned setting, so it is hardcoded rather than a key.
	listenAddr = ":9118"
	// upstreamNyaa / upstreamAB name the two proxied Prowlarr indexers. They
	// double as the per-tracker path segments the feed serves (see scopeFromPath)
	// and as the scope values upstreamForScope matches on.
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

// Deps are the clients the indexer server needs: an HTTP client for the Prowlarr
// per-indexer Torznab endpoints a search proxies. The curation set and the
// synthesized RSS feeds are not built here - the compare cycle builds and
// persists them (see FeedWriter) and the server reads that snapshot - so the
// server needs no SeaDex or Fribb client of its own.
type Deps struct {
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
// curated, and if so whether it is the best release. Every structurally valid
// identity signal the item carries must resolve to curated entries agreeing on
// the best/alt value; a signal that misses the curation set, or one that
// contradicts an earlier signal, rejects the whole item. This keeps an
// untrusted Torznab item from pairing a curated info hash with the page URL or
// download link of a different (alt or uncurated) torrent.
func (c *curation) lookup(hash, infoURL, guid string) (isBest, matched bool) {
	accept := func(candidate, ok bool) bool {
		if !ok || (matched && candidate != isBest) {
			return false
		}
		isBest = candidate
		matched = true
		return true
	}

	if h := validInfoHash(hash); h != "" {
		b, ok := c.byHash[h]
		if !accept(b, ok) {
			return false, false
		}
	}
	for _, raw := range []string{infoURL, guid} {
		k := trackerKeyFromURL(raw)
		if k == "" {
			continue
		}
		b, ok := c.byKey[k]
		if !accept(b, ok) {
			return false, false
		}
	}
	return isBest, matched
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation,
// and periodic RSS checks from the two synthesized per-tracker feeds. Both come
// from snap, the materialized feed the compare cycle builds and persists (see
// FeedWriter); the server loads it on start and reloads it when the file changes
// (a cycle - in this process or the `poll` subcommand - rewrote it), reading it
// under mu. The server never fetches SeaDex or Fribb itself.
type Indexer struct {
	cfg       Config
	snap      snapshot
	log       *slog.Logger
	path      string
	snapMod   time.Time
	upstreams []*upstream // wired once in New; immutable afterwards (not guarded by mu)
	// reloadMu coalesces concurrent snapshot refreshes: only one request runs
	// reload's stat/read/unmarshal at a time; the rest serve the current
	// immutable snapshot (see reload). mu still guards the published snapshot.
	reloadMu sync.Mutex
	mu       sync.RWMutex
}

// New builds the Torznab feed server from cfg and deps, wiring one upstream per
// configured Prowlarr Torznab URL. snapshotPath is where the compare cycle
// persists the materialized feed (config.DefaultIndexerFeedPath in production);
// it is loaded now so a restart serves the last feed immediately, and reloaded
// on change while running. An empty path serves an empty feed (used in tests).
func New(cfg *Config, deps Deps, snapshotPath string) *Indexer {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		log:  log,
		path: snapshotPath,
		cfg:  *cfg,
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
	// Warm the feed from the last persisted snapshot so a restart serves
	// immediately rather than empty until the next cycle.
	ix.reload(context.Background())
	return ix
}

// Run serves the Torznab endpoint from the persisted feed snapshot until ctx is
// cancelled. The endpoint listens immediately (so an arr's caps Test succeeds
// right away); it serves whatever feed the last compare cycle persisted (empty
// until the first cycle on a fresh install) and reloads the snapshot when a
// cycle rewrites it. It owns no health marker - the daemon that runs it does -
// so a feed failure never flips container health.
func (ix *Indexer) Run(ctx context.Context) error {
	// Fail closed at the network boundary: config.Validate (validateIndexer)
	// already rejects a configured feed with an empty feed_api_key on the
	// daemon path, but any alternate construction of the exported Indexer must
	// never bind and serve the feed unauthenticated - the AnimeBytes RSS feed
	// embeds ab_passkey in its download links.
	if ix.cfg.APIKey == "" {
		return errors.New("indexer: indexer.feed_api_key is empty; refusing to serve the Torznab feed unauthenticated")
	}
	// Bind up front so a port-in-use error surfaces synchronously here and is
	// returned to the daemon's startIndexer, which logs it. The feed owns no
	// health marker (the compare loop does), so a bind failure never flips
	// container health.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("indexer listen on %s: %w", listenAddr, err)
	}

	// The HTTP surface rides the shared webhttp plumbing (server bootstrap +
	// graceful shutdown). Logging is the standard access line (method, PATH
	// only, status, duration, request id) - adopted here because webhttp's
	// RequestLogger logs r.URL.Path and never the query string, so the Torznab
	// apikey (which arrives as a query parameter) cannot leak into the access
	// log; it sits outermost so a recovered panic logs as its 500. serve's own
	// domain line (scope/params/result counts) complements it - that line
	// whitelists the params it logs and likewise never logs apikey. Recoverer
	// turns a handler panic into a logged 500 rendered as a Torznab <error>
	// via torznabErrorResponder - not net/http's bare connection close, and
	// not webhttp's default JSON envelope, which is the wrong wire shape for
	// this XML endpoint. WriteTimeout stays unset (NewServer's streaming-safe
	// default): a search proxies an upstream Prowlarr query of unbounded
	// latency, so a fixed write deadline would truncate a slow search
	// mid-response.
	handler := webhttp.Chain(ix.handler(),
		webhttp.Logging(webhttp.WithLogger(ix.log)),
		webhttp.Recoverer(
			webhttp.WithRecoverLogger(ix.log),
			webhttp.WithRecoverResponder(torznabErrorResponder),
		),
	)
	srv := webhttp.NewServer(handler,
		webhttp.WithReadHeaderTimeout(readHeaderTimeout),
		webhttp.WithReadTimeout(readTimeout),
		webhttp.WithIdleTimeout(idleTimeout),
	)

	ix.log.Info("seadex-scout indexer listening",
		"addr", listenAddr, "upstreams", len(ix.upstreams))

	if err := webhttp.Run(ctx, srv, ln, nil, webhttp.WithShutdownGrace(shutdownGrace)); err != nil {
		return fmt.Errorf("indexer server: %w", err)
	}
	ix.log.Info("indexer shutdown complete", "cause", context.Cause(ctx))
	return nil
}

// torznabErrorResponder is the webhttp Recoverer ErrorResponder for the Torznab
// feed: it renders a recovered panic's 500 as a Torznab <error> document on the
// XML content type the arrs expect, in place of webhttp's default JSON envelope.
// Recoverer already logged the panic and only calls this when the response has
// not been committed; this just writes the body.
func torznabErrorResponder(w http.ResponseWriter, _ *http.Request, status int, _, msg string) {
	noCacheHeaders(w.Header())
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, renderError(errCodeUnknown, msg))
}

// noCacheHeaders marks a Torznab response as non-cacheable. The authenticated
// /ab RSS body embeds the operator's AnimeBytes passkey in its download links,
// so no cache may retain the representation beyond the request lifetime.
func noCacheHeaders(h http.Header) {
	h.Set("Cache-Control", "private, no-store, max-age=0")
	h.Set("Pragma", "no-cache")
}

// reload refreshes the served feed from the persisted snapshot when the file on
// disk carries a different mtime than the loaded copy (or nothing is loaded
// yet). A compare cycle - in this process (the daemon loop) or another (the
// `poll` subcommand) - rewrites the snapshot atomically, so a cheap mtime check
// per request picks up a new feed without the server ever fetching SeaDex
// itself. The comparison is inequality, not strictly-newer: a /config volume
// restored from backup, or a file replaced by an atomic rename that preserves
// an OLDER mtime, must still install (a strictly-After guard would wedge the
// server on the stale in-memory snapshot until restart); an unchanged mtime
// still skips. A missing file leaves the current (possibly empty) feed in
// place; a malformed or unreadable file is logged and ignored, so a bad write
// never blanks a live feed.
//
// Concurrent calls coalesce: after a cycle rewrites the snapshot, every
// in-flight request observes the newer mtime at once, and without coalescing
// each would independently read and unmarshal up to maxFeedBytes before the
// under-mu recheck let only one install it. reloadMu.TryLock lets exactly one
// request refresh; the rest return immediately and serve the current immutable
// snapshot (the next request picks up the newly installed one).
func (ix *Indexer) reload(ctx context.Context) {
	if ix.path == "" {
		return
	}
	if !ix.reloadMu.TryLock() {
		return
	}
	defer ix.reloadMu.Unlock()
	info, err := os.Stat(ix.path)
	if err != nil {
		// A missing file is the normal fresh-install case; anything else
		// (EACCES, EIO) silently freezes the served feed, so make it visible.
		if !errors.Is(err, fs.ErrNotExist) {
			ix.log.Warn("indexer feed snapshot stat failed; keeping current feed", "path", ix.path, "error", err)
		}
		return
	}
	ix.mu.RLock()
	loaded := ix.snapMod
	ix.mu.RUnlock()
	// Any mtime CHANGE triggers the reload (see the doc comment): inequality
	// rather than After, so a backup-restored or preserved-older-mtime
	// replacement installs instead of wedging the server on the loaded copy.
	if info.ModTime().Equal(loaded) {
		return
	}
	snap, ok := ix.readSnapshot(ctx)
	if !ok {
		return
	}
	ix.mu.Lock()
	// Re-check under the write lock. reloadMu already serializes the whole
	// stat/read/install sequence, so no concurrent reload can install between
	// our stat and here today; this guard is defense in depth (never
	// re-installing a copy of what is already loaded) should the TryLock
	// coalescing above ever change. Same inequality comparison as the outer
	// check: any mtime CHANGE - including an older one from a backup restore
	// or a preserved-mtime replace - installs; only equality skips.
	if info.ModTime().Equal(ix.snapMod) {
		ix.mu.Unlock()
		return
	}
	ix.snap = snap
	ix.snapMod = info.ModTime()
	ix.mu.Unlock()
	ix.log.Info("indexer feed snapshot loaded",
		"path", ix.path, "hashes", len(snap.ByHash), "keys", len(snap.ByKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed))
}

// readSnapshot is reload's read/decode error policy: it bounded-reads and
// decodes the persisted feed snapshot, reporting ok=false on any failure so
// the caller keeps the current feed. A shutdown cancellation is silent; an
// unreadable or malformed file is logged (a bad write must never blank a live
// feed).
func (ix *Indexer) readSnapshot(ctx context.Context) (snapshot, bool) {
	data, err := atomicfile.ReadBounded(ctx, ix.path, maxFeedBytes)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			ix.log.Warn("indexer feed snapshot unreadable; keeping current feed", "path", ix.path, "error", err)
		}
		return snapshot{}, false
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		ix.log.Warn("indexer feed snapshot malformed; keeping current feed", "path", ix.path, "error", err)
		return snapshot{}, false
	}
	return snap, true
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
	if ix.cfg.APIKey != "" {
		// Hash both values to fixed-length digests before the constant-time
		// compare: ConstantTimeCompare short-circuits on differing lengths,
		// which would otherwise leak the configured key's length (CWE-208).
		provided := sha256.Sum256([]byte(q.Get("apikey")))
		expected := sha256.Sum256([]byte(ix.cfg.APIKey))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			ix.log.Info("indexer request rejected", "reason", "bad apikey", "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	// Every authenticated caps/error/feed response is marked non-cacheable up
	// front: the /ab RSS body embeds the operator's AnimeBytes passkey in its
	// download links, and a browser, intermediary, or explicitly configured
	// reverse-proxy cache must never retain that credential-bearing body
	// beyond the request.
	noCacheHeaders(w.Header())
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
	// A total upstream failure (every queried Prowlarr upstream failed) is
	// reported as a Torznab <error>, not an empty 200 feed: an empty feed reads
	// as a clean "no SeaDex match" to the arr, which would silently record a
	// Prowlarr outage as a successful no-results search. A partial failure (one
	// of several upstreams answered) keeps the degraded-but-successful feed.
	if stats.upstreamFailed {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = io.WriteString(w, renderError(errCodeUnknown,
			"upstream Prowlarr query failed; search results unavailable"))
		ix.log.Info("indexer request rejected", "scope", scope, "reason", "upstream query failed")
		return
	}
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
// feed (feed - an empty-q periodic check) rather than a proxied search, whether
// the search's queried upstream(s) ALL failed (upstreamFailed - serve renders a
// Torznab <error> instead of an empty feed then), how many raw results the
// tracker(s) returned via Prowlarr (search only), and how many items were
// returned after curation or synthesis (curated).
type queryStats struct {
	answered       bool
	feed           bool
	upstreamFailed bool
	upstream       int
	curated        int
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
func (ix *Indexer) query(ctx context.Context, q url.Values, scope string) ([]item, queryStats) {
	if !servesQuery(q) {
		return nil, queryStats{}
	}
	// Pick up a newer feed snapshot a cycle may have written (this process's
	// daemon loop, or the `poll` subcommand in another process) before serving.
	ix.reload(ctx)

	var (
		items []item
		stats queryStats
	)
	if strings.TrimSpace(q.Get("q")) == "" {
		items = ix.feedFor(scope)
		stats = queryStats{answered: true, feed: true, curated: len(items)}
	} else {
		raw, failed := ix.fetchRaw(ctx, upstreamParams(q), scope)
		ix.mu.RLock()
		// The snapshot maps are safe to read after the lock is released: reload
		// installs a fresh snapshot and never mutates the loaded maps in place
		// (the same invariant feedFor documents for the feed slices).
		set := curation{byHash: ix.snap.ByHash, byKey: ix.snap.ByKey}
		ix.mu.RUnlock()
		items = markAndDedupe(raw, &set)
		stats = queryStats{answered: true, upstreamFailed: failed, upstream: len(raw), curated: len(items)}
	}

	items = filterByCats(items, parseCats(q.Get("cat")))
	if stats.feed {
		items = applyPaging(items, q)
	}
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	return items, stats
}

// applyPaging honors an explicit Torznab offset/limit (advertised in t=caps)
// on the synthesized feed. Absent or invalid params leave the feed unpaged
// (it is already capped at feedWindow); the proxied search path forwards
// these params to Prowlarr instead, so it never pages locally.
func applyPaging(items []item, q url.Values) []item {
	if off, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil && off > 0 {
		if off >= len(items) {
			return nil
		}
		items = items[off:]
	}
	if lim, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && lim > 0 && lim < len(items) {
		items = items[:lim]
	}
	return items
}

// feedFor returns the synthesized RSS feed for a tracker scope (nyaa or ab),
// read under the lock since reload replaces the snapshot when a cycle rewrites
// it. The returned slice is safe to use after the lock is released: reload
// installs a fresh snapshot with new backing arrays and never mutates the old
// ones, so a slice handed out here stays immutable even across a swap. Callers
// must only read it (never append/write in place).
func (ix *Indexer) feedFor(scope string) []item {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	switch scope {
	case upstreamNyaa:
		return ix.snap.NyaaFeed
	case upstreamAB:
		return ix.snap.ABFeed
	default:
		return nil
	}
}

// fetchRaw queries the scope's upstream and returns the raw results, before
// any curation filtering, plus whether the query was a total upstream failure
// (every queried upstream failed - with per-tracker scoping that is the one
// upstream the scope names). On failed=true serve renders a Torznab <error>
// instead of a fake-empty 200 feed, so a Prowlarr outage surfaces as a failed
// search in the arr rather than a clean no-results one. Returns nil,false when
// no upstream is configured for the scope (a standing misconfiguration, not a
// query failure) or when the caller cancelled the request.
func (ix *Indexer) fetchRaw(ctx context.Context, params url.Values, scope string) (items []item, failed bool) {
	// upstreams is wired once in New, before any request can arrive, and is
	// never mutated afterwards; mu guards only the snapshot fields.
	u := upstreamForScope(ix.upstreams, scope)
	if u == nil {
		// A search reached a scope whose Prowlarr upstream is not configured
		// (e.g. an /ab search with only nyaa_torznab_url set): the empty result
		// is a permanent misconfiguration, not a no-match, so say so.
		ix.log.Warn("search for tracker scope with no configured upstream; returning empty",
			"scope", scope)
		return nil, false
	}

	items, err := u.search(ctx, params)
	if err != nil {
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err())) {
			// Caller (the arr) went away or its request deadline fired; not an
			// upstream fault. A Prowlarr HTTP client timeout leaves ctx.Err()
			// nil and should warn.
			return nil, false
		}
		ix.log.Warn("upstream query failed", "upstream", u.name, "error", err)
		return nil, true
	}
	return items, false
}

// markAndDedupe keeps the curated releases, stamps each with the best/alt
// marker, and drops intra-upstream duplicates by guid (a torrent listed under
// several title aliases carries distinct guids and is deliberately kept).
func markAndDedupe(raw []item, set *curation) []item {
	seen := make(map[string]struct{}, len(raw))
	out := make([]item, 0, len(raw))
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

// upstreamForScope returns the upstream a scope targets (nyaa or ab), or nil
// when no configured upstream matches. Scope is always a specific tracker here
// (serve rejects an unscoped request) and New wires at most one upstream per
// name, so a single match is the only case.
func upstreamForScope(all []*upstream, scope string) *upstream {
	for _, u := range all {
		if u.name == scope {
			return u
		}
	}
	return nil
}

// servesQuery reports whether the feed answers a request by querying the
// trackers, or returns empty without contacting them. It answers movie searches
// (`t=movie`, or a `t=search` carrying the Movies category), season searches
// (`tvsearch` with no `ep`) and bare/RSS searches, and special/generic text
// searches - but NOT a per-episode query: a `tvsearch` with an `ep`, or a
// `t=search` whose `q` ends in the absolute episode number Sonarr appends (e.g.
// "Frieren 01"). Sonarr issues a season search too, which returns the pack, so
// dropping the per-episode queries loses nothing for a series while sparing the
// trackers one query per episode per scene-title alias. Specials and movies are
// single releases (not packs), so they are always answered - a film search comes
// through as `t=search` with the movie's year in `q`, so it is recognized by its
// Movies category rather than the trailing-number heuristic (which the year
// would otherwise trip).
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
		// A Movies-category search is a film (single release), always answered. It
		// must not fall through to the anime episode-skip below: a movie query
		// ends in its year (e.g. "From Up on Poppy Hill 2011"), which the
		// trailingEpisode regex would otherwise misread as a per-episode number.
		if requestsMovies(q.Get("cat")) {
			return true
		}
		return !trailingEpisode.MatchString(strings.TrimSpace(q.Get("q")))
	}
}

// requestsMovies reports whether the Torznab category list targets Movies
// (2000-2999) - a film search, which is a single release and always answered.
func requestsMovies(cat string) bool {
	for c := range parseCats(cat) {
		if c >= catMovies && c < catMovies+1000 {
			return true
		}
	}
	return false
}

// trailingEpisode matches the absolute episode number Sonarr appends to an anime
// title query (a space then a 2-4 digit number, e.g. "Frieren 01"), which marks a
// per-episode search the feed does not answer on the basic-search (t=search) path.
// NOTE: this regex cannot tell an appended episode from a title that itself ends in
// a 2-4 digit number, so "Mob Psycho 100" also matches and is skipped on the
// t=search path (a 1-digit tail like "Steins;Gate 0" does NOT match). That is safe
// for the whole-season grab: Sonarr issues the season search as t=tvsearch (the
// tvsearch case above, always answered), which delivers the pack; this heuristic
// only governs the basic-search fallback, where a per-episode barrage is the risk.
var trailingEpisode = regexp.MustCompile(`\s+\d{2,4}$`)

// filterByCats keeps items whose category is requested (an anime item satisfies
// a request for its TV parent). An empty request keeps everything; an item with
// no categories is kept (Prowlarr already applied the forwarded cat filter).
func filterByCats(items []item, cats map[int]bool) []item {
	if len(cats) == 0 {
		return items
	}
	out := make([]item, 0, len(items))
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
