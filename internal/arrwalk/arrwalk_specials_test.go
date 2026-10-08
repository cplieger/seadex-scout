package arrwalk

import (
	"errors"
	"maps"
	"testing"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/slogx/capture"
)

func specialEp(number int, f *arrapi.EpisodeFile) arrapi.Episode {
	return arrapi.Episode{SeasonNumber: 0, EpisodeNumber: number, HasFile: f != nil, EpisodeFile: f}
}

func TestWalkSonarrReadsSeasonZeroEpisodeFiles(t *testing.T) {
	v2 := &arrapi.QualityModel{Revision: &arrapi.Revision{Version: 2}}
	span := &arrapi.EpisodeFile{ID: 50, SeasonNumber: 0, ReleaseGroup: "MTBB", Quality: v2}
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Show"}},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "SubsPlease"), {ID: 50, SeasonNumber: 0, ReleaseGroup: "MTBB", Quality: v2}},
		},
		episodes: map[int][]arrapi.Episode{
			1: {withFile(1, 1, 1, 10), specialEp(5, span), specialEp(6, span), specialEp(7, nil)},
		},
	}
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("Walk items = %d, want 1", len(snap.Items))
	}
	it := snap.Items[0]
	file := library.SpecialEpisode{Group: "mtbb", Revision: release.ArrRevision(2, false), HasFile: true}
	want := map[int]library.SpecialEpisode{5: file, 6: file, 7: {}}
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
		series: []arrapi.Series{{ID: 1, Title: "Seasons only"}, {ID: 2, Title: "Has a special"}},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "A"), epFile(2, "A")},
			2: {epFile(1, "B"), epFile(0, "C")},
		},
		episodes: map[int][]arrapi.Episode{
			1: {specialEp(1, &arrapi.EpisodeFile{SeasonNumber: 0, ReleaseGroup: "X"})},
			2: {specialEp(1, &arrapi.EpisodeFile{SeasonNumber: 0, ReleaseGroup: "C"})},
		},
	}
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()}).Walk(t.Context())
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
		series:      []arrapi.Series{{ID: 1, Title: "Show"}},
		files:       map[int][]arrapi.EpisodeFile{1: {epFile(1, "A"), epFile(0, "B")}},
		episodesErr: map[int]error{1: errors.New("boom")},
	}
	logger, rec := capture.New()
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	it := snap.Items[0]
	if it.Specials != nil || it.SeasonEpisodes != nil || it.Failed || snap.Partial || len(it.SeasonGroups[1]) != 1 {
		t.Errorf("Walk after a failed episode list read = specials %+v episodes %v failed %v partial %v groups %v; want unknown specials and sizes on a comparable item",
			it.Specials, it.SeasonEpisodes, it.Failed, snap.Partial, it.SeasonGroups)
	}
	if !rec.HasAttr("sonarr episode list read failed; this series' upgrade sizes are unknown and its films and specials stay uncompared", "id", "1") {
		t.Errorf("messages = %q, want the episode list failure WARN naming series 1 and its specials", rec.Messages())
	}
}

func TestSpecialEpisodesSkipsWhatIsNotASeasonZeroEpisode(t *testing.T) {
	got, ok := specialEpisodes([]arrapi.Episode{
		{SeasonNumber: 1, EpisodeNumber: 3, HasFile: true},
		{SeasonNumber: 0, EpisodeNumber: 0},
		{SeasonNumber: 0, EpisodeNumber: 4},
	})
	if want := map[int]library.SpecialEpisode{4: {}}; !ok || !maps.Equal(got, want) {
		t.Errorf("specialEpisodes(mixed) = %+v, %v, want %+v, true", got, ok, want)
	}
	if got, ok := specialEpisodes(nil); got != nil || !ok {
		t.Errorf("specialEpisodes(nil) = %+v, %v, want nil, true", got, ok)
	}
}

func TestWalkSonarrSeasonZeroFileWithoutPayloadLeavesSpecialsUnknown(t *testing.T) {
	stripped := arrapi.Episode{SeasonNumber: 0, EpisodeNumber: 9, HasFile: true}
	fs := &fakeSonarr{
		series:   []arrapi.Series{{ID: 1, Title: "Show"}},
		files:    map[int][]arrapi.EpisodeFile{1: {epFile(1, "A"), epFile(0, "B")}},
		episodes: map[int][]arrapi.Episode{1: {specialEp(4, &arrapi.EpisodeFile{SeasonNumber: 0, ReleaseGroup: "B"}), stripped}},
	}
	logger, rec := capture.New()
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if it := snap.Items[0]; it.Specials != nil || it.Failed || snap.Partial {
		t.Errorf("Walk with hasFile and no episodeFile on S00E09 = specials %+v failed %v partial %v; want unknown specials on a comparable item",
			it.Specials, it.Failed, snap.Partial)
	}
	if !rec.HasAttr("sonarr specials episode list marks a file it does not send; the series' films and specials stay uncompared", "id", "1") {
		t.Errorf("messages = %q, want the payload-missing WARN naming series 1", rec.Messages())
	}
}

func TestWalkSonarrSeasonZeroFileWithNoListedEpisodeWarns(t *testing.T) {
	fs := &fakeSonarr{
		series:   []arrapi.Series{{ID: 1, Title: "Show"}},
		files:    map[int][]arrapi.EpisodeFile{1: {epFile(1, "A"), epFile(0, "B")}},
		episodes: map[int][]arrapi.Episode{1: {{SeasonNumber: 0, EpisodeNumber: 0}}},
	}
	logger, rec := capture.New()
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: logger}).Walk(t.Context())
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
