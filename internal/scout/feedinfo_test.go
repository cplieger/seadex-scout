package scout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestFeedEntryInfoFallbackChain pins the synthesis title source order the
// feed writer depends on: the arr's own title from the persisted library
// snapshot first (keyed via the record's routed ids), the AniList canonical
// title (Titles[0], romaji-first) from the persisted memo next, and a zero
// title last (the writer then derives from file names). The movie typing and
// the RESOLVED season ride along whenever the record exists.
func TestFeedEntryInfoFallbackChain(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
		{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{555}},
		{AniListID: 3, Type: "MOVIE", IMDbIDs: []string{"tt0000001"}},
		{AniListID: 4, Type: "TV", TvdbID: 999},
		{AniListID: 5, Type: "OVA", TvdbID: 777},
		// A MAPPED record with no mapping type at all: an animap record with
		// no type and an override omitting `type` both produce this shape.
		{AniListID: 20},
		// The same untyped shape, but carrying a positive season.tvdb and no
		// routed arr id: the memo's MOVIE format must win the season too.
		{AniListID: 21, SeasonTvdb: 3},
		// Untyped, routing nothing and with no memo format: nothing types it.
		{AniListID: 22},
	}})
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
	info := feedInfo(idx, lib, nil, memo).Entry

	sonarr := info(1)
	if sonarr.Title != "Frieren: Beyond Journey's End" || sonarr.Year != 2023 {
		t.Errorf("info(1) = %+v, want the Sonarr item's own title/year", sonarr)
	}
	if sonarr.Season != 2 || !sonarr.SeasonKnown || sonarr.Kind != indexer.KindSeries {
		t.Errorf("info(1) typing = %+v, want a series resolved to season 2", sonarr)
	}

	if tmdb := info(2); tmdb.Title != "A Silent Voice" || tmdb.Kind != indexer.KindMovie {
		t.Errorf("info(2) = %+v, want the Radarr title via the TMDB movie id", tmdb)
	}
	if imdb := info(3); imdb.Title != "Your Name" || imdb.Kind != indexer.KindMovie {
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
	if memoOnly := info(6); memoOnly.Title != "Memo Only Show" || memoOnly.Year != 2020 || memoOnly.Kind != indexer.KindUnknown || memoOnly.SeasonKnown {
		t.Errorf("info(6) = %+v, want the memo title with zero typing", memoOnly)
	}

	// Unmapped id whose memo DOES carry the AniList format: the typing comes
	// from it. Without this the app knew an entry was a movie and still
	// routed its feed item to Anime/5070, so Radarr - which filters on
	// Movies/2000 - never saw that movie in the RSS feed at all.
	if film := info(8); film.Title != "Memo Only Film" || film.Kind != indexer.KindMovie || film.SeasonKnown {
		t.Errorf("info(8) = %+v, want the memo title typed as a movie", film)
	}
	// A memo-typed special resolves its season the same way a mapping-typed one
	// does, so an unmapped OVA still labels S00 instead of losing its season.
	if ova := info(9); ova.Title != "Memo Only OVA" || ova.Kind != indexer.KindSeries || ova.Season != 0 || !ova.SeasonKnown {
		t.Errorf("info(9) = %+v, want the memo title typed as a special resolved to season 0", ova)
	}

	// A MAPPED record whose mapping `type` is empty carries no typing either, so
	// the memo's format types it: without this the app knew the entry was a
	// movie and still published it under Anime/5070, where Radarr never sees
	// it, which is the mapped-but-untyped shape.
	if untyped := info(20); untyped.Title != "Untyped Film" || untyped.Kind != indexer.KindMovie || untyped.SeasonKnown {
		t.Errorf("info(20) = %+v, want the memo title typed as a movie", untyped)
	}

	// The same untyped shape carrying a positive season.tvdb: the caller
	// resolves that season BEFORE the memo types the entry as a movie, and a
	// movie pins no season at all (resolvedSeason's Radarr-first arm), so the
	// season must not survive the memo typing. Otherwise the feed publishes a
	// Movies/2000 item whose synthesized title carries an SNN season label,
	// which Radarr can fail to parse and match.
	if untypedSeason := info(21); untypedSeason.Title != "Untyped Season Film" || untypedSeason.Kind != indexer.KindMovie || untypedSeason.SeasonKnown || untypedSeason.Season != 0 {
		t.Errorf("info(21) = %+v, want a memo-typed movie with no resolved season", untypedSeason)
	}

	// A negative memo entry supplies nothing.
	if notFound := info(7); notFound.Title != "" {
		t.Errorf("info(7) = %+v, want no title from a not-found memo entry", notFound)
	}

	if untypedUnknown := info(22); untypedUnknown.Kind != indexer.KindUnknown || untypedUnknown.SeasonKnown {
		t.Errorf("info(22) = %+v, want KindUnknown and no season: nothing types the record", untypedUnknown)
	}

	// Entirely unknown: the zero EntryInfo (file-name fallback downstream).
	if unknown := info(999); unknown.Title != "" || unknown.SeasonKnown || unknown.Kind != indexer.KindUnknown {
		t.Errorf("info(999) = %+v, want the zero EntryInfo", unknown)
	}
}

