package match

import (
	"context"
	"testing"

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
	li := buildLibIndex(snap)

	// A movie record whose IMDb id collides with the Sonarr series must not match.
	movieCollide := &mapping.Record{Type: "MOVIE", IMDbIDs: []string{"tt4279012"}}
	if it := li.findByID(movieCollide); it != nil {
		t.Errorf("movie record must not match a Sonarr series, got %q", it.Title)
	}

	// A movie record with a real Radarr TMDB movie id matches the movie.
	movieOK := &mapping.Record{Type: "MOVIE", TmdbMovies: []int{20}}
	if it := li.findByID(movieOK); it == nil || it.Arr != library.ArrRadarr {
		t.Errorf("movie record should match the Radarr movie, got %v", it)
	}

	// A series record matches the Sonarr series by TVDB id.
	series := &mapping.Record{Type: "TV", TvdbID: 10}
	if it := li.findByID(series); it == nil || it.Arr != library.ArrSonarr {
		t.Errorf("series record should match the Sonarr series, got %v", it)
	}
}

// TestMatchTitleFallbackOnIdlessRecord covers the gap-fill fallthrough: when
// Fribb has the AniList entry but its record carries no arr id (a split
// mapping), the matcher falls back to the AniList title match and still links
// the entry to the library item.
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
