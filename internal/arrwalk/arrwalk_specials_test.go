package arrwalk

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/slogx/capture"
)

func specialEp(number, fileID int) sonarr.Episode {
	return sonarr.Episode{Season: 0, Number: number, HasFile: fileID > 0, FileID: fileID}
}

func TestWalkSonarrReadsSeasonZeroEpisodeFiles(t *testing.T) {
	v2 := &arrapi.Revision{Version: 2}
	fs := &fakeSonarr{
		series: []sonarr.Series{{ID: 1, Title: "Show"}},
		files: map[int][]sonarr.EpisodeFile{
			1: {listedFile(10, 1, "SubsPlease"), {ID: 50, Season: 0, ReleaseGroup: "MTBB", Revision: v2}},
		},
		episodes: map[int][]sonarr.Episode{
			1: {withFile(1, 1, 1, 10), specialEp(5, 50), specialEp(6, 50), specialEp(7, 0)},
		},
	}
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("Walk items = %d, want 1", len(snap.Items))
	}
	it := snap.Items[0]
	file := library.ListedEpisode{Group: "mtbb", Revision: release.ArrRevision(2, false), HasFile: true}
	want := map[int]library.ListedEpisode{5: file, 6: file, 7: {}}
	if !maps.Equal(it.Specials, want) {
		t.Errorf("Walk Specials = %+v, want %+v", it.Specials, want)
	}
	if len(it.SeasonGroups[0]) != 1 || it.SeasonGroups[0][0] != "mtbb" || it.Failed || snap.Partial {
		t.Errorf("Walk item = groups %v failed %v partial %v, want season 0 [mtbb] from the file list, not failed, not partial",
			it.SeasonGroups, it.Failed, snap.Partial)
	}
	wantEpisodes := map[int]map[int]library.Episode{1: {1: {File: 10, Absolute: 1}}, 0: {5: {File: 50}, 6: {File: 50}}}
	if !maps.EqualFunc(it.SeasonEpisodes, wantEpisodes, maps.Equal) {
		t.Errorf("Walk SeasonEpisodes = %v, want %v from the same episode list", it.SeasonEpisodes, wantEpisodes)
	}
	if got := fs.episodesCalls[1]; got != 1 {
		t.Errorf("Episodes calls for series 1 = %d, want 1: one list serves the specials and the sizes", got)
	}
}

func TestWalkSonarrKeepsSpecialsOnlyWhereItHoldsASeasonZeroFile(t *testing.T) {
	fs := &fakeSonarr{
		series: []sonarr.Series{{ID: 1, Title: "Seasons only"}, {ID: 2, Title: "Has a special"}},
		files: map[int][]sonarr.EpisodeFile{
			1: {listedFile(1, 1, "A"), listedFile(2, 2, "A")},
			2: {listedFile(3, 1, "B"), listedFile(4, 0, "C")},
		},
		episodes: map[int][]sonarr.Episode{
			1: {specialEp(1, 1)},
			2: {specialEp(1, 4)},
		},
	}
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, it := range snap.Items {
		switch it.ArrID {
		case 1:
			if it.Specials != nil {
				t.Errorf("series 1 Specials = %+v, want nil: it holds no season-0 file", it.Specials)
			}
		case 2:
			if !it.Specials[1].HasFile || it.Specials[1].Group != "c" {
				t.Errorf("series 2 Specials = %+v, want episode 1 on group c", it.Specials)
			}
		}
	}
}

func TestWalkSonarrEpisodeListFailureLeavesSpecialsUnknown(t *testing.T) {
	fs := &fakeSonarr{
		series:      []sonarr.Series{{ID: 1, Title: "Show"}},
		files:       map[int][]sonarr.EpisodeFile{1: {epFile(1, "A"), epFile(0, "B")}},
		episodesErr: map[int]error{1: errors.New("boom")},
	}
	logger, rec := capture.New()
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	it := snap.Items[0]
	if it.Specials != nil || it.SeasonEpisodes != nil || it.Failed || snap.Partial || len(it.SeasonGroups[1]) != 1 {
		t.Errorf("Walk after a failed episode list read = specials %+v episodes %v failed %v partial %v groups %v; want unknown specials and sizes on a comparable item",
			it.Specials, it.SeasonEpisodes, it.Failed, snap.Partial, it.SeasonGroups)
	}
	if !rec.HasAttr("sonarr episode list read failed; this series' upgrade sizes and episode listing are unknown and its films and specials stay uncompared", "id", "1") {
		t.Errorf("messages = %q, want the episode list failure WARN naming series 1 and its specials", rec.Messages())
	}
}

func TestSpecialEpisodesSkipsWhatIsNotASeasonZeroEpisode(t *testing.T) {
	got, ok := specialEpisodes([]sonarr.Episode{
		{Season: 1, Number: 3, HasFile: true},
		{Season: 0, Number: 0},
		{Season: 0, Number: 4},
	}, nil)
	if want := map[int]library.ListedEpisode{4: {}}; !ok || !maps.Equal(got, want) {
		t.Errorf("specialEpisodes(mixed) = %+v, %v, want %+v, true", got, ok, want)
	}
	if got, ok := specialEpisodes(nil, nil); got != nil || !ok {
		t.Errorf("specialEpisodes(nil) = %+v, %v, want nil, true", got, ok)
	}
}