// TestFeedEntryInfoArrConsistentRouting pins the arr-consistency rule
// inherited from the matcher: a MOVIE record resolves only against Radarr
// items, so a movie whose mapping record carries a TV-colliding id must not
// take a same-keyed Sonarr item's title (it falls through to the memo).
func TestFeedEntryInfoArrConsistentRouting(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		// A MOVIE record whose TMDB id collides with a Sonarr item's TmdbID
		// (disjoint namespaces over the same small-int key space).
		{AniListID: 1, Type: "MOVIE", TmdbMovies: []int{42}},
	}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 5, TmdbID: 42, Title: "Same-Named Series"},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"The Actual Movie"}, Year: 2019},
	}}
	got := feedInfo(idx, lib, nil, memo).Entry(1)
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
	got := feedInfo(mapping.NewIndex(mapping.Source{}), &library.Snapshot{}, nil, memo).Entry(1)
	if got.Title != "Expired Show" {
		t.Errorf("info(1).Title = %q, want the expired memo title still used", got.Title)
	}
}

// TestFeedEntryInfoFailedPlaceholderStillTitles pins that a Failed placeholder
// item (a partial prior walk) still supplies its title: identity fields
// survive an episode-fetch failure, so the feed's title source does not
// degrade with one flaky walk.
func TestFeedEntryInfoFailedPlaceholderStillTitles(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 123}}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "Flaky Show", Failed: true},
	}}
	got := feedInfo(idx, lib, nil, match.Memo{}).Entry(1)
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
	got := feedInfo(mapping.NewIndex(mapping.Source{}), &library.Snapshot{}, nil, memo).Entry(1)
	if got.Title != "" || got.Year != 0 {
		t.Errorf("info(1) = %+v, want the zero EntryInfo for an empty-titles memo entry", got)
	}
}

// TestFeedEntryInfoEmptyArrTitleFallsBackToMemo pins the documented fallback
// chain when the library item exists but its Title carries no text: an unusable
// arr title must not short-circuit the chain - the memo's canonical title (the
// stronger remaining source) is returned, while the record's movie typing and
// resolved season ride along untouched. A whitespace-only title is the same
// SEMANTIC empty as "": synthesizeTitle trims downstream, so admitting it here
// would suppress the memo and fall all the way to file-name derivation.
func TestFeedEntryInfoEmptyArrTitleFallsBackToMemo(t *testing.T) {
	for name, arrTitle := range map[string]string{
		"empty":           "",
		"whitespace only": " \t ",
	} {
		t.Run(name, func(t *testing.T) {
			idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
				{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
			}})
			lib := &library.Snapshot{Items: []library.Item{
				{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: arrTitle, Year: 2023},
			}}
			memo := match.Memo{Entries: map[int]match.MemoEntry{
				1: {Titles: []string{"Memo Title"}, Year: 2021},
			}}
			got := feedInfo(idx, lib, nil, memo).Entry(1)
			if got.Title != "Memo Title" || got.Year != 2021 {
				t.Errorf("info(1) = %+v, want the memo title/year when the arr title is %q", got, arrTitle)
			}
			if got.Season != 2 || !got.SeasonKnown || got.Kind != indexer.KindSeries {
				t.Errorf("info(1) typing = %+v, want the season-2 series resolution intact", got)
			}
		})
	}
}

// TestResolvedSeason pins the mapped season-semantics rule at its one home for the
// feed: a positive TVDB season wins, a mapping-typed special with no positive season
// resolves to the specials bucket (a MAPPED season zero, which the feed must tell
// apart from an absent season), and anything else - an absolute-numbered run, a
// title-only match, an untyped record - resolves nothing. The precedence row matters
// most: a record that is BOTH typed special and carries a positive season keeps the
// positive season, because that is the season the arr files it under.
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
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 123}}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 123, Title: "Arr Own Title", Year: 2023},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"AniList Romaji Title"}, Year: 2021},
	}}
	got := feedInfo(idx, lib, nil, memo).Entry(1)
	if got.Title != "Arr Own Title" || got.Year != 2023 {
		t.Errorf("info(1) = %+v, want the arr's own title/year to win over the memo", got)
	}
}

