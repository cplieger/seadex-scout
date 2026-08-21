package payload

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestNamesDropsEmptyNamesPreservesOrder(t *testing.T) {
	files := []seadex.File{
		{Name: "episode 01.mkv"},
		{Name: ""},
		{Name: "episode 02.mkv"},
	}

	got := Names(files)
	want := []string{"episode 01.mkv", "episode 02.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

// TestNamesMaxInt64LengthKeepsOnlyPrimary pins the overflow
// boundary of the ceil-half threshold: a JSON-valid file length of
// math.MaxInt64 must not wrap the threshold negative and let a tiny
// marker-bearing extra survive beside the primary payload. The extra is a
// type-gate SURVIVOR (a video file with no creditless marker), so the size
// layer alone must exclude it.
func TestNamesMaxInt64LengthKeepsOnlyPrimary(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - 01 [1080p][HEVC].mkv", Length: math.MaxInt64},
		{Name: "Making Of [BDRemux].mkv", Length: 50_000_000},
	}

	got := Names(files)
	want := []string{"Show - 01 [1080p][HEVC].mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want only the primary name %v", got, want)
	}
}

// TestNamesUsesCeilingHalfThreshold pins the ceiling-half (not
// floor-half) primary-payload threshold at the odd-maximum boundary: with a
// maximum length of 3 the cutoff is 2, so a length-1 extra is excluded and a
// length-2 extra is included. A floor-half regression would keep the length-1
// extra and slip past the existing strictly-below property.
func TestNamesUsesCeilingHalfThreshold(t *testing.T) {
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
			if got := Names(files); !slices.Equal(got, tt.want) {
				t.Errorf("Names(%+v) = %v, want %v", files, got, tt.want)
			}
		})
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

