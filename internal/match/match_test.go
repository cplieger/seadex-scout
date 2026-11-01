package match

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// fakeAniList is a stub AniListClient returning canned media by AniList ID.
type fakeAniList struct{ media map[int]anilist.Media }

func (f fakeAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	if m, ok := f.media[id]; ok {
		return m, nil
	}
	return anilist.Media{}, anilist.ErrNotFound
}

func (f fakeAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	out := make(map[int]anilist.Media, len(ids))
	for _, id := range ids {
		if m, ok := f.media[id]; ok {
			out[id] = m
		}
	}
	return anilist.BatchResult{Media: out, Verdicts: batchVerdicts(ids, out)}, nil
}

// batchVerdicts builds the per-id verdicts a CLEAN batch over ids produces:
// VerdictFound for every id media answered, VerdictAbsent for the rest (a
// completed chunk said AniList has no such anime).
func batchVerdicts(ids []int, media map[int]anilist.Media) map[int]anilist.Verdict {
	return batchVerdictsAbsentAs(ids, media, anilist.VerdictAbsent)
}

// batchVerdictsAbsentAs is batchVerdicts with the verdict an UNANSWERED id gets
// spelled out, for a fake modelling a chunk whose absences prove nothing
// (VerdictUnverified) or ids no request ever covered (VerdictUnrequested). A
// found id is VerdictFound either way: a poisoned chunk's valid records are
// still valid.
func batchVerdictsAbsentAs(ids []int, media map[int]anilist.Media, absent anilist.Verdict) map[int]anilist.Verdict {
	verdicts := make(map[int]anilist.Verdict, len(ids))
	for _, id := range ids {
		if _, ok := media[id]; ok {
			verdicts[id] = anilist.VerdictFound
			continue
		}
		verdicts[id] = absent
	}
	return verdicts
}

// TestLibIndexRoutesNoWrongArrShadowing covers the index-build side of the
// arr gate: when a Sonarr series and a Radarr movie share the same TMDB id
// (disjoint TV/movie namespaces over one key space), the Radarr route finds
// the movie and the Sonarr route the series regardless of snapshot item order.
func TestLibIndexRoutesNoWrongArrShadowing(t *testing.T) {
	movie := library.Item{Arr: library.ArrRadarr, ArrID: 2, Title: "Some Movie", TmdbID: 20, TvdbID: 10}
	series := library.Item{Arr: library.ArrSonarr, ArrID: 1, Title: "Some Series", TvdbID: 10, TmdbID: 20}
	orders := map[string][]library.Item{
		"movie first":  {movie, series},
		"series first": {series, movie},
	}
	for name, items := range orders {
		t.Run(name, func(t *testing.T) {
			li := NewLibIndex(&library.Snapshot{Items: items})
			rec := &mapping.Record{TvdbID: 10, TmdbMovies: []int{20}}
			if it := li.Radarr(rec); it == nil || it.Arr != library.ArrRadarr {
				t.Errorf("Radarr(rec) = %v, want the Radarr movie (series must not shadow it)", it)
			}
			if it := li.Sonarr(rec); it == nil || it.Arr != library.ArrSonarr {
				t.Errorf("Sonarr(rec) = %v, want the Sonarr series (movie must not shadow it)", it)
			}
		})
	}
}

// TestMatchTitleFallbackOnIdlessRecord covers the gap-fill fallthrough: when
// the mapping has the AniList entry but its record carries no arr id (a split
// mapping), the matcher falls back to the AniList title match and still links
// the entry to the library item - yet the entry counts as Unmapped coverage,
// not a Hit: the ID bridge by definition could not resolve an arr id, so
// "mapped" must not claim it.
func TestMatchTitleFallbackOnIdlessRecord(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Heaven's Feel I", TmdbID: 283984, Year: 2017},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 20791}}}) // no route: the split-mapping gap
	fake := fakeAniList{media: map[int]anilist.Media{
		20791: {Titles: []string{"Heaven's Feel I"}, Format: "MOVIE", Year: 2017},
	}}
	m := New(Config{AniList: fake})

	res := m.Match(t.Context(), []seadex.Entry{{AniListID: 20791}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 1 {
		t.Errorf("expected a match to the Radarr movie, got %+v", got.Item)
	}
	if got.Source != sourceTitle {
		t.Errorf("source = %q, want %q", got.Source, sourceTitle)
	}
	if res.Coverage.Unmapped[library.ArrRadarr] != 1 {
		t.Errorf("coverage unmapped[radarr] = %d, want 1 (an id-less record is an ID-bridge miss even when the title fallback links it)", res.Coverage.Unmapped[library.ArrRadarr])
	}
	if len(res.Coverage.Hits) != 0 {
		t.Errorf("coverage hits = %v, want none (no arr id was resolved by the ID mapping)", res.Coverage.Hits)
	}
}

// countingAniList counts every AniList request - single Fetch AND batched
// FetchMany share the one counter (always answering not-found), so calls == 0
// proves a path spends no AniList traffic of either kind.
type countingAniList struct{ calls int }

func (c *countingAniList) Fetch(_ context.Context, _ int) (anilist.Media, error) {
	c.calls++
	return anilist.Media{}, anilist.ErrNotFound
}

func (c *countingAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	c.calls++
	media := map[int]anilist.Media{}
	return anilist.BatchResult{Media: media, Verdicts: batchVerdicts(ids, media)}, nil
}

// TestMatchNoTitleFallbackWhenRecordHasArrID verifies the AniList title fallback
// is reserved for id-less records: a record that carries an arr id but whose
// anime is not in the library resolves to unmapped WITHOUT an AniList call, so a
// cold cycle does not query AniList for every SeaDex entry the operator lacks.
func TestMatchNoTitleFallbackWhenRecordHasArrID(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "In Library", TvdbID: 111},
	}}
	// The record carries a TVDB id (555) that is absent from the library.
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 999, TvdbID: 555}}})
	fake := &countingAniList{}
	m := New(Config{AniList: fake})

	res := m.Match(t.Context(), []seadex.Entry{{AniListID: 999}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(res.Matches))
	}
	if got := res.Matches[0]; got.InLibrary() || got.Source != sourceUnmapped {
		t.Errorf("want an unmapped miss, got source=%q inLibrary=%v", got.Source, got.InLibrary())
	}
	if fake.calls != 0 {
		t.Errorf("AniList queried %d times; a record with an arr id must not trigger the title fallback", fake.calls)
	}
	if res.Coverage.Hits[library.ArrSonarr] != 1 {
		t.Errorf("coverage hits[sonarr] = %d, want 1 (the ID mapping resolved an arr id; the item is merely absent from the library)", res.Coverage.Hits[library.ArrSonarr])
	}
	if len(res.Coverage.Unmapped) != 0 {
		t.Errorf("coverage unmapped = %v, want none for a resolved-but-absent record", res.Coverage.Unmapped)
	}
}

