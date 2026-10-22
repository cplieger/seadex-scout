package match

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// linksDocument is a version 2 document whose entries reach their anime
// through parts, as animap publishes them: AniList 1563 is the second
// cour of AniDB 127 (its episodes 21 to 49, stated on season 2), 2593 one film
// of AniDB 4932 (season 1), 2269 three specials of AniDB 5000, and 6090 the
// whole of AniDB 1466, a film off TVDB.
func linksDocument() *mapread.Document {
	stated := func(season, episode int) *[]mapread.Target {
		return &[]mapread.Target{{Season: season, Episode: episode}}
	}
	part := func(aid int, kind mapread.Kind, start, end int) []mapread.Part {
		return []mapread.Part{{AniDBID: aid, Kind: kind, Start: start, End: end}}
	}
	return &mapread.Document{
		Version: mapread.Version, GeneratedAt: "2026-10-17T00:00:00Z",
		Sources:     mapread.Sources{Mapping: mapread.MappingSource{Revision: "r"}, AniDB: mapread.AniDBSource{TitlesDump: "2026-10-17"}},
		Attribution: mapread.Attribution{License: "ODbL-1.0", LicenseURL: "u", ContentsLicense: "DbCL-1.0", ContentsLicenseURL: "u", Notice: "n"},
		Anime: []mapread.Anime{
			{AniDBID: 127, Type: "TV", TVDBID: 71634, Last: mapread.Last{Regular: 49}, Episodes: []mapread.Run{
				{Kind: mapread.Regular, Start: 1, End: 20, TVDB: &[]mapread.Target{{Season: 1, Episode: 1, Default: true}}},
				{Kind: mapread.Regular, Start: 21, End: 49, TVDB: stated(2, 1)},
			}},
			{AniDBID: 1466, Type: "OVA", IMDbIDs: []string{"tt0167846"}, Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{
				{Kind: mapread.Regular, Start: 1, End: 1, TMDBMovies: []int{66033}},
			}},
			{AniDBID: 4932, Type: "TV", TVDBID: 82099, Last: mapread.Last{Regular: 7}, Episodes: []mapread.Run{
				{Kind: mapread.Regular, Start: 1, End: 7, TVDB: stated(1, 1)},
			}},
			{AniDBID: 5000, Type: "TV", TVDBID: 254931, Last: mapread.Last{Regular: 12, Special: 3}, Episodes: []mapread.Run{
				{Kind: mapread.Special, Start: 1, End: 3, TVDB: stated(0, 2)},
			}},
		},
		Entries: []mapread.Entry{
			{AniListID: 435, Type: "TV", Parts: []mapread.Part{{AniDBID: 127, Kind: mapread.Regular}}},
			{AniListID: 1563, Type: "TV", Parts: part(127, mapread.Regular, 21, 49)},
			{AniListID: 2269, Type: "SPECIAL", Parts: part(5000, mapread.Special, 1, 3)},
			{AniListID: 2593, Type: "MOVIE", Parts: part(4932, mapread.Regular, 1, 1)},
			{AniListID: 6090, Type: "OVA", Parts: []mapread.Part{{AniDBID: 1466, Kind: mapread.Regular}}},
		},
	}
}

// linksIndex serves linksDocument as the latest release of a GitHub-shaped
// host and loads it through the mapping loader, as the scout does.
func linksIndex(t *testing.T) *mapping.Index {
	t.Helper()
	body, err := mapread.Encode(linksDocument())
	if err != nil {
		t.Fatal(err)
	}
	const tag = "v2026.10.17"
	sum := sha256.Sum256(body)
	prefix := "/" + mapping.DefaultRepo + "/releases/"
	var ts *httptest.Server
	ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case prefix + "latest":
			w.Header().Set("Location", ts.URL+prefix+"tag/"+tag)
			w.WriteHeader(http.StatusFound)
		case prefix + "download/" + tag + "/animap.json.sha256":
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  animap.json\n"))
		case prefix + "download/" + tag + "/animap.json":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	src := mapread.Source{Repo: mapping.DefaultRepo, Base: ts.URL}
	_, idx, err := mapping.NewLoader(ts.Client(), src, mapping.WithLogger(slog.New(slog.DiscardHandler))).Load(t.Context(), nil)
	if err != nil {
		t.Fatalf("load the links document: %v", err)
	}
	return idx
}

func seq(first, last int) []int {
	var out []int
	for k := first; k <= last; k++ {
		out = append(out, k)
	}
	return out
}

