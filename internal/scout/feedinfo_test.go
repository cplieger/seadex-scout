package scout

import (
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

// TestFeedEntryInfoFallbackChain pins the synthesis title source order the
// feed writer depends on: the arr's own title from the persisted library
// snapshot first (keyed via the record's routed ids), the AniList canonical
// title (Titles[0], romaji-first) from the persisted memo next, and a zero
// title last (the writer then derives from file names). Fribb typing and the
// mapped season ride along whenever the record exists.
func TestFeedEntryInfoFallbackChain(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
		{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{555}},
		{AniListID: 3, Type: "MOVIE", IMDbIDs: []string{"tt0000001"}},
		{AniListID: 4, Type: "TV", TvdbID: 999},
		{AniListID: 5, Type: "OVA", TvdbID: 777},
	})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "Frieren: Beyond Journey's End", Year: 2023},
		{Arr: library.ArrRadarr, ArrID: 11, TmdbID: 555, Title: "A Silent Voice", Year: 2016},
		{Arr: library.ArrRadarr, ArrID: 12, ImdbID: "tt0000001", Title: "Your Name", Year: 2016},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		4: {Titles: []string{"Sousou no Frieren", "Frieren"}, Year: 2023},
		6: {Titles: []string{"Memo Only Show"}, Year: 2020},
		7: {NotFound: true},
	}}
	info := feedEntryInfo(idx, lib, memo)

	sonarr := info(1)
	if sonarr.Title != "Frieren: Beyond Journey's End" || sonarr.Year != 2023 {
		t.Errorf("info(1) = %+v, want the Sonarr item's own title/year", sonarr)
	}
	if sonarr.SeasonTvdb != 2 || sonarr.IsMovie || sonarr.IsSpecial {
		t.Errorf("info(1) typing = %+v, want SeasonTvdb=2 series", sonarr)
	}

	if tmdb := info(2); tmdb.Title != "A Silent Voice" || !tmdb.IsMovie {
		t.Errorf("info(2) = %+v, want the Radarr title via the TMDB movie id", tmdb)
	}
	if imdb := info(3); imdb.Title != "Your Name" || !imdb.IsMovie {
		t.Errorf("info(3) = %+v, want the Radarr title via the IMDb id", imdb)
	}

	// Mapped but not in the library: the AniList canonical title (Titles[0]).
	if anilist := info(4); anilist.Title != "Sousou no Frieren" || anilist.Year != 2023 {
		t.Errorf("info(4) = %+v, want the AniList canonical (romaji-first) title", anilist)
	}

	// Mapped, not in the library, no memo entry: typing survives, no title.
	if ova := info(5); ova.Title != "" || !ova.IsSpecial {
		t.Errorf("info(5) = %+v, want a title-less special", ova)
	}

	// Unmapped id with a memo entry: title from the memo, no typing.
	if memoOnly := info(6); memoOnly.Title != "Memo Only Show" || memoOnly.Year != 2020 || memoOnly.IsMovie {
		t.Errorf("info(6) = %+v, want the memo title with zero typing", memoOnly)
	}

	// A negative memo entry supplies nothing.
	if notFound := info(7); notFound.Title != "" {
		t.Errorf("info(7) = %+v, want no title from a not-found memo entry", notFound)
	}

	// Entirely unknown: the zero EntryInfo (file-name fallback downstream).
	if unknown := info(999); unknown.Title != "" || unknown.SeasonTvdb != 0 || unknown.IsMovie {
		t.Errorf("info(999) = %+v, want the zero EntryInfo", unknown)
	}
}

// TestFeedEntryInfoArrConsistentRouting pins the arr-consistency rule
// inherited from the matcher: a MOVIE record resolves only against Radarr
// items, so a movie whose Fribb record carries a TV-colliding id must not
// take a same-keyed Sonarr item's title (it falls through to the memo).
func TestFeedEntryInfoArrConsistentRouting(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		// A MOVIE record whose TMDB id collides with a Sonarr item's TmdbID
		// (disjoint namespaces over the same small-int key space).
		{AniListID: 1, Type: "MOVIE", TmdbMovies: []int{42}},
	})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 5, TmdbID: 42, Title: "Same-Named Series"},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"The Actual Movie"}, Year: 2019},
	}}
	got := feedEntryInfo(idx, lib, memo)(1)
	if got.Title != "The Actual Movie" {
		t.Errorf("info(1).Title = %q, want the AniList fallback (a movie record must not resolve a Sonarr item)", got.Title)
	}
}

// TestFeedEntryInfoUsesExpiredMemoTitles pins the deliberate expiry bypass: a
// memo entry past its AniList re-fetch expiry still supplies the show title -
// a stale title beats a file-name derivation, and expiry governs re-fetch, not
// usability.
func TestFeedEntryInfoUsesExpiredMemoTitles(t *testing.T) {
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"Expired Show"}, Expiry: time.Now().Add(-time.Hour)},
	}}
	got := feedEntryInfo(mapping.NewIndex(nil), &library.Snapshot{}, memo)(1)
	if got.Title != "Expired Show" {
		t.Errorf("info(1).Title = %q, want the expired memo title still used", got.Title)
	}
}

// TestFeedEntryInfoFailedPlaceholderStillTitles pins that a Failed placeholder
// item (a partial prior walk) still supplies its title: identity fields
// survive an episode-fetch failure, so the feed's title source does not
// degrade with one flaky walk.
func TestFeedEntryInfoFailedPlaceholderStillTitles(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 123}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "Flaky Show", Failed: true},
	}}
	got := feedEntryInfo(idx, lib, match.Memo{})(1)
	if got.Title != "Flaky Show" {
		t.Errorf("info(1).Title = %q, want the Failed placeholder's title", got.Title)
	}
}

// TestFeedEntryInfoEmptyMemoTitles pins the memo guard's third clause: a
// cached, found memo entry whose Titles slice is empty supplies nothing - the
// closure must return the zero EntryInfo instead of indexing Titles[0].
func TestFeedEntryInfoEmptyMemoTitles(t *testing.T) {
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{}, Year: 2020},
	}}
	got := feedEntryInfo(mapping.NewIndex(nil), &library.Snapshot{}, memo)(1)
	if got.Title != "" || got.Year != 0 {
		t.Errorf("info(1) = %+v, want the zero EntryInfo for an empty-titles memo entry", got)
	}
}
