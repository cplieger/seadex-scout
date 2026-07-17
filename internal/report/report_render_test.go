package report

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// TestTrackerURLs pins the alert link-splitting rules: the first Nyaa link wins
// the nyaa slot, the first AnimeBytes link wins the ab slot, and when no Nyaa
// link exists the first other public link (e.g. AnimeTosho) stands in as the
// public URL so an alert never renders an empty public link while one exists.
func TestTrackerURLs(t *testing.T) {
	tests := []struct {
		name     string
		wantNyaa string
		wantAB   string
		links    []compare.ReleaseLink
	}{
		{
			name: "nyaa and ab split",
			links: []compare.ReleaseLink{
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
				{Tracker: "AB", URL: "https://animebytes.tv/t/1"},
			},
			wantNyaa: "https://nyaa.si/view/1",
			wantAB:   "https://animebytes.tv/t/1",
		},
		{
			name:     "ab only leaves nyaa empty",
			links:    []compare.ReleaseLink{{Tracker: "animebytes", URL: "https://animebytes.tv/t/2"}},
			wantNyaa: "",
			wantAB:   "https://animebytes.tv/t/2",
		},
		{
			name:     "other public tracker fills the nyaa slot",
			links:    []compare.ReleaseLink{{Tracker: "AnimeTosho", URL: "https://animetosho.org/v/3"}},
			wantNyaa: "https://animetosho.org/v/3",
			wantAB:   "",
		},
		{
			name: "real nyaa link wins over an earlier other-public link",
			links: []compare.ReleaseLink{
				{Tracker: "AnimeTosho", URL: "https://animetosho.org/v/3"},
				{Tracker: "nyaa", URL: "https://nyaa.si/view/4"},
			},
			wantNyaa: "https://nyaa.si/view/4",
			wantAB:   "",
		},
		{
			name: "first of each tracker wins",
			links: []compare.ReleaseLink{
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/5"},
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/6"},
				{Tracker: "AB", URL: "https://animebytes.tv/t/5"},
				{Tracker: "AB", URL: "https://animebytes.tv/t/6"},
			},
			wantNyaa: "https://nyaa.si/view/5",
			wantAB:   "https://animebytes.tv/t/5",
		},
		{name: "no links", links: nil, wantNyaa: "", wantAB: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nyaa, ab := trackerURLs(tc.links)
			if nyaa != tc.wantNyaa {
				t.Errorf("nyaa = %q, want %q", nyaa, tc.wantNyaa)
			}
			if ab != tc.wantAB {
				t.Errorf("ab = %q, want %q", ab, tc.wantAB)
			}
		})
	}
}

// TestSeadexTags pins the alert-footer tag line per status arm, including the
// arms the existing finding fixtures never exercise (incomplete, theoretical,
// mixed-group), the unknown-kind suppression, and the no-dual-audio case.
func TestSeadexTags(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		finding compare.Finding
	}{
		{
			name:    "better with full detail",
			finding: compare.Finding{Status: compare.StatusBetter, Kind: "encode", Resolution: "1080p", DualAudio: true},
			want:    "best · encode · 1080p · dual-audio",
		},
		{
			name:    "incomplete bare",
			finding: compare.Finding{Status: compare.StatusIncomplete},
			want:    "incomplete",
		},
		{
			name:    "theoretical with remux and resolution",
			finding: compare.Finding{Status: compare.StatusTheoretical, Kind: "remux", Resolution: "2160p"},
			want:    "theoretical-best · remux · 2160p",
		},
		{
			name:    "mixed group with dual audio",
			finding: compare.Finding{Status: compare.StatusMixedGroup, DualAudio: true},
			want:    "mixed-group · dual-audio",
		},
		{
			name:    "unverifiable with resolution",
			finding: compare.Finding{Status: compare.StatusUnverifiable, Resolution: "1080p"},
			want:    "unverifiable · 1080p",
		},
		{
			name:    "unknown kind is suppressed",
			finding: compare.Finding{Status: compare.StatusBetter, Kind: "unknown", Resolution: "720p"},
			want:    "best · 720p",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := seadexTags(&tc.finding); got != tc.want {
				t.Errorf("seadexTags = %q, want %q", got, tc.want)
			}
		})
	}
}
