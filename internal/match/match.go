// Package match links SeaDex entries to library items. It resolves an entry's
// AniList ID to arr IDs through the Fribb mapping (overrides already applied),
// and on a miss falls back to an AniList title lookup plus a conservative
// normalized-title-plus-year match against the library. It also reports
// ID-mapping coverage and maintains a memo so a given AniList ID is fetched at
// most once (titles and format do not change).
package match

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// arrUnknown labels coverage for an entry whose arr could not be determined.
const arrUnknown = "unknown"

// Source records how an entry was linked to a library item.
type Source string

const (
	// SourceID means the AniList ID resolved to an arr ID via the Fribb map.
	SourceID Source = "id"
	// SourceTitle means the AniList title fallback matched a library item.
	SourceTitle Source = "title"
	// SourceUnmapped means no library item was found for the entry.
	SourceUnmapped Source = "unmapped"
)

// AniListClient is the AniList fallback surface the matcher needs: a single
// lookup for the per-entry path and a batched lookup the matcher uses to
// pre-warm the memo for a whole cycle in a handful of requests.
type AniListClient interface {
	Fetch(ctx context.Context, aniListID int) (anilist.Media, error)
	FetchMany(ctx context.Context, ids []int) (map[int]anilist.Media, error)
}

// Match is the result of linking one SeaDex entry.
type Match struct {
	Item   *library.Item
	Arr    string
	Source Source
	Entry  seadex.Entry
	Record mapping.Record
}

// InLibrary reports whether the entry was matched to a library item.
func (m *Match) InLibrary() bool { return m.Item != nil }

// Coverage counts ID-mapping outcomes per arr for the coverage metrics.
type Coverage struct {
	Hits     map[string]int
	Unmapped map[string]int
}

// MemoEntry is a cached AniList lookup (titles/format/year), or a negative
// result, keyed by AniList ID in a Memo.
type MemoEntry struct {
	Format   string   `json:"format,omitempty"`
	Titles   []string `json:"titles,omitempty"`
	Year     int      `json:"year,omitempty"`
	NotFound bool     `json:"not_found,omitempty"`
}

// Memo persists AniList fallback lookups across cycles.
type Memo struct {
	Entries map[int]MemoEntry `json:"entries,omitempty"`
}

// Result bundles the per-entry matches, the coverage counts, and the updated
// memo to persist. Degraded is set when a needed AniList fallback lookup could
// not be completed because of a transient/upstream error (not a definitive
// not-found), so the caller can preserve prior findings rather than treat the
// missing matches as resolved.
type Result struct {
	Coverage Coverage
	Memo     Memo
	Matches  []Match
	Degraded bool
}

// Matcher links entries using the mapping index and the AniList fallback.
type Matcher struct {
	anilist AniListClient
	log     *slog.Logger
}

// NewMatcher builds a Matcher. logger may be nil.
func NewMatcher(anilistClient AniListClient, logger *slog.Logger) *Matcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{anilist: anilistClient, log: logger}
}

// Match links every entry to a library item (where present), returning the
// matches, ID-mapping coverage, and the updated memo. It never fails as a
// whole: an AniList fallback error for one entry is logged and that entry is
// left unmatched.
func (m *Matcher) Match(ctx context.Context, entries []seadex.Entry, snap *library.Snapshot, idx *mapping.Index, memo Memo) Result {
	lib := buildLibIndex(snap)
	if memo.Entries == nil {
		memo.Entries = make(map[int]MemoEntry)
	}
	cov := Coverage{Hits: make(map[string]int), Unmapped: make(map[string]int)}
	run := &matchRun{
		m:    m,
		lib:  lib,
		idx:  idx,
		memo: &memo,
		cov:  &cov,
		gate: &lookupGate{outage: m.prefetch(ctx, entries, idx, lib, &memo)},
	}
	matches := make([]Match, 0, len(entries))
	for i := range entries {
		if ctx.Err() != nil {
			// A cancelled cycle (routine shutdown SIGTERM) is not an AniList
			// fault: skip the remaining entries instead of failing each one's
			// lookup with context.Canceled, and flag the cycle degraded so the
			// caller preserves prior findings.
			run.degraded = true
			m.log.Debug("match interrupted; remaining entries skipped", "matched", len(matches), "total", len(entries))
			break
		}
		matches = append(matches, run.matchEntry(ctx, &entries[i]))
	}
	return Result{Coverage: cov, Memo: memo, Matches: matches, Degraded: run.degraded}
}

