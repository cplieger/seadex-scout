package mapping

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/cplieger/animap/mapread"
)

// facts is one projected entry: its Record and the facts MappingFor serves
// for it.
type facts struct {
	rec Record
	m   Mapping
}

// projected decodes doc's encoding, as a refresh does, and projects it into
// an Index's view of each AniList entry.
func projected(t *testing.T, body []byte) (projection, map[int]facts) {
	t.Helper()
	doc, err := mapread.Decode(bytes.NewReader(body), mapread.Limits{})
	if err != nil {
		t.Fatalf("mapread.Decode: %v", err)
	}
	p, err := project(doc, maxAnimeEpisodes)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	idx := NewIndex(Source{Records: p.records, Mappings: p.mappings, AniListMappings: p.aniListMappings})
	out := make(map[int]facts, len(p.records))
	for _, r := range p.records {
		m, _ := idx.MappingFor(&r)
		out[r.AniListID] = facts{rec: r, m: m}
	}
	return p, out
}

func sameMapping(a, b Mapping) bool {
	return slices.Equal(a.Seasons, b.Seasons) && a.SeasonsTvdb == b.SeasonsTvdb &&
		slices.Equal(a.Specials, b.Specials) && slices.Equal(a.Episodes, b.Episodes) && a.SpecialsTvdb == b.SpecialsTvdb && a.SpecialsEpisodes == b.SpecialsEpisodes &&
		a.EpisodesTvdb == b.EpisodesTvdb && a.EpisodesSeason == b.EpisodesSeason && a.SpecialEpisode == b.SpecialEpisode
}