// TestNamesLayeredRule pins the combined eligibility rule's layer
// interplay on the exact cases where the two historical rules (compare/
// audit's size-only torrentFileNames, the indexer's name-only
// isContentMediaFile filter) diverged: type gate
// first, size refinement among the survivors, with the no-lengths and
// no-content-survivor fallbacks keeping the rule total.
func TestNamesLayeredRule(t *testing.T) {
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
			if got := Names(tc.files); !slices.Equal(got, tc.want) {
				t.Errorf("Names(%+v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}

// TestIsSampleExtraMarkers pins the sample marker's token boundaries and its
// ASCII case classes. It is the census's sample guard now that the size floor
// deliberately admits a strongly skewed real episode, so a missed spelling
// ("sample" lowercase, an underscore-delimited marker, a "Sample/" directory)
// would let a sample clip count as an episode and inflate a lone episode into
// a "pack"; a false positive ("Samples" as a title word) would delete real
// evidence.
func TestIsSampleExtraMarkers(t *testing.T) {
	cases := map[string]bool{
		"Show S01E01 Sample.mkv":      true,
		"show s01e01 sample.mkv":      true,
		"Show S01E01 SaMpLe.mkv":      true,
		"Show_sample_01.mkv":          true,
		"Show S01E01 Sample01.mkv":    true,
		"Sample/Show S01E01.mkv":      true,
		"sample.mkv":                  true,
		"Show S01E01 [1080p].mkv":     false,
		"Show Samples Collection.mkv": false,
		"Resampled Show - 01.mkv":     false,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			if got := IsSampleExtra(name); got != want {
				t.Errorf("IsSampleExtra(%q) = %t, want %t", name, got, want)
			}
			if want && ContentMediaFile(name) {
				t.Errorf("ContentMediaFile(%q) = true, want false (a sample is not content evidence)", name)
			}
		})
	}
}

// TestPopulationMedianAnchoredFloor pins the census rule against the
// primary-payload rule. Population answers "how many distinct episodes
// does this torrent span", where a shorter file is a legitimately shorter
// episode; primaryFiles answers "which names vote on the release's quality",
// where anything far below the primary is a diluting extra. Anchoring the
// census floor on the MAXIMUM instead deletes every regular
// episode of any pack carrying one over-long file, so the pack reads as a single
// episode. Both rules must still exclude an episode-shaped sample.
//
// It is the assertion the extraction of this package bought (l-f195, h-f21):
// the two floors are compared HERE, where both are decided, instead of the
// difference living as prose in one package and a test in another.
func TestPopulationMedianAnchoredFloor(t *testing.T) {
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
			// lone episode into a "pack". It is dropped by NAME here
			// (IsSampleExtra), not by the census floor - at these two lengths
			// the floor no longer excludes it, which is the whole point: the
			// ratio between a sample and its payload overlaps the ratio
			// between two real episodes of unequal length.
			files:          withFirst(seadex.File{Name: "Show S01E00 Sample [480p].mkv", Length: 200 << 20}, episodes(1, gib)),
			wantPopulation: 1,
			wantPayload:    1,
		},
		"marked sample is excluded at payload size": {
			// The name gate is size-independent: a sample as large as the
			// episode - or larger - still cannot vote or be counted.
			files:          withFirst(seadex.File{Name: "Show S01E00 sample.mkv", Length: 4 * gib}, episodes(1, gib)),
			wantPopulation: 1,
			wantPayload:    1,
		},
		"sample-only list still yields evidence": {
			// The type-gate fallback keeps the rule total: a record whose
			// every name is a sample falls back to all named files rather
			// than losing its evidence entirely.
			files:          []seadex.File{{Name: "Show sample.mkv", Length: gib}},
			wantPopulation: 1,
			wantPayload:    1,
		},
		"no positive lengths falls back to the type gate": {
			files:          []seadex.File{{Name: "Show - 07.mkv"}, {Name: "Show - 08.mkv"}},
			wantPopulation: 2,
			wantPayload:    2,
		},
		"no type survivor falls back to every named file": {
			// The fallback pool is BOTH names, and on a two-file pool the
			// lower-middle anchor cannot exclude either (the smaller file IS
			// the anchor). Harmless by contract: Population returns a file
			// SUBSET, not a census verdict, so a consumer that counts episodes
			// re-applies the type gate and drops the .nfo - while the
			// primary-payload rule still resolves the container alone.
			files:          []seadex.File{{Name: "Show S01 remux.iso", Length: 20 * gib}, {Name: "Show S01 remux.nfo", Length: 1 << 10}},
			wantPopulation: 2,
			wantPayload:    1,
		},
		"MaxInt64 length does not wrap the floor negative": {
			// The overflow invariant is that the floor stays positive and
			// anchored on a real pool value: the lower middle is 1, so the
			// census keeps both files rather than wrapping negative and
			// keeping everything by accident. Keeping both is unobservable
			// here - the two files carry the SAME episode token, so the
			// indexer's census still reads one episode - and the
			// primary-payload rule still keeps only the MaxInt64 file.
			files:          withFirst(seadex.File{Name: "Show S01E01 [1080p].mkv", Length: math.MaxInt64}, episodes(1, 1)),
			wantPopulation: 2,
			wantPayload:    1,
		},
		"two-file pack keeps the shorter episode": {
			// The characterization case for the median anchor: on a two-file
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
		"two-file pack keeps a strongly skewed episode": {
			// The midpoint anchor only protected the smaller file while the
			// larger stayed within ~3x of it, so a two-part OVA (or a season
			// pair bundling the franchise movie) whose finale runs 4x its
			// sibling still lost the sibling, read as one episode, and was
			// served under that episode's own SxxExx marker (l-f234 /
			// d-gpt-u3c1-1). The lower-middle anchor keeps both.
			files: []seadex.File{
				{Name: "Show S01E01 [1080p].mkv", Length: gib},
				{Name: "Show S01E02 [1080p].mkv", Length: 4 * gib},
			},
			wantPopulation: 2,
			wantPayload:    1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := len(Population(tt.files)); got != tt.wantPopulation {
				t.Errorf("len(Population()) = %d, want %d", got, tt.wantPopulation)
			}
			if got := len(primaryFiles(tt.files)); got != tt.wantPayload {
				t.Errorf("len(primaryFiles()) = %d, want %d", got, tt.wantPayload)
			}
		})
	}
}
