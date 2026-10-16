package match

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// linksFixture holds animap's records for AniList 435, 1563, 2593, 2269 and
// 6090 as a build with overlay links publishes them. It is extracted from
// that build's animap.json, never written by a test:
//
//	jq '{version, records: [.records[] | select(.anilist_id == (435,1563,2593,2269,6090))]}' animap.json
const linksFixture = "testdata/animap_links.json"

func linksIndex(t *testing.T) *mapping.Index {
	t.Helper()
	body, err := os.ReadFile(linksFixture)
	if err != nil {
		t.Fatalf("read %s (extract it with the jq command in its comment): %v", linksFixture, err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(ts.Close)
	_, idx, err := mapping.NewLoader(ts.Client(), ts.URL, mapping.WithLogger(slog.New(slog.DiscardHandler))).Load(t.Context(), nil)
	if err != nil {
		t.Fatalf("load %s: %v (re-extract it with the jq command in its comment)", linksFixture, err)
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
			t.Fatalf("Lookup(%d): no record in %s", al, linksFixture)
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
	if _, m, ok := facts(2269); !ok || !slices.Equal(m.Specials, []int{2, 3, 4}) || m.Episodes != nil {
		t.Errorf("MappingFor(2269) = %+v ok=%v, want specials [2 3 4]", m, ok)
	}
	if rec, m, ok := facts(6090); rec.AniDBID != 1466 || ok {
		t.Errorf("6090 = %+v, facts %+v ok=%v; want AniDB 1466 and the AniDB join, which holds no facts", rec, m, ok)
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
