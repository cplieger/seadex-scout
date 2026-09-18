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
		},
		{
			name:       "no recommended release but incomplete falls back to an info nudge",
			seasons:    map[int][]string{1: {"subsplease"}},
			entry:      seadex.Entry{AniListID: 5, Incomplete: true},
			wantCount:  1,
			wantStatus: StatusIncomplete,
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
				if got[0].Status != tt.wantStatus {
					t.Errorf("status = %q, want %q", got[0].Status, tt.wantStatus)
				}
			}
		})
	}
}

// TestCompareWholeSeriesDropsSiblingMappedSeasons is Gintama's live shape on the
// findings path: a seasonless entry whose siblings map seasons 5-8 and 10 has
// only its own S1-S4 left in the aggregate, all of them its SeaDex best, so the
// daemon goes silent where it emitted a false have_unlisted and a false
// mixed_group_manual before. Without the sibling summary the same match reports.
func TestCompareWholeSeriesDropsSiblingMappedSeasons(t *testing.T) {
	seasons := map[int][]string{1: {"cbt"}, 2: {"cbt"}, 3: {"cbt"}, 4: {"cbt"}, 5: {"kh"}, 6: {"kh"}, 7: {"kh"}, 8: {"kh"}, 10: {"kh"}}
	entry := bestEntry(918, "CBT")

	m := wholeSeriesMatch(seasons, entry)
	m.SiblingSeasons = []int{5, 6, 7, 8, 10}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("findings = %+v, want none (every season of the entry's own carries its best group)", got)
	}

	contaminated := wholeSeriesMatch(seasons, entry)
	if got := comparer(filter.Options{}, false).Compare([]match.Match{contaminated}); len(got) != 1 {
		t.Fatalf("findings without the sibling summary = %+v, want 1 (the contaminated verdict this fix removes)", got)
	}
}