// matchRun carries one Match call's shared state so the per-entry helpers do
// not thread seven parameters (two of them out-params) through every call.
type matchRun struct {
	m    *Matcher
	lib  *libIndex
	idx  *mapping.Index
	memo *Memo
	cov  *Coverage
	// gate carries the fast-fail state for per-id AniList lookups: ids covered
	// by a totally-failed batch prefetch and, once the consecutive-failure
	// breaker trips, every remaining uncached id fail fast instead of
	// re-hitting the down upstream.
	gate *lookupGate
	// degraded is set when a needed AniList fallback lookup could not be
	// completed because of a transient/upstream error; Match surfaces it as
	// Result.Degraded.
	degraded bool
}

// prefetch batch-fetches into the memo every AniList id the per-entry pass will
// consult but has not cached, so a cold cycle costs a handful of batched AniList
// requests instead of one request per id-less entry. A PARTIAL batch failure is
// best-effort: an id a partial batch does not return is left uncached and falls
// through to matchEntry's single Fetch, so batching never changes the match
// result, only the request count. A TOTAL batch failure (no chunk succeeded -
// an AniList outage) instead returns the pending ids so the per-entry pass
// fails them fast: every per-id lookup would be doomed against the same outage,
// and the unbounded futile tail of requests would only stall the cycle.
func (m *Matcher) prefetch(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, lib *libIndex, memo *Memo) map[int]struct{} {
	if ctx.Err() != nil {
		// Mirror the per-entry loop's cancellation guard: a batch issued on an
		// already-cancelled cycle can only fail with context.Canceled, and the
		// loop below breaks (and flags the cycle degraded) before using it.
		return nil
	}
	ids := pendingAniListIDs(entries, idx, lib, memo)
	if len(ids) == 0 {
		return nil
	}
	fetched, err := m.anilist.FetchMany(ctx, ids)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		// A cancellation is not a fault (same contract as Scout.save).
		m.log.Debug("anilist batch prefetch cancelled",
			"requested", len(ids), "fetched", len(fetched))
	case len(fetched) == 0:
		// TOTAL failure: FetchMany aborts on the first chunk error, so an
		// empty result means zero chunks succeeded (an outage, not a partial
		// miss). Degrade fast: fail the pending ids immediately instead of
		// regressing to one doomed per-id request each.
		m.log.Warn("anilist batch prefetch failed; skipping per-id fallback for pending ids",
			"requested", len(ids), "error", err)
		outage := make(map[int]struct{}, len(ids))
		for _, id := range ids {
			outage[id] = struct{}{}
		}
		return outage
	default:
		m.log.Warn("anilist batch prefetch incomplete; remaining ids fall back to per-id fetch",
			"requested", len(ids), "fetched", len(fetched), "error", err)
	}
	for _, id := range ids {
		if media, ok := fetched[id]; ok {
			memo.Entries[id] = MemoEntry{Titles: media.Titles, Format: media.Format, Year: media.Year}
			continue
		}
		if err == nil {
			// The batch completed without returning this id: AniList has no such
			// media. Memoize the negative so it is not re-fetched this run.
			memo.Entries[id] = MemoEntry{NotFound: true}
		}
		// err != nil and id not returned: leave uncached so matchEntry retries it
		// via the single Fetch.
	}
	return nil
}

