// Package match links SeaDex entries to library items. An entry's mapping
// record (overrides already applied) routes it: a TMDB movie id to Radarr, a
// TVDB id to Sonarr, both to both, each route one copy. An entry with no route
// falls back to an AniList title lookup plus a conservative
// normalized-title-plus-year match against the library.
package match

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/mediatype"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/titlekey"
)

// arrUnknown labels coverage for an entry whose arr could not be determined.
const arrUnknown = "unknown"

type source string

const (
	// SourceID means the AniList ID resolved to an arr ID via the mapping.
	SourceID source = "id"
	// sourceTitle means the AniList title fallback matched a library item.
	sourceTitle source = "title"
	// sourceUnmapped means no library item was found for the entry.
	sourceUnmapped source = "unmapped"
)

// Match is the result of linking one SeaDex entry.
type Match struct {
	Item *library.Item
	// SiblingSeasons are the positive TVDB seasons OTHER mapping records sharing
	// this record's tvdb id map (mapping.Index.SiblingSeasons), resolved here
	// because the matcher is where the record and the index meet. A whole-series
	// comparison drops them from its aggregate; every other scope ignores them.
	// Nil for a record with no tvdb id and for an unmapped entry.
	SiblingSeasons []int
	// Seasons are the entry's OWN TVDB season ranges from the map
	// (mapping.Index.MappingFor), nil when the map names none. A whole-series
	// comparison judges exactly these seasons when present, so a split show's
	// entries are each judged against their own run.
	Seasons []mapping.SeasonRange
	// Specials are the TVDB season-0 episodes the entry IS
	// (mapping.Index.MappingFor), nil when the map does not place it there or
	// its record is not offered on season 0 (align.ScopeOffered).
	Specials []int
	// Episodes are the episodes of its positive TVDB season the entry IS
	// (mapping.Index.MappingFor), nil when the map does not place it there or
	// its record is not scoped to that season (align.ScopeSeason).
	Episodes []int
	// Uncompared is the entry's copy in the other arr when copies judged only
	// one: it holds no file for the entry. align.ClaimsCoverage decides whether
	// the entry covers it.
	Uncompared *library.Item
	Arr        string
	Source     source
	Entry      seadex.Entry
	Record     mapping.Record
}

// InLibrary reports whether the entry was matched to a library item.
func (m *Match) InLibrary() bool { return m.Item != nil }

// Special reports whether the copy is a special: a Sonarr copy whose record
// lands only on season 0 (mapping.Record.SonarrSpecial). A Radarr copy and a
// title-fallback match never are.
func (m *Match) Special() bool { return m.Arr == library.ArrSonarr && m.Record.SonarrSpecial() }

// SpecialsUnknown reports whether the entry is placed on season-0 episodes of
// a series whose season-0 files are unknown (library.Item.SpecialsUnknown):
// such an entry has no verdict this pass, which is not a resolved one.
func (m *Match) SpecialsUnknown() bool {
	return len(m.Specials) > 0 && m.InLibrary() && m.Item.SpecialsUnknown()
}

// AlignEntry is the match as align.Decide reads it. It aliases m.Record.
func (m *Match) AlignEntry() *align.Entry {
	return &align.Entry{Record: &m.Record, SiblingSeasons: m.SiblingSeasons, Seasons: m.Seasons, Specials: m.Specials, Episodes: m.Episodes, AniListID: m.Entry.AniListID}
}

// coverage counts ID-mapping outcomes per arr for the cycle-complete coverage
// log line. Hits counts route copies, so a both-routes entry is a hit in each
// arr, whether or not the item is in the library (a routed id absent from the
// library is a missing item, not a mapping gap).
type coverage struct {
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
	Coverage      coverage
	Memo          Memo
	IncompleteIDs map[int]struct{}
	// HiddenIDs are the entries an in-library copy was dropped from (a special
	// with specials excluded, or a series the map places the film on no unit
	// of). They are evaluated though no match carries them, so a prior finding
	// on the dropped copy resolves.
	HiddenIDs map[int]struct{}
	Matches   []Match
	Degraded  bool
}

