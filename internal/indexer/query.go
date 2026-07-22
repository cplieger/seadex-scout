package indexer

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	// maxItems caps a rendered feed as a safety bound. It evicts from the
	// RENDERED view only: the persisted journal is bounded by age alone
	// (feedJournalMaxAge, journal.go), and Torznab paging (applyPaging)
	// keeps every journaled item reachable across pages.
	maxItems = 1000
	// defaultCapsLimit is the default result count advertised in t=caps.
	defaultCapsLimit = 100
)

// curation is the set of SeaDex-tracked releases, keyed by info hash and by
// tracker key, each mapping to whether SeaDex marks that release best. byPair
// records which hash/key combinations were observed on the SAME SeaDex
// torrent (keyed by pairKey), so lookup can prove an item's two identity
// signals name one release rather than two same-marker ones. A nil byPair is
// a legacy snapshot persisted before the pair relation existed; lookup then
// falls back to the per-signal agreement gate until the next cycle rewrites
// the snapshot.
type curation struct {
	byHash map[string]bool
	byKey  map[string]bool
	byPair map[string]bool
}

// pairKey joins a validated info hash and a tracker key into the byPair
// relation key. The "|" separator appears in neither component (the hash is a
// 40-char hex run, the key is "<scope>:<digits>"), so two distinct hash/key
// pairs can never collide onto one relation key.
func pairKey(hash, key string) string { return hash + "|" + key }

// curationMatch accumulates the best/alt agreement state across an item's
// identity signals: accept admits a signal only when it resolves to a curated
// entry (ok) that agrees with every previously accepted signal on the
// best/alt value. Bookkeeping only; lookup owns the ordered policy.
type curationMatch struct {
	isBest  bool
	matched bool
}

// accept records one identity signal's curation result, reporting whether the
// signal keeps the item alive: a signal that missed the curation set (!ok) or
// contradicts an earlier signal's best/alt value rejects it.
func (m *curationMatch) accept(candidate, ok bool) bool {
	if !ok || (m.matched && candidate != m.isBest) {
		return false
	}
	m.isBest, m.matched = candidate, true
	return true
}

// lookup reports whether a release (by its info hash and page URLs) is SeaDex-
// curated, and if so whether it is the best release. Every structurally valid
// identity signal the item carries must resolve to curated entries agreeing on
// the best/alt value; a signal that misses the curation set, or one that
// contradicts an earlier signal, rejects the whole item. An item carrying BOTH
// a curated hash and a curated tracker key must additionally prove the exact
// pair was observed on a single SeaDex torrent (byPair): best/alt agreement
// alone would still admit torrent A's hash cross-wired with torrent B's key
// whenever both happen to be best (or both alt). Together these keep an
// untrusted Torznab item from pairing a curated info hash with the page URL or
// download link of a different torrent. scope binds tracker
// identity: a tracker key parsed from the item's URLs must belong to the
// endpoint being served, so a swapped upstream (or a cross-tracker item) cannot
// pass /ab an accepted Nyaa key or vice versa.
func (c *curation) lookup(scope, hash, infoURL, guid string) (isBest, matched bool) {
	var match curationMatch

	h := validInfoHash(hash)
	if h != "" {
		b, ok := c.byHash[h]
		if !match.accept(b, ok) {
			return false, false
		}
	}
	key, ok := c.acceptScopedKeys(scope, []string{infoURL, guid}, match.accept)
	if !ok {
		return false, false
	}
	// AnimeBytes exposes no info hash in Torznab, so a scoped tracker key is
	// mandatory there; Nyaa may still match a hash-only item.
	if scope == upstreamAB && key == "" {
		return false, false
	}
	// Both signals present and individually curated: require the persisted
	// pair relation to prove they belong to one release. A nil byPair is a
	// legacy snapshot written before the relation was persisted (an upgraded
	// resident server still serving the old file); the per-signal checks
	// above remain the gate until the next cycle rewrites the snapshot.
	if h != "" && key != "" && c.byPair != nil && !c.byPair[pairKey(h, key)] {
		return false, false
	}
	return match.isBest, match.matched
}

