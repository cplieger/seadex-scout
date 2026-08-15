package indexer

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cplieger/keyenc"
)

const (
	// maxItems caps a rendered feed as a safety bound. It evicts from the RENDERED
	// view only: the persisted journal is bounded by age alone, and Torznab paging
	// keeps every journaled item reachable across pages.
	maxItems = 1000
	// defaultCapsLimit is the default result count advertised in t=caps.
	defaultCapsLimit = 100
)

// curation is the set of SeaDex-tracked releases, keyed by info hash and by
// tracker key, each mapping to whether SeaDex marks that release best. byPair
// records which hash/key combinations were observed on the SAME SeaDex torrent, so
// lookup can prove an item's two identity signals name one release. A nil byPair
// is a legacy snapshot; lookup then FAILS CLOSED for items carrying both signals
// while single-signal matching keeps working.
type curation struct {
	byHash map[string]bool
	byKey  map[string]bool
	byPair map[string]bool
}

// pairKey joins a validated info hash and a tracker key into the byPair relation
// key. keyenc.Join is the app's ONE home for a composite key no field's content
// can forge, and escaping is element-wise, so two distinct hash/key pairs cannot
// collide whatever either component carries - where the old bare-'|' join was
// sound only while both producers' alphabets held. The relation is derived in
// memory on every load and never persisted, so the encoding is free to change.
func pairKey(hash, key string) string { return keyenc.Join(hash, key) }

// curationMatch accumulates the best/alt agreement state across an item's identity
// signals: accept admits a signal only when it resolves to a curated entry that
// agrees with every previously accepted one. Bookkeeping only; lookup owns policy.
type curationMatch struct {
	isBest  bool
	matched bool
}

// accept records one identity signal's curation result, reporting whether the
// signal keeps the item alive: a signal that missed the curation set or
// contradicts an earlier signal's best/alt value rejects it.
func (m *curationMatch) accept(candidate, ok bool) bool {
	if !ok || (m.matched && candidate != m.isBest) {
		return false
	}
	m.isBest, m.matched = candidate, true
	return true
}