// batchCountingAniList records batched vs single AniList calls (and batch sizes)
// to prove the matcher pre-warms the memo in one batch rather than one request
// per id-less entry. Fetch/FetchMany resolve ids from the canned media map.
type batchCountingAniList struct {
	media      map[int]anilist.Media
	batchSizes []int
	fetchCalls int
	batchCalls int
}

func (b *batchCountingAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	b.fetchCalls++
	if m, ok := b.media[id]; ok {
		return m, nil
	}
	return anilist.Media{}, anilist.ErrNotFound
}

func (b *batchCountingAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	b.batchCalls++
	b.batchSizes = append(b.batchSizes, len(ids))
	out := make(map[int]anilist.Media, len(ids))
	for _, id := range ids {
		if m, ok := b.media[id]; ok {
			out[id] = m
		}
	}
	return anilist.BatchResult{Media: out, Verdicts: batchVerdicts(ids, out)}, nil
}

// TestMatchBatchesAniListLookups verifies the cold-cycle path: several id-less
// records that need the AniList title fallback are resolved with ONE batched
// request (pre-warming the memo), so the per-entry pass makes zero single
// Fetch calls and every entry still title-matches its library item.
func TestMatchBatchesAniListLookups(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", TmdbID: 100, Year: 2020},
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Movie B", TmdbID: 200, Year: 2021},
		{Arr: library.ArrRadarr, ArrID: 3, Title: "Movie C", TmdbID: 300, Year: 2022},
	}}
	// Three records with no route (split mapping).
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 11},
		{AniListID: 22},
		{AniListID: 33},
	}})
	fake := &batchCountingAniList{media: map[int]anilist.Media{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020},
		22: {Titles: []string{"Movie B"}, Format: "MOVIE", Year: 2021},
		33: {Titles: []string{"Movie C"}, Format: "MOVIE", Year: 2022},
	}}
	m := New(Config{AniList: fake})

	res := m.Match(t.Context(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}, {AniListID: 33}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("want 1 batched AniList request, got %d (batch sizes %v)", fake.batchCalls, fake.batchSizes)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("want 0 single Fetch calls (the batch pre-warms the memo), got %d", fake.fetchCalls)
	}
	matched := 0
	for i := range res.Matches {
		if res.Matches[i].InLibrary() && res.Matches[i].Source == sourceTitle {
			matched++
		}
	}
	if matched != 3 {
		t.Errorf("want 3 title-matched entries, got %d", matched)
	}
}

// degradedAniList fails every lookup with a transient (non-not-found) error,
// modelling an AniList outage or rate-limit exhaustion.
type degradedAniList struct{}

func (degradedAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return anilist.Media{}, context.DeadlineExceeded
}

func (degradedAniList) FetchMany(context.Context, []int) (anilist.BatchResult, error) {
	return anilist.BatchResult{}, context.DeadlineExceeded
}

// TestMatchAniListTransientErrorDegrades covers the degraded path: when a needed
// AniList fallback lookup fails transiently (not ErrNotFound), Match flags the
// result Degraded and reports the id in IncompleteIDs so the caller preserves
// the affected entry's prior findings instead of resolving them, leaves the
// entry unmapped, and does NOT memoize the id (so it is retried next cycle
// rather than cached as a permanent miss).
func TestMatchAniListTransientErrorDegrades(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex(mapping.Source{}) // no mapping record: the entry resolves via AniList
	m := New(Config{AniList: degradedAniList{}})

	res := m.Match(t.Context(), []seadex.Entry{{AniListID: 42}}, snap, idx, Memo{})

	if !res.Degraded {
		t.Error("Degraded = false, want true when a needed AniList lookup fails transiently")
	}
	if got := res.Coverage.Unmapped[arrUnknown]; got != 1 {
		t.Errorf("coverage unmapped[unknown] = %d, want 1 for the failed no-record fallback", got)
	}
	if _, ok := res.IncompleteIDs[42]; !ok || len(res.IncompleteIDs) != 1 {
		t.Errorf("IncompleteIDs = %v, want exactly {42} (the transiently failed lookup)", res.IncompleteIDs)
	}
	if len(res.Matches) != 1 || res.Matches[0].Source != sourceUnmapped {
		t.Errorf("entry should be unmapped on a transient failure, got %+v", res.Matches)
	}
	if _, cached := res.Memo.Entries[42]; cached {
		t.Error("a transient failure must not memoize the id (it must be retried next cycle)")
	}
}

