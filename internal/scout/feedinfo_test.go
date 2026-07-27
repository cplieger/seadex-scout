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
// title last (the writer then derives from file names). The movie typing and
// the RESOLVED season ride along whenever the record exists.
func TestFeedEntryInfoFallbackChain(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
		{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{555}},
		{AniListID: 3, Type: "MOVIE", IMDbIDs: []string{"tt0000001"}},
		{AniListID: 4, Type: "TV", TvdbID: 999},
		{AniListID: 5, Type: "OVA", TvdbID: 777},
		// A MAPPED record with no Fribb typing at all: the tolerant Fribb
		// decoder and an override omitting `type` both produce this shape.
		{AniListID: 20},
		// The same untyped shape, but carrying a positive season.tvdb and no
		// routed arr id: the memo's MOVIE format must win the season too.
		{AniListID: 21, SeasonTvdb: 3},
	})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "Frieren: Beyond Journey's End", Year: 2023},
		{Arr: library.ArrRadarr, ArrID: 11, TmdbID: 555, Title: "A Silent Voice", Year: 2016},
		{Arr: library.ArrRadarr, ArrID: 12, ImdbID: "tt0000001", Title: "Your Name", Year: 2016},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		4:  {Titles: []string{"Sousou no Frieren", "Frieren"}, Year: 2023},
		6:  {Titles: []string{"Memo Only Show"}, Year: 2020},
		7:  {NotFound: true},
		8:  {Titles: []string{"Memo Only Film"}, Year: 2019, Format: "MOVIE"},
		9:  {Titles: []string{"Memo Only OVA"}, Year: 2018, Format: "OVA"},
		20: {Titles: []string{"Untyped Film"}, Year: 2017, Format: "MOVIE"},
		21: {Titles: []string{"Untyped Season Film"}, Year: 2016, Format: "MOVIE"},
	}}
	info := feedEntryInfo(idx, lib, memo)

	sonarr := info(1)
	if sonarr.Title != "Frieren: Beyond Journey's End" || sonarr.Year != 2023 {
		t.Errorf("info(1) = %+v, want the Sonarr item's own title/year", sonarr)
	}
	if sonarr.Season != 2 || !sonarr.SeasonKnown || sonarr.IsMovie {
		t.Errorf("info(1) typing = %+v, want a series resolved to season 2", sonarr)
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
	// A special resolves to the specials bucket: a MAPPED season zero, which
	// the feed must be able to tell apart from "no season resolved".
	if ova := info(5); ova.Title != "" || ova.Season != 0 || !ova.SeasonKnown {
		t.Errorf("info(5) = %+v, want a title-less special resolved to season 0", ova)
	}

	// Unmapped id with a memo entry carrying NO format: title from the memo,
	// and the documented unmapped-to-Anime default survives (zero typing).
	if memoOnly := info(6); memoOnly.Title != "Memo Only Show" || memoOnly.Year != 2020 || memoOnly.IsMovie || memoOnly.SeasonKnown {
		t.Errorf("info(6) = %+v, want the memo title with zero typing", memoOnly)
	}

	// Unmapped id whose memo DOES carry the AniList format: the typing comes
	// from it (l-f70). Without this the app knew an entry was a movie and still
	// routed its feed item to Anime/5070, so Radarr - which filters on
	// Movies/2000 - never saw that movie in the RSS feed at all.
	if film := info(8); film.Title != "Memo Only Film" || !film.IsMovie || film.SeasonKnown {
		t.Errorf("info(8) = %+v, want the memo title typed as a movie", film)
	}
	// A memo-typed special resolves its season the same way a Fribb-typed one
	// does, so an unmapped OVA still labels S00 instead of losing its season.
	if ova := info(9); ova.Title != "Memo Only OVA" || ova.IsMovie || ova.Season != 0 || !ova.SeasonKnown {
		t.Errorf("info(9) = %+v, want the memo title typed as a special resolved to season 0", ova)
	}

	// A MAPPED record whose Fribb `type` is empty carries no typing either, so
	// the memo's format types it: without this the app knew the entry was a
	// movie and still published it under Anime/5070, where Radarr never sees
	// it (the l-f70 symptom, left open for the mapped-but-untyped shape).
	if untyped := info(20); untyped.Title != "Untyped Film" || !untyped.IsMovie || untyped.SeasonKnown {
		t.Errorf("info(20) = %+v, want the memo title typed as a movie", untyped)
	}

	// The same untyped shape carrying a positive season.tvdb: the caller
	// resolves that season BEFORE the memo types the entry as a movie, and a
	// movie pins no season at all (resolvedSeason's Radarr-first arm), so the
	// season must not survive the memo typing. Otherwise the feed publishes a
	// Movies/2000 item whose synthesized title carries an SNN season label,
	// which Radarr can fail to parse and match.
	if untypedSeason := info(21); untypedSeason.Title != "Untyped Season Film" || !untypedSeason.IsMovie || untypedSeason.SeasonKnown || untypedSeason.Season != 0 {
		t.Errorf("info(21) = %+v, want a memo-typed movie with no resolved season", untypedSeason)
	}

	// A negative memo entry supplies nothing.
	if notFound := info(7); notFound.Title != "" {
		t.Errorf("info(7) = %+v, want no title from a not-found memo entry", notFound)
	}

	// Entirely unknown: the zero EntryInfo (file-name fallback downstream).
	if unknown := info(999); unknown.Title != "" || unknown.SeasonKnown || unknown.IsMovie {
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

// TestFeedEntryInfoEmptyArrTitleFallsBackToMemo pins the documented fallback
// chain when the library item exists but its Title is empty: an unusable arr
// title must not short-circuit the chain - the memo's canonical title (the
// stronger remaining source) is returned, while the record's movie typing and
// resolved season ride along untouched.
func TestFeedEntryInfoEmptyArrTitleFallsBackToMemo(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
	})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "", Year: 2023},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"Memo Title"}, Year: 2021},
	}}
	got := feedEntryInfo(idx, lib, memo)(1)
	if got.Title != "Memo Title" || got.Year != 2021 {
		t.Errorf("info(1) = %+v, want the memo title/year when the arr title is empty", got)
	}
	if got.Season != 2 || !got.SeasonKnown || got.IsMovie {
		t.Errorf("info(1) typing = %+v, want the season-2 series resolution intact", got)
	}
}

