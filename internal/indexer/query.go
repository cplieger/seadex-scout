package indexer

import (
	"context"
	"errors"
	"log/slog"
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

// --- Curation matching ---

// curation is the set of SeaDex-tracked releases, keyed by info hash and by
// tracker key, each mapping to whether SeaDex marks that release best. byPair
// records which hash/key combinations were observed on the SAME SeaDex
// torrent (keyed by pairKey), so lookup can prove an item's two identity
// signals name one release rather than two same-marker ones. A nil byPair is
// a legacy snapshot persisted before the pair relation existed; lookup then
// FAILS CLOSED for items carrying both signals (the relation cannot be
// proven) while single-signal matching keeps working, until the next cycle
// rewrites the snapshot with the relation.
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
// curated, and if so whether it is the best release. Every identity signal the
// item carries that the curation set KNOWS must agree with the others on the
// best/alt value; a signal that contradicts an earlier one rejects the whole
// item. An item carrying BOTH a curated hash and a curated tracker key must
// additionally prove the exact pair was observed on a single SeaDex torrent
// (byPair): best/alt agreement alone would still admit torrent A's hash
// cross-wired with torrent B's key whenever both happen to be best (or both
// alt). Together these keep an untrusted Torznab item from pairing a curated
// info hash with the page URL or download link of a different torrent. scope
// binds tracker identity: a tracker key parsed from the item's URLs must belong
// to the endpoint being served, so a swapped upstream (or a cross-tracker item)
// cannot pass /ab an accepted Nyaa key or vice versa.
//
// An info hash the curation set does not know is NOT a contradiction and does
// not veto the item: SeaDex records often carry no usable info hash (empty,
// short, or non-hex - the shape validInfoHash rejects, and the shape every AB
// record has), so the ownership fact registers only their tracker key, while
// Prowlarr's Nyaa results always carry the real hash. Reading that miss as
// "this hash names an uncurated release" vetoed an identity the curated page
// URL had already proven, and the curated release was invisible to the search
// with no diagnostic. Corroboration is what the hash is for: it can agree or
// disagree, but it cannot veto (the same reading the writer's title harvest
// settled for its own identity match). A hash the set DOES know still has to
// agree, and still has to prove co-membership with any curated key beside it.
//
// conflict reports the rejection kind for the caller's accounting: true when a
// curated signal was refused by a later identity check (the untrusted-response
// shapes above), false for an item that simply carries nothing SeaDex curates -
// which is the overwhelming majority of a proxied search's results and is not
// worth reporting.
func (c *curation) lookup(scope, hash, infoURL, guid string) (isBest, matched, conflict bool) {
	var match curationMatch

	// curatedHash is the hash only once the set has vouched for it; an unknown
	// hash leaves it empty so the pair relation below has no phantom signal to
	// prove.
	var curatedHash string
	if h := validInfoHash(hash); h != "" {
		if b, ok := c.byHash[h]; ok {
			if !match.accept(b, true) {
				return false, false, match.matched
			}
			curatedHash = h
		}
	}
	key, ok, keyConflict := c.acceptScopedKeys(scope, []string{infoURL, guid}, &match)
	if !ok {
		return false, false, match.matched || keyConflict
	}
	// AnimeBytes exposes no info hash in Torznab, so a scoped tracker key is
	// mandatory there; Nyaa may still match a hash-only item.
	if scope == upstreamAB && key == "" {
		return false, false, match.matched
	}
	// Both signals present and individually curated: the persisted pair
	// relation must prove they belong to one release.
	if !c.acceptsObservedPair(curatedHash, key) {
		return false, false, match.matched
	}
	return match.isBest, match.matched, false
}

// acceptsObservedPair applies lookup's dual-signal relation check: an item
// carrying BOTH a curated info hash and a curated scoped tracker key must
// additionally prove the exact pair was observed on a single SeaDex torrent.
// With either signal absent there is no pair to prove, so it accepts.
//
// A nil byPair (a legacy snapshot written before the relation was persisted -
// an upgraded resident server still serving the old file) fails closed too:
// absence of the relation that proves co-membership is not permission to fall
// back to the weaker per-signal checks, which would admit torrent A's hash
// cross-wired with torrent B's key whenever both share a best/alt bit.
// Single-signal legacy matching (hash-only Nyaa, key-only AB) is unaffected,
// and the next cycle's snapshot rewrite restores dual-signal matching.
func (c *curation) acceptsObservedPair(hash, key string) bool {
	if hash == "" || key == "" {
		return true
	}
	return c.byPair != nil && c.byPair[pairKey(hash, key)]
}

// acceptScopedKeys applies lookup's tracker-key arm: every tracker key parsed
// from the given page URLs must belong to scope (a key for a different tracker
// rejects the item outright, and is reported as an identity conflict), must
// agree with every other parsed key on the SAME release identity (healthy
// Prowlarr emits the same tracker id in comments and guid, so two URLs naming
// different curated torrents are an invalid untrusted response and fail closed
// - even when both ids happen to share a best/alt value), and must pass
// m.accept (curated, agreeing on best/alt). It reports the resolved scoped key
// (key - "" when the URLs carried none; lookup's AB rule and hash/key pair
// check need it), whether the item survives (ok), and whether the rejection was
// a STRUCTURAL one (conflict) the request line must count as an identity
// conflict on its own evidence rather than only when some earlier signal was
// already curated.
func (c *curation) acceptScopedKeys(scope string, urls []string, m *curationMatch) (key string, ok, conflict bool) {
	var identity string
	for _, raw := range urls {
		k := trackerKeyFromURL(raw)
		if k == "" {
			continue
		}
		if scopeOfKey(k) != scope {
			// A key naming ANOTHER tracker is an untrusted-response shape, not
			// an uncurated release, and it must be reported as one WITHOUT
			// depending on a curated hash having been accepted first: the
			// likeliest producer is an upstream Torznab URL wired to the wrong
			// Prowlarr indexer, where every result is out of scope and no other
			// signal is curated - so keying the conflict on m.matched made that
			// standing misconfiguration read as a clean no-match on every
			// search.
			return identity, false, true
		}
		if identity != "" && k != identity {
			return identity, false, true
		}
		identity = k
		b, curated := c.byKey[k]
		if !m.accept(b, curated) {
			return identity, false, false
		}
	}
	return identity, true, false
}

// --- Request dispatch and accounting ---

// torznabFault is the one way query tells serve a request could not be
// answered with a feed. It carries exactly the three arguments rejectTorznab
// needs, so serve renders any fault without knowing which condition produced
// it - and an outcome that forgets to build one cannot degrade into the
// false-empty 200 a zero-valued flag on the log record would produce (an arr
// records that as a clean no-match).
type torznabFault struct {
	summary string
	detail  string
	code    int
}

// snapshotUnavailableFault is the one fault for "no snapshot to serve from",
// raised both while the startup warm load is still running and after a load
// fault before any successful install. Single-homed so the two conditions cannot
// drift into two different wire messages, so the detail names BOTH states rather
// than asserting a failure the still-loading case has not had.
func snapshotUnavailableFault() *torznabFault {
	return &torznabFault{
		summary: "feed snapshot unavailable",
		code:    errCodeUnknown,
		detail:  "feed snapshot unavailable: the persisted SeaDex feed has not finished loading, or failed to load; results unavailable until a snapshot loads",
	}
}

// queryStats summarizes one request for the per-request log line: whether the
// feed answered it (answered), whether it was served from the synthesized RSS
// feed (feed - an empty-q periodic check) rather than a proxied search, how
// many upstream results survived the Prowlarr fetch's download-URL origin
// filter (search only), and how many items survived curation or synthesis
// (curated - counted before the category filter and paging trim the served
// view). Observability only: a request that cannot be answered with a feed
// travels as a torznabFault, not as a field here.
type queryStats struct {
	answered bool
	feed     bool
	// upstreamFetched is the RAW parsed-item count of the upstream page,
	// BEFORE filterDownloadURLs' origin gate; upstream is the post-gate
	// survivor count. A gap between them is the origin filter dropping items,
	// which is otherwise invisible after its once-per-onset WARN.
	upstreamFetched int
	upstream        int
	curated         int
	// identityConflicts counts search results dropped because a curated
	// identity signal was CONTRADICTED by another signal on the same item (a
	// cross-torrent hash/key pair, two different tracker ids, an out-of-scope
	// key), as opposed to the ordinary "not curated by SeaDex" drop that
	// accounts for nearly every filtered result. Without it a tampered or
	// misbehaving upstream reads exactly like a clean no-match.
	identityConflicts int
}

// query returns the feed items for a request (restricted to scope's tracker),
// a queryStats summary for logging, and a non-nil torznabFault when the
// request could not be answered with a feed at all.
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
func (ix *Indexer) query(ctx context.Context, q url.Values, scope string) ([]item, queryStats, *torznabFault) {
	if !servesQuery(q) {
		return nil, queryStats{}, nil
	}
	// Run's warm load owns the cache's reload gate until its filesystem
	// syscalls return, and a wedged /config mount has no bound of its own:
	// entering refresh here would park this request on that gate while it holds
	// a query/feed slot, since net/http's WriteTimeout cannot cancel a handler.
	// Answer the snapshot-unavailable fault immediately instead, so the arr sees
	// a fault it can back off from rather than a hung request (see
	// snapshotCache.warmPending).
	if ix.cache.warmPending() {
		return nil, queryStats{answered: true}, snapshotUnavailableFault()
	}
	// Pick up a newer feed snapshot a cycle may have written (this process's
	// daemon loop, or the `poll` subcommand in another process) before serving.
	ix.cache.refresh(ctx)
	// A snapshot that failed to load before any successful install is a local
	// fault, not an empty catalogue: serving the synthesized feed would blank
	// it, and a search would filter every Prowlarr result against nil
	// curation maps - both false-empty. Answer with a fault (serve renders a
	// Torznab <error>, exactly like an unavailable Prowlarr dependency)
	// without contacting a tracker.
	if ix.cache.unavailable() {
		return nil, queryStats{answered: true}, snapshotUnavailableFault()
	}

	var (
		items []item
		stats queryStats
		fault *torznabFault
	)
	if isFeedRequest(q) {
		items = ix.feedFor(scope)
		stats = queryStats{answered: true, feed: true, curated: len(items)}
	} else {
		raw, fetched, failed := ix.fetchRaw(ctx, upstreamParams(q), scope)
		set := ix.cache.curation()
		var conflicts int
		items, conflicts = markAndDedupe(raw, &set, scope)
		stats = queryStats{
			answered:        true,
			upstreamFetched: fetched, upstream: len(raw), curated: len(items),
			identityConflicts: conflicts,
		}
		if failed {
			// A total upstream failure (every queried Prowlarr upstream
			// failed) is reported as a Torznab <error>, not an empty 200
			// feed: an empty feed reads as a clean "no SeaDex match" to the
			// arr, which would silently record a Prowlarr outage as a
			// successful no-results search. A partial failure (one of several
			// upstreams answered) keeps the degraded-but-successful feed.
			fault = &torznabFault{
				summary: "upstream query failed",
				code:    errCodeUnknown,
				detail:  "upstream Prowlarr query failed; search results unavailable",
			}
		}
	}

	if stats.feed {
		// The category filter applies to the SYNTHESIZED feed only: those items
		// carry the app's own Fribb-typed vocabulary (categoriesFor - Movies for a
		// film, Anime otherwise), so the client's cat list is meaningful against
		// them. Proxied search results carry the TRACKER's categories instead -
		// both proxied trackers are anime trackers, so a film arrives as Anime
		// 5070 - and cat was already forwarded upstream (upstreamParams), so
		// re-filtering here would empty every Movies-category search.
		items = filterByCats(items, parseCats(q.Get("cat")))
		items = applyPaging(ix.log, items, q)
	}
	if len(items) > maxItems {
		// The rendered view is capped; say so, so a short feed is never
		// mistaken for a short catalogue (the render path WARNs on its own
		// byte-budget truncation for the same reason).
		ix.log.Warn("feed trimmed to the rendered-item cap",
			"available", len(items), "max_items", maxItems)
		items = items[:maxItems]
	}
	return items, stats, fault
}

// isFeedRequest reports whether a request is the empty-query periodic RSS check
// served from the synthesized journal rather than a proxied search - the
// condition query dispatches on. server.go's rejectMissingABPasskey applies the
// same emptiness test inline, earlier in the same request, so the two readings
// must agree: the AnimeBytes passkey error is rendered for exactly the requests
// this predicate selects.
func isFeedRequest(q url.Values) bool { return strings.TrimSpace(q.Get("q")) == "" }

// --- Serving the synthesized feed ---

// applyPaging honors the Torznab offset/limit params (advertised in t=caps)
// on the synthesized feed. A request without a usable limit gets the
// advertised default, defaultCapsLimit, newest-first (the feed is sorted
// newest-first), so the caps document is honest; the arrs always send an
// explicit limit, so real consumers are unaffected. An explicit limit behaves
// as before, an absent or invalid offset leaves the window anchored at the
// newest item, and the proxied search path pages at the UPSTREAM instead, so
// it never pages locally (it forwards offset to Prowlarr and always asks for
// the full decoder window - see upstreamParams). A present-but-unusable limit
// or offset is logged at Debug so a misconfigured client is diagnosable.
func applyPaging(log *slog.Logger, items []item, q url.Values) []item {
	rawOffset := strings.TrimSpace(q.Get("offset"))
	off, offErr := strconv.Atoi(rawOffset)
	switch {
	case offErr == nil && off > 0:
		if off >= len(items) {
			return nil
		}
		items = items[off:]
	case rawOffset != "" && (offErr != nil || off < 0):
		// An empty or numeric-zero offset IS the first page, so only a
		// present-but-unusable value (non-numeric, overflowing, or negative)
		// is named here: the window it asked for was discarded and the
		// response comes from the newest page instead, which the per-request
		// access line does not carry.
		log.Debug("unusable Torznab offset param; using the first page",
			"offset", logParam(rawOffset), "default", 0)
	}
	limit := defaultCapsLimit
	raw := strings.TrimSpace(q.Get("limit"))
	if lim, err := strconv.Atoi(raw); err == nil && lim > 0 {
		limit = lim
	} else if raw != "" {
		// A present-but-unusable limit silently becomes the advertised
		// default; name it so a misconfigured client is diagnosable.
		log.Debug("unusable Torznab limit param; using the advertised default",
			"limit", logParam(raw), "default", defaultCapsLimit)
	}
	if limit < len(items) {
		items = items[:limit]
	}
	return items
}

// feedFor returns the synthesized RSS feed for a tracker scope (nyaa or ab),
// read through the snapshot cache, which owns the locking (a cycle rewrite
// replaces the snapshot under it). A scope whose Prowlarr Torznab URL is not
// configured serves nothing, even when the loaded snapshot carries items for it
// (a stale snapshot written before the operator turned the tracker off): the
// README documents an empty per-tracker URL as that tracker's off switch, and
// the /ab feed embeds the
// operator's passkey, so an off tracker's empty-q response must be the same
// shape as a tracker with no data - never the credential-bearing feed. The
// returned slice is safe to use after the cache's read returns: reload installs
// a fresh snapshot with new backing arrays and never mutates the old ones, so a
// slice handed out here stays immutable even across a swap. Callers must only
// read it (never append/write in place).
func (ix *Indexer) feedFor(scope string) []item {
	// The enablement gate is the SERVER's, not the cache's: whether a tracker's
	// feed may be served at all is config policy, while the cache only answers
	// what is loaded. An unknown scope is not enabled, so it returns nil here.
	if !ix.enablement.enabled(scope) {
		return nil
	}
	feed := ix.cache.feed(scope)
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

// --- Proxied upstream search ---

// fetchRaw queries the scope's upstream and returns the raw results, before
// any curation filtering, the RAW parsed-item count of the upstream page
// (fetched - counted BEFORE the download-URL origin filter, so a gap between
// it and len(items) is that filter dropping items), plus whether the query was
// a total upstream failure
// (every queried upstream failed - with per-tracker scoping that is the one
// upstream the scope names). On failed=true query builds a torznabFault so
// serve renders a Torznab <error>
// instead of a fake-empty 200 feed, so a Prowlarr outage surfaces as a failed
// search in the arr rather than a clean no-results one. Returns nil,0,false when
// no upstream is configured for the scope (a standing misconfiguration, not a
// query failure) or when the caller cancelled the request.
func (ix *Indexer) fetchRaw(ctx context.Context, params url.Values, scope string) (items []item, fetched int, failed bool) {
	// upstreams is wired once in New, before any request can arrive, and is
	// never mutated afterwards, so it needs no synchronization; the snapshot
	// fields live behind snapshotCache's own lock.
	u := upstreamForScope(ix.upstreams, scope)
	if u == nil {
		// A search reached a scope whose Prowlarr upstream is not configured
		// (e.g. an /ab search with only nyaa_torznab_url set): the empty result
		// is a permanent misconfiguration, not a no-match, so say so - once.
		// The state cannot change while the process runs, and an arr left
		// pointing at a turned-off tracker searches a season per series, so
		// repeats drop to Debug (see noUpstreamWarned).
		log := ix.log.Debug
		if w, ok := ix.noUpstreamWarned[scope]; ok && w.CompareAndSwap(false, true) {
			log = ix.log.Warn
		}
		log("search for tracker scope with no configured upstream; returning empty",
			"scope", scope)
		return nil, 0, false
	}

	items, fetched, err := u.search(ctx, params)
	if err != nil {
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err())) {
			// Caller (the arr) went away or its request deadline fired; not an
			// upstream fault. A Prowlarr HTTP client timeout leaves ctx.Err()
			// nil and should warn.
			//
			// Say so at Debug: the empty-and-not-failed return is otherwise
			// indistinguishable from a genuine no-match in the request INFO
			// line (upstream_fetched=0 upstream=0 curated=0), and this is the
			// longest abandonment window of the three - its two siblings, the
			// gate wait and the response write, each already record one.
			ix.log.Debug("upstream query abandoned by the caller; returning empty",
				"upstream", u.name, "scope", scope)
			return nil, 0, false
		}
		ix.log.Warn("upstream query failed", "upstream", u.name, "error", err)
		return nil, 0, true
	}
	return items, fetched, false
}

