package match

import (
	"context"
	"log/slog"
	"testing"
	"time"

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

func (f fakeAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	out := make(map[int]anilist.Media, len(ids))
	for _, id := range ids {
		if m, ok := f.media[id]; ok {
			out[id] = m
		}
	}
	return out, nil
}

// TestFindByIDArrConsistency covers the arr-gate: a MOVIE record must resolve
// only to a Radarr movie and a series record only to a Sonarr series, so a movie
// whose Fribb record shares an IMDb id with a same-named Sonarr series (TVDB
// conflates them) does not silently mis-link.
func TestFindByIDArrConsistency(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Death Parade", TvdbID: 10, ImdbID: "tt4279012"},
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Some Movie", TmdbID: 20, ImdbID: "tt2222222"},
	}}
	li := NewLibIndex(snap)

	// A movie record whose IMDb id collides with the Sonarr series must not match.
	movieCollide := &mapping.Record{Type: "MOVIE", IMDbIDs: []string{"tt4279012"}}
	if it := li.FindByID(movieCollide); it != nil {
		t.Errorf("movie record must not match a Sonarr series, got %q", it.Title)
	}

	// A movie record with a real Radarr TMDB movie id matches the movie.
	movieOK := &mapping.Record{Type: "MOVIE", TmdbMovies: []int{20}}
	if it := li.FindByID(movieOK); it == nil || it.Arr != library.ArrRadarr {
		t.Errorf("movie record should match the Radarr movie, got %v", it)
	}

	// A series record matches the Sonarr series by TVDB id.
	series := &mapping.Record{Type: "TV", TvdbID: 10}
	if it := li.FindByID(series); it == nil || it.Arr != library.ArrSonarr {
		t.Errorf("series record should match the Sonarr series, got %v", it)
	}
}

// TestFindByIDNoWrongArrShadowing covers the index-build side of the arr gate:
// when a Sonarr series and a Radarr movie share the same TMDB id (disjoint
// TV/movie namespaces over one key space) or the same IMDb id (TVDB reuses the
// movie's id on the parent series), the movie record must resolve the Radarr
// movie regardless of snapshot item order - the wrong-arr item must not shadow
// the right-arr one in the pooled index.
func TestFindByIDNoWrongArrShadowing(t *testing.T) {
	movie := library.Item{Arr: library.ArrRadarr, ArrID: 2, Title: "Some Movie", TmdbID: 20, ImdbID: "tt2222222"}
	series := library.Item{Arr: library.ArrSonarr, ArrID: 1, Title: "Some Series", TvdbID: 10, TmdbID: 20, ImdbID: "tt2222222"}
	orders := map[string][]library.Item{
		"movie first":  {movie, series},
		"series first": {series, movie},
	}
	for name, items := range orders {
		t.Run(name, func(t *testing.T) {
			li := NewLibIndex(&library.Snapshot{Items: items})

			byTmdb := &mapping.Record{Type: "MOVIE", TmdbMovies: []int{20}}
			if it := li.FindByID(byTmdb); it == nil || it.Arr != library.ArrRadarr {
				t.Errorf("TMDB movie lookup = %v, want the Radarr movie (series must not shadow it)", it)
			}
			byImdb := &mapping.Record{Type: "MOVIE", IMDbIDs: []string{"tt2222222"}}
			if it := li.FindByID(byImdb); it == nil || it.Arr != library.ArrRadarr {
				t.Errorf("IMDb movie lookup = %v, want the Radarr movie (series must not shadow it)", it)
			}
			bySeries := &mapping.Record{Type: "TV", TvdbID: 10}
			if it := li.FindByID(bySeries); it == nil || it.Arr != library.ArrSonarr {
				t.Errorf("TVDB series lookup = %v, want the Sonarr series", it)
			}
		})
	}
}

