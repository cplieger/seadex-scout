package audit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func placedOVAReport(t *testing.T, torrents ...seadex.Torrent) Report {
	t.Helper()
	item := library.Item{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Free!", TvdbID: 700, Groups: []string{"g", "oz"}, HasFile: true,
		SeasonGroups: map[int][]string{0: {"g", "oz"}, 1: {"g"}},
		Specials:     map[int]library.SpecialEpisode{4: {Group: "oz", HasFile: true}, 15: {Group: "g", HasFile: true}, 16: {}},
	}
	snap := &library.Snapshot{Items: []library.Item{item}}
	rec := mapping.Record{AniListID: 5, Type: "OVA", TvdbID: 700, SeasonKind: mapping.SeasonPresent}
	idx := mapping.NewIndex(mapping.Source{Records: []mapping.Record{rec, {AniListID: 9, Type: "TV", TvdbID: 700}}})
	if len(torrents) == 0 {
		torrents = []seadex.Torrent{{IsBest: true, ReleaseGroup: "G", Tracker: "Nyaa", URL: "https://nyaa.si/view/5"}}
	}
	entry := seadex.Entry{AniListID: 5, Torrents: torrents}
	matches := []match.Match{{Item: &snap.Items[0], Arr: library.ArrSonarr, Source: match.SourceID, Entry: entry, Record: rec, Specials: []int{15, 16}}}
	return New(Config{}).Audit(matches, snap, idx, nil)
}

func TestAuditPlacedSpecialRowNamesItsEpisodes(t *testing.T) {
	rep := placedOVAReport(t)
	var row *reportRow
	for i := range rep.Rows {
		if rep.Rows[i].AniListID == 5 {
			row = &rep.Rows[i]
		}
	}
	if row == nil {
		t.Fatalf("Audit rows = %+v, want a row for AniList 5", rep.Rows)
	}
	if row.Verdict != VerdictBest || row.Scope != align.ScopeEpisodes || row.Approx || !slices.Equal(row.CurrentGroups, []string{"g"}) {
		t.Errorf("Audit row = verdict %q scope %v approx %v groups %v, want have_best on episodes from [g], exact", row.Verdict, row.Scope, row.Approx, row.CurrentGroups)
	}
	if !slices.Equal(row.Episodes, []int{15, 16}) || !slices.Equal(row.MissingEpisodes, []int{16}) {
		t.Errorf("Audit row episodes = %v missing %v, want [15 16] and [16]", row.Episodes, row.MissingEpisodes)
	}
	if got := scopeCell(row); got != "S00E15-E16 (missing S00E16)" {
		t.Errorf("scopeCell(row) = %q, want %q", got, "S00E15-E16 (missing S00E16)")
	}
	if rep.Totals[string(VerdictNotOnSeaDex)] != 1 {
		t.Errorf("not_on_seadex rows = %d, want 1: a special answers for its episodes, not for the series", rep.Totals[string(VerdictNotOnSeaDex)])
	}
	if rep.Items.AllBest != 1 {
		t.Errorf("Items.AllBest = %d, want 1: the placed special is a compared row", rep.Items.AllBest)
	}
}

func TestAuditPlacedSpecialOnAnAltKeepsItsSeriesOutOfAllBest(t *testing.T) {
	rep := placedOVAReport(t,
		seadex.Torrent{IsBest: true, ReleaseGroup: "X", Tracker: "Nyaa", URL: "https://nyaa.si/view/6"},
		seadex.Torrent{ReleaseGroup: "G", Tracker: "Nyaa", URL: "https://nyaa.si/view/5"},
	)
	if rep.Totals[string(verdictAlt)] != 1 {
		t.Fatalf("Audit totals = %v, want the placed special as one have_alt row", rep.Totals)
	}
	if rep.Items.AllBest != 0 || rep.Items.AllBestOrAlt != 1 {
		t.Errorf("Items = %+v, want AllBest 0 and AllBestOrAlt 1: a special held only as an alt counts against its series", rep.Items)
	}
}