// Config configures a Matcher.
type Config struct {
	AniList AniListClient
	// Logger may be nil: slog.Default.
	Logger *slog.Logger
	// ExcludeSpecials drops every copy that is a special (Match.Special).
	ExcludeSpecials bool
}

// Matcher links entries using the mapping index and the AniList fallback.
type Matcher struct {
	anilist AniListClient
	log     *slog.Logger
	// now and rand feed the memo-expiry policy (the run clock and the TTL
	// jitter draw). New fixes them to time.Now and rand.Float64; tests override
	// the fields for deterministic, sleep-free expiry coverage.
	now             func() time.Time
	rand            func() float64
	excludeSpecials bool
}

// New builds a Matcher.
func New(cfg Config) *Matcher {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{
		anilist:         cfg.AniList,
		log:             logger,
		now:             time.Now,
		rand:            rand.Float64,
		excludeSpecials: cfg.ExcludeSpecials,
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
	cov := coverage{Hits: make(map[string]int), Unmapped: make(map[string]int)}
	outage := m.prefetch(ctx, entries, idx, &memo, now)
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
		matches = append(matches, run.matchEntry(ctx, &entries[i])...)
	}
	// Cancellation can arrive while the final entry is being matched, after
	// the loop's boundary check.
	if ctx.Err() != nil {
		run.degraded = true
	}
	// Match does NOT prune the memo.
	return Result{at: now, Coverage: cov, Memo: memo, Matches: matches, Degraded: run.degraded, IncompleteIDs: run.incomplete, HiddenIDs: run.hidden}
}