// TestFeedEntryInfoMappingTypingWinsOverMemoFormat pins the documented gate on
// the memo-format tier: it applies ONLY when the mapping had nothing to say.
// A mapped record's own typing and resolved season must survive a memo entry
// carrying a contradicting AniList format, or a mapped series would route to
// Movies/2000 and lose its season - Sonarr filters on Anime/5070, so it would
// never see the show in the RSS feed. The existing format rows use unmapped
// ids only, so dropping the record-absent guard passes them all.
func TestFeedEntryInfoMappingTypingWinsOverMemoFormat(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 123, SeasonTvdb: 2}}})
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"Memo Title"}, Year: 2021, Format: "MOVIE"},
	}}
	got := feedInfo(idx, &library.Snapshot{}, nil, memo).Entry(1)
	if got.Kind != indexer.KindSeries {
		t.Errorf("info(1) = %+v, want KindSeries: a mapped record's mapping type wins over the memo format", got)
	}
	if got.Season != 2 || !got.SeasonKnown {
		t.Errorf("info(1) = %+v, want the record's resolved season 2 intact", got)
	}
	if got.Title != "Memo Title" {
		t.Errorf("info(1).Title = %q, want the memo title (the record is not in the library)", got.Title)
	}
}

// TestFeedEntryInfoLibraryHitKeepsSeriesTyping pins the precedence between the
// library-hit early return and the memo-format typing tier: an UNTYPED mapping record
// still routes a positive TVDB id through RoutedIDs' series arm, so it can HIT a
// Sonarr item, and that hit is the stronger typing evidence whatever format the memo
// carries. Re-typing such an entry from a memoized MOVIE format would publish a show
// that is in Sonarr under Movies/2000, where Sonarr (which filters on Anime/5070)
// never sees it. No other row can catch a hoist of applyMemoTyping above the early
// return: they all lack a library hit.
func TestFeedEntryInfoLibraryHitKeepsSeriesTyping(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 30, TvdbID: 555, SeasonTvdb: 2}}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 555, Title: "Untyped But In Sonarr", Year: 2022},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		30: {Titles: []string{"Memo Film Title"}, Year: 2019, Format: "MOVIE"},
	}}
	got := feedInfo(idx, lib, nil, memo).Entry(30)
	if got.Title != "Untyped But In Sonarr" || got.Year != 2022 {
		t.Errorf("info(30) = %+v, want the Sonarr item's own title and year", got)
	}
	if got.Kind != indexer.KindSeries {
		t.Errorf("info(30) = %+v, want KindSeries: a Sonarr library hit outranks a memo MOVIE format", got)
	}
	if got.Season != 2 || !got.SeasonKnown {
		t.Errorf("info(30) = %+v, want the record's resolved season 2 intact", got)
	}
}

// TestFeedEntryInfoRoutedUntypedRecordIgnoresMemoMovieFormat pins
// feedInfo's gate on applyMemoTyping: an UNTYPED mapping record that
// still routes a positive TVDB id through RoutedIDs' series arm is series
// evidence, so a retained (expired) memo entry carrying a MOVIE format must
// not re-type it: that would publish a Sonarr-routed show under Movies/2000,
// where Sonarr never sees it. The memo's TITLE tier still runs. Every other
// untyped row in this file has TvdbID 0, so this arm is otherwise unexercised.
func TestFeedEntryInfoRoutedUntypedRecordIgnoresMemoMovieFormat(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{{AniListID: 31, TvdbID: 556}}})
	lib := &library.Snapshot{}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		31: {Titles: []string{"Memo Film Title"}, Year: 2019, Format: "MOVIE"},
	}}
	got := feedInfo(idx, lib, nil, memo).Entry(31)
	if got.Kind != indexer.KindSeries {
		t.Errorf("info(31) = %+v, want KindSeries: a routed TVDB id outranks a stale memo MOVIE format", got)
	}
	if got.Title != "Memo Film Title" || got.Year != 2019 {
		t.Errorf("info(31) = %+v, want the memo title tier to still supply the title", got)
	}
}

