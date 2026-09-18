package indexer

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestTwinTitle pins the title shape twinTitle documents, and its three gates:
// nothing for a Radarr target, for episode 0, or for a blank series title.
func TestTwinTitle(t *testing.T) {
	tor := &seadex.Torrent{
		ReleaseGroup: "G",
		DualAudio:    true,
		Files:        []seadex.File{{Name: "Lelouch of the Resurrection (1080p) [G].mkv"}},
	}
	tests := []struct {
		name string
		info EntryInfo
		want string
	}{
		{name: "sonarr film with an episode", info: EntryInfo{Target: TargetSonarr, IsMovie: true, SpecialEpisode: 4, SeriesTitle: "Code Geass"}, want: "Code Geass S00E04 1080p Dual Audio [G]"},
		{name: "radarr target", info: EntryInfo{Target: TargetRadarr, IsMovie: true, SpecialEpisode: 4, SeriesTitle: "Code Geass"}},
		{name: "no episode", info: EntryInfo{Target: TargetSonarr, IsMovie: true, SeriesTitle: "Code Geass"}},
		{name: "blank series title", info: EntryInfo{Target: TargetSonarr, IsMovie: true, SpecialEpisode: 4, SeriesTitle: "  "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := twinTitle(tor, &tc.info); got != tc.want {
				t.Errorf("twinTitle(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}
}

// TestTwinGUIDKeysToTheSameTracker pins the identity rule the twin rests on: the
// fragment GUID passes trackerKeyFromURL to the SAME key as the original for both
// tracker URL shapes (a Nyaa view URL, an AnimeBytes torrentid URL), so the search
// path still recognizes the twin as the curated release while Prowlarr and
// Sonarr, which key on the GUID string, keep the two items apart.
func TestTwinGUIDKeysToTheSameTracker(t *testing.T) {
	for _, guid := range []string{
		"https://nyaa.si/view/1234567",
		"https://animebytes.tv/torrents.php?id=1&torrentid=1167293",
	} {
		twin := twinGUID(guid)
		if twin == guid || twin == "" {
			t.Fatalf("twinGUID(%q) = %q, want a distinct non-empty GUID", guid, twin)
		}
		if got, want := trackerKeyFromURL(twin), trackerKeyFromURL(guid); got != want || want == "" {
			t.Errorf("trackerKeyFromURL(twin %q) = %q, want the original's %q", twin, got, want)
		}
	}
	if twinGUID("") != "" {
		t.Error(`twinGUID("") != "", want empty`)
	}
}

// TestTwinVote pins the holders-agree fold on the twin title: agreement resolves
// it, disagreement vetoes it (and stays a veto through merge), an abstaining
// holder changes nothing, and nobody voting resolves nothing while candidates
// tells a veto from an abstention.
func TestTwinVote(t *testing.T) {
	tests := []struct {
		name           string
		titles         []string
		want           string
		wantCandidates int
	}{
		{name: "none", titles: nil, want: "", wantCandidates: 0},
		{name: "one", titles: []string{"Series S00E04 [G]"}, want: "Series S00E04 [G]", wantCandidates: 1},
		{name: "agree", titles: []string{"Series S00E04 [G]", "Series S00E04 [G]"}, want: "Series S00E04 [G]", wantCandidates: 2},
		{name: "one abstains", titles: []string{"", "Series S00E04 [G]", ""}, want: "Series S00E04 [G]", wantCandidates: 1},
		{name: "disagree", titles: []string{"Series S00E02 [G]", "Series S00E03 [G]"}, want: "", wantCandidates: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v twinVote
			for _, title := range tc.titles {
				v.add(title)
			}
			if got := v.resolve(); got != tc.want {
				t.Errorf("resolve() = %q, want %q", got, tc.want)
			}
			if v.candidates != tc.wantCandidates {
				t.Errorf("candidates = %d, want %d", v.candidates, tc.wantCandidates)
			}
		})
	}
	var a, b twinVote
	a.add("Series S00E02 [G]")
	b.add("Series S00E03 [G]")
	a.merge(b)
	if a.resolve() != "" || a.candidates != 2 {
		t.Errorf("merge of two different titles resolve() = %q candidates %d, want a veto with 2 candidates", a.resolve(), a.candidates)
	}
	var c, d twinVote
	c.add("Series S00E02 [G]")
	d.merge(c)
	if d.resolve() != "Series S00E02 [G]" {
		t.Errorf("merge into an empty vote resolve() = %q, want the merged title", d.resolve())
	}
}
