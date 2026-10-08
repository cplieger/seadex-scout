package indexer

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// bootstrapGUID and bootstrapDownloadURL are the placeholder's identity. The URL
// must be absolute and in an <enclosure>: with no <link>, servarr's parser drops
// an item without one and resolves a relative value against the indexer's URL
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Indexers/Torznab/TorznabRssParser.cs#L145-L155).
const (
	bootstrapGUID        = "seadex-scout:bootstrap"
	bootstrapDownloadURL = "http://seadex-scout.invalid/bootstrap.torrent"
)

// curation is the set of SeaDex-tracked releases, keyed by info hash and by
// tracker key, each mapping to what every owner of that signal agreed on. byPair
// records which hash/key combinations were observed on the SAME SeaDex torrent, so
// lookup can prove an item's two identity signals name one release. A nil byPair
// is a legacy snapshot; lookup then FAILS CLOSED for items carrying both signals
// while single-signal matching keeps working. twins is the twin catalogue a
// special search is answered from, keyed by seriesQueryKey.
type curation struct {
	byHash map[string]curatedSignal
	byKey  map[string]curatedSignal
	byPair map[string]bool
	twins  map[string][]catalogueTwin
}

// curatedSignal is what one identity signal's owners agreed on, folded at build
// time: whether any of them marks the release best or is not a film, and the
// tvdb id and film twin title they agree on, each as a vote keeping
// contradiction apart. A vote must carry
// "contradicted" as its own state, because a bare zero conflates it with "no
// owner supplied one" and then reads as abstention at the cross-signal fold,
// where an agreeing sibling signal would override the contradiction.
type curatedSignal struct {
	twin    twinVote
	vote    tvdbVote
	isBest  bool
	nonFilm bool
}

// pairKey joins a validated info hash and a tracker key into the byPair relation
// key. keyenc.Join is the app's ONE home for a composite key no field's content
// can forge, and escaping is element-wise, so two distinct hash/key pairs cannot
// collide whatever either component carries, where a bare-'|' join would be sound
// only while both producers' alphabets held. The relation is derived in
// memory on every load and never persisted, so the encoding is free to change.
func pairKey(hash, key string) string { return keyenc.Join(hash, key) }

// curationMatch accumulates the agreement state across an item's identity
// signals: accept admits a signal only when it resolves to a curated entry that
// agrees with every previously accepted one, and folds that signal's tvdb and
// twin votes into the item's. Bookkeeping only; lookup owns policy.
type curationMatch struct {
	twin    twinVote
	vote    tvdbVote
	isBest  bool
	nonFilm bool
	matched bool
}

// accept records one identity signal's curation result, reporting whether the
// signal keeps the item alive: a signal that missed the curation set or
// contradicts an earlier signal's best/alt value rejects it.
//
// Both votes are MERGED, conflict bit included, so a signal whose own holders
// disagreed vetoes the attribute instead of abstaining beside an agreeing sibling.
func (m *curationMatch) accept(sig curatedSignal, ok bool) bool {
	if !ok || (m.matched && sig.isBest != m.isBest) {
		return false
	}
	m.isBest, m.matched = sig.isBest, true
	m.nonFilm = m.nonFilm || sig.nonFilm
	m.vote.merge(sig.vote)
	m.twin.merge(sig.twin)
	return true
}

// curationVerdict is lookup's answer for one search result: whether the release
// is curated at all, whether it is the best one, whether it was rejected by an
// identity CONTRADICTION rather than by not being curated, whether any holder is
// not a film, and the two facts every holder of every accepted signal agrees on -
// the TVDB id (0 when none supplied one or they disagreed) and the film twin
// title ("" likewise).
type curationVerdict struct {
	sonarrTitle string
	tvdbID      int
	isBest      bool
	nonFilm     bool
	matched     bool
	conflict    bool
}

