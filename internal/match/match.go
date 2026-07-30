// Package match links SeaDex entries to library items. It resolves an entry's
// AniList ID to arr IDs through the Fribb mapping (overrides already applied),
// and on a miss falls back to an AniList title lookup plus a conservative
// normalized-title-plus-year match against the library. It also reports
// ID-mapping coverage and maintains a memo of AniList lookups (positive
// answers and not-found negatives) so each id is fetched at most once per
// expiry window.
//
// Memo entries expire because AniList data is not immutable: entries are
// created and English titles added after licensing, so a permanent memo would
// hold a stale answer forever (a show added to AniList later would stay
// not-found; a later-added title would never be seen). Every memo write
// stamps the entry with an explicit expiry - now plus a uniform random TTL in
// [memoMinTTL, memoMaxTTL) (mean two weeks, ±25% jitter) - so entries written
// together renew spread out instead of in lockstep. Expiry is lazy: an
// expired entry is a lookup miss that re-enters the existing batched prefetch
// (or the per-entry fetch) and is re-stamped on renewal - and when the upstream
// that would renew it is unreachable, the match serves the expired positive
// rather than nothing (Memo.staleMedia; the pass still counts as degraded) -
// and entries still
// expired when a CLEAN (non-degraded) Match pass ends are pruned from the
// returned memo, EXCEPT the positives whose AniList id SeaDex still curates -
// those stay as stale feed-title/type fallback data (Memo.StaleTitle,
// Memo.StaleFormat), whose readers ignore expiry on purpose. A degraded pass
// could not renew anything, so it retains every expired entry. An entry
// carrying no expiry at all (written by a build older than this policy) reads
// as expired and is re-fetched: there is no migration, deliberately. The
// batched prefetch (up to 50 ids per request) amortizes renewals, so a few
// expiries per day cost effectively nothing.
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
// An untyped record whose AniList format makes an id it ALREADY carried usable
// counts here too (matchIDLessEntry's re-entry): the id resolved, even though
// the typing came from AniList. Unmapped counts every entry the ID bridge could
// not resolve: no Fribb record at all, a record still without a usable arr id
// (counted here even when the AniList title fallback links it), or an unusable
// AniList id.
type Coverage struct {
	Hits     map[string]int
	Unmapped map[string]int
}

// Result bundles the per-entry matches, the coverage counts, and the updated
// memo to persist. Degraded is set when a needed AniList fallback lookup could
// not be completed because of a transient/upstream error (not a definitive
// not-found), so the caller can preserve prior findings rather than treat the
// missing matches as resolved. IncompleteIDs scopes that degradation: it holds
// exactly the AniList ids whose needed lookup failed transiently this pass, so
// the caller can preserve the affected entries' prior findings while handling
// the unaffected majority normally. An id served from the memo or answered
// with a definitive not-found is complete, never in the set; a pass cut short
// by context cancellation is Degraded with the ids it never attempted absent
// from the set (the caller treats a shutdown as a whole-cycle event). With a
// live context, Degraded is true exactly when IncompleteIDs is non-empty.
type Result struct {
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
	// jitter draw). NewMatcher fixes them to time.Now and rand.Float64; tests
	// override the fields for deterministic, sleep-free expiry coverage.
	now  func() time.Time
	rand func() float64
}