// TestMatchTitleFallbackOnIdlessRecord covers the gap-fill fallthrough: when
// Fribb has the AniList entry but its record carries no arr id (a split
// mapping), the matcher falls back to the AniList title match and still links
// the entry to the library item - yet the entry counts as Unmapped coverage,
// not a Hit: the ID bridge by definition could not resolve an arr id, so
// "mapped" must not claim it.
func TestMatchTitleFallbackOnIdlessRecord(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Heaven's Feel I", TmdbID: 283984, Year: 2017},
	}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 20791, Type: "MOVIE"}}) // no tmdb/imdb: the split-mapping gap
	fake := fakeAniList{media: map[int]anilist.Media{
		20791: {Titles: []string{"Heaven's Feel I"}, Format: "MOVIE", Year: 2017},
	}}
	m := NewMatcher(fake, nil)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 20791}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 1 {
		t.Fatalf("expected a match to the Radarr movie, got %+v", got.Item)
	}
	if got.Source != SourceTitle {
		t.Errorf("source = %q, want %q", got.Source, SourceTitle)
	}
	if res.Coverage.Unmapped[library.ArrRadarr] != 1 {
		t.Errorf("coverage unmapped[radarr] = %d, want 1 (an id-less record is an ID-bridge miss even when the title fallback links it)", res.Coverage.Unmapped[library.ArrRadarr])
	}
	if len(res.Coverage.Hits) != 0 {
		t.Errorf("coverage hits = %v, want none (no arr id was resolved by the ID mapping)", res.Coverage.Hits)
	}
}

// countingAniList records how many times Fetch is called (always returning
// not-found), to prove which match paths consult AniList.
type countingAniList struct{ calls int }

func (c *countingAniList) Fetch(_ context.Context, _ int) (anilist.Media, error) {
	c.calls++
	return anilist.Media{}, anilist.ErrNotFound
}

func (c *countingAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	c.calls++
	return map[int]anilist.Media{}, nil
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
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 999, Type: "TV", TvdbID: 555}})
	fake := &countingAniList{}
	m := NewMatcher(fake, nil)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 999}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(res.Matches))
	}
	if got := res.Matches[0]; got.InLibrary() || got.Source != SourceUnmapped {
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

func (b *batchCountingAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	b.batchCalls++
	b.batchSizes = append(b.batchSizes, len(ids))
	out := make(map[int]anilist.Media, len(ids))
	for _, id := range ids {
		if m, ok := b.media[id]; ok {
			out[id] = m
		}
	}
	return out, nil
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
	// Three id-less MOVIE records (split mapping: no tmdb/imdb on the record).
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"},
		{AniListID: 22, Type: "MOVIE"},
		{AniListID: 33, Type: "MOVIE"},
	})
	fake := &batchCountingAniList{media: map[int]anilist.Media{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020},
		22: {Titles: []string{"Movie B"}, Format: "MOVIE", Year: 2021},
		33: {Titles: []string{"Movie C"}, Format: "MOVIE", Year: 2022},
	}}
	m := NewMatcher(fake, nil)

	res := m.Match(context.Background(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}, {AniListID: 33}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("want 1 batched AniList request, got %d (batch sizes %v)", fake.batchCalls, fake.batchSizes)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("want 0 single Fetch calls (the batch pre-warms the memo), got %d", fake.fetchCalls)
	}
	matched := 0
	for i := range res.Matches {
		if res.Matches[i].InLibrary() && res.Matches[i].Source == SourceTitle {
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

func (degradedAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	return nil, context.DeadlineExceeded
}

// TestMatchAniListTransientErrorDegrades covers the degraded path: when a needed
// AniList fallback lookup fails transiently (not ErrNotFound), Match flags the
// result Degraded and reports the id in IncompleteIDs so the caller preserves
// the affected entry's prior findings instead of resolving them, leaves the
// entry unmapped, and does NOT memoize the id (so it is retried next cycle
// rather than cached as a permanent miss).
func TestMatchAniListTransientErrorDegrades(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex(nil) // no Fribb record: the entry resolves via AniList
	m := NewMatcher(degradedAniList{}, nil)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 42}}, snap, idx, Memo{})

	if !res.Degraded {
		t.Error("Degraded = false, want true when a needed AniList lookup fails transiently")
	}
	if got := res.Coverage.Unmapped[arrUnknown]; got != 1 {
		t.Errorf("coverage unmapped[unknown] = %d, want 1 for the failed no-record fallback", got)
	}
	if _, ok := res.IncompleteIDs[42]; !ok || len(res.IncompleteIDs) != 1 {
		t.Errorf("IncompleteIDs = %v, want exactly {42} (the transiently failed lookup)", res.IncompleteIDs)
	}
	if len(res.Matches) != 1 || res.Matches[0].Source != SourceUnmapped {
		t.Errorf("entry should be unmapped on a transient failure, got %+v", res.Matches)
	}
	if _, cached := res.Memo.Entries[42]; cached {
		t.Error("a transient failure must not memoize the id (it must be retried next cycle)")
	}
}

