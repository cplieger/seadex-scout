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
// (or the per-entry fetch) and is re-stamped on renewal, and entries still
// expired when a Match pass ends are pruned from the returned memo. Legacy
// entries persisted before the policy (no expiry field) are stamped on first
// load from the wider [memoMinMigration, memoMaxTTL) window, spreading the
// accumulated backlog's first renewal with no day-one stampede. The batched
// prefetch (up to 50 ids per request) amortizes renewals, so a few expiries
// per day cost effectively nothing.
package match

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/library"
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
// log line. Hits counts entries whose Fribb record carries a usable arr id -
// the ID bridge actually resolved an arr id - whether or not the item is in
// the library (a resolved id absent from the library is a missing item, not a
// mapping gap). Unmapped counts every entry the ID bridge could not resolve:
// no Fribb record at all, a record without a usable arr id (counted here even
// when the AniList title fallback links it), or an unusable AniList id.
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
// matches, ID-mapping coverage, and the updated memo to persist: legacy
// entries are migrated onto the expiry policy, renewed lookups are re-stamped,
// and entries still expired at the end of a clean pass are pruned. Degraded
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
	m.migrateMemo(&memo, now)
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
		// cycle's batch either way, so retention costs no AniList traffic.
		pruneExpired(&memo, now)
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
		// needsLookup under a present record means the record is id-less
		// (see aniListNeed): the ID bridge by definition could not resolve
		// an arr id, so the entry counts as Unmapped even when the AniList
		// title fallback below links it - keeping the cycle line's "mapped"
		// an honest count of actual ID-bridge resolutions. The title is the
		// only remaining link to the arr item, so consult AniList.
		r.cov.Unmapped[arr]++
		if matched := r.titleMatch(ctx, e, arr); matched != nil {
			return Match{Item: matched, Entry: *e, Record: *rec, Arr: arr, Source: SourceTitle}
		}
		return Match{Entry: *e, Record: *rec, Arr: arr, Source: SourceUnmapped}
	}
	// The record carries a usable arr id: the ID mapping resolved, so this
	// is a coverage hit whether or not the item is in the library.
	r.cov.Hits[arr]++
	if item != nil {
		return Match{Item: item, Entry: *e, Record: *rec, Arr: arr, Source: SourceID}
	}
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
	return Match{Item: item, Entry: *e, Record: mapping.Record{Type: mapping.NormalizeType(media.Format)}, Arr: item.Arr, Source: SourceTitle}
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
func (li *LibIndex) FindByID(rec *mapping.Record) *library.Item {
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
// see FindByID).
func (li *LibIndex) findMovie(rec *mapping.Record) *library.Item {
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
func (li *LibIndex) findByTitle(titles []string, year int, arr string, log *slog.Logger) *library.Item {
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
		safe := make([]string, len(titles))
		for i, t := range titles {
			safe[i] = runesafe.Sanitize(t)
		}
		log.Debug("title fallback ambiguous, treating as unmapped", "titles", safe, "candidates", len(candidates))
		return nil
	}
}

// titleCandidates returns the distinct library items whose (normalized) title
// or alternate title equals any of titles, optionally restricted to arr.
func (li *LibIndex) titleCandidates(titles []string, arr string) []*library.Item {
	seen := make(map[*library.Item]struct{})
	var candidates []*library.Item
	for _, title := range titles {
		candidates = li.appendTitleCandidates(candidates, seen, title, arr)
	}
	return candidates
}

// appendTitleCandidates appends the items indexed under title's normalized
// key that pass the arr restriction and are not already in seen.
func (li *LibIndex) appendTitleCandidates(candidates []*library.Item, seen map[*library.Item]struct{}, title, arr string) []*library.Item {
	key := titlekey.Normalize(title)
	if key == "" {
		return candidates
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