// TestResolvedSeason pins the Fribb season-semantics rule at its one home for
// the feed (l-f4): a positive TVDB season wins, a Fribb-typed special with no
// positive season resolves to the specials bucket (a MAPPED season zero, which
// the feed must be able to tell apart from an absent season), and anything else
// - an absolute-numbered run, a title-only match, an untyped record - resolves
// nothing. The precedence row matters most: a record that is BOTH typed special
// and carries a positive season keeps the positive season, because that is the
// season the arr files it under.
//
// The indexer used to re-derive this from raw Fribb fields projected into
// EntryInfo, which is why it lives here now: that package imports neither
// align nor mapping, so it cannot read Fribb semantics at all.
func TestResolvedSeason(t *testing.T) {
	tests := []struct {
		name      string
		rec       mapping.Record
		want      int
		wantKnown bool
	}{
		{"positive TVDB season", mapping.Record{Type: "TV", SeasonTvdb: 3}, 3, true},
		{"special resolves to the specials bucket", mapping.Record{Type: "OVA"}, 0, true},
		{"ONA is a special too", mapping.Record{Type: "ONA"}, 0, true},
		{"a special with a positive season keeps it", mapping.Record{Type: "SPECIAL", SeasonTvdb: 2}, 2, true},
		{"absolute-numbered run resolves nothing", mapping.Record{Type: "TV"}, 0, false},
		{"movie resolves nothing", mapping.Record{Type: "MOVIE"}, 0, false},
		{"untyped record resolves nothing", mapping.Record{}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			season, known := resolvedSeason(&tt.rec)
			if season != tt.want || known != tt.wantKnown {
				t.Errorf("resolvedSeason(%+v) = (%d, %v), want (%d, %v)", tt.rec, season, known, tt.want, tt.wantKnown)
			}
		})
	}
}

// TestFeedEntryInfoArrTitleWinsOverMemo pins the documented source ORDER when
// BOTH tiers can answer: the arr's own title from the persisted snapshot beats
// the AniList memo title, because the arr is guaranteed to parse its own title
// back out of a synthesized RSS title while a romaji alias may not match the
// monitored series at all. Every other case in this file has only one tier
// populated, so an inverted chain (memo consulted first) passes them all.
func TestFeedEntryInfoArrTitleWinsOverMemo(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 123}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "Arr Own Title", Year: 2023},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"AniList Romaji Title"}, Year: 2021},
	}}
	got := feedEntryInfo(idx, lib, memo)(1)
	if got.Title != "Arr Own Title" || got.Year != 2023 {
		t.Errorf("info(1) = %+v, want the arr's own title/year to win over the memo", got)
	}
}

// TestFeedEntryInfoFribbTypingWinsOverMemoFormat pins the documented gate on
// the memo-format tier (l-f70): it applies ONLY when Fribb had nothing to say.
// A mapped record's own typing and resolved season must survive a memo entry
// carrying a contradicting AniList format, or a mapped series would route to
// Movies/2000 and lose its season - Sonarr filters on Anime/5070, so it would
// never see the show in the RSS feed. The existing format rows use unmapped
// ids only, so dropping the record-absent guard passes them all.
func TestFeedEntryInfoFribbTypingWinsOverMemoFormat(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2}})
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"Memo Title"}, Year: 2021, Format: "MOVIE"},
	}}
	got := feedEntryInfo(idx, &library.Snapshot{}, memo)(1)
	if got.IsMovie {
		t.Errorf("info(1) = %+v, want IsMovie=false: a mapped record's Fribb typing wins over the memo format", got)
	}
	if got.Season != 2 || !got.SeasonKnown {
		t.Errorf("info(1) = %+v, want the record's resolved season 2 intact", got)
	}
	if got.Title != "Memo Title" {
		t.Errorf("info(1).Title = %q, want the memo title (the record is not in the library)", got.Title)
	}
}
