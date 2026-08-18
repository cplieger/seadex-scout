// Package match links SeaDex entries to library items. It resolves an entry's
// AniList ID to arr IDs through the Fribb mapping (overrides already applied),
// and on a miss falls back to an AniList title lookup plus a conservative
// normalized-title-plus-year match against the library.
package match

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/titlekey"
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

// Coverage counts ID-mapping outcomes per arr for the cycle-complete coverage
// log line. Hits counts entries whose record carries a usable arr id - the ID
// bridge resolved an arr id - whether or not the item is in the library (a
// resolved id absent from the library is a missing item, not a mapping gap).
type Coverage struct {
	Hits     map[string]int
	Unmapped map[string]int
}

// Result bundles the per-entry matches, the coverage counts, and the updated
// memo to persist. Degraded is set when a needed AniList fallback lookup could
// not be completed because of a transient/upstream error (not a definitive
// not-found), so the caller can preserve prior findings rather than treat the
// missing matches as resolved.
type Result struct {
	// at is the pass's single clock reading, carried so PruneMemo prunes against
	// the same instant every lookup and stamp in the pass compared against.
	at            time.Time
	Coverage      Coverage
	Memo          Memo
	IncompleteIDs map[int]struct{}
	Matches       []Match
	Degraded      bool
}

// Matcher links entries using the mapping index and the AniList fallback.
type Matcher struct {
	anilist AniListClient
	log     *slog.Logger
	// now and rand feed the memo-expiry policy (the run clock and the TTL
	// jitter draw). New fixes them to time.Now and rand.Float64; tests override
	// the fields for deterministic, sleep-free expiry coverage.
	now  func() time.Time
	rand func() float64
}

// New builds a Matcher. logger may be nil.
func New(anilistClient AniListClient, logger *slog.Logger) *Matcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{
		anilist: anilistClient,
		log:     logger,
		now:     time.Now,
		rand:    rand.Float64,
	}
}

// Match links every entry to a library item (where present), returning the
// matches, ID-mapping coverage, and the updated memo to persist: an expiry
// beyond anything this policy could have written is re-stamped, renewed lookups
// are re-stamped, and entries still expired at the end of a clean pass are
// pruned.
func (m *Matcher) Match(ctx context.Context, entries []seadex.Entry, snap *library.Snapshot, idx *mapping.Index, memo Memo) Result {
	lib := NewLibIndex(snap)
	if memo.Entries == nil {
		memo.Entries = make(map[int]MemoEntry)
	}
	now := m.now()
	m.restampSkewedExpiries(&memo, now)
	cov := Coverage{Hits: make(map[string]int), Unmapped: make(map[string]int)}
	outage := m.prefetch(ctx, entries, idx, lib, &memo, now)
	run := &matchRun{
		m:    m,
		lib:  lib,
		idx:  idx,
		memo: &memo,
		cov:  &cov,
		now:  now,
		gate: &lookupGate{outage: outage},
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
	// Cancellation can arrive while the final entry is being matched, after
	// the loop's boundary check.
	if ctx.Err() != nil {
		run.degraded = true
	}
	// Match does NOT prune the memo.
	return Result{at: now, Coverage: cov, Memo: memo, Matches: matches, Degraded: run.degraded, IncompleteIDs: run.incomplete}
}

// PruneMemo garbage-collects res.Memo against the WHOLE SeaDex catalogue,
// in place.
//
// It takes the Result rather than the memo because both of its preconditions are
// non-negotiable and both live there.
func (m *Matcher) PruneMemo(res *Result, catalogue []seadex.Entry) {
	if res.Degraded {
		// A degraded pass (outage, tripped breaker, shutdown) could not renew
		// what expired; keep those entries so the feed's stale-title tier still
		// serves them - they stay pending for the next batch either way, so
		// retention costs no AniList traffic.
		return
	}
	// The PASS's clock, not a fresh reading: pruning has to agree with the
	// lookups (matchRun.now), and a cold reconcile's match runs for ~25 minutes,
	// so a second reading can classify as expired an entry the pass just served
	// live and therefore never renewed.
	pruneExpired(&res.Memo, res.at, catalogue)
}

// matchRun carries one Match call's shared state so the per-entry helpers do
// not thread seven parameters (two of them out-params) through every call.
type matchRun struct {
	m    *Matcher
	lib  *LibIndex
	idx  *mapping.Index
	memo *Memo
	cov  *Coverage
	// gate carries the fast-fail state for per-id AniList lookups: ids covered
	// by a totally-failed batch prefetch and, once the consecutive-failure
	// breaker trips, every remaining uncached id fail fast instead of
	// re-hitting the down upstream.
	gate *lookupGate
	// incomplete accumulates the AniList ids whose needed lookup failed
	// transiently this pass (see markIncomplete); Match surfaces it as
	// Result.IncompleteIDs. Nil until the first failure.
	incomplete map[int]struct{}
	// now is the run's single clock reading: every expiry comparison and stamp
	// in one Match pass uses it, so a slow (rate-limited) pass cannot straddle
	// an expiry mid-run and prune agrees with the lookups.
	now time.Time
	// degraded is set when a needed AniList fallback lookup could not be
	// completed because of a transient/upstream error; Match surfaces it as
	// Result.Degraded.
	degraded bool
}

// aniListNeed classifies an entry's AniList-lookup need - the ONE definition
// of the trigger BOTH pendingAniListIDs (the batch prefetch) and matchEntry
// (the per-entry pass) consult, so the two cannot drift. item != nil means
// resolved by id (no lookup).
func aniListNeed(alID int, idx *mapping.Index, lib *LibIndex) (rec mapping.Record, recOK bool, item *library.Item, needsLookup bool) {
	if alID <= 0 {
		return mapping.Record{}, false, nil, false
	}
	rec, recOK = idx.Lookup(alID)
	if !recOK {
		return rec, false, nil, true
	}
	if found := lib.FindByID(&rec); found != nil {
		return rec, true, found, false
	}
	return rec, true, nil, !rec.HasArrIdentifier()
}

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
		return r.matchMappedEntry(ctx, e, &rec, item, needsLookup)
	}
	return r.matchUnmappedEntry(ctx, e)
}