// acceptScopedKeys applies lookup's tracker-key arm: every tracker key parsed
// from the given page URLs must belong to scope (a key for a different
// tracker rejects the item outright), must agree with every other parsed key
// on the SAME release identity (healthy Prowlarr emits the same tracker id in
// comments and guid, so two URLs naming different curated torrents are an
// invalid untrusted response and fail closed - even when both ids happen to
// share a best/alt value), and must pass accept (curated, agreeing on
// best/alt). It reports the resolved scoped key (key - "" when the URLs
// carried none; lookup's AB rule and hash/key pair check need it) and whether
// the item survives (ok).
func (c *curation) acceptScopedKeys(scope string, urls []string, accept func(candidate, ok bool) bool) (key string, ok bool) {
	var identity string
	for _, raw := range urls {
		k := trackerKeyFromURL(raw)
		if k == "" {
			continue
		}
		if scopeOfKey(k) != scope {
			return identity, false
		}
		if identity != "" && k != identity {
			return identity, false
		}
		identity = k
		b, curated := c.byKey[k]
		if !accept(b, curated) {
			return identity, false
		}
	}
	return identity, true
}

// queryStats summarizes one request for the per-request log line: whether the
// feed answered it (answered), whether it was served from the synthesized RSS
// feed (feed - an empty-q periodic check) rather than a proxied search,
// whether the persisted feed snapshot failed to load before any snapshot was
// installed (snapshotUnavailable - serve renders a Torznab <error> then,
// since a false-empty feed would record the local fault as a clean no-match),
// whether the search's queried upstream(s) ALL failed (upstreamFailed - serve
// renders a Torznab <error> instead of an empty feed then), how many upstream
// results survived the Prowlarr fetch's download-URL origin filter (search
// only), and how many items were returned after curation or synthesis
// (curated).
type queryStats struct {
	answered            bool
	feed                bool
	snapshotUnavailable bool
	upstreamFailed      bool
	upstream            int
	curated             int
}

// query returns the feed items for a request (restricted to scope's tracker)
// plus a queryStats summary for logging.
//
// An empty-q request (Prowlarr's caps/save test, or an RSS "latest" fetch) is
// served from the synthesized per-tracker SeaDex journal - the releases newly
// curated within the journal window, rendered as grabbable items - without
// contacting a tracker. This is the periodic new-release check: the arr parses
// each synthesized title and grabs what matches its library.
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
	// A snapshot that failed to load before any successful install is a local
	// fault, not an empty catalogue: serving the synthesized feed would blank
	// it, and a search would filter every Prowlarr result against nil
	// curation maps - both false-empty. Answer with the dedicated flag (serve
	// renders a Torznab <error>) without contacting a tracker.
	if ix.snapshotUnavailable() {
		return nil, queryStats{answered: true, snapshotUnavailable: true}
	}

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
		set := curation{byHash: ix.snap.ByHash, byKey: ix.snap.ByKey, byPair: ix.snap.ByPair}
		ix.mu.RUnlock()
		items = markAndDedupe(raw, &set, scope)
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

// snapshotUnavailable reports whether the startup snapshot-unavailable state
// (see the snapFailed field) is active, emitting its once-per-onset WARN on
// the first report so the local fault is visible without a per-request log
// storm. The state is set/cleared by reload's load paths; requests only read
// it here.
func (ix *Indexer) snapshotUnavailable() bool {
	ix.mu.RLock()
	failed, warned := ix.snapFailed, ix.snapFailedWarned
	ix.mu.RUnlock()
	if !failed {
		return false
	}
	if !warned {
		ix.mu.Lock()
		// Re-check under the write lock: concurrent requests racing the onset
		// must still emit the WARN exactly once, and an install that cleared
		// snapFailed in between must not re-arm a stale warning.
		if ix.snapFailed && !ix.snapFailedWarned {
			ix.snapFailedWarned = true
			ix.log.Warn("indexer feed snapshot unavailable; answering Torznab requests with an error until a snapshot loads",
				"path", ix.path)
		}
		ix.mu.Unlock()
	}
	return true
}