func TestAnimapLinksDecode(t *testing.T) {
	idx := linksIndex(t)
	facts := func(al int) (mapping.Record, mapping.Mapping, bool) {
		rec, found := idx.Lookup(al)
		if !found {
			t.Fatalf("Lookup(%d): no record", al)
		}
		m, ok := idx.MappingFor(&rec)
		return rec, m, ok
	}
	if _, m, ok := facts(1563); !ok || !slices.Equal(m.Episodes, seq(1, 29)) || m.EpisodesTvdb != 71634 || m.EpisodesSeason != 2 {
		t.Errorf("MappingFor(1563) = %+v ok=%v, want episodes 1-29 of TVDB 71634 season 2", m, ok)
	}
	if _, m, ok := facts(2593); !ok || !slices.Equal(m.Episodes, []int{1}) || m.EpisodesTvdb != 82099 || m.EpisodesSeason != 1 {
		t.Errorf("MappingFor(2593) = %+v ok=%v, want episode 1 of TVDB 82099 season 1", m, ok)
	}
	if _, m, ok := facts(2269); !ok || !slices.Equal(m.Specials, []int{2, 3, 4}) || m.SpecialsEpisodes != 3 || m.Episodes != nil {
		t.Errorf("MappingFor(2269) = %+v ok=%v, want specials [2 3 4]", m, ok)
	}
	if rec, m, ok := facts(6090); rec.AniDBID != 1466 || !slices.Equal(rec.TmdbMovies, []int{66033}) || ok {
		t.Errorf("6090 = %+v, facts %+v ok=%v; want AniDB 1466 with its film and the AniDB join, which holds no facts", rec, m, ok)
	}
	if rec, m, ok := facts(1563); rec.AniDBID != 0 || rec.TvdbID != 71634 || rec.SeasonKind != mapping.SeasonPresent || rec.SeasonTvdb != 2 || !ok ||
		!slices.Equal(m.Seasons, []mapping.SeasonRange{{Season: 2, First: 1, Last: 29}}) {
		t.Errorf("1563 = %+v, facts %+v ok=%v; want a link on TVDB 71634 season 2, its entry episodes 1 to 29", rec, m, ok)
	}
}

// listedSeason lists episodes first..last of season, held is the one with a file.
func listedSeason(first, last, held int) map[int]library.ListedEpisode {
	out := map[int]library.ListedEpisode{}
	for k := first; k <= last; k++ {
		out[k] = library.ListedEpisode{}
	}
	out[held] = library.ListedEpisode{Group: "mtbb", HasFile: true}
	return out
}

func TestAnimapLinksScopeThroughTheMatcher(t *testing.T) {
	idx := linksIndex(t)
	for _, tc := range []struct {
		name     string
		item     library.Item
		episodes []int
		kind     align.ScopeKind
		al       int
		season   int
	}{
		{
			name: "1563 fills season 2", al: 1563, kind: align.ScopeSeason, season: 2,
			item: library.Item{
				Arr: library.ArrSonarr, ArrID: 1, TvdbID: 71634, SeasonGroups: map[int][]string{2: {"mtbb"}},
				Listed: map[int]map[int]library.ListedEpisode{2: listedSeason(1, 29, 1)},
			},
		},
		{
			name: "2593 is one film of season 1", al: 2593, kind: align.ScopeEpisodes, season: 1, episodes: []int{1},
			item: library.Item{
				Arr: library.ArrSonarr, ArrID: 2, TvdbID: 82099, SeasonGroups: map[int][]string{1: {"mtbb"}},
				Listed: map[int]map[int]library.ListedEpisode{1: listedSeason(1, 7, 1)},
			},
		},
		{
			name: "2593 with season 1 unlisted", al: 2593, kind: align.ScopeSeason, season: 1,
			item: library.Item{Arr: library.ArrSonarr, ArrID: 2, TvdbID: 82099, SeasonGroups: map[int][]string{1: {"mtbb"}}},
		},
		{
			name: "2269 is three specials", al: 2269, kind: align.ScopeEpisodes, season: 0, episodes: []int{2, 3, 4},
			item: library.Item{
				Arr: library.ArrSonarr, ArrID: 3, TvdbID: 254931, SeasonGroups: map[int][]string{0: {"mtbb"}},
				Specials: listedSeason(1, 6, 2),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{tc.item}}
			res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: tc.al}}, snap, idx, Memo{})
			if len(res.Matches) != 1 || !res.Matches[0].InLibrary() {
				t.Fatalf("Match(%d) = %+v, want one match on the Sonarr item", tc.al, res.Matches)
			}
			m := &res.Matches[0]
			d := align.Decide(m.Item, m.AlignEntry(), &align.Listing{Best: []string{"mtbb"}})
			if d.Kind != tc.kind || d.Season != tc.season || !slices.Equal(d.Episodes, tc.episodes) {
				t.Errorf("Decide(%d) = %v season %d episodes %v, want %v season %d episodes %v", tc.al, d.Kind, d.Season, d.Episodes, tc.kind, tc.season, tc.episodes)
			}
		})
	}
}