// matchMappedEntry links an entry whose Fribb record resolved, tracking
// coverage per outcome (ID hit, id-less title fallback, or library miss).
func (r *matchRun) matchMappedEntry(ctx context.Context, e *seadex.Entry, rec *mapping.Record, item *library.Item, needsLookup bool) Match {
	arr := recordArr(rec)
	if needsLookup {
		return r.matchIDLessEntry(ctx, e, rec, arr)
	}
	// The record carries a usable arr id: the ID mapping resolved, so this
	// is a coverage hit whether or not the item is in the library.
	if item != nil {
		// The RESOLVED item's arr is authoritative over the type label's
		// routing: FindByID's secondary movie lookup can resolve a Radarr movie
		// from a non-MOVIE-typed record carrying an unambiguous movie TMDB id, and
		// the per-arr index split guarantees the item belongs to the
		// arr its id was looked up in - so for every record the type label
		// already routed correctly this is the same value recordArr returned.
		arr = item.Arr
		r.cov.Hits[arr]++
		return Match{Item: item, Entry: *e, Record: *rec, Arr: arr, Source: SourceID}
	}
	r.cov.Hits[arr]++
	// A record that carries its arr id but missed FindByID is simply not in
	// the library and is unmatched directly, with no AniList lookup - this
	// keeps the fallback off the ~thousands of SeaDex entries the operator
	// does not have, which otherwise dominate a cold cycle's AniList
	// traffic.
	return Match{Entry: *e, Record: *rec, Arr: arr, Source: SourceUnmapped}
}

// matchUnmappedEntry links an entry with no Fribb record through the AniList
// title fallback, counting it as unmapped coverage either way.
func (r *matchRun) matchUnmappedEntry(ctx context.Context, e *seadex.Entry) Match {
	media, ok := r.lookupAniList(ctx, e.AniListID)
	if !ok {
		r.cov.Unmapped[arrUnknown]++
		return Match{Entry: *e, Arr: arrUnknown, Source: SourceUnmapped}
	}
	arr := formatArr(media.Format)
	r.cov.Unmapped[arr]++
	item := r.lib.findByTitle(media.Titles, media.Year, arr, r.m.log)
	if item == nil {
		return Match{Entry: *e, Arr: arr, Source: SourceUnmapped}
	}
	return Match{Item: item, Entry: *e, Record: mapping.RecordFromFormat(media.Format), Arr: item.Arr, Source: SourceTitle}
}

