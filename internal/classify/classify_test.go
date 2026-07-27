package classify

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
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

func TestPayloadNamesDropsEmptyNamesPreservesOrder(t *testing.T) {
	files := []seadex.File{
		{Name: "episode 01.mkv"},
		{Name: ""},
		{Name: "episode 02.mkv"},
	}

	got := PayloadNames(files)
	want := []string{"episode 01.mkv", "episode 02.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("PayloadNames() = %v, want %v", got, want)
	}
}

// TestPayloadNamesMaxInt64LengthKeepsOnlyPrimary pins the overflow
// boundary of the ceil-half threshold: a JSON-valid file length of
// math.MaxInt64 must not wrap the threshold negative and let a tiny
// marker-bearing extra survive beside the primary payload. The extra is a
// type-gate SURVIVOR (a video file with no creditless marker), so the size
// layer alone must exclude it.
func TestPayloadNamesMaxInt64LengthKeepsOnlyPrimary(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - 01 [1080p][HEVC].mkv", Length: math.MaxInt64},
		{Name: "Making Of [BDRemux].mkv", Length: 50_000_000},
	}

	got := PayloadNames(files)
	want := []string{"Show - 01 [1080p][HEVC].mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("PayloadNames() = %v, want only the primary name %v", got, want)
	}
}

