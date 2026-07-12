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
