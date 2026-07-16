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
		seasons    map[int][]string
		name       string
		wantStatus Status
		wantSev    Severity
		entry      seadex.Entry
		wantCount  int
	}{
		{
			name:      "every real season already carries the recommended group is aligned",
			seasons:   map[int][]string{1: {"subsplease"}, 2: {"subsplease"}},
			entry:     bestEntry(1, "SubsPlease"),
			wantCount: 0,
		},
		{
			name:       "a single-group aggregate lacking the recommended group is a better_release finding",
			seasons:    map[int][]string{1: {"erai-raws"}, 2: {"erai-raws"}},
			entry:      bestEntry(2, "SubsPlease"),
			wantCount:  1,
			wantStatus: StatusBetter,
			wantSev:    SevWarn,
		},
		{
			// Mirrors the season-scoped arm's mixed-group guard: a NOT-aligned
			// aggregate spanning two groups is a manual-review nudge, not a
			// false better_release.
			name:       "a not-aligned multi-group aggregate is a mixed_group_manual nudge",
			seasons:    map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
			entry:      bestEntry(6, "SubsPlease"),
			wantCount:  1,
			wantStatus: StatusMixedGroup,
			wantSev:    SevInfo,
		},
		{
			// Alignment wins over the mixed-group nudge: every on-disk season
			// carries the recommended group, so the two-group union is silent.
			name:      "an aligned multi-group aggregate is silent",
			seasons:   map[int][]string{1: {"subsplease"}, 2: {"subsplease", "erai-raws"}},
			entry:     bestEntry(7, "SubsPlease"),
			wantCount: 0,
		},
		{
			name:      "only season 0 on disk (no real season) is silent",
			seasons:   map[int][]string{0: {"subsplease"}},
			entry:     bestEntry(3, "SubsPlease"),
			wantCount: 0,
		},
		{
			// File presence is checked before the recommendation-emptiness
			// nudge: with no real season on disk even a theoretical-only entry
			// is silent (the audit records this as no_file).
			name:      "no real season on disk with a theoretical-only entry is silent",
			seasons:   map[int][]string{0: {"subsplease"}},
			entry:     seadex.Entry{AniListID: 8, TheoreticalBest: "a stated remux"},
			wantCount: 0,
		},
		{
			name:    "incomplete entry with a best-less single-group aggregate is an info nudge",
			seasons: map[int][]string{1: {"erai-raws"}, 2: {"erai-raws"}},
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