// markAndDedupe keeps the curated releases, stamps each with the best/alt
// marker, and drops intra-upstream duplicates by guid (a torrent listed under
// several title aliases carries distinct guids and is deliberately kept). It
// also reports how many items were dropped by an identity CONTRADICTION rather
// than by simply not being curated (see lookup's conflict return), so that
// class - an untrusted Torznab response pairing a curated signal with a
// foreign one - is visible in the per-request line instead of reading as a
// clean no-match.
func markAndDedupe(raw []item, set *curation, scope string) (out []item, conflicts int) {
	seen := make(map[string]struct{}, len(raw))
	out = make([]item, 0, len(raw))
	for i := range raw {
		it := raw[i]
		isBest, matched, conflict := set.lookup(scope, it.InfoHash, it.InfoURL, it.GUID)
		if !matched {
			if conflict {
				conflicts++
			}
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
	return out, conflicts
}

// upstreamParams selects the Torznab query params to forward to Prowlarr,
// dropping our own apikey. It defaults the search type to a basic search and
// always asks the upstream for the FULL window the decoder accepts (maxItems).
//
// The client's own limit is deliberately NOT forwarded. A Torznab limit
// describes how many items the CLIENT wants back, and this endpoint filters
// the upstream page down to the SeaDex-curated releases locally - so
// forwarding it made the client's page size the upstream's truncation point
// and dropped every curated release sitting past it. Sonarr's season search
// arrives as limit=100 while a live AnimeBytes result set for one series runs
// to ~145 items: a curated torrent at upstream position 100+ was simply
// invisible, and the arr never paged for it (a handful of curated items looks
// like a last page for a limit of 100), so the release went missing with no
// diagnostic (h-f12).
//
// maxItems is the right window because it is what both ends of this app
// already agree on: the caps document advertises max=maxItems and
// parseTorznab rejects a response above maxUpstreamItems (== maxItems). At
// real Torznab item sizes a full page is ~1 MiB, well inside
// upstreamMaxBytes, so the fetch stays a single bounded attempt rather than
// the bounded-retry-then-Torznab-error an over-contract limit used to cause.
// offset is still forwarded verbatim: it names where in the upstream's own
// result list to start, which is not something curation reinterprets.
func upstreamParams(q url.Values) url.Values {
	out := url.Values{}
	for _, k := range []string{"t", "q", "cat", "season", "ep", "offset"} {
		if v := q.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	if out.Get("t") == "" {
		out.Set("t", "search")
	}
	out.Set("limit", strconv.Itoa(maxItems))
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

// --- Query admission ---

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

// --- Category filtering ---

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
		// The parent leg needs no domain guard: parseCats admits only positive
		// ids, so want[0] is always false, and c - c%1000 is 0 for every c <= 0
		// and every 0 < c < 1000 - the ids a `c >= 1000` guard would exclude are
		// already refused by the lookup itself.
		if want[c] || want[c-c%1000] {
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