// TestMatchTitleFallbackAmbiguousIsUnmapped covers the conservative-match
// invariant plus the no-Fribb-record resolution path: an entry with no mapping
// record is resolved through the AniList title fallback (exercising matchEntry's
// record-miss branch and formatArr), and when the normalized title matches more
// than one library item in the same arr the ambiguous set is treated as a miss
// (findByTitle's default branch) rather than guessed.
func TestMatchTitleFallbackAmbiguousIsUnmapped(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Clannad"},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "Clannad"},
	}}
	idx := mapping.NewIndex(nil) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		500: {Titles: []string{"Clannad"}, Format: "TV"},
	}}
	m := NewMatcher(fake, nil)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 500}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(res.Matches))
	}
	if got := res.Matches[0]; got.InLibrary() || got.Source != SourceUnmapped {
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
	idx := mapping.NewIndex(nil) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		500: {Titles: []string{"Clannad"}, Format: "TV", Year: 2010},
	}}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 500}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 2 {
		t.Fatalf("match item = %+v, want the 2010 series ArrID 2 (the year must disambiguate the two same-titled items)", got.Item)
	}
	if got.Source != SourceTitle {
		t.Errorf("source = %q, want %q", got.Source, SourceTitle)
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
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123}})
	fake := &countingAniList{}
	res := NewMatcher(fake, nil).Match(ctx, []seadex.Entry{{AniListID: 154587}}, snap, idx, Memo{})
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
	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 0}}, &library.Snapshot{}, mapping.NewIndex(nil), Memo{})
	if res.Degraded {
		t.Error("Degraded = true, want false for a non-positive AniList ID")
	}
	if len(res.Matches) != 1 || res.Matches[0].Source != SourceUnmapped || res.Matches[0].Arr != arrUnknown {
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
// Matcher.Match: an entry whose Fribb record carries the arr id resolves to the
// library item as SourceID, counts a coverage hit for its arr, and spends zero
// AniList requests (neither batch nor single).
func TestMatchResolvesByIDWithoutAniList(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren", TvdbID: 123},
	}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123}})
	fake := &countingAniList{}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 154587}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 7 {
		t.Fatalf("match item = %+v, want the Sonarr series ArrID 7", got.Item)
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

// TestMatchTitleFallbackSucceedsWithoutRecord pins the no-Fribb-record success
// path: an entry with no mapping record resolves through the AniList lookup and
// links to the single title+year library candidate as SourceTitle, with the arr
// taken from the matched item and the record type normalized from the AniList
// format.
func TestMatchTitleFallbackSucceedsWithoutRecord(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 3, Title: "Clannad", TvdbID: 555, Year: 2007},
	}}
	idx := mapping.NewIndex(nil) // no Fribb record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		600: {Titles: []string{"Clannad"}, Format: "TV", Year: 2007},
	}}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 600}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 3 {
		t.Fatalf("match item = %+v, want the Sonarr series ArrID 3", got.Item)
	}
	if got.Source != SourceTitle {
		t.Errorf("source = %q, want %q", got.Source, SourceTitle)
	}
	if got.Arr != library.ArrSonarr {
		t.Errorf("arr = %q, want %q (the matched item's arr)", got.Arr, library.ArrSonarr)
	}
	if got.Record.Type != "TV" {
		t.Errorf("record type = %q, want TV (normalized from the AniList format)", got.Record.Type)
	}
	if res.Coverage.Unmapped[library.ArrSonarr] != 1 {
		t.Errorf("coverage unmapped[sonarr] = %d, want 1 (no ID mapping existed)", res.Coverage.Unmapped[library.ArrSonarr])
	}
}

// TestMatchAltTitleFallbackFiltersArrAndDedupes pins the title-index corners in
// one flow: an id-less series record (TvdbID 0, so FindByID returns nil for a
// non-movie) falls to the title fallback; the library item is found via its
// ALTERNATE title; an unnormalizable AniList title ("!!!") is skipped; a
// same-titled item in the WRONG arr is filtered out; and the item matched under
// two of the AniList titles is deduplicated to a single unambiguous candidate.
func TestMatchAltTitleFallbackFiltersArrAndDedupes(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 9, Title: "Frieren: Beyond Journey's End", AltTitles: []string{"Sousou no Frieren"}, Year: 2023},
		{Arr: library.ArrRadarr, ArrID: 10, Title: "Sousou no Frieren", Year: 2023}, // wrong arr: must be filtered
	}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 154587, Type: "TV"}}) // id-less series record
	fake := fakeAniList{media: map[int]anilist.Media{
		154587: {Titles: []string{"!!!", "Sousou no Frieren", "Frieren: Beyond Journey's End"}, Format: "TV", Year: 2023},
	}}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 154587}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 9 {
		t.Fatalf("match item = %+v, want the Sonarr series ArrID 9 via its alternate title", got.Item)
	}
	if got.Source != SourceTitle {
		t.Errorf("source = %q, want %q", got.Source, SourceTitle)
	}
}