// TestFeedEntryInfoFilmOnSonarrItemKeepsItsOwnTitle pins the guarantee the
// widened FindByID would otherwise void. FindByID resolves a MOVIE record to
// the Sonarr series TVDB files the film under as a season-0 special, and
// feedInfo's first title tier IS FindByID - so without the gate all 50
// such entries would be served under the PARENT SERIES' title, which is a
// different work and one Radarr can never match. The film keeps its own
// SeaDex-derived name and still reports the Sonarr target and the tvdb id.
func TestFeedEntryInfoFilmOnSonarrItemKeepsItsOwnTitle(t *testing.T) {
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 1, Type: "MOVIE", TvdbID: 79525, SeasonKind: mapping.SeasonPresent},
		{AniListID: 2, Type: "MOVIE", TvdbID: 79525, TmdbMovies: []int{5528}, SeasonKind: mapping.SeasonPresent},
		{AniListID: 3, Type: "TV", TvdbID: 79525, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
		{AniListID: 4, Type: "MOVIE", TmdbMovies: []int{999}},
	}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 79525, Title: "Code Geass", Year: 2006},
		{Arr: library.ArrRadarr, ArrID: 11, TmdbID: 5528, Title: "Lelouch of the Resurrection", Year: 2019},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"Lelouch of the Resurrection"}, Year: 2019, Format: "MOVIE"},
	}}
	info := feedInfo(idx, lib, nil, memo).Entry

	film := info(1)
	if film.Title != "Lelouch of the Resurrection" {
		t.Errorf("info(1).Title = %q, want the film's own name (never the parent series')", film.Title)
	}
	if film.TvdbID != 79525 {
		t.Errorf("info(1).TvdbID = %d, want 79525 (the id that makes the offer consumable)", film.TvdbID)
	}
	if film.Kind != indexer.KindMovie {
		t.Errorf("info(1).Kind = %d, want KindMovie", film.Kind)
	}

	radarr := info(2)
	if radarr.Title != "Lelouch of the Resurrection" {
		t.Errorf("info(2) = %+v, want the Radarr item's own title", radarr)
	}

	series := info(3)
	if series.Title != "Code Geass" {
		t.Errorf("info(3) = %+v, want the Sonarr series' own title (the gate is for MOVIE records only)", series)
	}

	unmatched := info(4)
	if unmatched.TvdbID != 0 {
		t.Errorf("info(4).TvdbID = %d, want 0 (the record carries none)", unmatched.TvdbID)
	}
}