// aniListNeed classifies an entry's AniList-lookup need - the ONE definition
// of the trigger BOTH pendingAniListIDs (the batch prefetch) and matchEntry
// (the per-entry pass) consult, so the two cannot drift. item != nil means
// resolved by id (no lookup). needsLookup means AniList must be consulted:
// either no Fribb record exists at all, or the record is id-less (a split
// AniList<->arr mapping) so the title is the only remaining link. A record
// that HAS its arr id but missed findByID simply is not in the library, so
// no lookup (it would only confirm the miss); a non-positive id never
// resolves, so no lookup either.
func aniListNeed(alID int, idx *mapping.Index, lib *libIndex) (rec mapping.Record, recOK bool, item *library.Item, needsLookup bool) {
	if alID <= 0 {
		return mapping.Record{}, false, nil, false
	}
	rec, recOK = idx.Lookup(alID)
	if !recOK {
		return rec, false, nil, true
	}
	if found := lib.findByID(&rec); found != nil {
		return rec, true, found, false
	}
	return rec, true, nil, !rec.HasArrIdentifier()
}

// pendingAniListIDs returns the distinct AniList ids the match will look up but
// has not memoized: exactly the entries aniListNeed - the shared trigger
// matchEntry also consults - classifies as needing a lookup, so the batch
// fetches no more (which would re-introduce the not-in-library lookups the
// HasArrIdentifier gate removed) and no less than the per-entry pass needs.
func pendingAniListIDs(entries []seadex.Entry, idx *mapping.Index, lib *libIndex, memo *Memo) []int {
	seen := make(map[int]struct{})
	var ids []int
	add := func(alID int) {
		if _, done := memo.Entries[alID]; done {
			return
		}
		if _, dup := seen[alID]; dup {
			return
		}
		seen[alID] = struct{}{}
		ids = append(ids, alID)
	}
	for i := range entries {
		if _, _, _, needsLookup := aniListNeed(entries[i].AniListID, idx, lib); needsLookup {
			add(entries[i].AniListID)
		}
	}
	return ids
}

// transientFailureCap is the consecutive transient per-id AniList failure
// streak at which the matcher stops issuing further lookups for the cycle: an
// outage that begins after the first prefetch chunk succeeds looks like a
// PARTIAL batch failure to prefetch, so without this breaker every remaining
// uncached id regresses to one doomed (internally retried) request each - the
// same futile tail the total-outage fast-fail exists to avoid.
const transientFailureCap = 3

// lookupGate gates per-id AniList lookups for one Match pass: ids covered by a
// totally failed batch prefetch fail fast, and a streak of consecutive
// transient per-id failures trips the same fast-fail for every remaining
// uncached id.
type lookupGate struct {
	outage  map[int]struct{}
	streak  int
	tripped bool
}

// down reports whether the id must fail fast (outage-covered or breaker tripped).
func (g *lookupGate) down(id int) bool {
	if g.tripped {
		return true
	}
	_, down := g.outage[id]
	return down
}

// recordFailure counts a consecutive transient failure; it returns true on the
// call that trips the breaker.
func (g *lookupGate) recordFailure() bool {
	g.streak++
	if g.streak == transientFailureCap {
		g.tripped = true
		return true
	}
	return false
}

// recordSuccess resets the streak (a definitive answer - media or not-found -
// proves the upstream is answering).
func (g *lookupGate) recordSuccess() { g.streak = 0 }