// TestFindMovieResolvesByIMDbWhenTmdbMisses pins findMovie's IMDb fallback: a
// MOVIE record whose TMDB movie ids all miss the library must still resolve
// through its IMDb id to the Radarr movie.
func TestFindMovieResolvesByIMDbWhenTmdbMisses(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 2, Title: "Some Movie", TmdbID: 20, ImdbID: "tt2222222"},
	}}
	li := NewLibIndex(snap)
	rec := &mapping.Record{Type: "MOVIE", TmdbMovies: []int{999}, IMDbIDs: []string{"tt2222222"}}

	it := li.FindByID(rec)
	if it == nil || it.ArrID != 2 || it.Arr != library.ArrRadarr {
		t.Fatalf("FindByID = %+v, want the Radarr movie resolved via the IMDb fallback", it)
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
	idx := mapping.NewIndex(nil) // no record: matchEntry resolves via AniList
	fake := fakeAniList{media: map[int]anilist.Media{
		610: {Titles: []string{"Clannad"}, Format: "", Year: 2007},
	}}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 610}}, snap, idx, Memo{})

	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
	got := res.Matches[0]
	if !got.InLibrary() || got.Item.ArrID != 4 {
		t.Fatalf("match item = %+v, want the Sonarr series despite the unknown format", got.Item)
	}
	if got.Source != SourceTitle {
		t.Errorf("source = %q, want %q", got.Source, SourceTitle)
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
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 11, Type: "MOVIE"}}) // id-less: the lookup is needed
	memo := Memo{Entries: map[int]MemoEntry{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020, Expiry: time.Now().Add(time.Hour)},
	}}

	res := NewMatcher(degradedAniList{}, nil).Match(context.Background(), []seadex.Entry{{AniListID: 11}}, snap, idx, memo)

	if res.Degraded {
		t.Error("Degraded = true, want false: the live memo served every needed lookup")
	}
	if len(res.IncompleteIDs) != 0 {
		t.Errorf("IncompleteIDs = %v, want none for a memo-served pass", res.IncompleteIDs)
	}
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
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
		idx := mapping.NewIndex([]mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 111}})
		// FetchMany returns 40's media with an error (a failed later chunk):
		// a PARTIAL batch failure, so 41 and 42 fall to the per-id Fetch,
		// where 41 gets a definitive not-found and 42 a transient failure.
		fake := &partialThenPerIDAniList{media: map[int]anilist.Media{
			40: {Titles: []string{"Returned"}, Format: "TV"},
		}}

		res := NewMatcher(fake, nil).Match(context.Background(),
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
		idx := mapping.NewIndex(nil)

		res := NewMatcher(degradedAniList{}, nil).Match(context.Background(),
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

func (p *partialThenPerIDAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	if id == 41 {
		return anilist.Media{}, anilist.ErrNotFound
	}
	return anilist.Media{}, context.DeadlineExceeded
}

func (p *partialThenPerIDAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	out := make(map[int]anilist.Media)
	for _, id := range ids {
		if m, ok := p.media[id]; ok {
			out[id] = m
		}
	}
	return out, context.DeadlineExceeded
}

// TestNewLibIndexNilSnapshot pins the defensive nil-snapshot guard: a nil
// snapshot yields an empty but usable index whose lookups all miss instead of
// panicking.
func TestNewLibIndexNilSnapshot(t *testing.T) {
	li := NewLibIndex(nil)
	if li == nil {
		t.Fatal("NewLibIndex(nil) = nil, want an empty index")
	}
	if it := li.FindByID(&mapping.Record{Type: "TV", TvdbID: 10}); it != nil {
		t.Errorf("FindByID on an empty index = %+v, want nil", it)
	}
	if got := li.findByTitle([]string{"Clannad"}, 0, library.ArrSonarr, slog.New(slog.DiscardHandler)); got != nil {
		t.Errorf("findByTitle on an empty index = %+v, want nil", got)
	}
}