// matchIDLessEntry links an entry whose Fribb record exists but carries no arr
// id (a split AniList<->arr mapping), where the AniList title is the only
// remaining link to the arr item. It resolves AniList once: the format types an
// untyped record and picks the search arr, then the normalized title + year
// matches within that arr. Coverage counts under the resolved arr either way.
func (r *matchRun) matchIDLessEntry(ctx context.Context, e *seadex.Entry, rec *mapping.Record, arr string) Match {
	// needsLookup under a present record means the record is id-less (see
	// aniListNeed): the ID bridge could not resolve an arr id from the record
	// AS LOADED, so unless the AniList typing below makes one of the record's
	// own ids usable (the re-entry, which counts as a Hit), the entry counts as
	// Unmapped even when the AniList title fallback links it - keeping the cycle
	// line's "mapped" an honest count of ID resolutions. The title is the only
	// remaining link to the arr item, so consult AniList.
	media, ok := r.lookupAniList(ctx, e.AniListID)
	if !ok {
		r.cov.Unmapped[arr]++
		return Match{Entry: *e, Record: *rec, Arr: arr, Source: SourceUnmapped}
	}
	// An UNTYPED id-less record carries no routing evidence at all: recordArr
	// routes every non-MOVIE value (including "") to Sonarr, which would
	// restrict the title search to Sonarr and can miss the real Radarr movie
	// or bind a same-titled series.
	if rec.Type == "" {
		rec.Type = mapping.RecordFromFormat(media.Format).Type
		arr = formatArr(media.Format)
		// Typing can make identifiers the record ALREADY carried usable, so
		// the record may no longer be id-less: RoutedIDs only routes the
		// TMDB-movie/IMDb fields once the type says MOVIE, and both Fribb
		// (the object-form themoviedb_id movie list) and an operator override
		// (tmdb_movies / imdb_ids) can carry them on an untyped record.
		if rec.HasArrIdentifier() {
			return r.matchMappedEntry(ctx, e, rec, r.lib.FindByID(rec), false)
		}
	}
	r.cov.Unmapped[arr]++
	if matched := r.lib.findByTitle(media.Titles, media.Year, arr, r.m.log); matched != nil {
		return Match{Item: matched, Entry: *e, Record: *rec, Arr: matched.Arr, Source: SourceTitle}
	}
	return Match{Entry: *e, Record: *rec, Arr: arr, Source: SourceUnmapped}
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
	rec := mapping.RecordFromFormat(format)
	if rec.Type == "" {
		return arrUnknown
	}
	return recordArr(&rec)
}

// --- LibIndex: library snapshot lookup indexes (by arr ID and normalized title) ---

// LibIndex indexes a library snapshot by external ID and normalized title;
// the ID lookup is arr-consistent (see FindByID). Shared by the matcher and
// the feed-info builder (scout's feedEntryInfo).
type LibIndex struct {
	byTvdb  map[int]*library.Item
	byTmdb  map[int]*library.Item
	byImdb  map[string]*library.Item
	byTitle map[string][]*library.Item
}

// NewLibIndex builds the lookup indexes over a snapshot's items.
func NewLibIndex(snap *library.Snapshot) *LibIndex {
	li := &LibIndex{
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
		li.indexIDs(it)
		li.indexTitles(it)
	}
	return li
}

// indexIDs adds an item's external IDs to the ID indexes of its arr.
// Each ID index has exactly one arr-gated consumer (byTvdb only via the
// Sonarr branch of FindByID, byTmdb/byImdb only via findMovie's Radarr
// gate), so index each map only with items of the arr that consumes it.
func (li *LibIndex) indexIDs(it *library.Item) {
	switch it.Arr {
	case library.ArrSonarr:
		if it.TvdbID > 0 { // only positive ids are reachable: FindByID guards tvdb > 0
			li.byTvdb[it.TvdbID] = it
		}
	case library.ArrRadarr:
		if it.TmdbID > 0 { // only positive ids are reachable: findMovie guards id > 0
			li.byTmdb[it.TmdbID] = it
		}
		if key := imdbKey(it.ImdbID); key != "" {
			li.byImdb[key] = it
		}
	}
}

// indexTitles adds an item's primary and alternate titles to the title index.
func (li *LibIndex) indexTitles(it *library.Item) {
	li.addTitle(it.Title, it)
	for _, t := range it.AltTitles {
		li.addTitle(t, it)
	}
}

// addTitle indexes one title for an item under its normalized key.
func (li *LibIndex) addTitle(title string, it *library.Item) {
	if key := titlekey.Normalize(title); key != "" {
		li.byTitle[key] = append(li.byTitle[key], it)
	}
}

// FindByID looks up a library item by the arr IDs in a mapping record. The
// match must be arr-consistent: a MOVIE record resolves only to a Radarr movie
// and a series record only to a Sonarr series, so a movie whose Fribb record
// carries a TV themoviedb_id (or an IMDb id TVDB reuses for the parent series)
// cannot silently link to the same-named Sonarr series.
func (li *LibIndex) FindByID(rec *mapping.Record) *library.Item {
	if rec.IsMovie() {
		return li.findMovie(rec)
	}
	tvdb, _, _ := rec.RoutedIDs()
	// RoutedIDs already drops every non-usable id it returns, scalar and slice
	// alike, so the usability POLICY has one home there and this is a presence
	// check, not a second policy check.
	if tvdb > 0 {
		// byTvdb is populated only with Sonarr items (indexIDs' arr switch),
		// so the map miss IS the arr gate.
		return li.byTvdb[tvdb]
	}
	return li.findMovieByTMDB(rec.TmdbMovies)
}