// TestMatchIDLessRecordTransientErrorCountsUnderUnknown is the same outage
// against an entry the mapping DOES have a record for, only one with no route:
// no route names an arr and the failed lookup supplied no format, so the entry
// counts under the unknown bucket, never under an arr it was not routed to.
func TestMatchIDLessRecordTransientErrorCountsUnderUnknown(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 42}}})

	res := New(Config{AniList: degradedAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 42}}, &library.Snapshot{}, idx, Memo{})

	if got := res.Coverage.Unmapped[arrUnknown]; got != 1 {
		t.Errorf("coverage unmapped[unknown] = %d, want 1 (no route names an arr)", got)
	}
	if got := res.Coverage.Unmapped[library.ArrSonarr] + res.Coverage.Unmapped[library.ArrRadarr]; got != 0 {
		t.Errorf("coverage unmapped per arr = %v, want none", res.Coverage.Unmapped)
	}
	if !res.Degraded {
		t.Error("Degraded = false, want true when a needed AniList lookup fails transiently")
	}
	if _, ok := res.IncompleteIDs[42]; !ok || len(res.IncompleteIDs) != 1 {
		t.Errorf("IncompleteIDs = %v, want exactly {42}", res.IncompleteIDs)
	}
	if len(res.Matches) != 1 || res.Matches[0].Source != sourceUnmapped {
		t.Errorf("matches = %+v, want one unmapped entry", res.Matches)
	}
}

// TestMatchTitleFallbackAmbiguousIsUnmapped covers the conservative-match
// invariant plus the no-mapping-record resolution path: an entry with no mapping
// record is resolved through the AniList title fallback (exercising matchEntry's
// record-miss branch and formatArr), and when the normalized title matches more
// than one library item in the same arr the ambiguous set is treated as a miss
// (findByTitle's default branch) rather than guessed.
func TestMatchTitleFallbackAmbiguousIsUnmapped(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Clannad"},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Clannad"},
	}}
	idx := mapping.NewIndex(mapping.Source{}) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		500: {Titles: []string{"Clannad"}, Format: "TV"},
	}}
	m := New(Config{AniList: fake})

	res := m.Match(t.Context(), []seadex.Entry{{AniListID: 500}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(res.Matches))
	}
	if got := res.Matches[0]; got.InLibrary() || got.Source != sourceUnmapped {
		t.Errorf("an ambiguous title set must be unmapped, got source=%q inLibrary=%v", got.Source, got.InLibrary())
	}
}

// TestMatchTitleFallbackYearDisambiguatesAmbiguousTitles pins the
// disambiguation half of the conservative title+year contract: when the
// normalized title matches MORE than one library item, a known year must
// narrow the set to the single right item and link it, instead of treating
// the pre-narrowing multi-candidate set as ambiguous. This is the scenario
// filterByYear exists for (a remake/sequel sharing the original's title).
func TestMatchTitleFallbackYearDisambiguatesAmbiguousTitles(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Clannad", TvdbID: 100, Year: 2007},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Clannad", TvdbID: 200, Year: 2010},
	}}
	idx := mapping.NewIndex(mapping.Source{}) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		500: {Titles: []string{"Clannad"}, Format: "TV", Year: 2010},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 500}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 2 {
		t.Errorf("match item = %+v, want the 2010 series ArrID 2 (the year must disambiguate the two same-titled items)", got.Item)
	}
	if got.Source != sourceTitle {
		t.Errorf("source = %q, want %q", got.Source, sourceTitle)
	}
}

// TestMatchTitleFallbackRejectsWrongYear pins the conservative title+year
// contract: when a year is supplied, it is a hard constraint. A lone library
// item whose normalized title matches but whose year differs must NOT be
// accepted (it would link an id-less entry to the wrong remake/movie); the
// entry stays unmapped instead.
func TestMatchTitleFallbackRejectsWrongYear(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, ArrID: 1, Title: "Clannad", Year: 2007}}}
	li := NewLibIndex(snap)
	if got := li.findByTitle([]string{"Clannad"}, 2008, library.ArrSonarr, slog.New(slog.DiscardHandler)); got != nil {
		t.Fatalf("wrong-year title fallback matched %+v; want nil", got)
	}
}

// TestMatchCancelledContextStopsBeforeEntries pins the pre-cancelled context
// arm: a shutdown mid-cycle must stop before any entry is matched and flag the
// result degraded so the caller preserves prior findings, without spending any
// AniList request.
func TestMatchCancelledContextStopsBeforeEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap := &library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, TvdbID: 123, Title: "Frieren"}}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 154587, TvdbID: 123}}})
	fake := &countingAniList{}
	res := New(Config{AniList: fake}).Match(ctx, []seadex.Entry{{AniListID: 154587}}, snap, idx, Memo{})
	if !res.Degraded {
		t.Error("Degraded = false, want true when matching is interrupted by cancellation")
	}
	if len(res.Matches) != 0 {
		t.Errorf("matches = %d, want 0 when context is already cancelled", len(res.Matches))
	}
	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0 after cancellation", fake.calls)
	}
}