// TestPayloadNamesUsesCeilingHalfThreshold pins the ceiling-half (not
// floor-half) primary-payload threshold at the odd-maximum boundary: with a
// maximum length of 3 the cutoff is 2, so a length-1 extra is excluded and a
// length-2 extra is included. A floor-half regression would keep the length-1
// extra and slip past the existing strictly-below property.
func TestPayloadNamesUsesCeilingHalfThreshold(t *testing.T) {
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
			if got := PayloadNames(files); !slices.Equal(got, tt.want) {
				t.Errorf("PayloadNames(%+v) = %v, want %v", files, got, tt.want)
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

// TestIsCreditlessExtraCaseFolds pins the marker's strings.ToLower-faithful
// case classes on the two Unicode folds where Go regexp's (?i) SimpleFold
// diverges: a Turkish-uppercase CREDİTLESS (U+0130 folds to I/i) is a
// creditless extra, while a long-s CREDITLEſS (U+017F, which (?i) would have
// folded onto S) is not.
func TestIsCreditlessExtraCaseFolds(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Show NCOP01.mkv", true},
		{"show ncop01.mkv", true},
		{"Show NCED01v2.mkv", true},
		{"show nced01v2.mkv", true},
		{"Show NcEd01v2.mkv", true},
		{"Show creditless ED.mkv", true},
		{"Show CREDITLESS01v2.mkv", true},
		{"Show CRED\u0130TLESS01v2.mkv", true},  // Turkish-uppercase İ folds to i under strings.ToLower
		{"Show CREDITLE\u017FS01v2.mkv", false}, // long s must not fold onto S
		{"Show - 01 [1080p].mkv", false},
	}
	for _, tc := range cases {
		if got := IsCreditlessExtra(tc.name); got != tc.want {
			t.Errorf("IsCreditlessExtra(%q) = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestTorrentLargeUnicodeCreditlessExtraStaysEncode pins the classification
// consequence of the İ fold: a CREDİTLESS extra large enough to pass the
// size refinement must still be excluded by the type gate, so its BDRemux
// marker cannot flip an x265 episode payload to remux (and invert an
// operator's exclude_remux filter).
func TestTorrentLargeUnicodeCreditlessExtraStaysEncode(t *testing.T) {
	files := make([]seadex.File, 0, 13)
	for i := 1; i <= 12; i++ {
		files = append(files, seadex.File{
			Name:   fmt.Sprintf("Show - %02d [1080p][x265].mkv", i),
			Length: 1_400_000_000 + int64(i)*1_000_000,
		})
	}
	files = append(files, seadex.File{Name: "Show - CRED\u0130TLESS01v2 [BDRemux].mkv", Length: 1_400_000_000})
	torrent := &seadex.Torrent{ReleaseGroup: "cappybara", Tracker: "Nyaa", Files: files}

	got := Torrent(&seadex.Entry{}, torrent)

	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (a payload-sized CREDİTLESS extra's BDRemux marker must not override the episode payload)", got.Kind)
	}
}

// TestTorrentUnderscoreDelimitedCreditlessExtraDoesNotVote pins the boundary
// semantics of the creditless type gate: underscore is a scene delimiter for
// the rest of the classification stack, so an underscore-delimited NCED extra
// must be excluded like any other creditless file — its remux marker cannot
// outrank the episode payload's encode marker.
func TestTorrentUnderscoreDelimitedCreditlessExtraDoesNotVote(t *testing.T) {
	torrent := &seadex.Torrent{Files: []seadex.File{
		{Name: "Show_01_[1080p][x265].mkv", Length: 1000},
		{Name: "Show_NCED_01_[BDRemux].mkv", Length: 900},
	}}
	got := Torrent(&seadex.Entry{}, torrent)
	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (an underscore-delimited NCED extra must not vote)", got.Kind)
	}
}

// TestPayloadNamesLayeredRule pins the combined eligibility rule's layer
// interplay on the exact cases where the two historical rules (compare/
// audit's size-only torrentFileNames, the indexer's name-only
// isContentMediaFile filter) diverged — the h-f3 standardization: type gate
// first, size refinement among the survivors, with the no-lengths and
// no-content-survivor fallbacks keeping the rule total.
func TestPayloadNamesLayeredRule(t *testing.T) {
	cases := []struct {
		name  string
		files []seadex.File
		want  []string
	}{
		{
			// The size-only rule kept a creditless extra >= half the
			// largest file; the type gate excludes it whatever its size.
			name: "large creditless extra excluded by type gate",
			files: []seadex.File{
				{Name: "Movie [1080p].mkv", Length: 1000},
				{Name: "Movie NCED01 [BDRemux].mkv", Length: 900},
			},
			want: []string{"Movie [1080p].mkv"},
		},
		{
			// The name-only rule saw no video extension and returned no
			// evidence; the fallback applies the size rule over every named
			// file, so an unlisted container keeps classifying.
			name: "unlisted container falls back to size rule",
			files: []seadex.File{
				{Name: "Movie [1080p] Remux.iso", Length: 1000},
				{Name: "Sample.iso", Length: 10},
			},
			want: []string{"Movie [1080p] Remux.iso"},
		},
		{
			// The size-only rule kept every name on a lengths-less record
			// (sidecars included); the type gate filters them.
			name: "sidecars dropped on a lengths-less record",
			files: []seadex.File{
				{Name: "Show - 01 [1080p].mkv"},
				{Name: "Show - 01.ass"},
				{Name: "screens.png"},
			},
			want: []string{"Show - 01 [1080p].mkv"},
		},
		{
			// Deliberate: in a mixed-resolution batch the small specials do
			// not vote — the release is headlined by its primary payload.
			name: "mixed-resolution batch keeps the primary payload's verdict",
			files: []seadex.File{
				{Name: "Show - 01 [1080p].mkv", Length: 1_400_000},
				{Name: "Special - 01 [480p].mkv", Length: 200_000},
			},
			want: []string{"Show - 01 [1080p].mkv"},
		},
		{
			// A creditless-only torrent (an NC collection) still classifies
			// from its own names: zero type survivors falls back to every
			// named file rather than returning no evidence.
			name: "creditless-only list falls back to all names",
			files: []seadex.File{
				{Name: "NCOP01 [1080p].mkv", Length: 100},
				{Name: "NCED01 [1080p].mkv", Length: 100},
			},
			want: []string{"NCOP01 [1080p].mkv", "NCED01 [1080p].mkv"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PayloadNames(tc.files); !slices.Equal(got, tc.want) {
				t.Errorf("PayloadNames(%+v) = %v, want %v", tc.files, got, tc.want)
			}
		})
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

// TestObtainableAdapterPreservesRawURLForCrossCheck pins the adapter's wiring
// invariant that filter.Obtainable's own tests cannot cover: the RAW upstream
// URL must feed the AnimeBytes host cross-check (so a mislabeled schemeless AB
// URL is caught) while PublishURL supplies the actionable link. A
// mutant passing the canonical URL to both arguments returns true here.
func TestObtainableAdapterPreservesRawURLForCrossCheck(t *testing.T) {
	torrent := &seadex.Torrent{
		Tracker: "Nyaa",
		URL:     "animebytes.tv/torrents.php?id=1&torrentid=2",
	}
	rel := &release.Release{Tracker: "Nyaa", TrackerType: release.TrackerPublic}

	if got := Obtainable(rel, torrent, false); got {
		t.Error("Obtainable() = true, want false for a mislabeled schemeless AnimeBytes URL when AnimeBytes is disabled")
	}
}

// TestABEvidenceAdapterReadsRawEvidence pins the third adapter's policy surface
// at its defining site, mirroring the ABVisible and Obtainable adapter tests:
// an AB tracker label or definitively extracted raw-URL host evidence (absolute
// or schemeless animebytes.tv) grades ABDefinite, a hidden-host host:port form
// grades ABAmbiguous (evidence that settles nothing), and an honest public URL
// or an empty URL grades ABNone. The adapter must feed the RAW upstream URL
// (t.URL) to the host cross-check: a mutant passing PublishURL(t) (which drops
// the schemeless AB form under a public label to "") would grade that case
// ABNone and fail this test.
func TestABEvidenceAdapterReadsRawEvidence(t *testing.T) {
	tests := []struct {
		name    string
		torrent seadex.Torrent
		want    filter.ABEvidence
	}{
		{"AB label is definitive", seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}, filter.ABDefinite},
		{"absolute AB URL under a public label is definitive", seadex.Torrent{Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=1"}, filter.ABDefinite},
		{"schemeless AB URL under a public label is definitive", seadex.Torrent{Tracker: "Nyaa", URL: "animebytes.tv/torrents.php?id=1"}, filter.ABDefinite},
		{"hidden-host form settles nothing", seadex.Torrent{Tracker: "Nyaa", URL: "animebytes.tv:443/torrents.php?id=1"}, filter.ABAmbiguous},
		{"public tracker with public URL is not AB", seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}, filter.ABNone},
		{"empty URL carries no host evidence", seadex.Torrent{Tracker: "Nyaa", URL: ""}, filter.ABNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ABEvidence(&tt.torrent); got != tt.want {
				t.Errorf("ABEvidence(%q, %q) = %v, want %v", tt.torrent.Tracker, tt.torrent.URL, got, tt.want)
			}
		})
	}
}

// TestDivergedIncomplete pins the shared diverged-downgrade rule at its
// defining site, mirroring TestFallbackPrecedence: an incomplete entry
// downgrades a diverged comparison to the incomplete vocabulary, a complete
// entry does not. Cross-package callers (compare, audit.rowQualifier) pin the
// mapped vocabularies, but the one shared rule both map from had no test in
// its own package.
func TestDivergedIncomplete(t *testing.T) {
	if !DivergedIncomplete(&seadex.Entry{Incomplete: true}) {
		t.Error("DivergedIncomplete(incomplete entry) = false, want true")
	}
	if DivergedIncomplete(&seadex.Entry{}) {
		t.Error("DivergedIncomplete(complete entry) = true, want false")
	}
}

// TestPopulationFilesMedianAnchoredFloor pins the census rule against the
// primary-payload rule. PopulationFiles answers "how many distinct episodes
// does this torrent span", where a shorter file is a legitimately shorter
// episode; PayloadFiles answers "which names vote on the release's quality",
// where anything far below the primary is a diluting extra. Anchoring the
// census floor on the MAXIMUM (the d-u5-c4-1 regression) deleted every regular
// episode of any pack carrying one over-long file, so the pack read as a single
// episode. Both rules must still exclude an episode-shaped sample.
func TestPopulationFilesMedianAnchoredFloor(t *testing.T) {
	const gib = 1 << 30
	episodes := func(n int, size int64) []seadex.File {
		out := make([]seadex.File, 0, n)
		for e := 1; e <= n; e++ {
			out = append(out, seadex.File{Name: fmt.Sprintf("Show S01E%02d [1080p].mkv", e), Length: size})
		}
		return out
	}
	withFirst := func(first seadex.File, rest []seadex.File) []seadex.File {
		return append([]seadex.File{first}, rest...)
	}

	tests := map[string]struct {
		files          []seadex.File
		wantPopulation int
		wantPayload    int
	}{
		"double-length premiere keeps every episode": {
			// The regression case: max 2.5 GiB puts the payload floor at
			// 1.25 GiB and drops all 11 regular episodes; the median stays on
			// the episode population.
			files:          withFirst(seadex.File{Name: "Show S01E01 [1080p].mkv", Length: 5 * gib / 2}, episodes(11, 6*gib/5)),
			wantPopulation: 12,
			wantPayload:    1,
		},
		"bundled franchise movie keeps every episode": {
			files:          withFirst(seadex.File{Name: "Show Movie [1080p].mkv", Length: 4 * gib}, episodes(12, 6*gib/5)),
			wantPopulation: 13,
			wantPayload:    1,
		},
		"episode-shaped sample is still excluded": {
			// Both rules must drop it: a sample that counted would inflate a
			// lone episode into a "pack" (h-f10).
			files:          withFirst(seadex.File{Name: "Show S01E00 Sample [480p].mkv", Length: 200 << 20}, episodes(1, gib)),
			wantPopulation: 1,
			wantPayload:    1,
		},
		"no positive lengths falls back to the type gate": {
			files:          []seadex.File{{Name: "Show - 07.mkv"}, {Name: "Show - 08.mkv"}},
			wantPopulation: 2,
			wantPayload:    2,
		},
		"no type survivor falls back to every named file": {
			files:          []seadex.File{{Name: "Show S01 remux.iso", Length: 20 * gib}, {Name: "Show S01 remux.nfo", Length: 1 << 10}},
			wantPopulation: 1,
			wantPayload:    1,
		},
		"MaxInt64 length does not wrap the floor negative": {
			files:          withFirst(seadex.File{Name: "Show S01E01 [1080p].mkv", Length: math.MaxInt64}, episodes(1, 1)),
			wantPopulation: 1,
			wantPayload:    1,
		},
		"two-file pack keeps the shorter episode": {
			// The characterization case for the midpoint anchor: on a two-file
			// pool the upper-middle statistic IS the maximum, so the old
			// implementation floored at 1.25 GiB and counted this pack as ONE
			// episode - the pack then served as "Show S01E01" and Sonarr grabbed
			// it as a single episode. The property's even-pool bounds
			// deliberately admit either tie-break, so only this case fails under
			// the upper middle.
			files: []seadex.File{
				{Name: "Show S01E01 [1080p].mkv", Length: gib},
				{Name: "Show S01E02 [1080p].mkv", Length: 5 * gib / 2},
			},
			wantPopulation: 2,
			wantPayload:    1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := len(PopulationFiles(tt.files)); got != tt.wantPopulation {
				t.Errorf("len(PopulationFiles()) = %d, want %d", got, tt.wantPopulation)
			}
			if got := len(PayloadFiles(tt.files)); got != tt.wantPayload {
				t.Errorf("len(PayloadFiles()) = %d, want %d", got, tt.wantPayload)
			}
		})
	}
}