// PruneMemo garbage-collects res.Memo against the WHOLE SeaDex catalogue,
// in place.
//
// It takes the Result rather than the memo because both of its preconditions are
// non-negotiable and both live there.
func PruneMemo(res *Result, catalogue []seadex.Entry) {
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
	cov  *coverage
	// gate carries the fast-fail state for per-id AniList lookups: ids covered
	// by a totally-failed batch prefetch and, once the consecutive-failure
	// breaker trips, every remaining uncached id fail fast instead of
	// re-hitting the down upstream.
	gate *lookupGate
	// incomplete accumulates the AniList ids whose needed lookup failed
	// transiently this pass (see markIncomplete); Match surfaces it as
	// Result.IncompleteIDs. Nil until the first failure.
	incomplete map[int]struct{}
	// hidden accumulates Result.HiddenIDs. Nil until the first drop.
	hidden map[int]struct{}
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
// (the per-entry pass) consult, so the two cannot drift. Only an entry with
// no record, or a record with no route, needs a lookup.
func aniListNeed(alID int, idx *mapping.Index) (rec mapping.Record, recOK, needsLookup bool) {
	if alID <= 0 {
		return mapping.Record{}, false, false
	}
	rec, recOK = idx.Lookup(alID)
	return rec, recOK, !recOK || !rec.HasRoute()
}

// matchEntry links one entry: its route copies when its record has a route,
// else the AniList title fallback. The lookup trigger is aniListNeed, shared
// with the batch prefetch. r.gate fast-fails per-id AniList lookups doomed by
// an outage (see matchRun).
func (r *matchRun) matchEntry(ctx context.Context, e *seadex.Entry) []Match {
	rec, recOK, needsLookup := aniListNeed(e.AniListID, r.idx)
	switch {
	case !recOK && !needsLookup:
		// Non-positive AniList id: it can never resolve, so do not spend a
		// rate-limited AniList request confirming it (or degrade the whole
		// cycle when that request fails transiently).
		r.cov.Unmapped[arrUnknown]++
		return []Match{{Entry: *e, Arr: arrUnknown, Source: sourceUnmapped}}
	case !recOK:
		return []Match{r.matchUnmappedEntry(ctx, e)}
	case needsLookup:
		return []Match{r.matchIDLessEntry(ctx, e, &rec)}
	}
	return r.matchMappedEntry(e, &rec)
}

// matchMappedEntry builds one copy per route of a routed record and returns
// the ones copies judges. A routed entry is never looked up on AniList, in
// the library or not: that keeps the fallback off the thousands of SeaDex
// entries the operator does not have.
func (r *matchRun) matchMappedEntry(e *seadex.Entry, rec *mapping.Record) []Match {
	var radarr, sonarr *Match
	if rec.RadarrRoute() {
		radarr = r.routeCopy(e, rec, r.lib.Radarr(rec), library.ArrRadarr)
	}
	if rec.SonarrRoute() {
		sonarr = r.routeCopy(e, rec, r.lib.Sonarr(rec), library.ArrSonarr)
	}
	return r.copies(radarr, sonarr)
}

// routeCopy is the entry's copy in arr, item nil when the library lacks it,
// counted as a coverage hit there either way.
func (r *matchRun) routeCopy(e *seadex.Entry, rec *mapping.Record, item *library.Item, arr string) *Match {
	r.cov.Hits[arr]++
	src := sourceUnmapped
	if item != nil {
		src = SourceID
	}
	m := r.mapped(&Match{Item: item, Entry: *e, Record: *rec, Arr: arr, Source: src})
	return &m
}

func (r *matchRun) mapped(m *Match) Match {
	m.SiblingSeasons = r.idx.SiblingSeasons(&m.Record)
	if facts, ok := r.idx.MappingFor(&m.Record); ok {
		m.Seasons = facts.Seasons
		switch kind, _ := align.RecordSeason(&m.Record); kind {
		case align.ScopeOffered:
			m.Specials = facts.Specials
		case align.ScopeSeason:
			m.Episodes = facts.Episodes
		}
	}
	return *m
}

// copies decides which of an entry's route copies are judged, nil meaning the
// route is absent. A copy left out is never returned, so no consumer filters
// one again: with specials excluded the Sonarr copy goes before the choice,
// and the Radarr copy is then judged alone.
func (r *matchRun) copies(radarr, sonarr *Match) []Match {
	sonarr = r.keptSonarr(radarr, sonarr)
	switch {
	case radarr == nil && sonarr == nil:
		return nil
	case sonarr == nil:
		return []Match{*radarr}
	case radarr == nil:
		return []Match{*sonarr}
	case !radarr.InLibrary() || !sonarr.InLibrary():
		return []Match{*radarr, *sonarr}
	}
	switch film, series := align.HoldsFile(radarr.Item, radarr.AlignEntry()), align.HoldsFile(sonarr.Item, sonarr.AlignEntry()); {
	case film && series:
		return []Match{*radarr, *sonarr}
	case series:
		sonarr.Uncompared = radarr.Item
		return []Match{*sonarr}
	default:
		// With no file in either, the Radarr copy is judged: a TMDB route means
		// every episode of the entry states that film.
		radarr.Uncompared = sonarr.Item
		return []Match{*radarr}
	}
}

// keptSonarr is the Sonarr copy left after the specials switch and the
// placement gate, nil when either drops it. A dropped in-library copy is
// recorded in r.hidden.
func (r *matchRun) keptSonarr(radarr, sonarr *Match) *Match {
	if sonarr == nil {
		return nil
	}
	hiddenSpecial := r.m.excludeSpecials && sonarr.Special()
	// A film's series is a copy only where the map places the film on some
	// unit of it: otherwise the series' other files would be judged as its own.
	unplacedSeries := radarr != nil && !align.PlacedInSeries(sonarr.AlignEntry())
	if !hiddenSpecial && !unplacedSeries {
		return sonarr
	}
	if sonarr.InLibrary() {
		if r.hidden == nil {
			r.hidden = make(map[int]struct{})
		}
		r.hidden[sonarr.Entry.AniListID] = struct{}{}
	}
	return nil
}

// matchUnmappedEntry links an entry with no mapping record through the AniList
// title fallback, counting it as unmapped coverage either way. A title match
// carries the zero Record: no placement, so a Sonarr match is the whole series.
func (r *matchRun) matchUnmappedEntry(ctx context.Context, e *seadex.Entry) Match {
	media, ok := r.lookupAniList(ctx, e.AniListID)
	if !ok {
		r.cov.Unmapped[arrUnknown]++
		return Match{Entry: *e, Arr: arrUnknown, Source: sourceUnmapped}
	}
	arr := formatArr(media.Format)
	r.cov.Unmapped[arr]++
	item := r.lib.findByTitle(media.Titles, media.Year, arr, r.m.log)
	if item == nil {
		return Match{Entry: *e, Arr: arr, Source: sourceUnmapped}
	}
	return Match{Item: item, Entry: *e, Arr: item.Arr, Source: sourceTitle}
}

// matchIDLessEntry links an entry whose mapping record has no route, where the
// AniList title is the only remaining link to the arr item: the format picks
// the arr, then the normalized title + year matches within it. It counts as
// unmapped coverage even when the title links it, keeping the cycle line's
// "mapped" an honest count of route copies.
func (r *matchRun) matchIDLessEntry(ctx context.Context, e *seadex.Entry, rec *mapping.Record) Match {
	media, ok := r.lookupAniList(ctx, e.AniListID)
	if !ok {
		r.cov.Unmapped[arrUnknown]++
		return Match{Entry: *e, Record: *rec, Arr: arrUnknown, Source: sourceUnmapped}
	}
	arr := formatArr(media.Format)
	r.cov.Unmapped[arr]++
	if matched := r.lib.findByTitle(media.Titles, media.Year, arr, r.m.log); matched != nil {
		return r.mapped(&Match{Item: matched, Entry: *e, Record: *rec, Arr: matched.Arr, Source: sourceTitle})
	}
	return r.mapped(&Match{Entry: *e, Record: *rec, Arr: arr, Source: sourceUnmapped})
}

// formatArr is the arr an AniList format's title search runs in: a film
// searches Radarr, any other format Sonarr, and an empty one both
// (arrUnknown).
func formatArr(format string) string {
	switch format = mediatype.Normalize(format); {
	case format == "":
		return arrUnknown
	case mediatype.IsMovie(format):
		return library.ArrRadarr
	}
	return library.ArrSonarr
}

// --- LibIndex: library snapshot lookup indexes (by arr ID and normalized title) ---

// LibIndex indexes a library snapshot by external ID and normalized title.
// Shared by the matcher and the feed-info builder (scout's feedInfo).
type LibIndex struct {
	byTvdb  map[int]*library.Item
	byTmdb  map[int]*library.Item
	byTitle map[string][]*library.Item
}

// NewLibIndex builds the lookup indexes over a snapshot's items.
func NewLibIndex(snap *library.Snapshot) *LibIndex {
	li := &LibIndex{
		byTvdb:  make(map[int]*library.Item),
		byTmdb:  make(map[int]*library.Item),
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

// indexIDs adds an item's external IDs to the ID indexes of its arr: byTvdb
// holds only Sonarr items, byTmdb only Radarr items, so for every reader a map
// miss is the arr gate.
func (li *LibIndex) indexIDs(it *library.Item) {
	switch it.Arr {
	case library.ArrSonarr:
		if it.TvdbID > 0 { // positive ids only: Sonarr looks a record with no TVDB id up under 0
			li.byTvdb[it.TvdbID] = it
		}
	case library.ArrRadarr:
		if it.TmdbID > 0 {
			li.byTmdb[it.TmdbID] = it
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

// Radarr is the Radarr film named by the first of rec's TMDB movie ids the
// library holds, or nil.
func (li *LibIndex) Radarr(rec *mapping.Record) *library.Item {
	for _, id := range rec.TmdbMovies { // canonicalize keeps only positive ids
		if it := li.byTmdb[id]; it != nil { // byTmdb holds only Radarr items
			return it
		}
	}
	return nil
}

// Sonarr is the Sonarr series rec's TVDB id names, or nil.
func (li *LibIndex) Sonarr(rec *mapping.Record) *library.Item {
	return li.byTvdb[rec.TvdbID] // byTvdb is Sonarr-only, positive ids only (indexIDs)
}

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