// TestMatchInvalidAniListIDSkipsLookupWithoutDegrading pins the non-positive
// AniList ID guard: garbage upstream data must not spend a rate-limited lookup,
// must not be memoized, and must not degrade the cycle.
func TestMatchInvalidAniListIDSkipsLookupWithoutDegrading(t *testing.T) {
	fake := &countingAniList{}
	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 0}}, &library.Snapshot{}, mapping.NewIndex(mapping.Source{}), Memo{})
	if res.Degraded {
		t.Error("Degraded = true, want false for a non-positive AniList ID")
	}
	if len(res.Matches) != 1 || res.Matches[0].Source != sourceUnmapped || res.Matches[0].Arr != arrUnknown {
		t.Errorf("matches = %+v, want one unknown/unmapped entry", res.Matches)
	}
	if got := res.Coverage.Unmapped[arrUnknown]; got != 1 {
		t.Errorf("unknown unmapped coverage = %d, want 1", got)
	}
	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0 for a non-positive ID", fake.calls)
	}
	if _, cached := res.Memo.Entries[0]; cached {
		t.Error("invalid ID 0 was memoized, want it ignored")
	}
}

// TestMatchResolvesByIDWithoutAniList pins the primary happy path through
// Matcher.Match: an entry whose mapping record carries the arr id resolves to the
// library item as SourceID, counts a coverage hit for its arr, and spends zero
// AniList requests (neither batch nor single).
func TestMatchResolvesByIDWithoutAniList(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren", TvdbID: 123},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 154587, TvdbID: 123}}})
	fake := &countingAniList{}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 154587}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 7 {
		t.Errorf("match item = %+v, want the Sonarr series ArrID 7", got.Item)
	}
	if got.Source != SourceID {
		t.Errorf("source = %q, want %q", got.Source, SourceID)
	}
	if got.Arr != library.ArrSonarr {
		t.Errorf("arr = %q, want %q", got.Arr, library.ArrSonarr)
	}
	if res.Coverage.Hits[library.ArrSonarr] != 1 {
		t.Errorf("coverage hits[sonarr] = %d, want 1", res.Coverage.Hits[library.ArrSonarr])
	}
	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0 for an ID-resolved entry", fake.calls)
	}
	if res.Degraded {
		t.Error("Degraded = true, want false on a clean ID match")
	}
}

// TestMatchTitleFallbackSucceedsWithoutRecord pins the no-mapping-record success
// path: an entry with no mapping record resolves through the AniList lookup and
// links to the single title+year library candidate as sourceTitle, with the arr
// taken from the matched item and no placement, so it is judged on the whole
// series.
func TestMatchTitleFallbackSucceedsWithoutRecord(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 3, Title: "Clannad", TvdbID: 555, Year: 2007},
	}}
	idx := mapping.NewIndex(mapping.Source{}) // no mapping record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		600: {Titles: []string{"Clannad"}, Format: "TV", Year: 2007},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 600}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 3 {
		t.Errorf("match item = %+v, want the Sonarr series ArrID 3", got.Item)
	}
	if got.Source != sourceTitle {
		t.Errorf("source = %q, want %q", got.Source, sourceTitle)
	}
	if got.Arr != library.ArrSonarr {
		t.Errorf("arr = %q, want %q (the matched item's arr)", got.Arr, library.ArrSonarr)
	}
	if kind, _ := align.RecordSeason(&got.Record); kind != align.ScopeWholeSeries || got.Special() {
		t.Errorf("title match scope = %v special %v, want the whole series and no special (the zero Record)", kind, got.Special())
	}
	if res.Coverage.Unmapped[library.ArrSonarr] != 1 {
		t.Errorf("coverage unmapped[sonarr] = %d, want 1 (no ID mapping existed)", res.Coverage.Unmapped[library.ArrSonarr])
	}
}

// TestMatchAltTitleFallbackFiltersArrAndDedupes pins the title-index corners in
// one flow: a record with no route falls to the title fallback; the library item is found via its
// ALTERNATE title; an unnormalizable AniList title ("!!!") is skipped; a
// same-titled item in the WRONG arr is filtered out; and the item matched under
// two of the AniList titles is deduplicated to a single unambiguous candidate.
func TestMatchAltTitleFallbackFiltersArrAndDedupes(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 9, Title: "Frieren: Beyond Journey's End", AltTitles: []string{"Sousou no Frieren"}, Year: 2023},
		{Arr: library.ArrRadarr, ArrID: 10, Title: "Sousou no Frieren", Year: 2023}, // wrong arr: must be filtered
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 154587}}}) // id-less series record
	fake := fakeAniList{media: map[int]anilist.Media{
		154587: {Titles: []string{"!!!", "Sousou no Frieren", "Frieren: Beyond Journey's End"}, Format: "TV", Year: 2023},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 154587}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 9 {
		t.Errorf("match item = %+v, want the Sonarr series ArrID 9 via its alternate title", got.Item)
	}
	if got.Source != sourceTitle {
		t.Errorf("source = %q, want %q", got.Source, sourceTitle)
	}
}

