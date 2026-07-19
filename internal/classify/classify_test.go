package classify

import (
	"fmt"
	"math"
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
	// Notes scoping: the SeaDex entry notes say "remux", but the per-file name
	// carries an HEVC encode marker, and per-file evidence wins for the file
	// (entry-wide notes only fill gaps).
	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (per-file HEVC marker beats the entry-notes remux)", got.Kind)
	}
	if !got.DualAudio {
		t.Error("Torrent() must preserve the SeaDex dual-audio flag")
	}
}

// TestTorrentNotesFillGapWhenFilesCarryNoMarker pins the gap-filling half of
// the notes-scoping contract: when the torrent's file names carry no remux or
// encode marker, the entry-wide SeaDex notes classify the release.
func TestTorrentNotesFillGapWhenFilesCarryNoMarker(t *testing.T) {
	entry := &seadex.Entry{Notes: "BD remux noted by SeaDex"}
	torrent := &seadex.Torrent{
		ReleaseGroup: "PMR",
		Tracker:      "Nyaa",
		Files:        []seadex.File{{Name: "Frieren - 01 (1080p).mkv"}},
	}

	got := Torrent(entry, torrent)

	if got.Kind != release.KindRemux {
		t.Errorf("Torrent() kind = %q, want remux from entry notes when the file names carry no marker", got.Kind)
	}
}