// TestProject_derivesTheRecordFromWhereTheEpisodesLand pins how the season
// kind and season, and each fact, follow from where an entry's episodes land
// on TVDB.
func TestProject_derivesTheRecordFromWhereTheEpisodesLand(t *testing.T) {
	const series = 500
	tests := []struct {
		name       string
		runs       []mapread.Run
		want       Mapping
		episodes   int
		wantKind   SeasonKind
		wantSeason int
	}{
		{
			name: "stated on one positive season", episodes: 3, runs: []mapread.Run{regular(1, 3, targets(at(2, 5)))},
			wantKind: SeasonPresent, wantSeason: 2,
			want: Mapping{Seasons: []SeasonRange{{Season: 2, First: 1, Last: 3}}, SeasonsTvdb: series, Episodes: []int{5, 6, 7}, EpisodesTvdb: series, EpisodesSeason: 2},
		},
		{
			name: "one positive season by default", episodes: 3, runs: []mapread.Run{regular(1, 3, targets(dflt(1, 1)))},
			wantKind: SeasonPresent, wantSeason: 1,
			want: Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 3}}, SeasonsTvdb: series},
		},
		{
			name: "one positive season and an episode landing nowhere", episodes: 3, runs: []mapread.Run{regular(1, 2, targets(at(1, 1)))},
			wantKind: SeasonPresent, wantSeason: 1,
			want: Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 2}}, SeasonsTvdb: series},
		},
		{
			name: "one positive season and season 0", episodes: 3,
			runs:     []mapread.Run{regular(1, 2, targets(at(1, 1))), regular(3, 3, targets(at(0, 4)))},
			wantKind: SeasonPresent, wantSeason: 1,
			want: Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 2}}, SeasonsTvdb: series},
		},
		{
			name: "two positive seasons", episodes: 4,
			runs:     []mapread.Run{regular(1, 2, targets(at(1, 1))), regular(3, 4, targets(at(2, 1)))},
			wantKind: SeasonAbsent,
			want:     Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 2}, {Season: 2, First: 3, Last: 4}}, SeasonsTvdb: series},
		},
		{
			name: "an episode with nothing stated", episodes: 3,
			runs:     []mapread.Run{regular(1, 1, nil), regular(2, 3, targets(at(2, 1)))},
			wantKind: SeasonAbsent,
			want:     Mapping{Seasons: []SeasonRange{{Season: 2, First: 2, Last: 3}}, SeasonsTvdb: series},
		},
		{
			name: "stated wholly on season 0", episodes: 2, runs: []mapread.Run{regular(1, 2, targets(at(0, 7)))},
			wantKind: SeasonPresent,
			want:     Mapping{Specials: []int{7, 8}, SpecialsTvdb: series, SpecialsEpisodes: 2},
		},
		{
			name: "season 0 and an episode landing nowhere", episodes: 2, runs: []mapread.Run{regular(1, 1, targets(at(0, 7)))},
			wantKind: SeasonPresent,
			want:     Mapping{Specials: []int{7}, SpecialsTvdb: series},
		},
		{
			name: "one episode on two season-0 episodes", episodes: 1, runs: []mapread.Run{regular(1, 1, targets(at(0, 3), at(0, 4)))},
			wantKind: SeasonPresent,
			want:     Mapping{Specials: []int{3, 4}, SpecialsTvdb: series, SpecialsEpisodes: 1},
		},
		{
			name: "season 0 by default", episodes: 1, runs: []mapread.Run{regular(1, 1, targets(dflt(0, 1)))},
			wantKind: SeasonPresent,
		},
		{
			name: "a default agreeing with the stated special episode", episodes: 3,
			runs:     []mapread.Run{regular(1, 1, targets(at(0, 2))), regular(2, 2, targets(dflt(0, 2))), regular(3, 3, targets(at(0, 2)))},
			wantKind: SeasonPresent,
			want:     Mapping{SpecialEpisode: 2, SpecialsTvdb: series},
		},
		{
			name: "a default naming another special episode", episodes: 3,
			runs:     []mapread.Run{regular(1, 1, targets(at(0, 51))), regular(2, 3, targets(dflt(0, 2)))},
			wantKind: SeasonPresent,
		},
		{
			name: "a default agreeing with the stated special episode beside one landing nowhere", episodes: 3,
			runs:     []mapread.Run{regular(1, 1, targets(at(0, 2))), regular(2, 2, targets(dflt(0, 2)))},
			wantKind: SeasonPresent,
		},
		{
			name: "a default agreeing with the stated special episode beside an unstated one", episodes: 3,
			runs:     []mapread.Run{regular(1, 1, targets(at(0, 2))), regular(2, 2, targets(dflt(0, 2))), regular(3, 3, nil)},
			wantKind: SeasonAbsent,
		},
		{
			name: "lands nowhere at all", episodes: 2,
			wantKind: SeasonPresent,
		},
		{
			name:     "no episodes",
			wantKind: SeasonAbsent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mapread.Anime{AniDBID: 10, Type: "TV", TVDBID: series, Last: mapread.Last{Regular: tc.episodes}, Episodes: tc.runs}
			e := mapread.Entry{AniListID: 1, Type: "TV", Parts: []mapread.Part{{AniDBID: 10, Kind: mapread.Regular}}}
			p, got := projected(t, encode(t, document([]mapread.Anime{a}, []mapread.Entry{e})))
			rec := got[1].rec
			if rec.AniDBID != 10 || rec.TvdbID != series || rec.SeasonKind != tc.wantKind || rec.SeasonTvdb != tc.wantSeason {
				t.Errorf("record = %+v, want AniDB 10 on TVDB %d with season %s %d", rec, series, tc.wantKind, tc.wantSeason)
			}
			if !sameMapping(got[1].m, tc.want) {
				t.Errorf("MappingFor = %+v, want %+v", got[1].m, tc.want)
			}
			if !sameMapping(p.mappings[10], tc.want) || len(p.aniListMappings) != 0 {
				t.Errorf("facts keyed %+v by AniDB and %+v by AniList, want %+v under AniDB 10 only", p.mappings, p.aniListMappings, tc.want)
			}
		})
	}
}