// TestFormatArr pins the AniList-format-to-arr routing used by the no-record
// fallback path: MOVIE routes to Radarr, any other non-empty format to Sonarr,
// and an empty format is unknown (so its coverage is counted under "unknown"
// and the title search is not arr-restricted).
func TestFormatArr(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{format: "MOVIE", want: library.ArrRadarr},
		{format: "movie", want: library.ArrRadarr},
		{format: "TV", want: library.ArrSonarr},
		{format: "OVA", want: library.ArrSonarr},
		{format: "", want: arrUnknown},
		{format: "   ", want: arrUnknown},
	}
	for _, tc := range tests {
		if got := formatArr(tc.format); got != tc.want {
			t.Errorf("formatArr(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}

// TestMatchEmptyFormatTitleFallbackSearchesBothArrs pins that an AniList media
// with an empty format (arr unknown) leaves the title search arr-UNRESTRICTED:
// the entry still title-matches the lone library candidate, its coverage is
// counted under "unknown", and the match's Arr comes from the matched item.
func TestMatchEmptyFormatTitleFallbackSearchesBothArrs(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 4, Title: "Clannad", TvdbID: 555, Year: 2007},
	}}
	idx := mapping.NewIndex(mapping.Source{}) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		610: {Titles: []string{"Clannad"}, Format: "", Year: 2007},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 610}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 4 {
		t.Errorf("match item = %+v, want the Sonarr series despite the unknown format", got.Item)
	}
	if got.Source != sourceTitle {
		t.Errorf("source = %q, want %q", got.Source, sourceTitle)
	}
	if got.Arr != library.ArrSonarr {
		t.Errorf("arr = %q, want %q (taken from the matched item)", got.Arr, library.ArrSonarr)
	}
	if res.Coverage.Unmapped[arrUnknown] != 1 {
		t.Errorf("unknown unmapped coverage = %d, want 1", res.Coverage.Unmapped[arrUnknown])
	}
}

// TestMatchMemoizedSteadyStateSurvivesOutage pins the memo's degradation
// shield (test b of mc-degradation-scoping): with every needed id served by a
// live memo entry, a TOTAL AniList outage triggers no lookup, no degradation,
// and no incomplete ids - the memoized steady state is outage-immune, so a
// long-running daemon does not degrade its cycles over an upstream it never
// needed to consult.
func TestMatchMemoizedSteadyStateSurvivesOutage(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", TmdbID: 100, Year: 2020},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 11}}}) // id-less: the lookup is needed
	memo := Memo{Entries: map[int]MemoEntry{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020, Expiry: time.Now().Add(time.Hour)},
	}}

	res := New(Config{AniList: degradedAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 11}}, snap, idx, memo)

	if res.Degraded {
		t.Error("Degraded = true, want false: the live memo served every needed lookup")
	}
	if len(res.IncompleteIDs) != 0 {
		t.Errorf("IncompleteIDs = %v, want none for a memo-served pass", res.IncompleteIDs)
	}
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != sourceTitle {
		t.Errorf("matches = %+v, want the one memo-served title match", res.Matches)
	}
}

// TestMatchIncompleteIDsScope pins Result.IncompleteIDs' membership rule: the
// set carries exactly the AniList ids whose NEEDED lookup failed transiently
// this pass. An id resolved by the ID bridge (no lookup), one answered by the
// batch prefetch, and one answered with a definitive not-found are complete
// and stay out; a total batch outage puts every pending id in through the
// fast-fail gate.
func TestMatchIncompleteIDsScope(t *testing.T) {
	t.Run("partial outage marks only the per-id transient failure", func(t *testing.T) {
		snap := &library.Snapshot{Items: []library.Item{
			{Arr: library.ArrSonarr, ArrID: 1, Title: "In Library", TvdbID: 111},
		}}
		idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 154587, TvdbID: 111}}})
		// FetchMany returns 40's media with an error (a failed later chunk):
		// a PARTIAL batch failure, so 41 and 42 fall to the per-id Fetch,
		// where 41 gets a definitive not-found and 42 a transient failure.
		fake := &partialThenPerIDAniList{media: map[int]anilist.Media{
			40: {Titles: []string{"Returned"}, Format: "TV"},
		}}

		res := New(Config{AniList: fake}).Match(t.Context(),
			[]seadex.Entry{{AniListID: 154587}, {AniListID: 40}, {AniListID: 41}, {AniListID: 42}},
			snap, idx, Memo{})

		if !res.Degraded {
			t.Error("Degraded = false, want true: one needed lookup failed transiently")
		}
		if _, ok := res.IncompleteIDs[42]; !ok || len(res.IncompleteIDs) != 1 {
			t.Errorf("IncompleteIDs = %v, want exactly {42}: the ID-resolved, batch-answered, and not-found ids are complete", res.IncompleteIDs)
		}
		if ent, ok := res.Memo.Entries[41]; !ok || !ent.NotFound {
			t.Errorf("memo[41] = %+v (present=%v), want the definitive not-found memoized", ent, ok)
		}
		if _, cached := res.Memo.Entries[42]; cached {
			t.Error("the transiently failed id 42 must stay un-memoized for next cycle's retry")
		}
	})
	t.Run("total outage marks every pending id", func(t *testing.T) {
		snap := &library.Snapshot{}
		idx := mapping.NewIndex(mapping.Source{})

		res := New(Config{AniList: degradedAniList{}}).Match(t.Context(),
			[]seadex.Entry{{AniListID: 41}, {AniListID: 42}}, snap, idx, Memo{})

		if !res.Degraded {
			t.Error("Degraded = false, want true on a total AniList outage")
		}
		if len(res.IncompleteIDs) != 2 {
			t.Errorf("IncompleteIDs = %v, want both pending ids (the outage fast-fail affects each needed entry)", res.IncompleteIDs)
		}
		for _, id := range []int{41, 42} {
			if _, ok := res.IncompleteIDs[id]; !ok {
				t.Errorf("IncompleteIDs missing %d: %v", id, res.IncompleteIDs)
			}
		}
	})
}

