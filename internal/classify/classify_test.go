package classify

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestTorrentBuildsSharedReleaseInput(t *testing.T) {
	entry := &seadex.Entry{Notes: "BD remux noted by SeaDex"}
	torrent := &seadex.Torrent{
		ReleaseGroup: "SubsPlease",
		Tracker:      "Nyaa",
		DualAudio:    true,
		Files: []seadex.File{
			{Name: "[SubsPlease] Frieren - 01 [1080p][HEVC].mkv"},
			{Name: ""},
		},
	}

	got := Torrent(entry, torrent)

	if got.Group != "SubsPlease" {
		t.Errorf("Torrent() group = %q, want SubsPlease", got.Group)
	}
	if got.Tracker != "Nyaa" || got.TrackerType != release.TrackerPublic {
		t.Errorf("Torrent() tracker = %q/%q, want Nyaa/public", got.Tracker, got.TrackerType)
	}
	if got.Resolution != "1080p" {
		t.Errorf("Torrent() resolution = %q, want 1080p", got.Resolution)
	}
	if got.Codec != "x265" {
		t.Errorf("Torrent() codec = %q, want x265", got.Codec)
	}
	if got.Kind != release.KindRemux {
		t.Errorf("Torrent() kind = %q, want remux from entry notes", got.Kind)
	}
	if !got.DualAudio {
		t.Error("Torrent() must preserve the SeaDex dual-audio flag")
	}
}

func TestTorrentFileNamesDropsEmptyNamesPreservesOrder(t *testing.T) {
	files := []seadex.File{
		{Name: "episode 01.mkv"},
		{Name: ""},
		{Name: "episode 02.mkv"},
	}

	got := TorrentFileNames(files)
	want := []string{"episode 01.mkv", "episode 02.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("TorrentFileNames() = %v, want %v", got, want)
	}
}
