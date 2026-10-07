package notify

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
)

func TestReportPreservesOnlyTheUnreadArrCopy(t *testing.T) {
	special := findingWithID("s", "Series", 5)
	film := findingWithID("r", "Film", 5)
	film.Arr = "radarr"
	cases := []struct {
		preserve Preserve
		name     string
		want     []string
	}{
		{name: "sonarr_copy", preserve: Preserve{{Arr: "sonarr", AniListID: 5}: {}}, want: []string{"Series"}},
		{name: "radarr_copy", preserve: Preserve{{Arr: "radarr", AniListID: 5}: {}}, want: []string{"Film"}},
		{name: "every_copy", preserve: Preserve{{AniListID: 5}: {}}, want: []string{"Film", "Series"}},
		{name: "other_entry", preserve: Preserve{{AniListID: 6}: {}}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier, _ := newCapturedNotifier()
			notifier.Report([]compare.Finding{special, film}, nil)
			notifier.Report(nil, tc.preserve)
			var got []string
			for _, f := range notifier.current {
				got = append(got, f.Title)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Report(nil, %v) after both copies kept %q, want %q", tc.preserve, got, tc.want)
			}
		})
	}
}

func TestFindingLineNamesThePlacedEpisodes(t *testing.T) {
	special := testFinding("k1", "Series")
	special.Scope, special.Episodes = "episodes", []int{9, 10, 12}
	film := special
	film.Arr, film.Scope, film.Episodes = "radarr", "movie", nil
	season := testFinding("k2", "Other")
	season.AniListID, season.Scope, season.Season = 1, "season", 2
	notifier, recorder := newCapturedNotifier()
	notifier.Report([]compare.Finding{special, film, season}, nil)
	msg := message(compare.StatusBetter)
	if !recorder.HasAttr(msg, "episodes", "S00E09-E10, S00E12") {
		t.Errorf("finding lines = %q, want one with episodes=S00E09-E10, S00E12", recorder.Messages())
	}
	if !recorder.HasAttr(msg, "episodes", "") {
		t.Errorf("finding lines = %q, want the season's line with an empty episodes", recorder.Messages())
	}
	lines := 0
	for _, rec := range recorder.Records() {
		if rec.Message == msg {
			lines++
		}
	}
	if lines != 3 {
		t.Errorf("finding lines = %d, want 3: the film's Radarr copy and its Sonarr special are two findings", lines)
	}
}