// partialThenPerIDAniList models a partial AniList incident for the
// IncompleteIDs scope test: FetchMany answers the ids present in media but
// fails the batch (a failed later chunk), and the per-id Fetch answers 41
// with a definitive not-found while everything else fails transiently.
type partialThenPerIDAniList struct{ media map[int]anilist.Media }

func (*partialThenPerIDAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	if id == 41 {
		return anilist.Media{}, anilist.ErrNotFound
	}
	return anilist.Media{}, context.DeadlineExceeded
}

func (p *partialThenPerIDAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	out := make(map[int]anilist.Media)
	for _, id := range ids {
		if m, ok := p.media[id]; ok {
			out[id] = m
		}
	}
	// A PARTIAL failure: a chunk completed (so the result is usable) and a
	// later one did not, so an id this batch did not answer proves nothing and
	// keeps the per-id fallback.
	return anilist.BatchResult{
		Media:    out,
		Verdicts: batchVerdictsAbsentAs(ids, out, anilist.VerdictUnverified),
	}, context.DeadlineExceeded
}

// TestNewLibIndexNilSnapshot pins the defensive nil-snapshot guard: a nil
// snapshot yields an empty but usable index whose lookups all miss instead of
// panicking.
func TestNewLibIndexNilSnapshot(t *testing.T) {
	li := NewLibIndex(nil)
	if li == nil {
		t.Fatal("NewLibIndex(nil) = nil, want an empty index")
	}
	if it := li.Sonarr(&mapping.Record{TvdbID: 10}); it != nil {
		t.Errorf("Sonarr on an empty index = %+v, want nil", it)
	}
	if it := li.Radarr(&mapping.Record{TmdbMovies: []int{20}}); it != nil {
		t.Errorf("Radarr on an empty index = %+v, want nil", it)
	}
	if got := li.findByTitle([]string{"Clannad"}, 0, library.ArrSonarr, slog.New(slog.DiscardHandler)); got != nil {
		t.Errorf("findByTitle on an empty index = %+v, want nil", got)
	}
}

// TestMatchTitleFallbackKeepsUnknownYearItem pins filterByYear's documented
// keep-unknown contract: a library item whose year is unknown (0) survives
// narrowing by a KNOWN AniList year - absence of year evidence is not a
// mismatch - so an id-less entry still title-matches a year-less library
// item. Hard-failing that item was a real one-direction-fatal asymmetry the
// current code documents; no other test exercises a year-less candidate
// against a known year.
func TestMatchTitleFallbackKeepsUnknownYearItem(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 6, Title: "Clannad", TvdbID: 555},
	}}
	idx := mapping.NewIndex(mapping.Source{}) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		620: {Titles: []string{"Clannad"}, Format: "TV", Year: 2007},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 620}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 6 {
		t.Errorf("match item = %+v, want the year-less Sonarr series kept through year narrowing", got.Item)
	}
	if got.Source != sourceTitle {
		t.Errorf("source = %q, want %q", got.Source, sourceTitle)
	}
}

// TestFindByTitleUnknownYearSiblingLeavesSetAmbiguous pins the documented
// consequence of filterByYear KEEPING unknown-year candidates: when a
// year-less library item shares a normalized title with the year-matching one,
// the surviving set stays ambiguous, so the conservative single-candidate gate
// refuses the match rather than picking the exact-year item. The refusal is
// what keeps a "fix" that prefers the exact-year candidate from silently
// linking an id-less SeaDex entry to a same-titled remake.
func TestFindByTitleUnknownYearSiblingLeavesSetAmbiguous(t *testing.T) {
	li := NewLibIndex(&library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Clannad", Year: 2007},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Clannad"}, // year unknown: kept by narrowing
	}})
	if got := li.findByTitle([]string{"Clannad"}, 2007, library.ArrSonarr, slog.New(slog.DiscardHandler)); got != nil {
		t.Fatalf("findByTitle matched %+v; want nil: the kept unknown-year sibling leaves the set ambiguous", got)
	}
}

// TestMatchIDLessRecordRoutesByAniListFormat covers a record with no route:
// AniList's format picks the arr the title search runs in, so a MOVIE format
// finds the Radarr movie rather than the same-titled series, and coverage
// counts there.
func TestMatchIDLessRecordRoutesByAniListFormat(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Kizumonogatari", TvdbID: 300, Year: 2016},
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Kizumonogatari", TmdbID: 400, Year: 2016},
	}}
	// The record exists but carries no route: the split mapping gap.
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 21403}}})
	fake := fakeAniList{media: map[int]anilist.Media{
		21403: {Titles: []string{"Kizumonogatari"}, Format: "MOVIE", Year: 2016},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 21403}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.Arr != library.ArrRadarr || got.Item.ArrID != 2 {
		t.Errorf("match item = %+v, want the Radarr movie the AniList format names", got.Item)
	}
	if got.Arr != library.ArrRadarr {
		t.Errorf("match arr = %q, want %q", got.Arr, library.ArrRadarr)
	}
	if res.Coverage.Unmapped[library.ArrRadarr] != 1 {
		t.Errorf("coverage unmapped[radarr] = %d, want 1 (counted under the resolved arr)", res.Coverage.Unmapped[library.ArrRadarr])
	}
	if res.Coverage.Unmapped[library.ArrSonarr] != 0 {
		t.Errorf("coverage unmapped[sonarr] = %d, want 0 (the record must not be attributed to Sonarr)", res.Coverage.Unmapped[library.ArrSonarr])
	}
}

