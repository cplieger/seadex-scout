package compare

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestCompareTierOnBetterRelease pins Finding.Tier on a diverged unit, and that
// tier classification cannot change the status or the recommendation: both
// equal those of the main decision in every case, including the untagged-alt
// case, where judging the alt rung in the main decision turns the finding
// unverifiable.
func TestCompareTierOnBetterRelease(t *testing.T) {
	best := seadex.Torrent{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1", DualAudio: true}
	tests := []struct {
		name     string
		held     string
		others   []seadex.Torrent
		opts     filter.Options
		wantTier Tier
	}{
		{
			name: "holds a SeaDex alt", held: "erai-raws",
			others:   []seadex.Torrent{{ReleaseGroup: "Erai-raws", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}},
			wantTier: TierAlt,
		},
		{
			name: "holds nothing SeaDex lists", held: "horriblesubs",
			others:   []seadex.Torrent{{ReleaseGroup: "Erai-raws", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}},
			wantTier: TierUnlisted,
		},
		{
			name: "holds a best the filters exclude", held: "judas",
			others:   []seadex.Torrent{{IsBest: true, ReleaseGroup: "Judas", Tracker: "Nyaa", URL: "https://nyaa.si/view/3"}},
			opts:     filter.Options{RequireDualAudio: true},
			wantTier: TierAlt,
		},
		{
			name: "an untagged alt leaves the tier undecided", held: "horriblesubs",
			others:   []seadex.Torrent{{ReleaseGroup: "", Tracker: "Nyaa", URL: "https://nyaa.si/view/4"}},
			wantTier: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Title: "Frieren", Groups: []string{tt.held}, SeasonGroups: map[int][]string{1: {tt.held}}}
			entry := seadex.Entry{AniListID: 154587, Torrents: append([]seadex.Torrent{best}, tt.others...)}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			got := comparer(tt.opts, false).Compare([]match.Match{m})

			if len(got) != 1 {
				t.Fatalf("Compare(held %s) = %+v, want one finding", tt.held, got)
			}
			if got[0].Status != StatusBetter || got[0].RecommendedGroup != "SubsPlease" {
				t.Errorf("Compare(held %s) status, recommended = %q, %q, want better_release, SubsPlease", tt.held, got[0].Status, got[0].RecommendedGroup)
			}
			if got[0].Tier != tt.wantTier {
				t.Errorf("Compare(held %s).Tier = %q, want %q", tt.held, got[0].Tier, tt.wantTier)
			}
		})
	}
}

// TestCompareTierOnlyOnBetterRelease pins that only a better_release finding
// carries a tier: a mixed-group unit holds several groups, so no single held
// release has one, and an incomplete entry's divergence is an info nudge with
// nothing complete to grab.
func TestCompareTierOnlyOnBetterRelease(t *testing.T) {
	tests := []struct {
		name       string
		held       []string
		incomplete bool
		wantStatus Status
	}{
		{name: "mixed group", held: []string{"a", "b"}, wantStatus: StatusMixedGroup},
		{name: "incomplete entry", held: []string{"a"}, incomplete: true, wantStatus: StatusIncomplete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Title: "Show", Groups: tt.held, SeasonGroups: map[int][]string{1: tt.held}}
			entry := seadex.Entry{AniListID: 7, Incomplete: tt.incomplete, Torrents: []seadex.Torrent{
				{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
				{ReleaseGroup: "a", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
			}}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			got := comparer(filter.Options{}, false).Compare([]match.Match{m})

			if len(got) != 1 || got[0].Status != tt.wantStatus {
				t.Fatalf("Compare(held %v, incomplete %v) = %+v, want one %s finding", tt.held, tt.incomplete, got, tt.wantStatus)
			}
			if got[0].Tier != "" {
				t.Errorf("Compare(held %v, incomplete %v).Tier = %q, want empty", tt.held, tt.incomplete, got[0].Tier)
			}
		})
	}
}

// TestCompareTierHidesAnimeBytesWhenOff pins the one source the tier skips: a
// definite AnimeBytes release is invisible with the toggle off, as in the
// report, so holding its group reads as unlisted there and as listed with the
// toggle on.
func TestCompareTierHidesAnimeBytesWhenOff(t *testing.T) {
	item := &library.Item{Title: "Frieren", Groups: []string{"pmr"}, SeasonGroups: map[int][]string{1: {"pmr"}}}
	entry := seadex.Entry{AniListID: 154587, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{ReleaseGroup: "PMR", Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	for _, tc := range []struct {
		c    *Comparer
		want Tier
	}{
		{c: comparer(filter.Options{}, false), want: TierUnlisted},
		{c: abComparer(), want: TierAlt},
	} {
		got := tc.c.Compare([]match.Match{m})
		if len(got) != 1 || got[0].Status != StatusBetter {
			t.Fatalf("Compare(animebytes=%v) = %+v, want one better_release finding", tc.c.animeBytes, got)
		}
		if got[0].Tier != tc.want {
			t.Errorf("Compare(animebytes=%v).Tier = %q, want %q", tc.c.animeBytes, got[0].Tier, tc.want)
		}
	}
}