// NewMatcher builds a Matcher. logger may be nil.
func NewMatcher(anilistClient AniListClient, logger *slog.Logger) *Matcher {
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
// pruned. Degraded
// passes retain expired entries as stale feed-title fallback data. The
// caller's memo.Entries map is updated in place (Result.Memo aliases it, not
// a copy), so the pre-call memo is not preserved. Match never fails as a
// whole: an AniList fallback error for one entry is logged, that entry is
// left unmatched, and its id is reported in Result.IncompleteIDs so the
// caller can scope its degradation handling to the affected entries.
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
	// the loop's boundary check. Classify it before the clean-pass-only prune
	// so the caller takes the whole-cycle interruption path and stale memo
	// entries remain available to the next cycle.
	if ctx.Err() != nil {
		run.degraded = true
	}
	if !run.degraded {
		// A degraded pass (outage, tripped breaker, shutdown) could not renew
		// what expired; keep those entries so the feed's stale-title tier
		// (scout/feedinfo.go) still serves them - they stay pending for next
		// cycle's batch either way, so retention costs no AniList traffic. A
		// clean pass still keeps the expired positives that tier reads for
		// entries SeaDex currently curates, which is why the catalogue is
		// passed in (pruneExpired).
		pruneExpired(&memo, now, entries)
	}
	return Result{Coverage: cov, Memo: memo, Matches: matches, Degraded: run.degraded, IncompleteIDs: run.incomplete}
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
// resolved by id (no lookup). needsLookup means AniList must be consulted:
// either no Fribb record exists at all, or the record is id-less (a split
// AniList<->arr mapping) so the title is the only remaining link. A record
// that HAS its arr id but missed FindByID simply is not in the library, so
// no lookup (it would only confirm the miss); a non-positive id never
// resolves, so no lookup either.
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
		// from a non-MOVIE-typed record carrying an unambiguous movie TMDB id
		// (h-f9), and arrItem guarantees the item belongs to the arr its id was
		// looked up in - so for every record the type label already routed
		// correctly this is the same value recordArr returned.
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
// One exception: if typing the record makes an identifier it already carried
// usable (a MOVIE type routing its TMDB-movie/IMDb ids), the record is no
// longer id-less and re-enters matchMappedEntry's ID-first branch.
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
	// or bind a same-titled series. AniList is authoritative for the format
	// here, so type the record from it and search the arr that format names
	// (arrUnknown - unrestricted - when the format is genuinely unknown).
	if rec.Type == "" {
		rec.Type = mapping.RecordFromFormat(media.Format).Type
		arr = formatArr(media.Format)
		// Typing can make identifiers the record ALREADY carried usable, so
		// the record may no longer be id-less: RoutedIDs only routes the
		// TMDB-movie/IMDb fields once the type says MOVIE, and both Fribb
		// (the object-form themoviedb_id movie list) and an operator override
		// (tmdb_movies / imdb_ids) can carry them on an untyped record. A
		// newly-usable id is stronger evidence than a title, so such a record
		// re-enters the ID-first branch: title-matching it could bind a
		// different same-titled movie while the id proves the intended one is
		// absent.
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
// Pooling both arrs added no lookup capability - it only let a wrong-arr
// item shadow the right-arr one under a shared key (TMDB movie and TV ids
// are disjoint namespaces over the same small-int key space, and TVDB
// reuses movie IMDb ids on the parent series), making FindByID/findMovie
// falsely miss a library item that IS present, depending on item order.
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
// cannot silently link to the same-named Sonarr series. NewLibIndex already
// indexes each ID map with only the arr that consumes it; the arrItem check
// restates that invariant at the lookup site as defense in depth.
//
// A record that routes NO series id and is not typed MOVIE still gets one more
// chance, against its unambiguous movie TMDB ids
// (mapping.Record.MovieTMDBIDs): a TMDB movie id is a Radarr id by
// construction, and the live Fribb body carries ~300 records shaped non-MOVIE
// type + no tvdb_id + a positive movie id, whose Radarr copy the type label
// alone could never resolve (h-f9). It is a SECONDARY lookup on purpose - a
// record that routes a TVDB id keeps series routing as its authoritative answer,
// unchanged - and it stays out of HasArrIdentifier, so a miss here still falls
// through to the AniList title fallback exactly as before rather than reading as
// "this record has its id, the item is simply absent".
func (li *LibIndex) FindByID(rec *mapping.Record) *library.Item {
	if rec.IsMovie() {
		return li.findMovie(rec)
	}
	tvdb, _, _ := rec.RoutedIDs()
	// RoutedIDs already drops every non-usable id it returns, scalar and slice
	// alike, so the usability POLICY has one home there and this is a presence
	// check, not a second policy check (l-f37/l-f109). The guard is kept in the
	// > 0 form deliberately, so it reads the same way as its slice siblings'
	// presence checks below and in catalogue.go.
	if tvdb > 0 {
		return arrItem(li.byTvdb[tvdb], library.ArrSonarr)
	}
	return li.findMovieByTMDB(rec.MovieTMDBIDs())
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
	for _, imdb := range imdbIDs { // RoutedIDs returns only usable ids
		key := imdbKey(imdb) // a usable id can still be padded; the index key is trimmed
		if key == "" {
			continue
		}
		if it := arrItem(li.byImdb[key], library.ArrRadarr); it != nil {
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
		if it := arrItem(li.byTmdb[id], library.ArrRadarr); it != nil {
			return it
		}
	}
	return nil
}

// imdbKey canonicalizes an IMDb id into its index/lookup key. HasArrIdentifier
// judges usability on the TRIMMED value, so the key must be trimmed too: a
// padded override id ("  tt0123456") otherwise reads as usable (suppressing the
// AniList title fallback) while never matching the item indexed under its own
// value. A blank or whitespace-only id yields "", which every caller skips.
func imdbKey(id string) string { return strings.TrimSpace(id) }

// arrItem returns it only when it is non-nil and belongs to arr, enforcing
// arr-consistency on an ID lookup.
func arrItem(it *library.Item, arr string) *library.Item {
	if it != nil && it.Arr == arr {
		return it
	}
	return nil
}

// narrowByYear applies the AniList year constraint to a title-fallback
// candidate set, returning the set unchanged when the year is unknown. When the
// constraint rejects every candidate the result is empty, which findByTitle
// treats as a miss - and that arm is logged here, because it is the actionable
// one: the title DID resolve library items and only the years disagreed (a
// December or split-cour premiere routinely differs by one), so the remedy is an
// overrides.json entry pinning the arr id. Counts and the AniList year only: no
// untrusted string crosses the log boundary.
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
// findByTitle already skips narrowing entirely when the ANILIST year is
// unknown, and hard-failing a library item for the same missing evidence
// made the asymmetry fatal in one direction and invisible in the other - an
// id-less Fribb record whose library item carries no year could never
// title-match at all. The single-candidate requirement still gates the final
// match, so a kept unknown-year candidate can only leave a set ambiguous (a
// miss) or let the one true candidate survive - never force a wrong match on
// its own.
func filterByYear(candidates []*library.Item, year int) []*library.Item {
	var out []*library.Item
	for _, it := range candidates {
		if it.Year == 0 || it.Year == year {
			out = append(out, it)
		}
	}
	return out
}
