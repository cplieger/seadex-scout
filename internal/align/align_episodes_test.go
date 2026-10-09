package align_test

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

func specialsItem() library.Item {
	return library.Item{
		Arr:          library.ArrSonarr,
		SeasonGroups: map[int][]string{0: {"mtbb", "oz"}, 1: {"subsplease"}},
		Specials: map[int]library.SpecialEpisode{
			4:  {Group: "oz", HasFile: true},
			9:  {Group: "mtbb", HasFile: true},
			10: {Group: "mtbb", HasFile: true},
			11: {},
		},
	}
}

var mappedZeroOVA = mapping.Record{Type: "OVA", TvdbID: 1, SeasonKind: mapping.SeasonPresent}

func TestDecideEpisodesJudgesOnlyThePlacedFiles(t *testing.T) {
	item := specialsItem()
	for _, tc := range []struct {
		name     string
		listing  align.Listing
		standing align.Standing
	}{
		{"its own group is best", align.Listing{Best: []string{"mtbb"}}, align.StandingBest},
		{"another work's group is not its best", align.Listing{Best: []string{"oz"}, Alt: []string{"mtbb"}}, align.StandingAlt},
		{"a group nobody lists", align.Listing{Best: []string{"subsplease"}}, align.StandingUnlisted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := align.Decide(&item, &align.Entry{Record: &mappedZeroOVA, Specials: []int{9, 10}}, &tc.listing)
			if d.Kind != align.ScopeEpisodes || d.Standing != tc.standing {
				t.Errorf("Decide(S00E09-E10, %+v) = %v/%v, want episodes/%v", tc.listing, d.Kind, d.Standing, tc.standing)
			}
			if !slices.Equal(d.Groups, []string{"mtbb"}) || d.Approx || d.Season != 0 {
				t.Errorf("Decide(S00E09-E10) = groups %v approx %v season %d, want [mtbb], exact, season 0", d.Groups, d.Approx, d.Season)
			}
			if !slices.Equal(d.Episodes, []int{9, 10}) || len(d.MissingEpisodes) != 0 {
				t.Errorf("Decide(S00E09-E10) episodes = %v missing %v, want [9 10] and none", d.Episodes, d.MissingEpisodes)
			}
		})
	}
}

func TestDecideEpisodesJudgesTheHeldParts(t *testing.T) {
	item := specialsItem()
	d := align.Decide(&item, &align.Entry{Record: &mappedZeroOVA, Specials: []int{10, 11}}, &align.Listing{Best: []string{"mtbb"}})
	if d.Kind != align.ScopeEpisodes || d.Standing != align.StandingBest {
		t.Errorf("Decide(S00E10-E11, E11 empty) = %v/%v, want episodes/best on the held part", d.Kind, d.Standing)
	}
	if !slices.Equal(d.MissingEpisodes, []int{11}) {
		t.Errorf("Decide(S00E10-E11) missing = %v, want [11]", d.MissingEpisodes)
	}
	none := align.Decide(&item, &align.Entry{Record: &mappedZeroOVA, Specials: []int{11}}, &align.Listing{Best: []string{"mtbb"}})
	if none.Kind != align.ScopeEpisodes || none.Standing != align.StandingNoFile || none.Approx || len(none.Groups) != 0 {
		t.Errorf("Decide(S00E11, empty) = %v/%v approx %v groups %v, want an exact episodes no_file", none.Kind, none.Standing, none.Approx, none.Groups)
	}
}

func TestDecideEpisodesFallsBackToOffered(t *testing.T) {
	unread := specialsItem()
	unread.Specials = nil
	listed := specialsItem()
	for _, tc := range []struct {
		name     string
		item     library.Item
		specials []int
	}{
		{"episodes never read", unread, []int{9}},
		{"a placed episode Sonarr does not list", listed, []int{9, 96}},
		{"no placement", listed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := align.Decide(&tc.item, &align.Entry{Record: &mappedZeroOVA, Specials: tc.specials}, &align.Listing{Best: []string{"mtbb"}})
			if d.Kind != align.ScopeOffered || d.Standing != align.StandingUnverified || !d.Approx || d.Episodes != nil {
				t.Errorf("Decide(%v) = %v/%v approx %v episodes %v, want the offered bucket, unverified and approximate",
					tc.specials, d.Kind, d.Standing, d.Approx, d.Episodes)
			}
		})
	}
}

func TestDecideEpisodesNeverOverridesTheArrOrASeason(t *testing.T) {
	film := library.Item{Arr: library.ArrRadarr, Groups: []string{"oz"}, HasFile: true}
	if d := align.Decide(&film, &align.Entry{Record: &mappedZeroOVA, Specials: []int{9}}, &align.Listing{Best: []string{"oz"}}); d.Kind != align.ScopeMovie {
		t.Errorf("Decide(Radarr film, placed) kind = %v, want movie", d.Kind)
	}
	item := specialsItem()
	season := mapping.Record{Type: "TV", TvdbID: 1, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1}
	if d := align.Decide(&item, &align.Entry{Record: &season, Specials: []int{9}}, &align.Listing{Best: []string{"subsplease"}}); d.Kind != align.ScopeSeason || d.Episodes != nil {
		t.Errorf("Decide(season 1, placed) = %v episodes %v, want season and no episodes", d.Kind, d.Episodes)
	}
}

func TestDecideEpisodesFoldsRevisionsPerGroup(t *testing.T) {
	v1 := release.Revision{Version: 1, Marker: release.RevisionNone}
	v2 := release.Revision{Version: 2, Marker: release.RevisionVersion}
	item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"mtbb"}}, Specials: map[int]library.SpecialEpisode{
		1: {Group: "mtbb", Revision: v1, HasFile: true},
		2: {Group: "mtbb", Revision: v1, HasFile: true},
	}}
	listing := align.Listing{Best: []string{"mtbb"}, BestRevisions: map[string]release.Revision{"mtbb": v2}}
	if d := align.Decide(&item, &align.Entry{Record: &mappedZeroOVA, Specials: []int{1, 2}}, &listing); d.Standing != align.StandingBestSuperseded {
		t.Errorf("Decide(both parts v1, SeaDex v2) = %v, want superseded", d.Standing)
	}
	item.Specials[2] = library.SpecialEpisode{Group: "mtbb", Revision: v2, HasFile: true}
	if d := align.Decide(&item, &align.Entry{Record: &mappedZeroOVA, Specials: []int{1, 2}}, &listing); d.Standing != align.StandingBest {
		t.Errorf("Decide(one part v2, SeaDex v2) = %v, want best", d.Standing)
	}
}

func TestClaimsCoverageIgnoresAPlacedSpecial(t *testing.T) {
	item := specialsItem()
	if align.ClaimsCoverage(&item, &mappedZeroOVA) {
		t.Error("ClaimsCoverage(a season-0 OVA on a Sonarr series) = true, want false")
	}
}

func TestEpisodeLabel(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{14}, "S00E14"},
		{[]int{9, 10}, "S00E09-E10"},
		{[]int{7, 12, 13, 15}, "S00E07, S00E12-E13, S00E15"},
		{[]int{96, 100}, "S00E96, S00E100"},
	} {
		if got := align.EpisodeLabel(tc.in); got != tc.want {
			t.Errorf("EpisodeLabel(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
