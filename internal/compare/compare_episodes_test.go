package compare

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestComparePlacedSpecialAlertsLikeASeason(t *testing.T) {
	entry := seadex.Entry{AniListID: 960, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "MTBB", Tracker: "Nyaa", URL: "https://nyaa.si/view/960"},
	}}
	rec := mapping.Record{Type: "OVA", TvdbID: 1, SeasonKind: mapping.SeasonPresent}
	item := func(group string) *library.Item {
		it := &library.Item{
			Title: "Series", Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"mtbb", "oz"}},
			Specials: map[int]library.SpecialEpisode{4: {Group: "mtbb", HasFile: true}, 9: {}, 10: {}},
		}
		if group != "" {
			it.Specials[9] = library.SpecialEpisode{Group: group, HasFile: true}
			it.Specials[10] = library.SpecialEpisode{Group: group, HasFile: true}
		}
		return it
	}
	compare := func(it *library.Item) []Finding {
		m := match.Match{Item: it, Arr: library.ArrSonarr, Entry: entry, Record: rec, Specials: []int{9, 10}}
		return comparer(filter.Options{}, false).Compare([]match.Match{m})
	}
	got := compare(item("oz"))
	if len(got) != 1 {
		t.Fatalf("Compare(S00E09-E10 on oz, best mtbb) = %+v, want one finding", got)
	}
	f := got[0]
	if f.Status != StatusBetter || f.Scope != "episodes" || f.Season != 0 || !slices.Equal(f.Episodes, []int{9, 10}) ||
		f.CurrentGroup != "oz" || f.Approx {
		t.Errorf("Compare(S00E09-E10 on oz) = status %q scope %q season %d episodes %v current %q approx %v; want better_release on episodes [9 10] from oz, exact",
			f.Status, f.Scope, f.Season, f.Episodes, f.CurrentGroup, f.Approx)
	}
	if got := compare(item("mtbb")); len(got) != 0 {
		t.Errorf("Compare(S00E09-E10 on its best group) = %+v, want none", got)
	}
	if got := compare(item("")); len(got) != 0 {
		t.Errorf("Compare(S00E09-E10 empty, another file on S00E04) = %+v, want none: nothing of its own is on disk", got)
	}
}