// TestProject_routesAndKeys pins the ids and join keys: a whole anime's
// films and its AniDB id, a link's facts under its AniList id, an anime no
// entry names keyed for an override, and an entry on two TVDB series or on
// none, which carries no series, season or facts.
func TestProject_routesAndKeys(t *testing.T) {
	anime := []mapread.Anime{
		{
			AniDBID: 1, Type: "MOVIE", TVDBID: 500, IMDbIDs: []string{"tt0000042"}, Last: mapread.Last{Regular: 1, Special: 2},
			Episodes: []mapread.Run{
				{Kind: mapread.Regular, Start: 1, End: 1, TVDB: targets(at(0, 3)), TMDBMovies: []int{42}},
				{Kind: mapread.Special, Start: 1, End: 2, TVDB: targets(at(0, 8))},
			},
		},
		{AniDBID: 2, Type: "TV", TVDBID: 600, Last: mapread.Last{Regular: 2}, Episodes: []mapread.Run{regular(1, 2, targets(at(1, 1)))}},
		{AniDBID: 3, Type: "MOVIE", Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{{Kind: mapread.Regular, Start: 1, End: 1, TMDBMovies: []int{77}}}},
		{AniDBID: 4, Type: "TV", TVDBID: 700, Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{regular(1, 1, targets(at(0, 1)))}},
	}
	entries := []mapread.Entry{
		{AniListID: 10, Type: "MOVIE", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Regular}}},
		{AniListID: 11, Type: "SPECIAL", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Special, Start: 1, End: 2}}},
		{AniListID: 12, Type: "TV", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Special, Start: 1, End: 1}, {AniDBID: 2, Kind: mapread.Regular, Start: 1, End: 2}}},
		{AniListID: 13, Type: "MOVIE", Parts: []mapread.Part{{AniDBID: 3, Kind: mapread.Regular}}},
		{AniListID: 14, Type: "TV", IDs: &mapread.IDs{AniDB: []int{2, 4}}},
	}
	p, got := projected(t, encode(t, document(anime, entries)))
	t.Run("a whole film", func(t *testing.T) {
		r := got[10].rec
		if r.AniDBID != 1 || r.TvdbID != 500 || !slices.Equal(r.TmdbMovies, []int{42}) || !slices.Equal(r.IMDbIDs, []string{"tt0000042"}) ||
			r.SeasonKind != SeasonPresent || r.SeasonTvdb != 0 {
			t.Errorf("record = %+v, want AniDB 1, TVDB 500 season 0, its film ids", r)
		}
		if !slices.Equal(got[10].m.Specials, []int{3}) || got[10].m.SpecialsEpisodes != 1 {
			t.Errorf("facts = %+v, want S00E03 answering 1 episode", got[10].m)
		}
	})
	t.Run("a link to specials", func(t *testing.T) {
		r := got[11].rec
		if r.AniDBID != 0 || r.TvdbID != 500 || r.TmdbMovies != nil || r.IMDbIDs != nil {
			t.Errorf("record = %+v, want no AniDB id, TVDB 500 and none of its parent's film ids", r)
		}
		want := Mapping{Specials: []int{8, 9}, SpecialsTvdb: 500, SpecialsEpisodes: 2}
		if !sameMapping(got[11].m, want) || !sameMapping(p.aniListMappings[11], want) {
			t.Errorf("facts = %+v, keyed %+v, want %+v under AniList 11", got[11].m, p.aniListMappings[11], want)
		}
	})
	t.Run("an entry on two TVDB series", func(t *testing.T) {
		r := got[12].rec
		if r.TvdbID != 0 || r.SeasonKind != SeasonAbsent || r.HasArrIdentifier() {
			t.Errorf("record = %+v, want no TVDB id and an absent season: the arrs key a series by one id", r)
		}
		if _, ok := p.aniListMappings[12]; ok {
			t.Errorf("facts for an entry on two series = %+v, want none", p.aniListMappings[12])
		}
	})
	t.Run("a film with no series", func(t *testing.T) {
		r := got[13].rec
		if r.TvdbID != 0 || r.SeasonKind != SeasonAbsent || !slices.Equal(r.TmdbMovies, []int{77}) || !r.HasArrIdentifier() {
			t.Errorf("record = %+v, want the TMDB movie and no series", r)
		}
	})
	t.Run("an ambiguous entry", func(t *testing.T) {
		if r := got[14].rec; r.AniDBID != 0 || r.TvdbID != 0 || r.Type != "TV" || r.SeasonKind != SeasonAbsent {
			t.Errorf("record = %+v, want a type and nothing else", r)
		}
	})
	t.Run("keys", func(t *testing.T) {
		if keys := slices.Sorted(maps.Keys(p.mappings)); !slices.Equal(keys, []int{1, 2, 4}) {
			t.Errorf("AniDB-keyed facts = %v, want every anime on a TVDB series with facts, AniDB 4 included", keys)
		}
		if len(p.records) != 5 {
			t.Errorf("records = %d, want one per entry, 5", len(p.records))
		}
	})
}