// lookup reports whether a release (by its info hash and page URLs) is SeaDex-
// curated, and if so whether it is the best release. Every identity signal the item
// carries that the curation set KNOWS must agree with the others on the best/alt
// value. An item carrying BOTH a curated hash and a curated tracker key must
// additionally prove the exact pair was observed on a single SeaDex torrent
// (byPair): agreement alone would still admit torrent A's hash cross-wired with
// torrent B's key whenever both are best. scope binds tracker identity, so a
// swapped upstream cannot pass /ab an accepted Nyaa key.
func (c *curation) lookup(scope, hash, infoURL, guid string) (isBest, matched, conflict bool) {
	var match curationMatch

	// curatedHash is the hash only once the set has vouched for it; an unknown hash
	// leaves it empty so the pair relation below has no phantom signal to prove.
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

// acceptsObservedPair applies lookup's dual-signal relation check: an item carrying
// BOTH a curated info hash and a curated scoped tracker key must prove the exact
// pair was observed on a single SeaDex torrent. With either signal absent there is
// no pair to prove. A nil byPair (a legacy snapshot an upgraded resident server is
// still serving) fails closed too: absence of the relation is not permission to
// fall back to the weaker per-signal checks. Single-signal legacy matching is
// unaffected, and the next cycle's rewrite restores dual-signal matching.
func (c *curation) acceptsObservedPair(hash, key string) bool {
	if hash == "" || key == "" {
		return true
	}
	return c.byPair != nil && c.byPair[pairKey(hash, key)]
}

// acceptScopedKeys applies lookup's tracker-key arm: every tracker key parsed from
// the given page URLs must belong to scope, must agree with every other parsed key
// on the SAME release identity (healthy Prowlarr emits the same tracker id in
// comments and guid, so two URLs naming different curated torrents are an invalid
// response and fail closed), and must pass m.accept. It reports the resolved scoped
// key, whether the item survives, and whether the rejection was a STRUCTURAL one
// the request line must count as an identity conflict on its own evidence.
func (c *curation) acceptScopedKeys(scope string, urls []string, m *curationMatch) (key string, ok, conflict bool) {
	var identity string
	for _, raw := range urls {
		k := trackerKeyFromURL(raw)
		if k == "" {
			continue
		}
		if scopeOfKey(k) != scope {
			// A key naming ANOTHER tracker is an untrusted-response shape, not an
			// uncurated release, and must be reported as one WITHOUT depending on a
			// curated hash having been accepted first: the likeliest producer is an
			// upstream wired to the wrong Prowlarr indexer, where every result is out
			// of scope - which used to read as a clean no-match on every search.
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

// torznabFault is the one way query tells serve a request could not be answered
// with a feed. It carries exactly the three arguments rejectTorznab needs, so an
// outcome that forgets to build one cannot degrade into the false-empty 200 a
// zero-valued flag would produce (an arr records that as a clean no-match).
type torznabFault struct {
	summary string
	detail  string
	code    int
}

// snapshotUnavailableFault is the one fault for "no snapshot to serve from", raised
// both while the startup warm load is still running and after a load fault before
// any successful install. Single-homed so the two conditions cannot drift into two
// wire messages, which is why the detail names BOTH states.
func snapshotUnavailableFault() *torznabFault {
	return &torznabFault{
		summary: "feed snapshot unavailable",
		code:    errCodeUnknown,
		detail:  "feed snapshot unavailable: the persisted SeaDex feed has not finished loading, or failed to load; results unavailable until a snapshot loads",
	}
}

// queryStats summarizes one request for the per-request log line: whether the feed
// answered it, whether it was served from the synthesized RSS feed rather than a
// proxied search, how many upstream results survived the download-URL origin
// filter, and how many items survived curation or synthesis (counted before the
// category filter and paging). Observability only: an unanswerable request travels
// as a torznabFault, not as a field here.
type queryStats struct {
	answered bool
	feed     bool
	// upstreamFetched is the RAW parsed-item count of the upstream page, BEFORE
	// filterDownloadURLs' origin gate; upstream is the post-gate survivor count. A
	// gap between them is that filter dropping items, otherwise invisible.
	upstreamFetched int
	upstream        int
	curated         int
	// identityConflicts counts search results dropped because a curated identity
	// signal was CONTRADICTED by another signal on the same item, as opposed to the
	// ordinary not-curated drop. Without it a tampered or misbehaving upstream reads
	// exactly like a clean no-match.
	identityConflicts int
}

// query returns the feed items for a request (restricted to scope's tracker), a
// queryStats summary, and a non-nil torznabFault when the request could not be
// answered with a feed at all.
func (ix *Indexer) query(ctx context.Context, q url.Values, scope string) ([]item, queryStats, *torznabFault) {
	if !servesQuery(q) {
		return nil, queryStats{}, nil
	}
	// A disabled tracker has NO feed to read and NO upstream to search, whatever the
	// snapshot's state, so neither off-switch response may be gated by snapshot
	// state: answering the snapshot-unavailable fault would fail a deliberately-off
	// tracker on an unrelated local fault - the Prowlarr save-test for the RSS leg,
	// and for the other every search the arr still sends, where an <error> counts
	// toward disabling this indexer, RSS included.
	enabled := ix.enablement.enabled(scope)
	if isFeedRequest(q) && !enabled {
		return nil, queryStats{answered: true, feed: true}, nil
	}
	// Nothing here LOADS. The served snapshot is installed off the request path, so a
	// request is one atomic read of whatever is current: no syscall, no gate, no
	// wait. That is why a wedged /config mount can no longer strand a handler.
	if enabled && ix.cache.unavailable() {
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
			// A total upstream failure is reported as a Torznab <error>, not an empty
			// 200 feed: an empty feed reads as a clean no-match, which would record a
			// Prowlarr outage as a successful search. A partial failure keeps the
			// degraded-but-successful feed.
			fault = &torznabFault{
				summary: "upstream query failed",
				code:    errCodeUnknown,
				detail:  "upstream Prowlarr query failed; search results unavailable",
			}
		}
	}

	if stats.feed {
		// The category filter applies to the SYNTHESIZED feed only: those items carry
		// the app's own Fribb-typed vocabulary, so the client's cat list is meaningful
		// against them. Proxied results carry the TRACKER's categories and cat was
		// already forwarded upstream, so re-filtering would empty every Movies search.
		items = filterByCats(items, parseCats(q.Get("cat")))
		items = applyPaging(ix.log, items, q)
	}
	if len(items) > maxItems {
		// The rendered view is capped; say so, so a short feed is never mistaken for a
		// short catalogue.
		ix.log.Warn("feed trimmed to the rendered-item cap",
			"available", len(items), "max_items", maxItems)
		items = items[:maxItems]
	}
	return items, stats, fault
}

// isFeedRequest reports whether a request is the empty-query periodic RSS check
// served from the synthesized journal rather than a proxied search. The ONE home of
// that reading: query dispatches on it and rejectMissingABPasskey selects the same
// requests through it, so the passkey error covers exactly those requests.
func isFeedRequest(q url.Values) bool { return strings.TrimSpace(q.Get("q")) == "" }

// applyPaging honors the Torznab offset/limit params (advertised in t=caps) on the
// synthesized feed. A request without a usable limit gets the advertised default,
// newest-first, so the caps document is honest; the arrs always send an explicit
// limit. An absent or invalid offset leaves the window anchored at the newest item,
// and the proxied search path pages at the UPSTREAM instead. A present-but-unusable
// value is logged at Debug so a misconfigured client is diagnosable.
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
		// present-but-unusable value is named here: the window it asked for was
		// discarded and the response comes from the newest page instead.
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

// feedFor returns the synthesized RSS feed for a tracker scope, read through the
// snapshot cache, which owns the locking. A scope whose Prowlarr Torznab URL is not
// configured serves nothing, even when the loaded snapshot carries items for it:
// an empty per-tracker URL is that tracker's documented off switch, and the /ab
// feed embeds the operator's passkey, so an off tracker's response must be the same
// shape as a tracker with no data. The returned slice is safe to use after the read
// returns - reload installs fresh backing arrays and never mutates the old ones -
// but callers must only read it.
func (ix *Indexer) feedFor(scope string) []item {
	// The enablement gate is the SERVER's, not the cache's: whether a tracker's feed
	// may be served at all is config policy, while the cache only answers what is
	// loaded.
	if !ix.enablement.enabled(scope) {
		return nil
	}
	feed := ix.cache.feed(scope)
	// The serve boundary speaks the WIRE vocabulary only: strip the journal
	// bookkeeping by projecting each record onto its embedded item, so the render
	// path cannot depend on persisted-only fields.
	items := make([]item, len(feed))
	for i := range feed {
		items[i] = feed[i].item
	}
	return items
}

// fetchRaw queries the scope's upstream and returns the raw results before any
// curation filtering, the RAW parsed-item count of the upstream page (counted
// BEFORE the download-URL origin filter, so a gap is that filter dropping items),
// plus whether the query was a total upstream failure. On failed=true query builds
// a torznabFault so serve renders a Torznab <error> instead of a fake-empty 200
// feed. Returns nil,0,false when no upstream is configured for the scope (a
// standing misconfiguration) or when the caller cancelled the request.
func (ix *Indexer) fetchRaw(ctx context.Context, params url.Values, scope string) (items []item, fetched int, failed bool) {
	// upstreams is wired once in New, before any request can arrive, and never
	// mutated, so it needs no synchronization; the snapshot lives behind its cache.
	u := upstreamForScope(ix.upstreams, scope)
	if u == nil {
		// A search reached a scope whose Prowlarr upstream is not configured: the
		// empty result is a permanent misconfiguration, not a no-match, so say so -
		// once. The state cannot change while the process runs, so repeats drop to
		// Debug (see noUpstreamWarned).
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
			// Caller (the arr) went away or its request deadline fired; not an upstream
			// fault - a Prowlarr client timeout leaves ctx.Err() nil and should warn.
			ix.log.Debug("upstream query abandoned by the caller; returning empty",
				"upstream", u.name, "scope", scope)
			return nil, 0, false
		}
		// The credentials class is ERROR here for the same reason as on the harvest
		// path: it cannot clear without the operator, and this is the site whose
		// consequence escalates - every rejected search answers a Torznab <error>,
		// which counts toward the arr disabling this indexer, RSS included.
		if permanentUpstreamCredentialError(err) {
			ix.log.Error("upstream rejected the credentials; searches will keep failing until an operator fixes it, "+
				"and an arr counts these failures toward disabling this indexer (RSS included) - "+
				"check indexer.prowlarr_api_key and the per-tracker Torznab URL",
				"upstream", u.name, "error", err)
			return nil, 0, true
		}
		ix.log.Warn("upstream query failed", "upstream", u.name, "error", err)
		return nil, 0, true
	}
	return items, fetched, false
}

// markAndDedupe keeps the curated releases, stamps each with the best/alt marker,
// and drops intra-upstream duplicates by guid (a torrent listed under several title
// aliases carries distinct guids and is deliberately kept). It also reports how many
// items were dropped by an identity CONTRADICTION rather than by not being curated,
// so that class is visible in the per-request line instead of reading as no-match.
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

// upstreamParams selects the Torznab query params to forward to Prowlarr, dropping
// our own apikey. It defaults the search type to a basic search and always asks the
// upstream for the FULL window the decoder accepts (maxItems).
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

// upstreamForScope returns the upstream a scope targets, or nil when no configured
// upstream matches. Scope is always a specific tracker here and New wires at most
// one upstream per name, so a single match is the only case.
func upstreamForScope(all []*upstream, scope string) *upstream {
	for _, u := range all {
		if u.name == scope {
			return u
		}
	}
	return nil
}

// servesQuery reports whether the feed answers a request by querying the trackers, or
// returns empty without contacting them.
func servesQuery(q url.Values) bool {
	switch strings.ToLower(strings.TrimSpace(q.Get("t"))) {
	case "movie", "movie-search", "moviesearch":
		return true
	case "tvsearch", "tv-search":
		// Season 0 is Sonarr's specials bucket: specials are single releases, so a
		// season-0 per-episode search is always answered rather than skipped.
		return strings.TrimSpace(q.Get("ep")) == "" || strings.TrimSpace(q.Get("season")) == "0"
	default: // "search", "", specials, generic, RSS
		// A Movies-category search is a film (single release), always answered. It must
		// not fall through to the episode-skip below: a movie query ends in its year,
		// which trailingEpisode would misread as a per-episode number.
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
// title query (a space then a 2-4 digit number), which marks a per-episode search
// the feed does not answer on the basic-search path. NOTE: it cannot tell an
// appended episode from a title that itself ends in a 2-4 digit number, so "Mob
// Psycho 100" is also skipped there. That is safe for the whole-season grab, which
// arrives as t=tvsearch and is always answered.
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

// categoryMatch reports whether an item's categories satisfy the requested set: an
// item category matches when requested exactly or by its Torznab parent category
// (the multiple-of-1000 floor, e.g. anime 5070's parent is TV 5000).
func categoryMatch(itemCats []int, want map[int]bool) bool {
	if len(itemCats) == 0 {
		return true
	}
	for _, c := range itemCats {
		// The parent leg needs no domain guard: parseCats admits only positive ids, so
		// want[0] is always false and the ids a `c >= 1000` guard would exclude are
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
