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

// TestCompareAltGroups pins Finding.AltGroups: the normalized, de-duplicated,
// sorted groups the entry lists only as alts. A group with any best torrent is
// never an alt, including a best the content filters exclude, and a definite
// AnimeBytes alt is hidden while the toggle is off.
func TestCompareAltGroups(t *testing.T) {
	best := seadex.Torrent{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1", DualAudio: true}
	tests := []struct {
		name   string
		others []seadex.Torrent
		c      *Comparer
		want   []string
	}{
		{
			name: "alts listed normalized and sorted",
			others: []seadex.Torrent{
				{ReleaseGroup: "Judas", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
				{ReleaseGroup: "Erai-raws", Tracker: "Nyaa", URL: "https://nyaa.si/view/3"},
				{ReleaseGroup: "erai-raws", Tracker: "Nyaa", URL: "https://nyaa.si/view/4"},
			},
			c:    comparer(filter.Options{}, false),
			want: []string{"erai-raws", "judas"},
		},
		{
			name: "a best group is never an alt",
			others: []seadex.Torrent{
				{ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/5"},
				{IsBest: true, ReleaseGroup: "Kawaiika", Tracker: "Nyaa", URL: "https://nyaa.si/view/6"},
				{ReleaseGroup: "Kawaiika", Tracker: "Nyaa", URL: "https://nyaa.si/view/7"},
				{ReleaseGroup: "Judas", Tracker: "Nyaa", URL: "https://nyaa.si/view/8"},
			},
			c:    comparer(filter.Options{RequireDualAudio: true}, false),
			want: []string{"judas"},
		},
		{
			name:   "an AnimeBytes alt is hidden with the toggle off",
			others: []seadex.Torrent{{ReleaseGroup: "PMR", Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}},
			c:      comparer(filter.Options{}, false),
			want:   nil,
		},
		{
			name:   "an AnimeBytes alt is listed with the toggle on",
			others: []seadex.Torrent{{ReleaseGroup: "PMR", Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}},
			c:      abComparer(),
			want:   []string{"pmr"},
		},
		{
			name: "an entry with only best releases has no alt",
			c:    comparer(filter.Options{}, false),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Title: "Frieren", Groups: []string{"horriblesubs"}, SeasonGroups: map[int][]string{1: {"horriblesubs"}}}
			entry := seadex.Entry{AniListID: 154587, Torrents: append([]seadex.Torrent{best}, tt.others...)}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			got := tt.c.Compare([]match.Match{m})

			if len(got) != 1 || got[0].Status != StatusBetter || got[0].RecommendedGroup != "SubsPlease" {
				t.Fatalf("Compare(%s) = %+v, want one better_release finding recommending SubsPlease", tt.name, got)
			}
			if !slices.Equal(got[0].AltGroups, tt.want) {
				t.Errorf("Compare(%s).AltGroups = %q, want %q", tt.name, got[0].AltGroups, tt.want)
			}
		})
	}
}

// TestCompareAltGroupsOnEveryStatus pins that the alt groups ride a finding
// whatever its status, so a reader of the finding line never depends on it.
func TestCompareAltGroupsOnEveryStatus(t *testing.T) {
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
				{ReleaseGroup: "Judas", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
			}}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			got := comparer(filter.Options{}, false).Compare([]match.Match{m})

			if len(got) != 1 || got[0].Status != tt.wantStatus {
				t.Fatalf("Compare(held %v, incomplete %v) = %+v, want one %s finding", tt.held, tt.incomplete, got, tt.wantStatus)
			}
			if want := []string{"judas"}; !slices.Equal(got[0].AltGroups, want) {
				t.Errorf("Compare(held %v, incomplete %v).AltGroups = %q, want %q", tt.held, tt.incomplete, got[0].AltGroups, want)
			}
		})
	}
}