func TestProject_anEpisodeFiledOnAnotherSeries(t *testing.T) {
	onOther := mapread.Run{Kind: mapread.Regular, Start: 1, End: 1, AniDBEpisodeIDs: []int{4001}, TVDB: targets(mapread.Target{Series: 950, Season: 1, Episode: 3})}
	for _, tc := range []struct {
		episode2   *[]mapread.Target
		name       string
		wantKind   SeasonKind
		wantSeason int
		wantStated bool
	}{
		{name: "unstated on its own series", wantKind: SeasonAbsent},
		{name: "nowhere on its own series", episode2: targets(), wantKind: SeasonPresent, wantSeason: 1, wantStated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mapread.Anime{AniDBID: 40, Type: "TV", TVDBID: 900, Last: mapread.Last{Regular: 2}, EpisodesListed: true, Episodes: []mapread.Run{
				onOther, {Kind: mapread.Regular, Start: 2, End: 2, AniDBEpisodeIDs: []int{4002}, TVDB: tc.episode2},
			}}
			e := mapread.Entry{AniListID: 41, Type: "TV", Parts: []mapread.Part{{AniDBID: 40, Kind: mapread.Regular, Start: 1, End: 2}}}
			p, got := projected(t, encode(t, document([]mapread.Anime{a}, []mapread.Entry{e})))
			if rec := got[41].rec; rec.TvdbID != 950 || rec.SeasonKind != tc.wantKind || rec.SeasonTvdb != tc.wantSeason {
				t.Errorf("record = %+v, want TVDB 950 with season %s %d", rec, tc.wantKind, tc.wantSeason)
			}
			if want := []SeasonRange{{Season: 1, First: 1, Last: 1}}; !slices.Equal(got[41].m.Seasons, want) || got[41].m.SeasonsTvdb != 950 {
				t.Errorf("facts = %+v, want season 1 of TVDB 950 for entry episode 1", got[41].m)
			}
			_, stated := p.aniListFullyStated[41]
			_, animeStated := p.fullyStated[40]
			if stated != tc.wantStated || animeStated != tc.wantStated {
				t.Errorf("fully stated = %v by AniList and %v by AniDB, want %v", stated, animeStated, tc.wantStated)
			}
		})
	}
}

