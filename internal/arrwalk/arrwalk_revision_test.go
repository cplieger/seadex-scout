package arrwalk

import (
	"errors"
	"testing"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/release"
)

func revisedEpFile(season int, group string, version int, isRepack bool) arrapi.EpisodeFile {
	f := epFile(season, group)
	f.Quality = &arrapi.QualityModel{Revision: &arrapi.Revision{Version: version, IsRepack: isRepack}}
	return f
}

func walkOneSeries(t *testing.T, files []arrapi.EpisodeFile) Item {
	t.Helper()
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Show"}},
		files:  map[int][]arrapi.EpisodeFile{1: files},
	}
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("Walk items = %d, want 1", len(snap.Items))
	}
	return snap.Items[0]
}

func TestWalkSonarrFoldsNewestRevisionPerSeasonAndGroup(t *testing.T) {
	v2 := release.Revision{Version: 2, Marker: release.RevisionVersion}
	none1 := release.Revision{Version: 1, Marker: release.RevisionNone}
	it := walkOneSeries(t, []arrapi.EpisodeFile{
		revisedEpFile(1, "G", 1, false),
		revisedEpFile(1, "G", 1, false),
		revisedEpFile(1, "G", 2, false),
		revisedEpFile(1, "K", 1, false),
		revisedEpFile(0, "G", 1, false),
	})
	tests := []struct {
		season int
		group  string
		want   release.Revision
	}{
		{1, "g", v2},
		{1, "k", none1},
		{0, "g", none1},
	}
	for _, tc := range tests {
		if got := it.SeasonRevisions[tc.season][tc.group]; got != tc.want {
			t.Errorf("SeasonRevisions[%d][%q] = %+v, want %+v", tc.season, tc.group, got, tc.want)
		}
	}
	if got := it.Revisions["g"]; got != v2 {
		t.Errorf("Revisions[g] = %+v, want %+v", got, v2)
	}
	if got := it.Revisions["k"]; got != none1 {
		t.Errorf("Revisions[k] = %+v, want %+v", got, none1)
	}
}

func TestWalkSonarrRevisionFallsBackToExplicitNameToken(t *testing.T) {
	named := epFile(1, "G")
	named.SceneName = "[G] Show - 03v2 (BD 1080p)"
	it := walkOneSeries(t, []arrapi.EpisodeFile{named})
	if got := it.SeasonRevisions[1]["g"]; got != (release.Revision{Version: 2, Marker: release.RevisionVersion}) {
		t.Errorf("SeasonRevisions[1][g] with no recorded quality and a v2 scene name = %+v, want explicit v2", got)
	}
}

func TestWalkSonarrUnknownRevisionPoisonsItsUnit(t *testing.T) {
	unread := epFile(1, "G")
	unread.RelativePath = "Season 01/Show - S01E02 - Title [Bluray-1080p].mkv"
	it := walkOneSeries(t, []arrapi.EpisodeFile{revisedEpFile(1, "G", 2, false), unread, revisedEpFile(2, "G", 1, false)})
	if got := it.SeasonRevisions[1]["g"]; got.Known() {
		t.Errorf("SeasonRevisions[1][g] beside a file with no revision evidence = %+v, want unknown", got)
	}
	if got := it.SeasonRevisions[2]["g"]; got != (release.Revision{Version: 1, Marker: release.RevisionNone}) {
		t.Errorf("SeasonRevisions[2][g] = %+v, want v1 (the unknown file is in another season)", got)
	}
	if got := it.Revisions["g"]; got.Known() {
		t.Errorf("Revisions[g] = %+v, want unknown when any of the group's files is unknown", got)
	}
}

func TestWalkSonarrFilelessAndFailedSeriesCarryNoRevisions(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Fileless"}, {ID: 2, Title: "Failed"}, {ID: 3, Title: "Fine"}},
		files:  map[int][]arrapi.EpisodeFile{1: {}, 3: {revisedEpFile(1, "G", 1, false)}},
		epErr:  map[int]error{2: errors.New("episode fetch boom")},
	}
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, it := range snap.Items {
		if it.ArrID == 3 {
			continue
		}
		if it.SeasonRevisions != nil || it.Revisions != nil {
			t.Errorf("%s revisions = %+v / %+v, want nil maps", it.Title, it.SeasonRevisions, it.Revisions)
		}
	}
}

func TestWalkRadarrCarriesRecordedRepackRevision(t *testing.T) {
	repack := release.Revision{Version: 2, Marker: release.RevisionRepack}
	fr := &fakeRadarr{movies: []arrapi.Movie{{
		ID: 1, Title: "Given", HasFile: true,
		MovieFile: &arrapi.MovieFile{
			ReleaseGroup: "Grp",
			RelativePath: "Given (2020) [Bluray-1080p].mkv",
			Quality:      &arrapi.QualityModel{Revision: &arrapi.Revision{Version: 2, IsRepack: true}},
		},
	}}}
	snap, err := NewWalker(&Config{Radarr: fr, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("Walk items = %d, want 1", len(snap.Items))
	}
	it := snap.Items[0]
	if got := it.Revisions["grp"]; got != repack {
		t.Errorf("Revisions[grp] = %+v, want %+v", got, repack)
	}
	if it.Current.Revision != repack {
		t.Errorf("Current.Revision = %+v, want %+v", it.Current.Revision, repack)
	}
	if it.SeasonRevisions != nil {
		t.Errorf("SeasonRevisions = %+v, want nil for a movie", it.SeasonRevisions)
	}
}