// applyPaging honors the Torznab offset/limit params (advertised in t=caps)
// on the synthesized feed. A request without a usable limit gets the
// advertised default, defaultCapsLimit, newest-first (the feed is sorted
// newest-first), so the caps document is honest; the arrs always send an
// explicit limit, so real consumers are unaffected. An explicit limit behaves
// as before, an absent or invalid offset leaves the window anchored at the
// newest item, and the proxied search path forwards these params to Prowlarr
// instead, so it never pages locally.
func applyPaging(items []item, q url.Values) []item {
	if off, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil && off > 0 {
		if off >= len(items) {
			return nil
		}
		items = items[off:]
	}
	limit := defaultCapsLimit
	if lim, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && lim > 0 {
		limit = lim
	}
	if limit < len(items) {
		items = items[:limit]
	}
	return items
}

// feedFor returns the synthesized RSS feed for a tracker scope (nyaa or ab),
// read under the lock since reload replaces the snapshot when a cycle rewrites
// it. A scope whose Prowlarr Torznab URL is not configured serves nothing,
// even when the loaded snapshot carries items for it (a stale snapshot written
// before the operator turned the tracker off): the README documents an empty
// per-tracker URL as that tracker's off switch, and the /ab feed embeds the
// operator's passkey, so an off tracker's empty-q response must be the same
// shape as a tracker with no data - never the credential-bearing feed. The
// returned slice is safe to use after the lock is released: reload installs a
// fresh snapshot with new backing arrays and never mutates the old ones, so a
// slice handed out here stays immutable even across a swap. Callers must only
// read it (never append/write in place).
func (ix *Indexer) feedFor(scope string) []item {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var feed []journalItem
	switch scope {
	case upstreamNyaa:
		if ix.cfg.NyaaTorznabURL == "" {
			return nil
		}
		feed = ix.snap.NyaaFeed
	case upstreamAB:
		if ix.cfg.ABTorznabURL == "" {
			return nil
		}
		feed = ix.snap.ABFeed
	default:
		return nil
	}
	// The serve boundary speaks the WIRE vocabulary only: strip the journal
	// bookkeeping (never rendered) by projecting each record onto its
	// embedded item, so the render path cannot depend on persisted-only
	// fields.
	items := make([]item, len(feed))
	for i := range feed {
		items[i] = feed[i].item
	}
	return items
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

	items, _, err := u.search(ctx, params)
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
func markAndDedupe(raw []item, set *curation, scope string) []item {
	seen := make(map[string]struct{}, len(raw))
	out := make([]item, 0, len(raw))
	for i := range raw {
		it := raw[i]
		isBest, matched := set.lookup(scope, it.InfoHash, it.InfoURL, it.GUID)
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
		// Season 0 is Sonarr's specials bucket: specials are single releases
		// (never packs), so a season-0 per-episode search is always answered
		// rather than skipped like an ordinary season's episode barrage.
		return strings.TrimSpace(q.Get("ep")) == "" || strings.TrimSpace(q.Get("season")) == "0"
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

// categoryMatch reports whether an item's categories satisfy the requested
// set: an item category matches when requested exactly or by its Torznab
// parent category (the multiple-of-1000 floor, e.g. anime 5070's parent is TV
// 5000) - generalizing the previous anime->TV special case.
func categoryMatch(itemCats []int, want map[int]bool) bool {
	if len(itemCats) == 0 {
		return true
	}
	for _, c := range itemCats {
		if want[c] || (c >= 1000 && want[c-c%1000]) {
			return true
		}
	}
	return false
}

// parseCats parses a comma-separated torznab category list into a set.
func parseCats(s string) map[int]bool {
	out := make(map[int]bool)
	for part := range strings.SplitSeq(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out[n] = true
		}
	}
	return out
}