// TestProject_anEpisodeOffTVDBLandsNowhere pins mapread's reading of an
// AniDB episode off TVDB as landing nowhere on the entry's series, which is
// no evidence that the map states it: Decode refuses "tvdb": [] on a row
// with no tvdb_id, so its run names no tvdb.
func TestProject_anEpisodeOffTVDBLandsNowhere(t *testing.T) {
	for _, tc := range []struct {
		film mapread.Anime
		name string
	}{
		{
			name: "listed",
			film: mapread.Anime{AniDBID: 52, Type: "MOVIE", Last: mapread.Last{Regular: 1}, EpisodesListed: true, Episodes: []mapread.Run{
				{Kind: mapread.Regular, Start: 1, End: 1, AniDBEpisodeIDs: []int{5201}, TMDBMovies: []int{77}},
			}},
		},
		{
			name: "not listed",
			film: mapread.Anime{AniDBID: 52, Type: "MOVIE", Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{
				{Kind: mapread.Regular, Start: 1, End: 1, TMDBMovies: []int{77}},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anime := []mapread.Anime{
				{AniDBID: 50, Type: "TV", TVDBID: 960, Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{regular(1, 1, targets(at(1, 1)))}},
				tc.film,
			}
			e := mapread.Entry{AniListID: 51, Type: "TV", Parts: []mapread.Part{{AniDBID: 50, Kind: mapread.Regular}, {AniDBID: 52, Kind: mapread.Regular}}}
			p, got := projected(t, encode(t, document(anime, []mapread.Entry{e})))
			if rec := got[51].rec; rec.TvdbID != 960 || rec.SeasonKind != SeasonPresent || rec.SeasonTvdb != 1 {
				t.Errorf("record = %+v, want TVDB 960 season 1", rec)
			}
			if want := []SeasonRange{{Season: 1, First: 1, Last: 1}}; !slices.Equal(got[51].m.Seasons, want) || got[51].m.SeasonsTvdb != 960 {
				t.Errorf("facts = %+v, want season 1 of TVDB 960 for entry episode 1", got[51].m)
			}
			if _, stated := p.aniListFullyStated[51]; stated {
				t.Error("entry reads fully stated, want not: an episode off TVDB is stated nowhere")
			}
		})
	}
}

func TestProject_readsAnAnimeWithNoSeriesOfItsOwn(t *testing.T) {
	a := mapread.Anime{AniDBID: 60, Type: "OVA", Last: mapread.Last{Regular: 2}, Episodes: []mapread.Run{
		regular(1, 2, targets(mapread.Target{Series: 980, Season: 2, Episode: 4})),
	}}
	e := mapread.Entry{AniListID: 61, Type: "OVA", Parts: []mapread.Part{{AniDBID: 60, Kind: mapread.Regular}}}
	p, got := projected(t, encode(t, document([]mapread.Anime{a}, []mapread.Entry{e})))
	want := Mapping{Seasons: []SeasonRange{{Season: 2, First: 1, Last: 2}}, SeasonsTvdb: 980, Episodes: []int{4, 5}, EpisodesTvdb: 980, EpisodesSeason: 2}
	if rec := got[61].rec; rec.AniDBID != 60 || rec.TvdbID != 980 || rec.SeasonKind != SeasonPresent || rec.SeasonTvdb != 2 {
		t.Errorf("record = %+v, want AniDB 60 on TVDB 980 season 2", rec)
	}
	if !sameMapping(p.mappings[60], want) || !sameMapping(got[61].m, want) {
		t.Errorf("facts keyed %+v by AniDB 60, served %+v, want %+v", p.mappings[60], got[61].m, want)
	}
	if _, stated := p.fullyStated[60]; !stated {
		t.Error("AniDB 60 does not read fully stated, want every episode stated on TVDB 980")
	}
}

// TestProject_overrideOnAnAnimeNoEntryNamesKeepsItsSeasonRanges pins that
// an anime's season ranges travel with their series: an override naming an
// anime no entry is the whole of keeps them on that series, and loses them
// on another.
func TestProject_overrideOnAnAnimeNoEntryNamesKeepsItsSeasonRanges(t *testing.T) {
	a := mapread.Anime{AniDBID: 20, Type: "TV", TVDBID: 800, Last: mapread.Last{Regular: 8}, Episodes: []mapread.Run{regular(1, 8, targets(dflt(1, 1)))}}
	e := mapread.Entry{AniListID: 30, Type: "TV", Parts: []mapread.Part{{AniDBID: 20, Kind: mapread.Regular, Start: 1, End: 4}}}
	p, _ := projected(t, encode(t, document([]mapread.Anime{a}, []mapread.Entry{e})))
	want := []SeasonRange{{Season: 1, First: 1, Last: 8}}
	for _, tc := range []struct {
		want   []SeasonRange
		name   string
		series int
	}{
		{name: "on its series", series: 800, want: want},
		{name: "on another series", series: 801},
	} {
		t.Run(tc.name, func(t *testing.T) {
			override := Record{AniListID: 31, Type: "TV", AniDBID: 20, TvdbID: tc.series, SeasonKind: SeasonAbsent}
			idx := NewIndex(Source{Records: p.records, Mappings: p.mappings, AniListMappings: p.aniListMappings, Overrides: []Record{override}})
			if got, _ := idx.MappingFor(&override); !slices.Equal(got.Seasons, tc.want) {
				t.Errorf("MappingFor(override of AniDB 20 on TVDB %d).Seasons = %+v, want %+v", tc.series, got.Seasons, tc.want)
			}
		})
	}
}

// fixture is a subset of animap's 2026-10 build (content hash fe2e67a1…6fe7ef
// in the earlier version 2 shape), rewritten in the new shape: the entries
// these tests name and every anime their parts reach, with every TVDB
// placement kept, the anime's TMDB movie ids moved onto its regular
// episodes, and AniDB's episode listing left out, so its tvdb [] runs went
// with it.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/animap-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestProject_realBuild pins the projection of real-build entries, each one
// kind of answer.
func TestProject_realBuild(t *testing.T) {
	_, got := projected(t, fixture(t))
	tests := []struct {
		name       string
		kind       SeasonKind
		aniList    int
		tvdb       int
		season     int
		specials   []int
		episodes   []int
		count      int
		special    int
		seasons    []SeasonRange
		allSeasons bool
	}{
		{
			name: "Cowboy Bebop on one season", aniList: 1, tvdb: 76885, kind: SeasonPresent, season: 1,
			seasons: []SeasonRange{{Season: 1, First: 1, Last: 26}}, allSeasons: true,
		},
		{
			name: "absolute numbering whose rows place only the second half", aniList: 132, tvdb: 79056, kind: SeasonAbsent,
			seasons: []SeasonRange{{Season: 2, First: 26, Last: 49}}, allSeasons: true,
		},
		{
			name: "an absolute node whose rows put every episode on one season", aniList: 99749, tvdb: 114801, kind: SeasonPresent, season: 8,
			seasons: []SeasonRange{{Season: 8, First: 1, Last: 51}}, episodes: seq(1, 51), allSeasons: true,
		},
		{name: "a film whose six parts are one special", aniList: 2013, tvdb: 80054, kind: SeasonPresent, specials: []int{23}, count: 6},
		{name: "a special filed under season 5 that lands nowhere", aniList: 21745, tvdb: 102261, kind: SeasonPresent},
		{
			name: "a film filed under season 3 stated on season 6", aniList: 21286, tvdb: 79460, kind: SeasonPresent, season: 6,
			seasons: []SeasonRange{{Season: 6, First: 1, Last: 22}}, episodes: seq(1, 22), allSeasons: true,
		},
		{
			name: "cours split by rows", aniList: 28, tvdb: 76014, kind: SeasonAbsent,
			seasons: []SeasonRange{{Season: 1, First: 1, Last: 26}, {Season: 2, First: 27, Last: 52}, {Season: 3, First: 53, Last: 69}}, allSeasons: true,
		},
		{name: "a link to four specials", aniList: 10020, tvdb: 194961, kind: SeasonPresent, specials: []int{1, 2, 3, 4}, count: 4},
		{name: "a film whose rows and default name one special", aniList: 1006, tvdb: 80009, kind: SeasonPresent, special: 2},
		{name: "a film whose rows and defaults name different specials", aniList: 12117, tvdb: 72454, kind: SeasonPresent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := got[tc.aniList]
			if !ok {
				t.Fatalf("AniList %d has no record", tc.aniList)
			}
			if f.rec.TvdbID != tc.tvdb || f.rec.SeasonKind != tc.kind || f.rec.SeasonTvdb != tc.season {
				t.Errorf("record = %+v, want TVDB %d season %s %d", f.rec, tc.tvdb, tc.kind, tc.season)
			}
			if !tc.allSeasons {
				tc.seasons = f.m.Seasons
			}
			want := Mapping{Seasons: tc.seasons, Specials: tc.specials, Episodes: tc.episodes, SpecialsEpisodes: tc.count, SpecialEpisode: tc.special}
			if len(tc.seasons) > 0 {
				want.SeasonsTvdb = tc.tvdb
			}
			if tc.specials != nil || tc.special != 0 {
				want.SpecialsTvdb = tc.tvdb
			}
			if tc.episodes != nil {
				want.EpisodesTvdb, want.EpisodesSeason = tc.tvdb, tc.season
			}
			if !sameMapping(f.m, want) {
				t.Errorf("facts = %+v, want %+v", f.m, want)
			}
		})
	}
}