// matchEntry links one entry: ID resolution first, AniList title fallback next.
// The lookup trigger is aniListNeed, shared with the batch prefetch. r.gate
// fast-fails per-id AniList lookups doomed by an outage (see matchRun).
func (r *matchRun) matchEntry(ctx context.Context, e *seadex.Entry) Match {
	rec, recOK, item, needsLookup := aniListNeed(e.AniListID, r.idx, r.lib)
	if !recOK && !needsLookup {
		// Non-positive AniList id: it can never resolve, so do not spend a
		// rate-limited AniList request confirming it (or degrade the whole
		// cycle when that request fails transiently).
		r.cov.Unmapped[arrUnknown]++
		return Match{Entry: *e, Arr: arrUnknown, Source: SourceUnmapped}
	}
	if recOK {
		arr := recordArr(&rec)
		r.cov.Hits[arr]++
		if item != nil {
			return Match{Item: item, Entry: *e, Record: rec, Arr: arr, Source: SourceID}
		}
		// needsLookup here means the record is id-less (see aniListNeed): the
		// title is the only remaining link to the arr item, so consult AniList.
		// A record that carries its arr id but missed findByID is simply not in
		// the library and is unmapped directly - this keeps the fallback off
		// the ~thousands of SeaDex entries the operator does not have, which
		// otherwise dominate a cold cycle's AniList traffic.
		if needsLookup {
			if matched := r.titleMatch(ctx, e, arr); matched != nil {
				return Match{Item: matched, Entry: *e, Record: rec, Arr: arr, Source: SourceTitle}
			}
		}
		return Match{Entry: *e, Record: rec, Arr: arr, Source: SourceUnmapped}
	}

	media, ok := r.lookupAniList(ctx, e.AniListID)
	if !ok {
		r.cov.Unmapped[arrUnknown]++
		return Match{Entry: *e, Arr: arrUnknown, Source: SourceUnmapped}
	}
	arr := formatArr(media.Format)
	r.cov.Unmapped[arr]++
	item = r.lib.findByTitle(media.Titles, media.Year, arr, r.m.log)
	if item == nil {
		return Match{Entry: *e, Arr: arr, Source: SourceUnmapped}
	}
	return Match{Item: item, Entry: *e, Record: mapping.Record{Type: mapping.NormalizeType(media.Format)}, Arr: item.Arr, Source: SourceTitle}
}

// lookupAniList consults the memo, then AniList. A not-found result is memoized
// (negatively) so it is not re-fetched; a transient error is not memoized so it
// is retried next cycle. An id the gate reports down (covered by a
// totally-failed batch prefetch, or the consecutive-transient-failure breaker
// tripped) fails fast without a per-id request: the same outage would doom it,
// and the id stays un-memoized so it is retried next cycle.
func (r *matchRun) lookupAniList(ctx context.Context, aniListID int) (anilist.Media, bool) {
	if ent, ok := r.memo.Entries[aniListID]; ok {
		if ent.NotFound {
			return anilist.Media{}, false
		}
		return anilist.Media{Titles: ent.Titles, Format: ent.Format, Year: ent.Year}, true
	}
	if r.gate.down(aniListID) {
		// Degrade fast through the existing accounting (the prefetch already
		// logged the single outage WARN): the cycle preserves prior findings
		// rather than treating the missing match as resolved.
		r.degraded = true
		return anilist.Media{}, false
	}
	media, err := r.m.anilist.Fetch(ctx, aniListID)
	if err != nil {
		if errors.Is(err, anilist.ErrNotFound) {
			r.gate.recordSuccess()
			r.memo.Entries[aniListID] = MemoEntry{NotFound: true}
		} else {
			// A transient/upstream error (network, context cancellation, rate-limit
			// exhaustion) means this needed fallback lookup could not be completed.
			// Flag the cycle degraded so the caller preserves prior findings rather
			// than treating the missing match as a resolved finding, and leave the
			// id un-memoized so it is retried next cycle.
			r.degraded = true
			if errors.Is(err, context.Canceled) {
				// A cancellation is not a fault (same contract as Scout.save):
				// log at Debug so a redeploy is not attributed to an AniList outage.
				r.m.log.Debug("anilist fallback cancelled", "al_id", aniListID)
			} else {
				r.m.log.Warn("anilist fallback failed", "al_id", aniListID, "error", err)
				if r.gate.recordFailure() {
					r.m.log.Warn("anilist fallback failing repeatedly; failing remaining lookups fast this cycle",
						"consecutive_failures", transientFailureCap)
				}
			}
		}
		return anilist.Media{}, false
	}
	r.gate.recordSuccess()
	r.memo.Entries[aniListID] = MemoEntry{Titles: media.Titles, Format: media.Format, Year: media.Year}
	return media, true
}

