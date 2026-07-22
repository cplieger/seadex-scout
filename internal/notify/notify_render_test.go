package notify

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

// TestTrackerURLsRoutesMislabeledABURLToABSlot pins the URL-aware half of
// the AB routing rule: the tracker label is untrusted upstream data, so a
// link labeled "Nyaa" whose URL points at animebytes.tv must land in the AB
// slot (hidden while the toggle is off), never in the public/Nyaa slot, and
// the genuine Nyaa link still wins the nyaa slot.
func TestTrackerURLsRoutesMislabeledABURLToABSlot(t *testing.T) {
	links := []compare.ReleaseLink{
		{Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=9"},
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/9"},
	}
	nyaa, ab := trackerURLs(links)
	if ab != "https://animebytes.tv/torrents.php?id=9" {
		t.Errorf("ab = %q, want the mislabeled animebytes.tv URL routed to the AB slot", ab)
	}
	if nyaa != "https://nyaa.si/view/9" {
		t.Errorf("nyaa = %q, want the genuine Nyaa URL", nyaa)
	}
}

// TestTrackerURLsMalformedURLFailsClosedToABSlot pins the conservative fail
// direction trackerURLs documents: a link whose raw URL is malformed,
// host-hiding, or has a non-ASCII (homoglyph) host is unclassifiable, so it
// must fill the AB slot (hidden while the toggle is off) and never render as
// the clickable public URL - even when its tracker label claims a public
// tracker. The genuine Nyaa link still wins the nyaa slot.
func TestTrackerURLsMalformedURLFailsClosedToABSlot(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "malformed URL", url: "https://animebytes.tv exploit"},
		{name: "hidden host form", url: "https:/animebytes.tv/torrents.php?id=9"},
		{name: "non-ascii homoglyph host", url: "https://animebytes\uff0etv/torrents.php?id=9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			links := []compare.ReleaseLink{
				{Tracker: "Nyaa", URL: tc.url},
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/9"},
			}
			nyaa, ab := trackerURLs(links)
			if ab != tc.url {
				t.Errorf("ab = %q, want the unclassifiable URL %q routed to the AB slot (fail closed)", ab, tc.url)
			}
			if nyaa != "https://nyaa.si/view/9" {
				t.Errorf("nyaa = %q, want the genuine Nyaa URL, never the unclassifiable one", nyaa)
			}
		})
	}
}