func TestWalkSonarrSeasonZeroFileWithoutFileIDLeavesSpecialsUnknown(t *testing.T) {
	stripped := sonarr.Episode{Season: 0, Number: 9, HasFile: true}
	fs := &fakeSonarr{
		series:   []sonarr.Series{{ID: 1, Title: "Show"}},
		files:    map[int][]sonarr.EpisodeFile{1: {listedFile(1, 1, "A"), listedFile(2, 0, "B")}},
		episodes: map[int][]sonarr.Episode{1: {specialEp(4, 2), stripped}},
	}
	logger, rec := capture.New()
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if it := snap.Items[0]; it.Specials != nil || it.Failed || snap.Partial {
		t.Errorf("Walk with hasFile and no file id on S00E09 = specials %+v failed %v partial %v; want unknown specials on a comparable item",
			it.Specials, it.Failed, snap.Partial)
	}
	if !rec.HasAttr("sonarr specials episode list marks a file it does not send; the series' films and specials stay uncompared", "id", "1") {
		t.Errorf("messages = %q, want the file-unknown WARN naming series 1", rec.Messages())
	}
}

func TestWalkSonarrSeasonZeroFileWithNoListedEpisodeWarns(t *testing.T) {
	fs := &fakeSonarr{
		series:   []sonarr.Series{{ID: 1, Title: "Show"}},
		files:    map[int][]sonarr.EpisodeFile{1: {epFile(1, "A"), epFile(0, "B")}},
		episodes: map[int][]sonarr.Episode{1: {{Season: 0, Number: 0}}},
	}
	logger, rec := capture.New()
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if it := snap.Items[0]; !it.SpecialsUnknown() {
		t.Errorf("Walk with a season-0 file and no positive season-0 episode = specials %+v, want unknown", it.Specials)
	}
	if !rec.HasAttr("sonarr specials episode list names no season-0 episode beside a season-0 file; the series' films and specials stay uncompared", "id", "1") {
		t.Errorf("messages = %q, want the empty-list WARN naming series 1", rec.Messages())
	}
}

// Listed holds every positive-season episode Sonarr lists, with or without a
// file, and leaves out a season holding an episode whose file state is unknown.
func TestWalkSonarrListsEveryPositiveSeasonEpisode(t *testing.T) {
	v2 := &arrapi.Revision{Version: 2}
	fs := &fakeSonarr{
		series: []sonarr.Series{{ID: 1, Title: "Films"}},
		files:  map[int][]sonarr.EpisodeFile{1: {{ID: 10, Season: 1, ReleaseGroup: "MTBB", Revision: v2}}},
		episodes: map[int][]sonarr.Episode{1: {
			{Season: 1, Number: 1, HasFile: true, FileID: 10},
			{Season: 1, Number: 2},
			{Season: 2, Number: 1, HasFile: true},
			{Season: 2, Number: 2},
			specialEp(1, 0),
		}},
	}
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := map[int]map[int]library.ListedEpisode{1: {
		1: {Group: "mtbb", Revision: release.ArrRevision(2, false), HasFile: true},
		2: {},
	}}
	if got := snap.Items[0].Listed; !maps.EqualFunc(got, want, maps.Equal) {
		t.Errorf("Walk Listed = %+v, want %+v: season 1 in full, season 2 unknown, no season 0", got, want)
	}
	fs.episodesErr = map[int]error{1: errors.New("boom")}
	if snap, _, err = NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context()); err != nil || snap.Items[0].Listed != nil {
		t.Errorf("Walk with the episode list unreadable: Listed = %+v, %v; want nil", snap.Items[0].Listed, err)
	}
}

// A series with no file still has Sonarr's episode list, so an entry placed
// on part of a season is judged on its own episodes, there missing.
func TestWalkSonarrFilelessSeriesListsItsEpisodes(t *testing.T) {
	season := make([]sonarr.Episode, 7)
	for i := range season {
		season[i] = sonarr.Episode{Season: 1, Number: i + 1}
	}
	fs := &fakeSonarr{
		series:   []sonarr.Series{{ID: 1, Title: "Films", IDs: sonarr.ExternalIDs{TVDB: 42}}},
		files:    map[int][]sonarr.EpisodeFile{1: {}},
		episodes: map[int][]sonarr.Episode{1: season},
	}
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	item := snap.Items[0]
	if item.HasFile || len(item.Listed[1]) != 7 {
		t.Fatalf("Walk(fileless) HasFile %v, Listed %+v; want no file and season 1's seven episodes listed", item.HasFile, item.Listed)
	}
	rec := mapping.Record{TvdbID: 42, TmdbMovies: []int{1}, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1}
	d := align.Decide(&item, &align.Entry{Record: &rec, Episodes: []int{1}}, &align.Listing{Best: []string{"mtbb"}})
	if d.Kind != align.ScopeEpisodes || d.Season != 1 || !slices.Equal(d.Episodes, []int{1}) || !slices.Equal(d.MissingEpisodes, []int{1}) {
		t.Errorf("Decide(S01E01 of a fileless season) = %v season %d episodes %v missing %v; want episodes on season 1, E1 placed and missing", d.Kind, d.Season, d.Episodes, d.MissingEpisodes)
	}
}