// findMovie resolves a MOVIE record to a Radarr movie by TMDB movie id, then by
// IMDb id (the fields mapping.Record.RoutedIDs enumerates, preserving the
// TMDB-before-IMDb lookup order). Only Radarr items match (arr-consistency,
// see FindByID).
func (li *LibIndex) findMovie(rec *mapping.Record) *library.Item {
	_, tmdbMovies, imdbIDs := rec.RoutedIDs()
	if it := li.findMovieByTMDB(tmdbMovies); it != nil {
		return it
	}
	for _, imdb := range imdbIDs { // RoutedIDs returns only canonical, usable ids
		if it := li.byImdb[imdb]; it != nil { // byImdb holds only Radarr items
			return it
		}
	}
	return nil
}

// findMovieByTMDB resolves the first of ids that names an indexed Radarr movie.
// Shared by findMovie (a MOVIE record's routed ids) and FindByID's secondary
// cross-type lookup, so the movie-id half of the ID bridge has one lookup.
func (li *LibIndex) findMovieByTMDB(ids []int) *library.Item {
	for _, id := range ids { // callers pass only usable (positive) ids
		if it := li.byTmdb[id]; it != nil { // byTmdb holds only Radarr items
			return it
		}
	}
	return nil
}

// imdbKey canonicalizes a library Item's IMDb id into its index/lookup key.
// Only library-side inputs reach it: a mapping Record's ids are already
// canonical, because Record.canonicalize trims them at every producer and
// mapping.buildIndex reapplies the invariant to a decoded cache, so RoutedIDs
// cannot return a padded or blank id.
func imdbKey(id string) string { return strings.TrimSpace(id) }

// narrowByYear applies the AniList year constraint to a title-fallback
// candidate set, returning the set unchanged when the year is unknown.
func narrowByYear(candidates []*library.Item, year int, log *slog.Logger) []*library.Item {
	if year == 0 {
		return candidates
	}
	narrowed := filterByYear(candidates, year)
	if len(narrowed) == 0 && len(candidates) > 0 {
		log.Debug("title fallback year mismatch, treating as unmapped",
			"anilist_year", year, "candidates", len(candidates))
	}
	return narrowed
}

// findByTitle performs the conservative title fallback: it collects candidates
// matching any of the titles (restricted to the arr when known), narrows by
// year when known, and returns a match only when exactly one candidate remains.
// Both miss arms - an over-constrained year and an ambiguous set - are logged.
func (li *LibIndex) findByTitle(titles []string, year int, arr string, log *slog.Logger) *library.Item {
	candidates := narrowByYear(li.titleCandidates(titles, arr), year, log)
	switch len(candidates) {
	case 1:
		return candidates[0]
	case 0:
		return nil
	default:
		j := logattr.NewJoiner()
		for i, t := range titles {
			if i > 0 && !j.WriteSep(", ") {
				break
			}
			if !j.Write(t) {
				break
			}
		}
		log.Debug("title fallback ambiguous, treating as unmapped", "titles", j.String(), "candidates", len(candidates))
		return nil
	}
}

// titleCandidates returns the distinct library items whose (normalized) title
// or alternate title equals any of titles, restricted to arr unless arr is
// arrUnknown (the one "arr not known" sentinel: an entry whose AniList format
// gave no arr evidence searches both arrs).
func (li *LibIndex) titleCandidates(titles []string, arr string) []*library.Item {
	seen := make(map[*library.Item]struct{})
	var candidates []*library.Item
	for _, title := range titles {
		candidates = li.appendTitleCandidates(candidates, seen, title, arr)
	}
	return candidates
}

// appendTitleCandidates appends the items indexed under title's normalized
// key that pass the arr restriction (arrUnknown = unrestricted) and are not
// already in seen.
func (li *LibIndex) appendTitleCandidates(candidates []*library.Item, seen map[*library.Item]struct{}, title, arr string) []*library.Item {
	key := titlekey.Normalize(title)
	if key == "" {
		return candidates
	}
	for _, it := range li.byTitle[key] {
		if arr != arrUnknown && it.Arr != arr {
			continue
		}
		if _, dup := seen[it]; dup {
			continue
		}
		seen[it] = struct{}{}
		candidates = append(candidates, it)
	}
	return candidates
}

// filterByYear narrows candidates to those whose year matches, KEEPING items
// whose year is unknown (0): absence of year evidence is not a mismatch.
func filterByYear(candidates []*library.Item, year int) []*library.Item {
	var out []*library.Item
	for _, it := range candidates {
		if it.Year == 0 || it.Year == year {
			out = append(out, it)
		}
	}
	return out
}