// TestFeedEntryInfoProjectsTheEpisodeFacts pins the projection of the
// record's episode facts onto the feed metadata. The twin's season-0 run is
// stamped ONLY for the offered class, beside the title of the Sonarr series it
// is filed under, whichever arr the entry resolved to (one special episode
// with an unknown count included); a film keeps its own name. Season ranges
// ride along as the map splits them: One Piece's season 16 is two ranges
// around entry episode 590, which lands on season 0. A film's ranges never
// ride along.
func TestFeedEntryInfoProjectsTheEpisodeFacts(t *testing.T) {
	ranges := []mapping.SeasonRange{{Season: 15, First: 517, Last: 578}, {Season: 16, First: 579, Last: 589}, {Season: 16, First: 591, Last: 628}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 1, Type: "MOVIE", TvdbID: 79525, AniDBID: 6008, SeasonKind: mapping.SeasonPresent},
		{AniListID: 2, Type: "MOVIE", TvdbID: 79525, TmdbMovies: []int{5528}, AniDBID: 6008, SeasonKind: mapping.SeasonPresent},
		{AniListID: 3, Type: "TV", TvdbID: 79525, AniDBID: 7949, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
		{AniListID: 4, Type: "TV", TvdbID: 81797, AniDBID: 69, SeasonKind: mapping.SeasonAbsent},
		{AniListID: 5, Type: "MOVIE", TvdbID: 70000, AniDBID: 6009, SeasonKind: mapping.SeasonPresent},
		{AniListID: 6, Type: "MOVIE", TvdbID: 79525, AniDBID: 9999, SeasonKind: mapping.SeasonPresent},
		{AniListID: 7, Type: "OVA", TvdbID: 79525, AniDBID: 8001, SeasonKind: mapping.SeasonPresent},
		{AniListID: 8, Type: "MOVIE", TvdbID: 79525, AniDBID: 905, SeasonKind: mapping.SeasonPresent},
		{AniListID: 9, Type: "MOVIE", TvdbID: 81797, AniDBID: 6008, SeasonKind: mapping.SeasonPresent},
		{AniListID: 10, Type: "MOVIE", TvdbID: 79525, AniDBID: 906, SeasonKind: mapping.SeasonPresent},
		{AniListID: 11, Type: "TV", TvdbID: 79056, AniDBID: 272, SeasonKind: mapping.SeasonAbsent},
		{AniListID: 12, Type: "MOVIE", TvdbID: 79525, AniDBID: 359, SeasonKind: mapping.SeasonPresent},
		{AniListID: 13, Type: "MOVIE", TvdbID: 81797, AniDBID: 359, SeasonKind: mapping.SeasonPresent},
		{AniListID: 14, Type: "MOVIE", TvdbID: 81797, AniDBID: 7000, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 3},
	}, Mappings: map[int]mapping.Mapping{
		6008: {Specials: []int{4}, SpecialsTvdb: 79525, SpecialsEpisodes: 1},
		6009: {Specials: []int{4}, SpecialsTvdb: 70000, SpecialsEpisodes: 1},
		7949: {Specials: []int{1}, SpecialsTvdb: 79525, SpecialsEpisodes: 1},
		69:   {Seasons: ranges, SeasonsTvdb: 81797},
		8001: {Specials: []int{9, 10}, SpecialsTvdb: 79525, SpecialsEpisodes: 2},
		905:  {Specials: []int{8, 12}, SpecialsTvdb: 79525, SpecialsEpisodes: 3},
		906:  {Specials: []int{8}, SpecialsTvdb: 79525},
		272:  {Seasons: []mapping.SeasonRange{{Season: 2, First: 26, Last: 49}}, SeasonsTvdb: 79056},
		359:  {SpecialEpisode: 2, SpecialsTvdb: 79525},
		7000: {Seasons: []mapping.SeasonRange{{Season: 3, First: 1, Last: 3}}, SeasonsTvdb: 81797},
	}})
	lib := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 79525, Title: "Code Geass", Year: 2006},
		{Arr: library.ArrRadarr, ArrID: 11, TmdbID: 5528, Title: "Lelouch of the Resurrection", Year: 2019},
		{Arr: library.ArrSonarr, ArrID: 12, TvdbID: 81797, Title: "One Piece", Year: 1999},
		{Arr: library.ArrSonarr, ArrID: 13, TvdbID: 70000, Title: "   "},
	}}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		1: {Titles: []string{"Lelouch of the Resurrection"}, Year: 2019, Format: "MOVIE"},
	}}
	info := feedInfo(idx, lib, nil, memo).Entry

	for _, tc := range []struct {
		name       string
		alID       int
		wantRun    []int
		wantCount  int
		wantSeries string
	}{
		{name: "a film only in Sonarr", alID: 1, wantRun: []int{4}, wantCount: 1, wantSeries: "Code Geass"},
		{name: "a film also in Radarr", alID: 2, wantRun: []int{4}, wantCount: 1, wantSeries: "Code Geass"},
		{name: "a positive-season TV record", alID: 3},
		{name: "a blank-titled series", alID: 5},
		{name: "no mapping", alID: 6},
		{name: "an OVA on two specials", alID: 7, wantRun: []int{9, 10}, wantCount: 2, wantSeries: "Code Geass"},
		{name: "a film whose three episodes are two specials", alID: 8, wantRun: []int{8, 12}, wantCount: 3, wantSeries: "Code Geass"},
		{name: "a film with an episode landing nowhere", alID: 10},
		{name: "an override naming another series", alID: 9},
		{name: "a film whose rows and default name one special", alID: 12, wantRun: []int{2}, wantSeries: "Code Geass"},
		{name: "an override moving that film to another series", alID: 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := info(tc.alID)
			if !slices.Equal(got.SpecialEpisodes, tc.wantRun) || got.SpecialsEpisodes != tc.wantCount || got.SeriesTitle != tc.wantSeries {
				t.Errorf("info(%d) = SpecialEpisodes %v (count %d) SeriesTitle %q, want %v (count %d) %q",
					tc.alID, got.SpecialEpisodes, got.SpecialsEpisodes, got.SeriesTitle, tc.wantRun, tc.wantCount, tc.wantSeries)
			}
		})
	}
	film := info(1)
	if film.Title != "Lelouch of the Resurrection" {
		t.Errorf("info(1).Title = %q, want the film's own name kept", film.Title)
	}
	if both := info(2); both.Kind != indexer.KindMovie || both.Title != "Lelouch of the Resurrection" {
		t.Errorf("info(2) = Kind %d Title %q, want the Radarr film unchanged", both.Kind, both.Title)
	}
	want := []indexer.SeasonRange{{Season: 15, First: 517, Last: 578}, {Season: 16, First: 579, Last: 589}, {Season: 16, First: 591, Last: 628}}
	if run := info(4); !slices.Equal(run.Seasons, want) {
		t.Errorf("info(4).Seasons = %+v, want %+v", run.Seasons, want)
	}
	if partial := info(11); !slices.Equal(partial.Seasons, []indexer.SeasonRange{{Season: 2, First: 26, Last: 49}}) {
		t.Errorf("info(11).Seasons = %+v, want the stated range kept for pack labels", partial.Seasons)
	}
	if film := info(14); film.Seasons != nil {
		t.Errorf("info(14).Seasons = %+v, want nil (a film pins no season, so its ranges never title a pack)", film.Seasons)
	}
}

// monogatariOrder is Sonarr 5's order for Monogatari (TVDB 102261), as
// arrapi's recorded testdata/sonarr/v5-api-v5/series-monogatari.json has it.
var monogatariOrder = &library.Order{Type: "alternate", Name: "Book Order"}

