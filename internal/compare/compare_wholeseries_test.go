package compare

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func wholeSeriesMatch(seasons map[int][]string, entry seadex.Entry) match.Match {
	return match.Match{
		Item:   &library.Item{Title: "Absolute Run", Arr: library.ArrSonarr, SeasonGroups: seasons},
		Arr:    library.ArrSonarr,
		Entry:  entry,
		Record: mapping.Record{Type: "TV", SeasonTvdb: 0},
	}
}

func bestEntry(alID int, group string) seadex.Entry {
	return seadex.Entry{AniListID: alID, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: group, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}}
}

func TestCompareWholeSeries(t *testing.T) {
	tests := []struct {
		name       string
		seasons    map[int][]string
		entry      seadex.Entry
		wantCount  int
		wantStatus Status
		wantSev    Severity
	}{
		{
			name:      "every real season already carries the recommended group is aligned",
			seasons:   map[int][]string{1: {"subsplease"}, 2: {"subsplease"}},
			entry:     bestEntry(1, "SubsPlease"),
			wantCount: 0,
		},
		{
			name:       "a real season lacking the recommended group is a better_release finding",
			seasons:    map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
			entry:      bestEntry(2, "SubsPlease"),
			wantCount:  1,
			wantStatus: StatusBetter,
			wantSev:    SevWarn,
		},
		{
			name:      "only season 0 on disk (no real season) is silent",
			seasons:   map[int][]string{0: {"subsplease"}},
			entry:     bestEntry(3, "SubsPlease"),
			wantCount: 0,
		},
		{
			name:    "incomplete entry with a best-less season is an info nudge",
			seasons: map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
			entry: func() seadex.Entry {
				e := bestEntry(4, "SubsPlease")
				e.Incomplete = true
				return e
			}(),
			wantCount:  1,
			wantStatus: StatusIncomplete,
			wantSev:    SevInfo,
		},
		{
			name:       "no recommended release but incomplete falls back to an info nudge",
			seasons:    map[int][]string{1: {"subsplease"}},
			entry:      seadex.Entry{AniListID: 5, Incomplete: true},
			wantCount:  1,
			wantStatus: StatusIncomplete,
			wantSev:    SevInfo,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := wholeSeriesMatch(tt.seasons, tt.entry)
			got := comparer(filter.Options{}, false).Compare([]match.Match{m})
			if len(got) != tt.wantCount {
				t.Fatalf("finding count = %d, want %d: %+v", len(got), tt.wantCount, got)
			}
			if tt.wantCount == 1 {
				if got[0].Status != tt.wantStatus || got[0].Severity != tt.wantSev {
					t.Errorf("status/severity = %q/%q, want %q/%q", got[0].Status, got[0].Severity, tt.wantStatus, tt.wantSev)
				}
			}
		})
	}
}