// lookup reports whether a release (by its info hash and page URLs) is SeaDex-
// curated, as a curationVerdict. Every identity signal the curation set KNOWS
// must agree with the others on best/alt, and an item carrying BOTH a curated
// hash and a curated tracker key must also prove the pair was observed on one
// SeaDex torrent (byPair), or A's hash cross-wired with B's key would pass
// whenever both are best. A rejection yields the zero verdict plus the conflict
// flag it earned.
func (c *curation) lookup(scope, hash, infoURL, guid string) curationVerdict {
	var match curationMatch

	// curatedHash is the hash only once the set has vouched for it; an unknown hash
	// leaves it empty so the pair relation below has no phantom signal to prove.
	var curatedHash string
	if h := validInfoHash(hash); h != "" {
		if sig, ok := c.byHash[h]; ok {
			if !match.accept(sig, true) {
				return curationVerdict{conflict: match.matched}
			}
			curatedHash = h
		}
	}
	key, ok, keyConflict := c.acceptScopedKeys(scope, []string{infoURL, guid}, &match)
	if !ok {
		return curationVerdict{conflict: match.matched || keyConflict}
	}
	// AnimeBytes exposes no info hash in Torznab, so a scoped tracker key is
	// mandatory there; Nyaa may still match a hash-only item.
	if scope == upstreamAB && key == "" {
		return curationVerdict{conflict: match.matched}
	}
	// Both signals present and individually curated: the persisted pair
	// relation must prove they belong to one release.
	if !c.acceptsObservedPair(curatedHash, key) {
		return curationVerdict{conflict: match.matched}
	}
	return curationVerdict{
		isBest: match.isBest, matched: match.matched, nonFilm: match.nonFilm,
		tvdbID: match.vote.resolve(), sonarrTitle: match.twin.resolve(),
	}
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
		sig, curated := c.byKey[k]
		if !m.accept(sig, curated) {
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
// answered it, whether it came from the synthesized RSS feed, the upstream counts
// around the download-URL origin filter, the real items that survived curation or
// synthesis (before the category filter and paging), and whether the placeholder
// was served. An unanswerable request travels as a torznabFault, not as a field.
type queryStats struct {
	answered    bool
	feed        bool
	placeholder bool
	// upstreamFetched is the RAW parsed-item count of the upstream page, BEFORE
	// filterDownloadURLs' origin gate; upstream is the post-gate survivor count. A
	// gap between them is that filter dropping items, otherwise invisible.
	upstreamFetched int
	upstream        int
	curated         int
	catalogue       int
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
	if _, _, special := specialsRequest(q); !servesQuery(q) && !special {
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
	class := requesterOf(q)
	if isFeedRequest(q) {
		items = ix.feedFor(scope, class)
		stats = queryStats{answered: true, feed: true, curated: len(items)}
	} else {
		items, stats, fault = ix.search(ctx, q, scope, class, enabled)
	}

	if stats.feed {
		// The category filter applies to the SYNTHESIZED feed only: those items carry
		// the app's own mapping-typed vocabulary, so the client's cat list is meaningful
		// against them. Proxied results carry the TRACKER's categories and cat was
		// already forwarded upstream, so re-filtering would empty every Movies search.
		cats := parseCats(q.Get("cat"))
		items = filterByCats(items, cats)
		// The placeholder is decided AFTER the category filter and through it, so it
		// covers a scope whose journal holds only the other arr's category and still
		// reaches exactly the requests a real item could. Paging runs after it so a
		// second page never repeats it.
		substituted := len(items) == 0
		if substituted {
			items = filterByCats([]item{bootstrapItem()}, cats)
		}
		items = applyPaging(ix.log, items, q)
		stats.placeholder = substituted && len(items) > 0
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

func (ix *Indexer) search(ctx context.Context, q url.Values, scope string, class requester, enabled bool) ([]item, queryStats, *torznabFault) {
	var (
		raw     []item
		fetched int
		failed  bool
	)
	// A special search servesQuery skips is answered from the curation alone.
	if servesQuery(q) {
		raw, fetched, failed = ix.fetchRaw(ctx, upstreamParams(q), scope)
	}
	set := ix.cache.curation()
	items, conflicts := markAndDedupe(raw, &set, scope, class)
	curated, added := len(items), 0
	if enabled && class != requesterMovies && firstPage(q) {
		items, added = set.answerSpecial(q, scope, ix.enablement.ABPasskey, items)
	}
	stats := queryStats{
		answered:        true,
		upstreamFetched: fetched, upstream: len(raw), curated: curated,
		catalogue:         added,
		identityConflicts: conflicts,
	}
	if !failed || len(items) > 0 {
		return items, stats, nil
	}
	// A total upstream failure is reported as a Torznab <error>, not an empty 200
	// feed: an empty feed reads as a clean no-match, which would record a Prowlarr
	// outage as a successful search. A partial failure, or one the catalogue still
	// answered, keeps the degraded-but-successful feed.
	return items, stats, &torznabFault{
		summary: "upstream query failed",
		code:    errCodeUnknown,
		detail:  "upstream Prowlarr query failed; search results unavailable",
	}
}

// isFeedRequest reports whether a request is the empty-query periodic RSS check
// served from the synthesized journal rather than a proxied search. The ONE home of
// that reading: query dispatches on it and rejectMissingABPasskey selects the same
// requests through it, so the passkey error covers exactly those requests.
func isFeedRequest(q url.Values) bool { return strings.TrimSpace(q.Get("q")) == "" }

// bootstrapItem is an ungrabbable placeholder: the arrs' and Prowlarr's add/test
// fail a zero-item feed, and a fresh journal is empty by design. Grab safety is
// layered: no episode, season, year, id or marker; 0 seeders, which the
// minimum-seeders rule rejects; and a .invalid host (RFC 6761 section 6.4) whose
// failed fetch counts as an indexer failure, so it is only the last layer.
func bootstrapItem() item {
	return item{
		// Epoch keeps it older than any real item, so a re-arm adds no RSS gap.
		PubDate:     time.Unix(0, 0).UTC(),
		Title:       "seadex-scout online - no curated releases yet",
		GUID:        bootstrapGUID,
		DownloadURL: bootstrapDownloadURL,
		Categories:  []int{catAnime, catMovies},
	}
}

func (it *item) isPlaceholder() bool { return it.GUID == bootstrapGUID }

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
func (ix *Indexer) feedFor(scope string, class requester) []item {
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
	items := make([]item, 0, len(feed))
	for i := range feed {
		original, twin := class.twinsFor(feed[i].SonarrTitle != "", feed[i].NonFilm)
		if original {
			items = append(items, feed[i].item)
		}
		if twin {
			items = append(items, sonarrTwin(&feed[i].item))
		}
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

// markAndDedupe keeps the curated releases, stamps each with the best/alt marker
// and the TVDB id its owners agree on, drops intra-upstream duplicates by guid (a
// torrent listed under several title aliases carries distinct guids and is
// deliberately kept), and pairs a release whose owners agree on a twin title with
// its search twin, per requester (twinsFor). It also reports how many items were
// dropped by an identity CONTRADICTION rather than by not being curated, so that
// class is visible in the per-request line instead of reading as no-match.
func markAndDedupe(raw []item, set *curation, scope string, class requester) (out []item, conflicts int) {
	seen := make(map[string]struct{}, len(raw))
	out = make([]item, 0, len(raw))
	for i := range raw {
		it := raw[i]
		verdict := set.lookup(scope, it.InfoHash, it.InfoURL, it.GUID)
		if !verdict.matched {
			if verdict.conflict {
				conflicts++
			}
			continue
		}
		it.DownloadVolumeFactor = dvfAlt
		if verdict.isBest {
			it.DownloadVolumeFactor = dvfBest
		}
		// The id rides beside the marker: a proxied result is the only path that
		// reaches a curation older than the 14-day journal, which is exactly what a
		// Wanted -> Cutoff Unmet search walks.
		it.TvdbID = verdict.tvdbID
		id := it.guid()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		original, twin := class.twinsFor(verdict.sonarrTitle != "", verdict.nonFilm)
		if original {
			out = append(out, it)
		}
		if twin {
			out = append(out, searchTwin(&it, verdict.sonarrTitle))
		}
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

// firstPage reports whether a search asks for its first page, the only one
// answerSpecial serves: the curation has no pages, so a later one would repeat it.
func firstPage(q url.Values) bool {
	off, err := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	return err != nil || off <= 0
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
		return strings.TrimSpace(q.Get("ep")) == "" || isSpecialsSeason(q.Get("season"))
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

// isSpecialsSeason reports whether a season param names season 0, in any
// spelling: Sonarr and Prowlarr send it as "00"
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Indexers/Newznab/NewznabRequestGenerator.cs#L633).
func isSpecialsSeason(season string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(season))
	return err == nil && n == 0
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

type requester int

const (
	requesterAny requester = iota
	requesterTV
	requesterMovies
)

// requesterOf classifies a request by its categories, every one TV (5000-5999)
// or every one Movies (2000-2999), and only without any by its search type.
// The type cannot lead: Prowlarr rewrites an id-less TV or movie search to
// t=search, while the categories it forwards are the ones the arr synced
// (https://github.com/Prowlarr/Prowlarr/blob/3c6e1d97ac485a70cc9d4063dd5d17af7a77584d/src/NzbDrone.Core/Indexers/Definitions/Newznab/NewznabRequestGenerator.cs#L52).
// A malformed category token makes the list unknown, so it is attributed to
// neither.
func requesterOf(q url.Values) requester {
	named, tv, movies := false, true, true
	for part := range strings.SplitSeq(q.Get("cat"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		c, err := strconv.Atoi(part)
		if err != nil {
			return requesterAny
		}
		named = true
		tv = tv && c >= catTV && c < catTV+1000
		movies = movies && c >= catMovies && c < catMovies+1000
	}
	switch {
	case named && tv:
		return requesterTV
	case named && movies:
		return requesterMovies
	case named:
		return requesterAny
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("t"))) {
	case "tvsearch", "tv-search":
		return requesterTV
	case "movie", "movie-search", "moviesearch":
		return requesterMovies
	}
	return requesterAny
}

// twinsFor reports which of a release's two items this requester is served,
// given whether it has a twin and whether its original is offered to Sonarr at
// all (a film's is not). Radarr never gets a twin. Sonarr gets the twin in
// place of the original, and never a film's own title: Sonarr can attribute
// one only by guessing an episode from it, one part of a multi-part special
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Parser/ParsingService.cs#L259).
func (r requester) twinsFor(hasTwin, sonarrOriginal bool) (original, twin bool) {
	switch r {
	case requesterTV:
		return !hasTwin && sonarrOriginal, hasTwin
	case requesterMovies:
		return true, false
	}
	return true, hasTwin
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
