package audit

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestScopeLabel(t *testing.T) {
	tests := []struct {
		name string
		row  Row
		want string
	}{
		{"movie", Row{Arr: library.ArrRadarr}, "movie"},
		{"special", Row{Arr: library.ArrSonarr, Special: true}, "special"},
		{"numbered season", Row{Arr: library.ArrSonarr, Season: 2}, "S2"},
		{"whole series", Row{Arr: library.ArrSonarr}, "series"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeLabel(&tt.row); got != tt.want {
				t.Errorf("scopeLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScopeCellMarksApprox(t *testing.T) {
	if got := scopeCell(&Row{Arr: library.ArrSonarr, Season: 2, Approx: true}); got != "S2 (approx)" {
		t.Errorf("scopeCell() = %q, want \"S2 (approx)\"", got)
	}
	if got := scopeCell(&Row{Arr: library.ArrSonarr, Season: 2}); got != "S2" {
		t.Errorf("scopeCell() = %q, want \"S2\"", got)
	}
}

func TestDisplayBestGroups(t *testing.T) {
	rels := []Release{
		{Group: "SubsPlease", Best: true},
		{Group: "subsplease", Best: true},
		{Group: "Erai", Best: false},
	}
	got := displayBestGroups(rels)
	if !reflect.DeepEqual(got, []string{"SubsPlease"}) {
		t.Errorf("displayBestGroups() = %v, want [SubsPlease] (best-only, case-insensitive dedupe, original case)", got)
	}
}

func TestGroupSets(t *testing.T) {
	rels := []Release{
		{Group: "SubsPlease", Best: true},
		{Group: "subsplease", Best: true},
		{Group: "Erai", Best: false},
	}
	best, alt := groupSets(rels)
	if !reflect.DeepEqual(best, []string{"subsplease"}) {
		t.Errorf("best = %v, want [subsplease]", best)
	}
	if !reflect.DeepEqual(alt, []string{"erai"}) {
		t.Errorf("alt = %v, want [erai]", alt)
	}
}

func TestEscapeCell(t *testing.T) {
	if got := escapeCell("a|b\nc"); got != "a\\|b c" {
		t.Errorf("escapeCell() = %q, want %q", got, "a\\|b c")
	}
}

func TestClassifyReleasesGatesAnimeBytes(t *testing.T) {
	entry := &seadex.Entry{Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", ReleaseGroup: "SubsPlease", IsBest: true, URL: "https://nyaa.si/view/1"},
		{Tracker: "AB", ReleaseGroup: "Commie", IsBest: false, URL: "/torrents.php?id=1"},
	}}

	off := NewAuditor(Config{}).classifyReleases(entry)
	if len(off) != 1 || off[0].Tracker != "Nyaa" {
		t.Errorf("with AnimeBytes off only the Nyaa release should survive, got %+v", off)
	}

	on := NewAuditor(Config{AnimeBytes: true}).classifyReleases(entry)
	if len(on) != 2 {
		t.Errorf("with AnimeBytes on both releases should be present, got %d", len(on))
	}
}

func TestLinksBuildsArrSeaDexAndBestOnly(t *testing.T) {
	row := &Row{
		Arr:       "sonarr",
		ArrURL:    "http://sonarr/series/frieren",
		SeaDexURL: "https://releases.moe/154587",
		Releases: []Release{
			{Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
			{Best: false, Tracker: "AB", URL: "https://animebytes.tv/x"},
		},
	}
	got := links(row)
	if !strings.Contains(got, "http://sonarr/series/frieren") {
		t.Error("links must include the arr deep-link")
	}
	if !strings.Contains(got, "https://releases.moe/154587") {
		t.Error("links must include the SeaDex entry link")
	}
	if !strings.Contains(got, "https://nyaa.si/view/1") {
		t.Error("links must include the best-release link")
	}
	if strings.Contains(got, "animebytes.tv/x") {
		t.Error("links must not include a non-best release link")
	}
}

func TestLinksEmptyIsPlaceholder(t *testing.T) {
	if got := links(&Row{}); got != emptyCell {
		t.Errorf("links() = %q, want empty-cell placeholder %q", got, emptyCell)
	}
}

func TestRenderMarkdownAndJSON(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows: []Row{{
			Title: "Frieren", Arr: "sonarr", Verdict: VerdictBest, Season: 1,
			CurrentGroups: []string{"subsplease"},
			Releases:      []Release{{Group: "SubsPlease", Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
		}},
	}
	md := RenderMarkdown(r)
	for _, want := range []string{"# SeaDex alignment report", "Frieren", string(VerdictBest)} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	if _, err := RenderJSON(r); err != nil {
		t.Errorf("RenderJSON: %v", err)
	}
}