func seq(from, to int) []int {
	out := make([]int, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// TestProject_onePieceEpisode590 pins the hole a single range per season
// would claim: AniList 21's entry episodes 579 to 589 and 591 to 628 land on
// season 16 and episode 590 on S00E39, so season 16 is two ranges and no
// range holds 590.
func TestProject_onePieceEpisode590(t *testing.T) {
	_, got := projected(t, fixture(t))
	f := got[21]
	if f.rec.SeasonKind != SeasonAbsent || f.rec.TvdbID != 81797 {
		t.Fatalf("record = %+v, want TVDB 81797 compared on the whole series", f.rec)
	}
	var sixteen []SeasonRange
	for _, r := range f.m.Seasons {
		if r.Season == 16 {
			sixteen = append(sixteen, r)
		}
		if r.First <= 590 && 590 <= r.Last {
			t.Errorf("range %+v holds episode 590, which lands on season 0", r)
		}
	}
	if want := []SeasonRange{{Season: 16, First: 579, Last: 589}, {Season: 16, First: 591, Last: 628}}; !slices.Equal(sixteen, want) {
		t.Errorf("season 16 ranges = %+v, want %+v", sixteen, want)
	}
}

// TestProject_seasonRangesAreTheMaximalRuns holds every entry of the
// fixture to the range rule, read back through mapread: each range's entry
// episodes all land on its season and the episodes either side do not, and
// every landing on a positive season sits in exactly one range of it.
func TestProject_seasonRangesAreTheMaximalRuns(t *testing.T) {
	body := fixture(t)
	_, got := projected(t, body)
	doc, err := mapread.Decode(bytes.NewReader(body), mapread.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	x := mapread.NewIndex(doc)
	for al, f := range got {
		if f.rec.TvdbID == 0 {
			continue
		}
		e, _ := x.Entry(al)
		on := make(map[int]map[int]bool)
		for _, ep := range x.Episodes(e) {
			for _, sr := range x.OnTVDB(ep).Series {
				for _, b := range sr.Seasons {
					if b.Season > 0 {
						if on[b.Season] == nil {
							on[b.Season] = make(map[int]bool)
						}
						on[b.Season][ep.Number] = true
					}
				}
			}
		}
		covered := make(map[int]map[int]int)
		for _, r := range f.m.Seasons {
			if on[r.Season][r.First-1] || on[r.Season][r.Last+1] {
				t.Errorf("AniList %d range %+v is not maximal", al, r)
			}
			for n := r.First; n <= r.Last; n++ {
				if !on[r.Season][n] {
					t.Errorf("AniList %d range %+v holds episode %d, which does not land on season %d", al, r, n, r.Season)
				}
				if covered[r.Season] == nil {
					covered[r.Season] = make(map[int]int)
				}
				covered[r.Season][n]++
			}
		}
		for s, eps := range on {
			for n := range eps {
				if covered[s][n] != 1 {
					t.Errorf("AniList %d episode %d on season %d is in %d ranges, want 1", al, n, s, covered[s][n])
				}
			}
		}
	}
}

// TestProject_boundsTheAnimeEpisodes pins the projection's own cap: the
// anime facts expand every regular episode of every anime on TVDB, which
// mapread's caps on the entries do not bound, so a document past the budget
// is refused whole and one at it is read.
func TestProject_boundsTheAnimeEpisodes(t *testing.T) {
	doc := func() *mapread.Document {
		return document([]mapread.Anime{
			{AniDBID: 1, TVDBID: 5, Last: mapread.Last{Regular: 6}},
			{AniDBID: 2, TVDBID: 6, Last: mapread.Last{Regular: 4}},
			{AniDBID: 3, Type: "TV", Last: mapread.Last{Regular: 100}},
			{AniDBID: 4, Type: "OVA", Last: mapread.Last{Regular: 2}, Episodes: []mapread.Run{regular(1, 1, targets(mapread.Target{Series: 8, Season: 1, Episode: 1}))}},
		}, nil)
	}
	if _, err := project(doc(), 12); err != nil {
		t.Errorf("project(12 episodes on TVDB, budget 12) = %v, want the document read", err)
	}
	if p, err := project(doc(), 11); !errors.Is(err, mapread.ErrTooLarge) || p.records != nil || p.mappings != nil {
		t.Errorf("project(12 episodes on TVDB, budget 11) = %+v, %v, want nothing and mapread.ErrTooLarge", p, err)
	}
}

func TestMapping_SpecialRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		m         Mapping
		want      []int
		wantCount int
	}{
		{name: "every episode on season 0", m: Mapping{Specials: []int{4, 5}, SpecialsEpisodes: 2}, want: []int{4, 5}, wantCount: 2},
		{name: "an episode landing nowhere", m: Mapping{Specials: []int{4}}},
		{name: "one special episode beside a default", m: Mapping{SpecialEpisode: 4}, want: []int{4}},
		{name: "specials win over the special episode", m: Mapping{Specials: []int{4, 5}, SpecialsEpisodes: 2, SpecialEpisode: 9}, want: []int{4, 5}, wantCount: 2},
		{name: "no specials", m: Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 2}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := tc.m.SpecialRun()
			if !slices.Equal(got, tc.want) || n != tc.wantCount {
				t.Errorf("SpecialRun() = %v, %d, want %v, %d", got, n, tc.want, tc.wantCount)
			}
			if tc.m.Specials != nil && got != nil {
				got[0] = -1
				if tc.m.Specials[0] == -1 {
					t.Error("SpecialRun returned the Mapping's own slice")
				}
			}
		})
	}
}