// TestFeedEntryInfoWithholdsSeasonPlacementForNonOfficialSeries pins the
// season arm: Sonarr reads a release's season token in the series' own
// order, so a record whose TVDB id names a series Sonarr lists in another
// order than TheTVDB's aired one gets no TVDB season, no season ranges and
// no specials bucket, keeps its title and TVDB id, and is flagged
// NonAiredOrder. That holds for a failed placeholder, for a blank arr title
// on the memo tier, and for a series arr_tags keeps out of the snapshot's
// items, which the feed still serves to Sonarr.
func TestFeedEntryInfoWithholdsSeasonPlacementForNonOfficialSeries(t *testing.T) {
	ranges := []mapping.SeasonRange{{Season: 2, First: 1, Last: 26}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 102261, AniDBID: 9001, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
		{AniListID: 2, Type: "OVA", TvdbID: 102261, AniDBID: 9002, SeasonKind: mapping.SeasonPresent},
		{AniListID: 3, Type: "TV", TvdbID: 102262, AniDBID: 9003, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
		{AniListID: 4, Type: "TV", TvdbID: 102263, AniDBID: 9004, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
		{AniListID: 5, Type: "TV", TvdbID: 102264, AniDBID: 9005, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
	}, Mappings: map[int]mapping.Mapping{
		9001: {Seasons: ranges, SeasonsTvdb: 102261},
		9003: {Seasons: ranges, SeasonsTvdb: 102262},
		9004: {Seasons: ranges, SeasonsTvdb: 102263},
		9005: {Seasons: ranges, SeasonsTvdb: 102264},
	}})
	lib := &library.Snapshot{
		Items: []library.Item{
			{Arr: library.ArrSonarr, ArrID: 2189, TvdbID: 102261, Title: "Monogatari", Year: 2009, Order: monogatariOrder},
			{Arr: library.ArrSonarr, ArrID: 2190, TvdbID: 102262, Title: "Flaky Ordered Show", Order: monogatariOrder, Failed: true},
			{Arr: library.ArrSonarr, ArrID: 2191, TvdbID: 102263, Title: " ", Order: &library.Order{Type: "dvd"}},
		},
	}
	memo := match.Memo{Entries: map[int]match.MemoEntry{
		4: {Titles: []string{"Memo Ordered Show"}, Year: 2011},
		5: {Titles: []string{"Untagged Ordered Show"}, Year: 2012},
	}}
	info := feedInfo(idx, lib, []int{102261, 102262, 102263, 102264}, memo).Entry

	for _, tc := range []struct {
		name      string
		alID      int
		wantTitle string
	}{
		{name: "a season record", alID: 1, wantTitle: "Monogatari"},
		{name: "a special resolving to the specials bucket", alID: 2, wantTitle: "Monogatari"},
		{name: "a failed placeholder", alID: 3, wantTitle: "Flaky Ordered Show"},
		{name: "a blank arr title on the memo tier", alID: 4, wantTitle: "Memo Ordered Show"},
		{name: "a series arr_tags leaves out of the items", alID: 5, wantTitle: "Untagged Ordered Show"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := info(tc.alID)
			if got.Season != 0 || got.SeasonKnown {
				t.Errorf("info(%d) Season = (%d, %v), want (0, false)", tc.alID, got.Season, got.SeasonKnown)
			}
			if got.Seasons != nil {
				t.Errorf("info(%d).Seasons = %+v, want nil", tc.alID, got.Seasons)
			}
			if !got.NonAiredOrder {
				t.Errorf("info(%d).NonAiredOrder = false, want true", tc.alID)
			}
			if got.Title != tc.wantTitle {
				t.Errorf("info(%d).Title = %q, want %q", tc.alID, got.Title, tc.wantTitle)
			}
			if got.TvdbID == 0 || got.Kind != indexer.KindSeries {
				t.Errorf("info(%d) = TvdbID %d Kind %d, want the record's TVDB id on a series", tc.alID, got.TvdbID, got.Kind)
			}
		})
	}
}