func TestAuditPlacedCopyMatchDoesNotJudgeStillCovers(t *testing.T) {
	film := mapping.Record{AniListID: 1, AniDBID: 70, Type: "MOVIE", TvdbID: 500, TmdbMovies: []int{42}, SeasonKind: mapping.SeasonPresent}
	ova := film
	ova.Type = "OVA"
	// A second AniList entry for the same film catalogues it in Radarr.
	sameFilm := mapping.Record{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{42}}
	held := library.SpecialEpisode{Group: "mtbb", HasFile: true}
	for _, tc := range []struct {
		name          string
		records       []mapping.Record
		wantArr       string
		wantVerdict   Verdict
		onSpecial     library.SpecialEpisode
		wantScope     align.ScopeKind
		filmHolds     bool
		wantUncovered bool
	}{
		{"a film whose special holds the file", []mapping.Record{film}, library.ArrSonarr, VerdictBest, held, align.ScopeEpisodes, false, false},
		{"an OVA neither copy holds", []mapping.Record{ova, sameFilm}, library.ArrSonarr, VerdictNoFile, library.SpecialEpisode{}, align.ScopeEpisodes, false, true},
		{"an OVA only the film holds", []mapping.Record{ova}, library.ArrRadarr, VerdictBest, library.SpecialEpisode{}, align.ScopeMovie, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			radarr := library.Item{Arr: library.ArrRadarr, ArrID: 3, Title: "Film", TmdbID: 42}
			if tc.filmHolds {
				radarr.HasFile, radarr.Groups = true, []string{"mtbb"}
			}
			snap := &library.Snapshot{Items: []library.Item{radarr, {
				Arr: library.ArrSonarr, ArrID: 7, Title: "Series", TvdbID: 500, Groups: []string{"oz"}, HasFile: true,
				SeasonGroups: map[int][]string{0: {"oz"}},
				Specials:     map[int]library.SpecialEpisode{4: {Group: "oz", HasFile: true}, 9: tc.onSpecial},
			}}}
			idx := mapping.NewIndex(mapping.Source{Records: tc.records, Mappings: map[int]mapping.Mapping{70: {Specials: []int{9}, SpecialsTvdb: 500}}})
			entry := seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{
				{IsBest: true, ReleaseGroup: "MTBB", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
			}}
			res := match.New(nil, nil).Match(t.Context(), []seadex.Entry{entry}, snap, idx, match.Memo{})
			rep := New(Config{}).Audit(res.Matches, snap, idx, nil)
			var compared []reportRow
			seriesUncovered := false
			for _, row := range rep.Rows {
				switch {
				case row.Verdict != VerdictNotOnSeaDex:
					compared = append(compared, row)
				case row.Arr == library.ArrRadarr:
					t.Errorf("Audit(%s) lists the Radarr film as not_on_seadex, want it covered by entry 1", tc.name)
				default:
					seriesUncovered = true
				}
			}
			if len(compared) != 1 || compared[0].Arr != tc.wantArr || compared[0].Verdict != tc.wantVerdict || compared[0].Scope != tc.wantScope {
				t.Errorf("Audit(%s) compared rows = %+v, want one %s %q row on scope %v", tc.name, compared, tc.wantArr, tc.wantVerdict, tc.wantScope)
			}
			if seriesUncovered != tc.wantUncovered {
				t.Errorf("Audit(%s) lists the series as not_on_seadex = %v, want %v: a placed special never covers its series", tc.name, seriesUncovered, tc.wantUncovered)
			}
		})
	}
}

func TestAuditPlacedSpecialRowJSON(t *testing.T) {
	rep := placedOVAReport(t)
	b, err := json.Marshal(rep.Rows)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, want := range []string{`"scope":"episodes"`, `"episodes":[15,16]`, `"missing_episodes":[16]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("rows JSON = %s, want it to carry %s", b, want)
		}
	}
}
