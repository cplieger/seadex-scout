package payload

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestTotalSize pins the untrusted-arithmetic domain of the pack-size sum: the
// lengths come from the SeaDex record with no length constraint, so a negative
// file length and an int64 overflow across two large lengths both return 0
// (the feed's existing size-unknown representation) instead of rendering a
// negative enclosure length to the arrs; normal sums are unaffected.
func TestTotalSize(t *testing.T) {
	tests := []struct {
		name  string
		files []seadex.File
		want  int64
	}{
		{"sums normal lengths", []seadex.File{{Length: 100}, {Length: 250}}, 350},
		{"no files is zero", nil, 0},
		{"negative length rejected", []seadex.File{{Length: 100}, {Length: -1}}, 0},
		{"zero-length file does not zero the sum", []seadex.File{{Length: 0}, {Length: 250}}, 250},
		{"overflow across two files rejected", []seadex.File{{Length: math.MaxInt64}, {Length: math.MaxInt64}}, 0},
		{"exact MaxInt64 sum allowed", []seadex.File{{Length: math.MaxInt64 - 1}, {Length: 1}}, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TotalSize(tc.files); got != tc.want {
				t.Errorf("TotalSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLastSubmatchIndexFindsAMarkerAdjacentToThePrevious commits an absolute
// marker that begins exactly where the previous match ended, which happens
// whenever two delimiters sit between two markers. A scan resuming one byte past
// the previous match still finds every well-separated marker, so only this shape
// tells it apart. The randomized sibling reaches it only when its draw places two
// markers adjacently, so the input is pinned here.
func TestLastSubmatchIndexFindsAMarkerAdjacentToThePrevious(t *testing.T) {
	tests := map[string]struct {
		name string
		want []int
	}{
		// "Show - 07  - 1085 ": the first marker ends at 10 having consumed one
		// space, and the second starts there on the other one.
		"double-space separated markers": {"Show - 07  - 1085 ", []int{10, 18, 13, 17}},
		// The underscore-named form of the same shape ("_Show_-_02__-_03_").
		"double-underscore separated markers": {"Show_-_02__-_03_", []int{10, 16, 13, 15}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := LastSubmatchIndex(AbsoluteEpisode, tc.name)
			if len(got) != len(tc.want) {
				t.Fatalf("LastSubmatchIndex(AbsoluteEpisode, %q) = %v, want %v", tc.name, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("LastSubmatchIndex(AbsoluteEpisode, %q) = %v, want %v (index %d differs)", tc.name, got, tc.want, i)
				}
			}
		})
	}
}

// TestLastSubmatchIndex_isFindAllsLastMatchProperty pins that, for the two episode
// patterns, LastSubmatchIndex returns exactly FindAllStringSubmatchIndex's LAST
// element, and nil when there is no match. Every season and episode reading takes its
// span offsets from that return. A name assembled from repeated marker-shaped pieces
// is what makes the property discriminating: with one match any scan agrees.
func TestLastSubmatchIndex_isFindAllsLastMatchProperty(t *testing.T) {
	piece := rapid.SampledFrom([]string{
		"Show", " - ", "_", ".", "-", " ", "1080p", "v2", "NCED",
		"S01E01", "S1E7", "S02E05-E07", "S01E15v2", " - 07", "_-_02_", " - 1085 ",
	})
	rapid.Check(t, func(t *rapid.T) {
		re := EpisodeToken
		if rapid.Bool().Draw(t, "absolute_form") {
			re = AbsoluteEpisode
		}
		name := strings.Join(rapid.SliceOfN(piece, 0, 8).Draw(t, "pieces"), "")
		all := re.FindAllStringSubmatchIndex(name, -1)
		var want []int
		if len(all) > 0 {
			want = all[len(all)-1]
		}
		got := LastSubmatchIndex(re, name)
		if len(got) != len(want) {
			t.Fatalf("LastSubmatchIndex(%v, %q) = %v, want %v (the last of %d matches)", re, name, got, want, len(all))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("LastSubmatchIndex(%v, %q) = %v, want %v (index %d differs)", re, name, got, want, i)
			}
		}
	})
}

func TestSpans(t *testing.T) {
	file := func(name string) seadex.File { return seadex.File{Name: name, Length: 100} }
	tests := []struct {
		name  string
		files []seadex.File
		want  []EpisodeSpan
		ok    bool
	}{
		{name: "one SxxExx episode", files: []seadex.File{file("Show - S01E03 (1080p).mkv")}, want: []EpisodeSpan{{1, 3, 3}}, ok: true},
		{name: "a version suffix", files: []seadex.File{file("Show - S02E05v2 (1080p).mkv")}, want: []EpisodeSpan{{2, 5, 5}}, ok: true},
		{name: "a range in one file", files: []seadex.File{file("Show - S01E03-E04 (1080p).mkv")}, want: []EpisodeSpan{{1, 3, 4}}, ok: true},
		{name: "a range without the second E", files: []seadex.File{file("Show - S01E03-04 (1080p).mkv")}, want: []EpisodeSpan{{1, 3, 4}}, ok: true},
		{name: "an absolute number", files: []seadex.File{file("[Grp] Show - 07v2 [1080p].mkv")}, want: []EpisodeSpan{{AbsoluteSeason, 7, 7}}, ok: true},
		{name: "the last token wins", files: []seadex.File{file("Show S01E01 Remake - S01E09.mkv")}, want: []EpisodeSpan{{1, 9, 9}}, ok: true},
		{name: "a file with no token", files: []seadex.File{file("Show - S01E01.mkv"), file("Show Movie.mkv")}, ok: false},
		{name: "a token only the directory carries", files: []seadex.File{file("Show S01E01-E12/a.mkv"), file("Show S01E01-E12/b.mkv")}, ok: false},
		{name: "a backwards range", files: []seadex.File{file("Show - S01E09-E03.mkv")}, ok: false},
		{name: "episode zero", files: []seadex.File{file("Show - S01E00.mkv")}, ok: false},
		{name: "a range past the bound", files: []seadex.File{file("Show - S01E0001-E2001.mkv")}, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Spans(tc.files)
			if ok != tc.ok || (ok && !slices.Equal(got, tc.want)) {
				t.Errorf("Spans(%v) = %v, %v, want %v, %v", tc.files, got, ok, tc.want, tc.ok)
			}
		})
	}
}