// titleMatch resolves the entry through the AniList fallback and matches it to a
// library item by normalized title + year, restricted to arr. It returns nil on
// any miss (AniList failure, no candidate, or an ambiguous set). It bridges the
// case where Fribb has the entry but no usable arr id, so the AniList title is
// the only remaining link to the arr item.
func (r *matchRun) titleMatch(ctx context.Context, e *seadex.Entry, arr string) *library.Item {
	media, ok := r.lookupAniList(ctx, e.AniListID)
	if !ok {
		return nil
	}
	return r.lib.findByTitle(media.Titles, media.Year, arr, r.m.log)
}

// recordArr routes a mapping record to its arr (MOVIE -> Radarr, else Sonarr).
func recordArr(r *mapping.Record) string {
	if r.IsMovie() {
		return library.ArrRadarr
	}
	return library.ArrSonarr
}

// formatArr routes an AniList format to its arr (MOVIE -> Radarr, else Sonarr)
// by building a Record and reusing the mapping-owned decision, so the "MOVIE"
// token lives only in mapping. An empty format is unknown.
func formatArr(format string) string {
	norm := mapping.NormalizeType(format)
	if norm == "" {
		return arrUnknown
	}
	return recordArr(&mapping.Record{Type: norm})
}

// --- libIndex: library snapshot lookup indexes (by arr ID and normalized title) ---

// libIndex indexes a library snapshot by external ID and normalized title.
type libIndex struct {
	byTvdb  map[int]*library.Item
	byTmdb  map[int]*library.Item
	byImdb  map[string]*library.Item
	byTitle map[string][]*library.Item
}

// buildLibIndex builds the lookup indexes over a snapshot's items.
func buildLibIndex(snap *library.Snapshot) *libIndex {
	li := &libIndex{
		byTvdb:  make(map[int]*library.Item),
		byTmdb:  make(map[int]*library.Item),
		byImdb:  make(map[string]*library.Item),
		byTitle: make(map[string][]*library.Item),
	}
	if snap == nil {
		return li
	}
	for i := range snap.Items {
		it := &snap.Items[i]
		// Each ID index has exactly one arr-gated consumer (byTvdb only via the
		// Sonarr branch of findByID, byTmdb/byImdb only via findMovie's Radarr
		// gate), so index each map only with items of the arr that consumes it.
		// Pooling both arrs added no lookup capability - it only let a wrong-arr
		// item shadow the right-arr one under a shared key (TMDB movie and TV ids
		// are disjoint namespaces over the same small-int key space, and TVDB
		// reuses movie IMDb ids on the parent series), making findByID/findMovie
		// falsely miss a library item that IS present, depending on item order.
		switch it.Arr {
		case library.ArrSonarr:
			if it.TvdbID != 0 {
				li.byTvdb[it.TvdbID] = it
			}
		case library.ArrRadarr:
			if it.TmdbID != 0 {
				li.byTmdb[it.TmdbID] = it
			}
			if it.ImdbID != "" {
				li.byImdb[it.ImdbID] = it
			}
		}
		li.indexTitles(it)
	}
	return li
}

// indexTitles adds an item's primary and alternate titles to the title index.
func (li *libIndex) indexTitles(it *library.Item) {
	li.addTitle(it.Title, it)
	for _, t := range it.AltTitles {
		li.addTitle(t, it)
	}
}

// addTitle indexes one title for an item under its normalized key.
func (li *libIndex) addTitle(title string, it *library.Item) {
	if key := normalizeTitle(title); key != "" {
		li.byTitle[key] = append(li.byTitle[key], it)
	}
}