// TestMatchIDLessRecordUnknownFormatSearchesBothArrs pins the other half: when
// AniList names no usable format, formatArr yields arrUnknown and the title
// search stays UNRESTRICTED, so the Radarr movie, the only candidate, is linked
// and coverage counts under the unknown arr.
func TestMatchIDLessRecordUnknownFormatSearchesBothArrs(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Kizumonogatari", TmdbID: 400, Year: 2016},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 21403}}})
	fake := fakeAniList{media: map[int]anilist.Media{
		21403: {Titles: []string{"Kizumonogatari"}, Year: 2016},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 21403}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.Arr != library.ArrRadarr {
		t.Errorf("match item = %+v, want the Radarr movie an unrestricted search finds", got.Item)
	}
	if got.Arr != library.ArrRadarr {
		t.Errorf("match arr = %q, want %q (the matched item's arr)", got.Arr, library.ArrRadarr)
	}
	if res.Coverage.Unmapped[arrUnknown] != 1 {
		t.Errorf("coverage unmapped[%s] = %d, want 1", arrUnknown, res.Coverage.Unmapped[arrUnknown])
	}
}

// TestMatchRecordWithMovieIDResolvesByID pins that a TMDB movie id is the
// Radarr route whatever AniList says: the exact Radarr movie is linked though
// its arr title does not match the AniList title at all, and it counts as an
// ID hit rather than unmapped title coverage.
func TestMatchRecordWithMovieIDResolvesByID(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Wound Tale", TmdbID: 400, Year: 2016},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 21403, TmdbMovies: []int{400}}}})
	fake := fakeAniList{media: map[int]anilist.Media{
		21403: {Titles: []string{"Kizumonogatari"}, Format: "MOVIE", Year: 2016},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 21403}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 2 {
		t.Errorf("match item = %+v, want the Radarr movie the TMDB id names", got.Item)
	}
	if got.Source != SourceID {
		t.Errorf("source = %q, want %q", got.Source, SourceID)
	}
	if res.Coverage.Hits[library.ArrRadarr] != 1 {
		t.Errorf("coverage hits[radarr] = %d, want 1", res.Coverage.Hits[library.ArrRadarr])
	}
	if res.Coverage.Unmapped[library.ArrRadarr] != 0 {
		t.Errorf("coverage unmapped[radarr] = %d, want 0 (an ID resolution is not unmapped)", res.Coverage.Unmapped[library.ArrRadarr])
	}
}

// TestMatchRecordWithAbsentMovieIDStaysUnmapped is the same route seen
// from the miss side: the TMDB id PROVES the intended movie is not in the
// library, so a same-titled Radarr movie must not be linked in its place. The
// entry stays unmapped with no title fallback.
func TestMatchRecordWithAbsentMovieIDStaysUnmapped(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Kizumonogatari", TmdbID: 400, Year: 2016},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 21403, TmdbMovies: []int{999}}}})
	fake := fakeAniList{media: map[int]anilist.Media{
		21403: {Titles: []string{"Kizumonogatari"}, Format: "MOVIE", Year: 2016},
	}}

	res := New(Config{AniList: fake}).Match(t.Context(), []seadex.Entry{{AniListID: 21403}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if got.InLibrary() {
		t.Fatalf("match item = %+v, want none: the usable id proves the intended movie is absent", got.Item)
	}
	if got.Source != sourceUnmapped {
		t.Errorf("source = %q, want %q", got.Source, sourceUnmapped)
	}
	if res.Coverage.Hits[library.ArrRadarr] != 1 {
		t.Errorf("coverage hits[radarr] = %d, want 1 (the ID mapping resolved even though the library missed)", res.Coverage.Hits[library.ArrRadarr])
	}
}

func TestLibIndexRadarrReadsEveryTMDBMovieID(t *testing.T) {
	li := NewLibIndex(&library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Heaven's Feel I", TmdbID: 600},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Some Series", TvdbID: 700, TmdbID: 900},
		{Arr: library.ArrRadarr, ArrID: 3, Title: "Other Movie", TmdbID: 800},
	}})
	tests := []struct {
		name   string
		rec    mapping.Record
		wantID int
	}{
		{"a movie tmdb id resolves the radarr movie", mapping.Record{TmdbMovies: []int{600}}, 1},
		{"a later id resolves when the first misses", mapping.Record{TmdbMovies: []int{999, 800}}, 3},
		{"the first listed id wins", mapping.Record{TmdbMovies: []int{800, 600}}, 3},
		{"a tvdb id never reaches radarr", mapping.Record{TvdbID: 700}, 0},
		{"a sonarr series' tmdb id is not a radarr movie", mapping.Record{TmdbMovies: []int{900}}, 0},
		{"an absent movie tmdb id resolves nothing", mapping.Record{TmdbMovies: []int{999}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := tt.rec
			got := li.Radarr(&rec)
			if tt.wantID == 0 {
				if got != nil {
					t.Fatalf("Radarr() = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.ArrID != tt.wantID {
				t.Fatalf("Radarr() = %+v, want the item with ArrID %d", got, tt.wantID)
			}
		})
	}
}

// TestMatchRecordWithOnlyAMovieIDResolvesRadarrByID pins a record carrying
// only a TMDB movie id: it links the operator's Radarr copy by ID and counts as
// a Radarr coverage hit, and the AniList fake holds no media at all, so the
// resolution costs no rate-limited lookup.
func TestMatchRecordWithOnlyAMovieIDResolvesRadarrByID(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 7, Title: "Heaven's Feel III", TmdbID: 400, Year: 2020},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 102351, TmdbMovies: []int{400}}}})

	res := New(Config{AniList: fakeAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 102351}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 7 {
		t.Errorf("match item = %+v, want the Radarr movie the record's movie id names", got.Item)
	}
	if got.Source != SourceID {
		t.Errorf("source = %q, want %q", got.Source, SourceID)
	}
	if got.Arr != library.ArrRadarr {
		t.Errorf("match arr = %q, want %q", got.Arr, library.ArrRadarr)
	}
	if res.Coverage.Hits[library.ArrRadarr] != 1 {
		t.Errorf("coverage hits[radarr] = %d, want 1", res.Coverage.Hits[library.ArrRadarr])
	}
	if res.Coverage.Hits[library.ArrSonarr] != 0 || res.Coverage.Unmapped[library.ArrSonarr] != 0 {
		t.Errorf("coverage attributed to sonarr: hits=%d unmapped=%d, want 0/0",
			res.Coverage.Hits[library.ArrSonarr], res.Coverage.Unmapped[library.ArrSonarr])
	}
	if res.Degraded {
		t.Error("result degraded: the ID resolution must not consult AniList at all")
	}
}