// TestTorrentDualAudioStructuredFieldOnly pins the dual-audio sourcing at the
// adapter: the structured per-torrent SeaDex field is the only evidence — a
// flagged torrent classifies dual-audio whatever the text says, and an
// unflagged torrent never picks it up from the entry-wide notes or a file
// name, because notes describe every release in the entry and can even negate
// the marker ("lacks dual audio").
func TestTorrentDualAudioStructuredFieldOnly(t *testing.T) {
	tests := []struct {
		name    string
		notes   string
		file    string
		flagged bool
		want    bool
	}{
		{name: "flagged torrent with no text marker", notes: "", file: "Show - 01 [1080p].mkv", flagged: true, want: true},
		{name: "flagged torrent with negating notes", notes: "lacks dual audio", file: "Show - 01 [1080p].mkv", flagged: true, want: true},
		{name: "unflagged torrent with dual audio notes", notes: "this release is dual audio", file: "Show - 01 [1080p].mkv", flagged: false, want: false},
		{name: "unflagged torrent with negating notes", notes: "lacks dual audio", file: "Show - 01 [1080p].mkv", flagged: false, want: false},
		{name: "unflagged torrent with dual audio file name", notes: "", file: "Show - 01 [1080p][Dual Audio].mkv", flagged: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &seadex.Entry{Notes: tt.notes}
			torrent := &seadex.Torrent{
				ReleaseGroup: "PMR",
				Tracker:      "Nyaa",
				DualAudio:    tt.flagged,
				Files:        []seadex.File{{Name: tt.file}},
			}
			if got := Torrent(entry, torrent).DualAudio; got != tt.want {
				t.Errorf("Torrent() DualAudio = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTorrentFileNamesDropsEmptyNamesPreservesOrder(t *testing.T) {
	files := []seadex.File{
		{Name: "episode 01.mkv"},
		{Name: ""},
		{Name: "episode 02.mkv"},
	}

	got := torrentFileNames(files)
	want := []string{"episode 01.mkv", "episode 02.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("torrentFileNames() = %v, want %v", got, want)
	}
}

// TestTorrentFileNamesMaxInt64LengthKeepsOnlyPrimary pins the overflow
// boundary of the ceil-half threshold: a JSON-valid file length of
// math.MaxInt64 must not wrap the threshold negative and let a tiny
// marker-bearing extra survive beside the primary payload.
func TestTorrentFileNamesMaxInt64LengthKeepsOnlyPrimary(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - 01 [1080p][HEVC].mkv", Length: math.MaxInt64},
		{Name: "NCED [BDRemux].mkv", Length: 50_000_000},
	}

	got := torrentFileNames(files)
	want := []string{"Show - 01 [1080p][HEVC].mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("torrentFileNames() = %v, want only the primary name %v", got, want)
	}
}

// TestTorrentFileNamesUsesCeilingHalfThreshold pins the ceiling-half (not
// floor-half) primary-payload threshold at the odd-maximum boundary: with a
// maximum length of 3 the cutoff is 2, so a length-1 extra is excluded and a
// length-2 extra is included. A floor-half regression would keep the length-1
// extra and slip past the existing strictly-below property.
func TestTorrentFileNamesUsesCeilingHalfThreshold(t *testing.T) {
	tests := []struct {
		name      string
		extraSize int64
		want      []string
	}{
		{name: "below ceiling half is excluded", extraSize: 1, want: []string{"primary.mkv"}},
		{name: "at ceiling half is included", extraSize: 2, want: []string{"primary.mkv", "extra.mkv"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := []seadex.File{
				{Name: "primary.mkv", Length: 3},
				{Name: "extra.mkv", Length: tt.extraSize},
			}
			if got := torrentFileNames(files); !slices.Equal(got, tt.want) {
				t.Errorf("torrentFileNames(%+v) = %v, want %v", files, got, tt.want)
			}
		})
	}
}

// TestTorrentPrimaryPayloadIgnoresSmallExtraMarker pins the primary-payload
// selection: a best BD encode whose payload is twelve similarly-sized HEVC
// episodes plus one small BDRemux-named NCED extra must classify from the
// episodes (KindEncode), not let the tiny extra's remux marker override the
// whole recommendation (which would wrongly drop it under exclude_remux).
func TestTorrentPrimaryPayloadIgnoresSmallExtraMarker(t *testing.T) {
	files := make([]seadex.File, 0, 13)
	for i := 1; i <= 12; i++ {
		files = append(files, seadex.File{
			Name:   fmt.Sprintf("Show - %02d [1080p][HEVC].mkv", i),
			Length: 1_400_000_000 + int64(i)*1_000_000,
		})
	}
	files = append(files, seadex.File{Name: "Show - NCED01 [BDRemux].mkv", Length: 90_000_000})
	torrent := &seadex.Torrent{ReleaseGroup: "cappybara", Tracker: "Nyaa", Files: files}

	got := Torrent(&seadex.Entry{}, torrent)

	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (a small NCED extra's BDRemux marker must not override the episode payload)", got.Kind)
	}
	if got.Resolution != "1080p" {
		t.Errorf("Torrent() resolution = %q, want 1080p from the primary payload", got.Resolution)
	}
}

// TestFallbackPrecedence pins the shared empty-recommendation precedence at
// its defining site: theoretical beats incomplete - the one order compare's
// emptyResult and audit's rowQualifier both map their vocabulary from.
func TestFallbackPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		entry seadex.Entry
		want  EntryFallback
	}{
		{"theoretical only", seadex.Entry{TheoreticalBest: "remux"}, FallbackTheoretical},
		{"theoretical wins over incomplete", seadex.Entry{TheoreticalBest: "remux", Incomplete: true}, FallbackTheoretical},
		{"incomplete only", seadex.Entry{Incomplete: true}, FallbackIncomplete},
		{"neither flag", seadex.Entry{}, FallbackNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fallback(&tt.entry); got != tt.want {
				t.Errorf("Fallback(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

// TestABVisibleAdapterGatesOnRawEvidence pins the adapter's policy surface:
// the operator toggle admits everything; with the toggle off an AB label or
// an AB host in the RAW upstream URL hides the torrent, an absolute public
// URL stays visible, an empty URL carries no host evidence to cross-check
// (visible), and a hidden-host form hides conservatively.
func TestABVisibleAdapterGatesOnRawEvidence(t *testing.T) {
	tests := []struct {
		name    string
		torrent seadex.Torrent
		include bool
		want    bool
	}{
		{"AB label hidden when off", seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}, false, false},
		{"AB label visible when on", seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}, true, true},
		{"mislabeled AB URL hidden when off", seadex.Torrent{Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=1"}, false, false},
		{"public tracker with absolute URL visible when off", seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}, false, true},
		{"empty URL carries no host evidence and stays visible", seadex.Torrent{Tracker: "Nyaa", URL: ""}, false, true},
		{"hidden-host URL form hidden conservatively when off", seadex.Torrent{Tracker: "Nyaa", URL: "animebytes.tv:443/torrents.php?id=1"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ABVisible(&tt.torrent, tt.include); got != tt.want {
				t.Errorf("ABVisible(%q, %q, %v) = %v, want %v", tt.torrent.Tracker, tt.torrent.URL, tt.include, got, tt.want)
			}
		})
	}
}