// TestMatchOverrideOnAnotherSeriesDropsTheSeasonRanges pins that season
// ranges stay on the series they number: an override moving AniDB 70 from
// TVDB 500 to 600 must not scope 600 to 500's season 8, while one keeping
// 500 keeps them.
func TestMatchOverrideOnAnotherSeriesDropsTheSeasonRanges(t *testing.T) {
	ranges := []mapping.SeasonRange{{Season: 8, First: 1, Last: 12}}
	for _, tc := range []struct {
		want   []mapping.SeasonRange
		name   string
		series int
	}{
		{name: "moved to another series", series: 600},
		{name: "kept on its series", series: 500, want: ranges},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := mapping.NewIndex(mapping.Source{
				Records:   []mapping.Record{{AniListID: 70, Type: "TV", TvdbID: 500, AniDBID: 70, SeasonKind: mapping.SeasonAbsent}},
				Mappings:  map[int]mapping.Mapping{70: {Seasons: ranges, SeasonsTvdb: 500}},
				Overrides: []mapping.Record{{AniListID: 70, Type: "TV", TvdbID: tc.series, AniDBID: 70, SeasonKind: mapping.SeasonAbsent}},
			})
			snap := &library.Snapshot{Items: []library.Item{
				{Arr: library.ArrSonarr, ArrID: 1, TvdbID: tc.series, SeasonGroups: map[int][]string{1: {"mtbb"}, 8: {"mtbb"}}},
			}}
			res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 70}}, snap, idx, Memo{})
			if len(res.Matches) != 1 || !res.Matches[0].InLibrary() {
				t.Fatalf("Match = %+v, want one match on TVDB %d", res.Matches, tc.series)
			}
			if got := res.Matches[0].Seasons; !slices.Equal(got, tc.want) {
				t.Errorf("Seasons(override on TVDB %d) = %+v, want %+v", tc.series, got, tc.want)
			}
		})
	}
}

// TestMatchSeasonsAreTheStatedRanges pins which seasons a whole-series
// comparison judges as the entry's own: the ranges the map states, and none
// for an episode it states nothing for. AniList 1199's shape (its rows place
// season 1, and other episodes have nothing stated) must not take in an
// unclaimed season 5 holding another group.
func TestMatchSeasonsAreTheStatedRanges(t *testing.T) {
	stated := []mapping.SeasonRange{{Season: 1, First: 1, Last: 25}}
	idx := mapping.NewIndex(mapping.Source{
		Records:  []mapping.Record{{AniListID: 1199, Type: "TV", TvdbID: 79000, AniDBID: 270, SeasonKind: mapping.SeasonAbsent}},
		Mappings: map[int]mapping.Mapping{270: {Seasons: stated, SeasonsTvdb: 79000}},
	})
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, TvdbID: 79000, SeasonGroups: map[int][]string{1: {"mtbb"}, 5: {"other"}}},
	}}
	res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 1199}}, snap, idx, Memo{})
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() {
		t.Fatalf("Match = %+v, want one match on the Sonarr item", res.Matches)
	}
	m := &res.Matches[0]
	if !slices.Equal(m.Seasons, stated) {
		t.Errorf("Seasons(1199) = %+v, want the stated %+v", m.Seasons, stated)
	}
	d := align.Decide(m.Item, m.AlignEntry(), &align.Listing{Best: []string{"mtbb"}})
	if d.Kind != align.ScopeWholeSeries || !slices.Equal(d.Groups, []string{"mtbb"}) || d.Standing != align.StandingBest {
		t.Errorf("Decide(1199) = %v groups %v standing %v, want a whole-series comparison of season 1 alone, on the best group", d.Kind, d.Groups, d.Standing)
	}
}