// TestFeedEntryInfoKeepsSeasonPlacementForOfficialSeries is the converse: the
// same records on series in TheTVDB's aired order keep today's season, ranges
// and specials bucket and are not flagged, a series arr_tags leaves out of the
// items included.
func TestFeedEntryInfoKeepsSeasonPlacementForOfficialSeries(t *testing.T) {
	ranges := []mapping.SeasonRange{{Season: 2, First: 1, Last: 26}}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 102261, AniDBID: 9001, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
		{AniListID: 2, Type: "OVA", TvdbID: 102261, AniDBID: 9002, SeasonKind: mapping.SeasonPresent},
		{AniListID: 3, Type: "TV", TvdbID: 79525, AniDBID: 9003, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
	}, Mappings: map[int]mapping.Mapping{
		9001: {Seasons: ranges, SeasonsTvdb: 102261},
		9003: {Seasons: ranges, SeasonsTvdb: 79525},
	}})
	lib := &library.Snapshot{
		Items: []library.Item{
			{Arr: library.ArrSonarr, ArrID: 2189, TvdbID: 102261, Title: "Monogatari", Year: 2009},
		},
	}
	info := feedInfo(idx, lib, []int{102264}, match.Memo{}).Entry

	for _, alID := range []int{1, 3} {
		got := info(alID)
		if got.Season != 2 || !got.SeasonKnown || !slices.Equal(got.Seasons, []indexer.SeasonRange{{Season: 2, First: 1, Last: 26}}) {
			t.Errorf("info(%d) = Season (%d, %v) Seasons %+v, want season 2 and its range", alID, got.Season, got.SeasonKnown, got.Seasons)
		}
		if got.NonAiredOrder {
			t.Errorf("info(%d).NonAiredOrder = true, want false", alID)
		}
	}
	if special := info(2); special.Season != 0 || !special.SeasonKnown {
		t.Errorf("info(2) Season = (%d, %v), want the specials bucket (0, true)", special.Season, special.SeasonKnown)
	}
}

// TestFeedEntryInfoWithholdsTwinForNonOfficialSeries pins the twin arm: an
// offered record whose specials series is in a non-aired order gets no
// season-0 run, no count and no series title, so no "<series> S00Exx" twin
// names a TVDB special Sonarr would read in the other order. A film also in
// Radarr keeps its Radarr title and typing, and every record is flagged.
func TestFeedEntryInfoWithholdsTwinForNonOfficialSeries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		order      *library.Order
		nonAired   []int
		wantRun    []int
		wantCount  int
		wantSeries string
	}{
		{name: "non-official order", order: monogatariOrder, nonAired: []int{102261}},
		{name: "official order", wantRun: []int{4}, wantCount: 1, wantSeries: "Monogatari"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
				{AniListID: 1, Type: "MOVIE", TvdbID: 102261, AniDBID: 6008, SeasonKind: mapping.SeasonPresent},
				{AniListID: 2, Type: "MOVIE", TvdbID: 102261, TmdbMovies: []int{5528}, AniDBID: 6008, SeasonKind: mapping.SeasonPresent},
				{AniListID: 3, Type: "OVA", TvdbID: 102261, AniDBID: 6008, SeasonKind: mapping.SeasonPresent},
			}, Mappings: map[int]mapping.Mapping{
				6008: {Specials: []int{4}, SpecialsTvdb: 102261, SpecialsEpisodes: 1},
			}})
			lib := &library.Snapshot{
				Items: []library.Item{
					{Arr: library.ArrSonarr, ArrID: 2189, TvdbID: 102261, Title: "Monogatari", Year: 2009, Order: tc.order},
					{Arr: library.ArrRadarr, ArrID: 11, TmdbID: 5528, Title: "Kizumonogatari", Year: 2016},
				},
			}
			memo := match.Memo{Entries: map[int]match.MemoEntry{1: {Titles: []string{"Kizumonogatari"}, Year: 2016, Format: "MOVIE"}}}
			info := feedInfo(idx, lib, tc.nonAired, memo).Entry
			for _, alID := range []int{1, 2, 3} {
				got := info(alID)
				if !slices.Equal(got.SpecialEpisodes, tc.wantRun) || got.SpecialsEpisodes != tc.wantCount || got.SeriesTitle != tc.wantSeries {
					t.Errorf("info(%d) = SpecialEpisodes %v (count %d) SeriesTitle %q, want %v (count %d) %q",
						alID, got.SpecialEpisodes, got.SpecialsEpisodes, got.SeriesTitle, tc.wantRun, tc.wantCount, tc.wantSeries)
				}
				if got.NonAiredOrder != (tc.nonAired != nil) {
					t.Errorf("info(%d).NonAiredOrder = %v, want %v", alID, got.NonAiredOrder, tc.nonAired != nil)
				}
			}
			if film := info(2); film.Kind != indexer.KindMovie || film.Title != "Kizumonogatari" || film.TvdbID != 102261 {
				t.Errorf("info(2) = Kind %d Title %q TvdbID %d, want the Radarr film with its TVDB id", film.Kind, film.Title, film.TvdbID)
			}
		})
	}
}