// findByID looks up a library item by the arr IDs in a mapping record. The
// match must be arr-consistent: a MOVIE record resolves only to a Radarr movie
// and a series record only to a Sonarr series. This guards against a shared-ID
// collision in the pooled TMDB/IMDb indexes — a movie whose Fribb record carries
// a TV themoviedb_id (or an IMDb id TVDB reuses for the parent series) must not
// silently link to the same-named Sonarr series (it would produce an
// unscopable, meaningless row).
func (li *libIndex) findByID(rec *mapping.Record) *library.Item {
	if rec.IsMovie() {
		return li.findMovie(rec)
	}
	tvdb, _, _ := rec.RoutedIDs()
	if tvdb != 0 {
		return arrItem(li.byTvdb[tvdb], library.ArrSonarr)
	}
	return nil
}

// findMovie resolves a MOVIE record to a Radarr movie by TMDB movie id, then by
// IMDb id (the fields mapping.Record.RoutedIDs enumerates, preserving the
// TMDB-before-IMDb lookup order). Only Radarr items match (arr-consistency,
// see findByID).
func (li *libIndex) findMovie(rec *mapping.Record) *library.Item {
	_, tmdbMovies, imdbIDs := rec.RoutedIDs()
	for _, id := range tmdbMovies {
		if it := arrItem(li.byTmdb[id], library.ArrRadarr); it != nil {
			return it
		}
	}
	for _, imdb := range imdbIDs {
		if it := arrItem(li.byImdb[imdb], library.ArrRadarr); it != nil {
			return it
		}
	}
	return nil
}

// arrItem returns it only when it is non-nil and belongs to arr, enforcing
// arr-consistency on an ID lookup.
func arrItem(it *library.Item, arr string) *library.Item {
	if it != nil && it.Arr == arr {
		return it
	}
	return nil
}

// findByTitle performs the conservative title fallback: it collects candidates
// matching any of the titles (restricted to the arr when known), narrows by
// year when known, and returns a match only when exactly one candidate remains.
// An ambiguous set is logged and treated as a miss.
func (li *libIndex) findByTitle(titles []string, year int, arr string, log *slog.Logger) *library.Item {
	candidates := li.titleCandidates(titles, arr)
	if year != 0 {
		narrowed := filterByYear(candidates, year)
		if len(narrowed) == 0 {
			return nil
		}
		candidates = narrowed
	}
	switch len(candidates) {
	case 1:
		return candidates[0]
	case 0:
		return nil
	default:
		log.Debug("title fallback ambiguous, treating as unmapped", "titles", titles, "candidates", len(candidates))
		return nil
	}
}

// titleCandidates returns the distinct library items whose (normalized) title
// or alternate title equals any of titles, optionally restricted to arr.
func (li *libIndex) titleCandidates(titles []string, arr string) []*library.Item {
	seen := make(map[*library.Item]struct{})
	var candidates []*library.Item
	for _, t := range titles {
		key := normalizeTitle(t)
		if key == "" {
			continue
		}
		for _, it := range li.byTitle[key] {
			if arr != "" && arr != arrUnknown && it.Arr != arr {
				continue
			}
			if _, dup := seen[it]; dup {
				continue
			}
			seen[it] = struct{}{}
			candidates = append(candidates, it)
		}
	}
	return candidates
}

// filterByYear returns the candidates whose year equals year.
func filterByYear(candidates []*library.Item, year int) []*library.Item {
	var out []*library.Item
	for _, it := range candidates {
		if it.Year == year {
			out = append(out, it)
		}
	}
	return out
}

// reTitleStrip removes every character that is not a lowercase letter or digit.
var reTitleStrip = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeTitle lowercases a title and strips all non-alphanumeric characters
// so punctuation, spacing, and separators do not defeat an otherwise exact
// match. It is deliberately conservative (no transliteration or fuzzy edits).
func normalizeTitle(s string) string {
	return reTitleStrip.ReplaceAllString(strings.ToLower(s), "")
}