func TestMatchSeriesRecordMissingFromSonarrResolvesItsFilm(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 7, Title: "Some Film", TmdbID: 400, Year: 2020},
	}}
	rec := mapping.Record{AniListID: 102352, TvdbID: 79000, TmdbMovies: []int{400}, SeasonKind: mapping.SeasonPresent}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{rec}})

	res := New(Config{AniList: fakeAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 102352}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	if got := res.Matches[0]; !got.InLibrary() || got.Item.ArrID != 7 || got.Arr != library.ArrRadarr || got.Source != SourceID {
		t.Errorf("match = item %+v arr %q source %q, want the Radarr film by ID", got.Item, got.Arr, got.Source)
	}
	if res.Coverage.Hits[library.ArrRadarr] != 1 || res.Coverage.Hits[library.ArrSonarr] != 1 {
		t.Errorf("coverage hits = %v, want one hit per route, the absent series included", res.Coverage.Hits)
	}
}

// TestMatchStampsSiblingSeasons pins the producer rule: mapping
// semantics are resolved once, where the record and the index meet, so align
// never reads the map a second time. The matcher stamps the seasons OTHER records
// on the same tvdb id map positively; a record whose siblings map nothing, and an
// entry with no record at all, stamp nil.
func TestMatchStampsSiblingSeasons(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Gintama", TvdbID: 79895},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Frieren", TvdbID: 424536},
	}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 918, TvdbID: 79895, SeasonKind: mapping.SeasonAbsent},
		{AniListID: 100, TvdbID: 79895, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 5},
		{AniListID: 101, TvdbID: 79895, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 10},
		{AniListID: 154587, TvdbID: 424536, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
	}})
	entries := []seadex.Entry{{AniListID: 918}, {AniListID: 154587}}
	m := New(Config{AniList: fakeAniList{}})

	res := m.Match(t.Context(), entries, snap, idx, Memo{})

	want := map[int][]int{918: {5, 10}, 154587: nil}
	if len(res.Matches) != len(entries) {
		t.Fatalf("matches = %d, want %d", len(res.Matches), len(entries))
	}
	for _, got := range res.Matches {
		if !slices.Equal(got.SiblingSeasons, want[got.Entry.AniListID]) {
			t.Errorf("alID %d SiblingSeasons = %v, want %v", got.Entry.AniListID, got.SiblingSeasons, want[got.Entry.AniListID])
		}
	}
}

// TestMatchStampsOwnSeasons pins the same producer rule for the record's
// season ranges: the matcher is where the record and the index meet, so it
// stamps the entry's own TVDB season ranges onto the Match; a record the map
// places nowhere carries nil.
func TestMatchStampsOwnSeasons(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Fairy Tail", TvdbID: 114801},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Frieren", TvdbID: 424536},
	}}
	ranges := []mapping.SeasonRange{{Season: 5, First: 1, Last: 51}, {Season: 6, First: 52, Last: 90}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 20626, TvdbID: 114801, AniDBID: 9980, SeasonKind: mapping.SeasonAbsent},
		{AniListID: 154587, TvdbID: 424536, AniDBID: 17617, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
	}, Mappings: map[int]mapping.Mapping{9980: {Seasons: ranges, SeasonsTvdb: 114801}}})
	entries := []seadex.Entry{{AniListID: 20626}, {AniListID: 154587}}
	m := New(Config{AniList: fakeAniList{}})

	res := m.Match(t.Context(), entries, snap, idx, Memo{})

	want := map[int][]mapping.SeasonRange{20626: ranges, 154587: nil}
	if len(res.Matches) != len(entries) {
		t.Fatalf("matches = %d, want %d", len(res.Matches), len(entries))
	}
	for _, got := range res.Matches {
		if !slices.Equal(got.Seasons, want[got.Entry.AniListID]) {
			t.Errorf("alID %d Seasons = %v, want %v", got.Entry.AniListID, got.Seasons, want[got.Entry.AniListID])
		}
	}
}