// TestFeedInfoNonAiredOrderReadsThePersistedSet pins the TVDB-keyed half of
// the feed metadata: a stored render is judged by the series it names, so the
// lookup answers from the persisted order set alone, whether or not any
// mapping record or library item names that id.
func TestFeedInfoNonAiredOrderReadsThePersistedSet(t *testing.T) {
	empty := mapping.NewIndex(mapping.Source{})
	set := []int{78874, 102261}
	for _, tc := range []struct {
		name   string
		set    []int
		tvdbID int
		want   bool
	}{
		{name: "listed book order with no record or item", set: set, tvdbID: 102261, want: true},
		{name: "listed dvd order", set: set, tvdbID: 78874, want: true},
		{name: "aired series", set: set, tvdbID: 79525},
		{name: "no TVDB id", set: set, tvdbID: 0},
		{name: "no order set", tvdbID: 102261},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedInfo(empty, &library.Snapshot{}, tc.set, match.Memo{}).NonAiredOrder(tc.tvdbID); got != tc.want {
				t.Errorf("NonAiredOrder(%d) = %v, want %v", tc.tvdbID, got, tc.want)
			}
		})
	}
}

// TestStoredFilmWhoseTypingIsGoneKeepsItsTitleUnderNonAiredOrder runs the real
// feedInfo into the real feed writer. A film journaled with its Sonarr twin,
// whose mapping record is then gone with no memo to type it, is carried on its
// stored render once its series uses a non-aired order: it keeps its title and
// loses only the twin (docs/torznab-indexer.md "Series in another episode
// order"), because nothing says it is not a film.
func TestStoredFilmWhoseTypingIsGoneKeepsItsTitleUnderNonAiredOrder(t *testing.T) {
	const (
		alID = 21519
		url  = "https://nyaa.si/view/2133634"
	)
	path := filepath.Join(t.TempDir(), "feed.json")
	writer := indexer.NewFeedWriter(&indexer.FeedWriterConfig{Path: path, NyaaTorznabURL: "http://prowlarr/1/api"}, nil, nil)
	geass := library.Item{Arr: library.ArrSonarr, ArrID: 1, TvdbID: 79525, Title: "Code Geass", Year: 2006}

	rendered := []seadex.Entry{{AniListID: alID, Torrents: []seadex.Torrent{{
		Tracker: "Nyaa", URL: url, IsBest: true, ReleaseGroup: "G",
		Files: []seadex.File{{Length: 7, Name: "Lelouch of the Resurrection (1080p) [G].mkv"}},
	}}}}
	mapped := mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: alID, Type: "MOVIE", TvdbID: 79525, AniDBID: 14271, SeasonKind: mapping.SeasonPresent},
	}, Mappings: map[int]mapping.Mapping{
		14271: {Specials: []int{4}, SpecialsTvdb: 79525, SpecialsEpisodes: 1},
	}})
	aired := &library.Snapshot{Items: []library.Item{geass}}
	// A fresh install baselines what SeaDex lists, so the release journals on the pass after.
	if err := writer.Rebuild(t.Context(), nil, feedInfo(mapped, aired, nil, match.Memo{})); err != nil {
		t.Fatalf("baseline Rebuild: %v", err)
	}
	if err := writer.Rebuild(t.Context(), rendered, feedInfo(mapped, aired, nil, match.Memo{})); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	before := readNyaaFeed(t, path)
	if len(before) != 1 || before[0].SonarrTitle == "" {
		t.Fatalf("first pass feed = %+v, want one film item with its twin (the fixture's premise)", before)
	}

	// No files and no release group, and with the record gone no title: the
	// fresh render fails, so the item is carried on its stored render.
	unrenderable := []seadex.Entry{{AniListID: alID, Torrents: []seadex.Torrent{{Tracker: "Nyaa", URL: url, IsBest: true}}}}
	ordered := geass
	ordered.Order = &library.Order{Type: "alternate", Name: "Disney+ Order"}
	nonAired := &library.Snapshot{Items: []library.Item{ordered}}
	if err := writer.Rebuild(t.Context(), unrenderable, feedInfo(mapping.NewIndex(mapping.Source{}), nonAired, []int{79525}, match.Memo{})); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	after := readNyaaFeed(t, path)
	if len(after) != 1 {
		t.Fatalf("second pass feed = %+v, want the film kept", after)
	}
	if after[0].Title != before[0].Title {
		t.Errorf("Title = %q, want the stored %q", after[0].Title, before[0].Title)
	}
	if after[0].SonarrTitle != "" || after[0].SonarrGUID != "" {
		t.Errorf("twin = %q / %q, want both cleared", after[0].SonarrTitle, after[0].SonarrGUID)
	}
}

// readNyaaFeed decodes the persisted Nyaa journal's title and twin fields.
func readNyaaFeed(t *testing.T, path string) []struct{ Title, SonarrTitle, SonarrGUID string } {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read feed snapshot: %v", err)
	}
	var snap struct {
		NyaaFeed []struct{ Title, SonarrTitle, SonarrGUID string } `json:"nyaa_feed"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("decode feed snapshot: %v", err)
	}
	return snap.NyaaFeed
}
