package indexer

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// coveredEpisodes counts the distinct episodes a raw file list spans. It is the
// two-step census rule (narrow to the counted population, then count) spelled as
// one call, and it lives HERE because production has no use for that spelling:
// packEvidenceOf grades a torrent and needs both halves separately, so it calls
// contentPopulation and distinctEpisodes itself. Keeping this in feed.go made it
// a production function nothing but tests reached, which is what the deadcode
// gate reports.
func coveredEpisodes(files []seadex.File) int {
	return distinctEpisodes(contentPopulation(files))
}

// TestSortFeedRetainsOverflow pins the journal feed's ordering + retention
// contract: items are sorted newest-first by first-seen time and NOTHING is
// evicted by count - the persisted journal is bounded by age alone
// (feedJournalMaxAge), so a burst of new curation larger than any old window
// persists in full and gets its RSS exposure (growJournal has already marked
// every identity seen, so a count-evicted item could never re-enter). Size
// caps apply only to the rendered view (applyPaging + maxItems, query.go).
func TestSortFeedRetainsOverflow(t *testing.T) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	const n = 302 // larger than the retired 300-item persisted cap
	items := make([]journalItem, n)
	for i := range items {
		items[i] = journalItem{item: item{GUID: strconv.Itoa(i)}, FirstSeen: base.Add(time.Duration(i) * time.Minute)}
	}
	got := sortFeed(items)
	if len(got) != n {
		t.Fatalf("sortFeed returned %d items, want all %d (overflow must persist, never be count-evicted)", len(got), n)
	}
	newest := base.Add(time.Duration(n-1) * time.Minute)
	if !got[0].FirstSeen.Equal(newest) {
		t.Errorf("got[0].FirstSeen = %v, want %v (newest first)", got[0].FirstSeen, newest)
	}
	if !got[len(got)-1].FirstSeen.Equal(base) {
		t.Errorf("got[last].FirstSeen = %v, want %v (the oldest item is retained, not dropped)", got[len(got)-1].FirstSeen, base)
	}
}

// TestStripExt pins the extension handling the title synthesis relies on: a
// known video extension is dropped (case-insensitively), while any other
// trailing dotted token stays (a release name is not a path).
func TestStripExt(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Show - S01E01 (1080p) [G].mkv", "Show - S01E01 (1080p) [G]"},
		{"Show.MKV", "Show"},
		{"Show.webm", "Show"},
		{"Show.txt", "Show.txt"},
		{"Show v2.0", "Show v2.0"},
		{"noext", "noext"},
	}
	for _, tc := range tests {
		if got := stripExt(tc.in); got != tc.want {
			t.Errorf("stripExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEntryURL pins the info-link contract: a positive AniList id yields the
// releases.moe entry page (under the canonical site base) and a zero/negative
// id yields no link at all. The base-normalization rule belongs to
// seadex.EntryURL and is covered by internal/seadex/urls_test.go.
func TestEntryURL(t *testing.T) {
	if got := entryURL(154587); got != "https://releases.moe/154587" {
		t.Errorf("entryURL(154587) = %q, want the releases.moe entry page", got)
	}
	if got := entryURL(0); got != "" {
		t.Errorf("entryURL(0) = %q, want empty", got)
	}
	if got := entryURL(-3); got != "" {
		t.Errorf("entryURL(-3) = %q, want empty", got)
	}
}

// TestRepresentativeFileSkipsCreditlessForAbsolute pins the absolute-numbered
// fallback of the title source pick: with no SxxExx file present, a leading
// creditless extra (NCED) is skipped in favour of a real absolute-numbered
// episode, so the pack's collapsed title derives from an episode, not an extra.
func TestRepresentativeFileSkipsCreditlessForAbsolute(t *testing.T) {
	files := []seadex.File{
		{Name: "[Grp] Show - NCED (1080p).mkv"},
		{Name: "[Grp] Show - 07 (1080p).mkv"},
		{Name: "[Grp] Show - 08 (1080p).mkv"},
	}
	if got := representativeFile(files); got != "[Grp] Show - 07 (1080p).mkv" {
		t.Errorf("representativeFile = %q, want the first absolute-numbered episode", got)
	}
	if got := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}); got != "[Grp] Show (1080p)" {
		t.Errorf("derivedTitle = %q, want %q (collapsed from the episode, not the NCED)", got, "[Grp] Show (1080p)")
	}
}

// TestFeedSynthesisIgnoresSubHalfSizeSample pins the census's sample guard: a
// first-listed, episode-shaped sample clip must neither headline the title nor
// count as an episode, so the feed cannot disagree with the same torrent's
// finding or report row. The guard is payload's type gate reading the "Sample"
// marker in the NAME, not the census size floor: a sample's size ratio to its
// payload overlaps the ratio between two real episodes of unequal length, so
// the floor deliberately admits it (l-f234 / d-gpt-u3c1-1).
func TestFeedSynthesisIgnoresSubHalfSizeSample(t *testing.T) {
	const gib = 1 << 30
	files := []seadex.File{
		{Name: "Show S01E00 Sample [480p].mkv", Length: 200 << 20},
		{Name: "Show S01E01 [1080p].mkv", Length: gib},
	}
	if got := representativeFile(files); got != files[1].Name {
		t.Errorf("representativeFile = %q, want the real payload file %q", got, files[1].Name)
	}
	if got := coveredEpisodes(files); got != 1 {
		t.Errorf("coveredEpisodes = %d, want 1 (the sample is not payload evidence)", got)
	}
	if isPack(&seadex.Torrent{Files: files}) {
		t.Error("isPack = true, want false (one real episode plus a sample is not a pack)")
	}
	if got := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}); got != "Show S01E01 [1080p]" {
		t.Errorf("derivedTitle = %q, want the title derived from the real episode", got)
	}
}

// TestFeedSynthesisCollapsesMixedLengthPack pins that a real season pack
// whose premiere runs double length (or that bundles the franchise movie)
// still counts every episode and collapses to the season label. A
// max-anchored payload floor would drop the regular episodes below half the
// longest file, so coveredEpisodes returned 1, isPack went false, and the
// whole pack was served as "Show S01E01" - which Sonarr grabs as a single
// episode, without the FullSeason ranking a pack earns. This is the inverse
// of the v1.7.2 contract (a real multi-file pack collapses to the season;
// only a single-episode torrent keeps its SxxExx).
func TestFeedSynthesisCollapsesMixedLengthPack(t *testing.T) {
	const gib = 1 << 30
	episodes := func(from, to int, size int64) []seadex.File {
		out := make([]seadex.File, 0, to-from+1)
		for e := from; e <= to; e++ {
			out = append(out, seadex.File{Name: fmt.Sprintf("Show S01E%02d [1080p].mkv", e), Length: size})
		}
		return out
	}
	tests := map[string]struct {
		files    []seadex.File
		wantEps  int
		wantSeen int
	}{
		"double-length premiere": {
			files:    append(episodes(1, 1, 5*gib/2), episodes(2, 12, 6*gib/5)...),
			wantEps:  12,
			wantSeen: 12,
		},
		"bundled franchise movie": {
			// The movie carries no episode token, so it is not counted as an
			// episode - but it must not evict the episodes that are.
			files:    append([]seadex.File{{Name: "Show Movie [1080p].mkv", Length: 4 * gib}}, episodes(1, 12, 6*gib/5)...),
			wantEps:  12,
			wantSeen: 12,
		},
		"two-episode pack with a 4x finale": {
			// The two-file shape no size floor could judge (l-f234 /
			// d-gpt-u3c1-1): the midpoint anchor kept the shorter episode only
			// while the longer stayed within ~3x of it, so this pack read as
			// ONE episode and was served under that episode's own SxxExx
			// marker. The lower-middle anchor counts both, and the sample guard
			// that the floor used to carry is now the type gate's name check.
			files:    append(episodes(1, 1, gib), episodes(2, 2, 4*gib)...),
			wantEps:  2,
			wantSeen: 2,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tor := &seadex.Torrent{Files: tt.files}
			if got := coveredEpisodes(tt.files); got != tt.wantEps {
				t.Errorf("coveredEpisodes = %d, want %d (a shorter episode is still an episode)", got, tt.wantEps)
			}
			if !isPack(tor) {
				t.Error("isPack = false, want true (a multi-episode season pack)")
			}
			if got := seasonCounts(tt.files)[1]; got != tt.wantSeen {
				t.Errorf("seasonCounts[1] = %d, want %d", got, tt.wantSeen)
			}
			if got, want := derivedTitle(tor, EntryInfo{Title: "Show", Season: 1, SeasonKnown: true}), "Show S01 [1080p]"; got != want {
				t.Errorf("derivedTitle = %q, want %q (the pack must collapse to the season)", got, want)
			}
		})
	}
}

// TestCoveredEpisodesCountsExtensionAbuttingAbsoluteForm pins that the
// absolute-episode fallback fires when the episode number abuts the file
// extension ("Show - 07.mkv"): the tokens are matched against the
// extension-stripped name, so a two-episode torrent counts 2 episodes and its
// title collapses instead of reading as a single episode 7.
func TestCoveredEpisodesCountsExtensionAbuttingAbsoluteForm(t *testing.T) {
	files := []seadex.File{{Name: "Show - 07.mkv"}, {Name: "Show - 08.mkv"}}
	if got := coveredEpisodes(files); got != 2 {
		t.Errorf("coveredEpisodes = %d, want 2 (absolute form abutting the extension)", got)
	}
	if got := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}); got != "Show" {
		t.Errorf("derivedTitle = %q, want %q (two-episode pack collapses)", got, "Show")
	}
}

// TestCoveredEpisodesRecognizesUnderscoreAbsolutePacks pins the
// underscore-delimited absolute-order form ("_Show_-_01_"): such packs were
// previously unrecognized (the regex matched only the space-dash form), so a
// whole batch read as its first episode. The tokens must count per episode and
// the pack must collapse.
func TestCoveredEpisodesRecognizesUnderscoreAbsolutePacks(t *testing.T) {
	files := []seadex.File{
		{Name: "[Grp]_Show_-_01_(1080p).mkv"},
		{Name: "[Grp]_Show_-_02_(1080p).mkv"},
	}
	if got := coveredEpisodes(files); got != 2 {
		t.Errorf("coveredEpisodes = %d, want 2 (underscore-delimited absolute episodes)", got)
	}
	if !isPack(&seadex.Torrent{Files: files}) {
		t.Error("isPack = false, want true (an underscore-named absolute-order pack is a pack)")
	}
	// The synthesized-title path labels the pack from the show title; with a
	// known title the underscore pack gets a clean assembled title instead of
	// the first file's name.
	got := synthesizeTitle(&seadex.Torrent{Files: files, ReleaseGroup: "Grp"}, EntryInfo{Title: "Show", Season: 1, SeasonKnown: true})
	if want := "Show S01 1080p [Grp]"; got != want {
		t.Errorf("synthesizeTitle(underscore pack) = %q, want %q", got, want)
	}
}

// TestRepresentativeFileSkipsEpisodeNamedSidecar pins the media-file guard in
// representativeFile: an episode-named subtitle sidecar listed before the
// matching video must not become the title source, so the synthesized feed
// title derives from the media file, not a .ass name.
func TestRepresentativeFileSkipsEpisodeNamedSidecar(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - S01E01 (1080p) [Grp].ass"},
		{Name: "Show - S01E01 (1080p) [Grp].mkv"},
	}
	if got := representativeFile(files); got != files[1].Name {
		t.Errorf("representativeFile = %q, want media file %q", got, files[1].Name)
	}
	if got := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}); got != "Show - S01E01 (1080p) [Grp]" {
		t.Errorf("derivedTitle = %q, want title derived from the media file", got)
	}
}

// TestPackSeason pins the pack season resolution from the FULL file-list span:
// the dominant real season wins, ties break to the lowest, a specials-only
// pack is S00, and an absolute-numbered pack has no season evidence.
func TestPackSeason(t *testing.T) {
	tests := []struct {
		name   string
		files  []seadex.File
		want   int
		wantOK bool
	}{
		{
			name: "dominant real season wins over a leading special",
			files: []seadex.File{
				{Name: "Show - S00E01 (1080p).mkv"},
				{Name: "Show - S01E01 (1080p).mkv"},
				{Name: "Show - S01E02 (1080p).mkv"},
			},
			want: 1, wantOK: true,
		},
		{
			name: "tie breaks to the lowest real season",
			files: []seadex.File{
				{Name: "Show - S02E01.mkv"},
				{Name: "Show - S01E01.mkv"},
			},
			want: 1, wantOK: true,
		},
		{
			name: "specials-only pack is S00",
			files: []seadex.File{
				{Name: "Show - S00E01.mkv"},
				{Name: "Show - S00E02.mkv"},
			},
			want: 0, wantOK: true,
		},
		{
			name: "absolute-numbered pack carries no season evidence",
			files: []seadex.File{
				{Name: "[Grp] Show - 07 (1080p).mkv"},
				{Name: "[Grp] Show - 08 (1080p).mkv"},
			},
			want: 0, wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := packSeason(tc.files)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("packSeason = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestDerivedTitleMixedSeasonPackLabelsRealSeason pins the S00+S01 fix on the
// file-name-derived path: a pack bundling an S00 special with S01 episodes
// must label S01 (the dominant REAL season across the whole file list), not
// the S00 its representative (first) file happens to carry.
func TestDerivedTitleMixedSeasonPackLabelsRealSeason(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - S00E01 (1080p) [Grp].mkv"},
		{Name: "Show - S01E01 (1080p) [Grp].mkv"},
		{Name: "Show - S01E02 (1080p) [Grp].mkv"},
	}
	if got, want := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}), "Show - S01 (1080p) [Grp]"; got != want {
		t.Errorf("derivedTitle(S00+S01 pack) = %q, want %q (labeled by the dominant real season)", got, want)
	}
}

// TestSynthesizeTitle pins the assembled-title shapes: show title + season/
// episode marker + the real flags (resolution from file names, Dual Audio from
// the structured flag, the release group bracketed), a movie as
// "{Title} ({Year})", and the file-name derivation as the no-title fallback.
func TestSynthesizeTitle(t *testing.T) {
	packFiles := []seadex.File{
		{Name: "Frieren Beyond Journey's End - S01E07 (BD Remux 1080p) [PMR].mkv"},
		{Name: "Frieren Beyond Journey's End - S01E08 (BD Remux 1080p) [PMR].mkv"},
	}
	tests := []struct {
		name string
		want string
		t    seadex.Torrent
		meta EntryInfo
	}{
		{
			name: "season pack labels the Fribb season with flags",
			t:    seadex.Torrent{Files: packFiles, ReleaseGroup: "PMR", DualAudio: true},
			meta: EntryInfo{Title: "Frieren: Beyond Journey's End", Season: 1, SeasonKnown: true},
			want: "Frieren: Beyond Journey's End S01 1080p Dual Audio [PMR]",
		},
		{
			name: "pack without a Fribb season labels the file-derived season",
			t:    seadex.Torrent{Files: packFiles, ReleaseGroup: "PMR"},
			meta: EntryInfo{Title: "Frieren"},
			want: "Frieren S01 1080p [PMR]",
		},
		{
			name: "mixed S00+S01 pack labels the dominant real season",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "Show - S00E01 (1080p).mkv"},
				{Name: "Show - S01E01 (1080p).mkv"},
				{Name: "Show - S01E02 (1080p).mkv"},
			}, ReleaseGroup: "Grp"},
			meta: EntryInfo{Title: "Show"},
			want: "Show S01 1080p [Grp]",
		},
		{
			name: "single episode keeps its SxxExx",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "Scum.of.the.Brave.S01E05.1080p.CR.WEB-DL-VARYG.mkv"},
			}, ReleaseGroup: "VARYG"},
			meta: EntryInfo{Title: "Scum of the Brave", Season: 1, SeasonKnown: true},
			want: "Scum of the Brave S01E05 1080p [VARYG]",
		},
		{
			name: "single absolute episode keeps its number",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "[Grp] Some Show - 07 (1080p).mkv"},
			}, ReleaseGroup: "Grp"},
			meta: EntryInfo{Title: "Some Show"},
			want: "Some Show - 07 1080p [Grp]",
		},
		{
			name: "movie carries its year",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "A Silent Voice (2016) (BD 1080p x264 FLAC) [Group].mkv"},
			}, ReleaseGroup: "Group"},
			meta: EntryInfo{Title: "A Silent Voice", Year: 2016, IsMovie: true},
			want: "A Silent Voice (2016) 1080p [Group]",
		},
		{
			name: "movie without a year stays a bare title",
			t:    seadex.Torrent{Files: []seadex.File{{Name: "Movie [Grp].mkv"}}},
			meta: EntryInfo{Title: "Movie", IsMovie: true},
			want: "Movie",
		},
		{
			name: "specials pack without a season labels S00",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "Show OVA - 01.mkv"},
				{Name: "Show OVA - 02.mkv"},
			}},
			meta: EntryInfo{Title: "Show OVA", SeasonKnown: true},
			want: "Show OVA S00",
		},
		{
			name: "absolute pack with no season evidence stays a bare title",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "[Grp] Show - 07 (1080p).mkv"},
				{Name: "[Grp] Show - 08 (1080p).mkv"},
			}, ReleaseGroup: "Grp"},
			meta: EntryInfo{Title: "Show"},
			want: "Show 1080p [Grp]",
		},
		{
			name: "flags omit what is not held",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "Show - S01E01.mkv"},
				{Name: "Show - S01E02.mkv"},
			}},
			meta: EntryInfo{Title: "Show", Season: 1, SeasonKnown: true},
			want: "Show S01",
		},
		{
			name: "no show title falls back to file-name derivation",
			t: seadex.Torrent{Files: []seadex.File{
				{Name: "Show - S01E01 (1080p) [G].mkv"},
				{Name: "Show - S01E02 (1080p) [G].mkv"},
			}},
			meta: EntryInfo{},
			want: "Show - S01 (1080p) [G]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := synthesizeTitle(&tc.t, tc.meta); got != tc.want {
				t.Errorf("synthesizeTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

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
			if got := totalSize(tc.files); got != tc.want {
				t.Errorf("totalSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSynthesizeTitleFilelessAndMarkerlessFallbacks pins the two degenerate
// single-release shapes of the assembled-title path: a file-less torrent (no
// marker source at all) assembles from the show title and the flags it still
// holds, and a marker-less single video file (a movie-shaped OVA under a
// series typing) gets no episode marker rather than an invented one.
func TestSynthesizeTitleFilelessAndMarkerlessFallbacks(t *testing.T) {
	got := synthesizeTitle(&seadex.Torrent{ReleaseGroup: "Grp"}, EntryInfo{Title: "Show"})
	if want := "Show [Grp]"; got != want {
		t.Errorf("synthesizeTitle(file-less) = %q, want %q", got, want)
	}
	got = synthesizeTitle(&seadex.Torrent{Files: []seadex.File{{Name: "Show Movie.mkv"}}}, EntryInfo{Title: "Show OVA"})
	if want := "Show OVA"; got != want {
		t.Errorf("synthesizeTitle(marker-less single file) = %q, want %q", got, want)
	}
}

// TestPackSeasonIgnoresEpisodeNamedSidecars pins the media-file guard inside
// the season tally: episode-token-bearing sidecar files (.ass subtitles) do
// not vote for the pack's season label, so a pack whose subtitle set spans
// another season still labels by its video files' season.
func TestPackSeasonIgnoresEpisodeNamedSidecars(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - S01E01 (1080p).mkv"},
		{Name: "Show - S01E02 (1080p).mkv"},
		{Name: "Show - S02E01 (1080p).ass"},
		{Name: "Show - S02E02 (1080p).ass"},
		{Name: "Show - S02E03 (1080p).ass"},
	}
	season, ok := packSeason(files)
	if season != 1 || !ok {
		t.Errorf("packSeason = (%d, %v), want (1, true) (sidecar tokens must not outvote media files)", season, ok)
	}
}

// TestDerivedTitlePackWithDirectoryOnlyEpisodeTokens pins derivedTitle's final
// fallback (the one branch its tables missed): coveredEpisodes keys a file whose
// OWN base name carries no episode evidence on the FULL path (episodeKeyBase's
// fallback arm), while the title derives from path.Base of the representative
// file - so a pack whose SxxExx tokens live only in directory components is a
// pack with a token-less base, and the trimmed basename is served rather than an
// invented marker.
func TestDerivedTitlePackWithDirectoryOnlyEpisodeTokens(t *testing.T) {
	files := []seadex.File{
		{Name: "S01E01/Movie Cut A.mkv"},
		{Name: "S01E02/Movie Cut B.mkv"},
	}
	if got := coveredEpisodes(files); got != 2 {
		t.Fatalf("coveredEpisodes = %d, want 2 (tokens counted from the full path)", got)
	}
	if got := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}); got != "Movie Cut A" {
		t.Errorf("derivedTitle = %q, want %q (basename fallback when the base carries no episode token)", got, "Movie Cut A")
	}
}

// TestDerivedTitleAbsolutePackUnderSharedEpisodeTokenDirectory pins that
// per-file episode evidence outvotes a shared directory episode token: when a
// pack's files carry only absolute episode numbers in their own base names
// while a SHARED directory component carries an SxxExx token, each file must key on
// its own absolute number (episodeKeyBase), so the torrent reads as the multi-episode
// pack it is instead of collapsing onto the one directory token and being served as
// episode 1.
func TestDerivedTitleAbsolutePackUnderSharedEpisodeTokenDirectory(t *testing.T) {
	files := []seadex.File{
		{Name: "[Grp] Show S01E01-E12 [1080p]/[Grp] Show - 01 [1080p].mkv"},
		{Name: "[Grp] Show S01E01-E12 [1080p]/[Grp] Show - 02 [1080p].mkv"},
		{Name: "[Grp] Show S01E01-E12 [1080p]/[Grp] Show - 03 [1080p].mkv"},
	}
	if got := coveredEpisodes(files); got != 3 {
		t.Fatalf("coveredEpisodes = %d, want 3 (each file keys on its own absolute number,"+
			" not the shared directory token)", got)
	}
	tor := &seadex.Torrent{Files: files}
	if !isPack(tor) {
		t.Fatalf("isPack = false, want true for a three-episode pack")
	}
	if got := derivedTitle(tor, EntryInfo{}); strings.Contains(got, " - 01") {
		t.Errorf("derivedTitle = %q, want the episode number collapsed out of the pack title", got)
	}
}

// TestPackSeasonTieBreakIsOrderIndependent hardens the tie-break contract
// against map iteration order: seasonCounts hands packSeason a map, so a
// single-shot tie assertion could pass by iteration luck even if the
// lowest-season tie-break regressed (e.g. a c >= bestCount boundary slip).
// Repeating the evaluation makes the kill deterministic in practice.
func TestPackSeasonTieBreakIsOrderIndependent(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - S02E01.mkv"},
		{Name: "Show - S01E01.mkv"},
		{Name: "Show - S03E01.mkv"},
	}
	for range 100 {
		if got, ok := packSeason(files); got != 1 || !ok {
			t.Fatalf("packSeason = (%d, %v), want (1, true) on every evaluation (tie must break to the lowest real season regardless of map iteration order)", got, ok)
		}
	}
}

// TestDerivedTitleCollapsesOnlyLastEpisodeToken pins the LAST-token contract of
// the SxxExx collapse arm shared by derivedTitle and coveredEpisodes: a file name
// whose TITLE segment is itself SxxExx-shaped ("Show S02E00 Cut - S01E01")
// must key/collapse on the real trailing marker, preserving the title segment
// verbatim - a first-token regression reads the pack as one episode and
// mangles the served title.
func TestDerivedTitleCollapsesOnlyLastEpisodeToken(t *testing.T) {
	files := []seadex.File{
		{Name: "Show S02E00 Cut - S01E01 (1080p).mkv"},
		{Name: "Show S02E00 Cut - S01E02 (1080p).mkv"},
	}
	if got := coveredEpisodes(files); got != 2 {
		t.Fatalf("coveredEpisodes = %d, want 2 (episodes keyed on the LAST token; the title's SxxExx-shaped substring must not shadow the real marker)", got)
	}
	if got, want := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}), "Show S02E00 Cut - S01 (1080p)"; got != want {
		t.Errorf("derivedTitle = %q, want %q (only the LAST episode token collapses; the title's own SxxExx-shaped substring is preserved verbatim)", got, want)
	}
}

// TestDerivedTitleCollapsesOnlyLastAbsoluteEpisodeToken pins the LAST-token
// contract of the absolute-episode collapse arm: a title segment that is
// itself " - NN"-shaped ("Show - 07 (WEB) - 01") must be preserved and only
// the real trailing episode number collapsed/keyed - the exact case the arm's
// own comment names, previously untested.
func TestDerivedTitleCollapsesOnlyLastAbsoluteEpisodeToken(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - 07 (WEB) - 01.mkv"},
		{Name: "Show - 07 (WEB) - 02.mkv"},
	}
	if got := coveredEpisodes(files); got != 2 {
		t.Fatalf("coveredEpisodes = %d, want 2 (absolute episodes keyed on the LAST token)", got)
	}
	if got, want := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}), "Show - 07 (WEB)"; got != want {
		t.Errorf("derivedTitle = %q, want %q (only the LAST absolute token collapses; the ' - NN'-shaped title segment is preserved)", got, want)
	}
}

// TestSingleEpisodeMarkerUsesLastToken pins singleEpisodeMarker's LAST-token
// pick on the assembled-title path: a single episode whose title segment is
// itself SxxExx-shaped must synthesize with the real trailing marker, not the
// title's substring - a first-token regression serves the wrong episode
// identity to the arr.
func TestSingleEpisodeMarkerUsesLastToken(t *testing.T) {
	got := synthesizeTitle(&seadex.Torrent{Files: []seadex.File{
		{Name: "Show S02E00 Cut - S01E05 (1080p).mkv"},
	}}, EntryInfo{Title: "Show", Season: 1, SeasonKnown: true})
	if want := "Show S01E05 1080p"; got != want {
		t.Errorf("synthesizeTitle(single episode with a token-shaped title segment) = %q, want %q (the marker is the LAST SxxExx token)", got, want)
	}
}

// TestPackSeasonKeysOnLastToken pins the LAST-token rule inside the season
// tally (seasonCounts): each file votes with its trailing SxxExx token, so a
// pack whose title segment carries an S02-shaped substring still labels by
// the real S01 markers - a first-token regression mislabels the whole pack.
func TestPackSeasonKeysOnLastToken(t *testing.T) {
	files := []seadex.File{
		{Name: "Show S02E00 Cut - S01E01.mkv"},
		{Name: "Show S02E00 Cut - S01E02.mkv"},
	}
	season, ok := packSeason(files)
	if season != 1 || !ok {
		t.Errorf("packSeason = (%d, %v), want (1, true) (the season tally keys each file on its LAST token, not the title's S02-shaped substring)", season, ok)
	}
}

// TestEpisodeMarkerRelabelsCourLocalSeason pins the single-release half of
// the season correction (the pack arm already labels by the resolved season): a
// single-episode torrent whose file uses cour-local numbering (S01E07) under an
// entry resolved to season 3 must synthesize S03E07 - the arr's own numbering -
// never the file's S01E07, which points at a DIFFERENT episode of the parent
// series. An absolute "- NN" marker under a POSITIVE season and an entry with
// no resolved season (SeasonKnown false) pass through unchanged, and a file
// already on the resolved season is a no-op. A resolved season 0 (the specials
// bucket) is a real season here, not an absent one, so it relabels to S00 - and
// it is the one season an absolute marker IS rewritten under
// (specialsEpisodeMarker), since a bare "- 07" beside the parent series' title
// is byte-identical to the parent's regular episode 7.
func TestEpisodeMarkerRelabelsCourLocalSeason(t *testing.T) {
	tests := []struct {
		name string
		file string
		meta EntryInfo
		want string
	}{
		{"cour-local season relabeled", "Show - S01E07 (1080p) [G].mkv", EntryInfo{Season: 3, SeasonKnown: true}, "S03E07"},
		{"matching season is a no-op", "Show - S03E07 (1080p) [G].mkv", EntryInfo{Season: 3, SeasonKnown: true}, "S03E07"},
		{"unmapped entry keeps the file marker", "Show - S01E07 (1080p) [G].mkv", EntryInfo{}, "S01E07"},
		{"episode range keeps its range half", "Show - S01E01-E03 [G].mkv", EntryInfo{Season: 2, SeasonKnown: true}, "S02E01-E03"},
		{"absolute marker is never relabeled", "Show - 07 (1080p) [G].mkv", EntryInfo{Season: 3, SeasonKnown: true}, "- 07"},
		{"special relabels cour-local season to S00", "Show - S01E01 (1080p) [G].mkv", EntryInfo{SeasonKnown: true}, "S00E01"},
		{"absolute-marked special becomes a specials token", "Show - 07 (1080p) [G].mkv", EntryInfo{SeasonKnown: true}, "S00E07"},
		{"absolute-marked special drops its version suffix", "Show - 07v2 (1080p) [G].mkv", EntryInfo{SeasonKnown: true}, "S00E07"},
		{"marker-less special gains no invented token", "Show Movie [G].mkv", EntryInfo{SeasonKnown: true}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tor := seadex.Torrent{Files: []seadex.File{{Name: tc.file}}}
			if got := episodeMarker(&tor, tc.meta); got != tc.want {
				t.Errorf("episodeMarker(%q, season %d known %v) = %q, want %q", tc.file, tc.meta.Season, tc.meta.SeasonKnown, got, tc.want)
			}
		})
	}
}

// TestEpisodeTokenIgnoresDashJoinedResolution pins the token boundary: a
// dash-joined resolution after the episode ("S01E07-1080p") must not be
// swallowed by the E-less range arm as the bogus range "S01E07-1080" - the
// single-episode marker keeps the real token, and a pack collapse leaves no
// stray "p" behind. Genuine ranges (dash-joined digits NOT followed by more
// alphanumerics) and underscore-delimited names keep matching.
func TestEpisodeTokenIgnoresDashJoinedResolution(t *testing.T) {
	single := seadex.Torrent{Files: []seadex.File{{Name: "Show - S01E07-1080p [G].mkv"}}}
	if got, want := episodeMarker(&single, EntryInfo{}), "S01E07"; got != want {
		t.Errorf("single marker = %q, want %q (resolution not swallowed)", got, want)
	}
	pack := seadex.Torrent{Files: []seadex.File{
		{Name: "Show - S01E07-1080p [G].mkv"},
		{Name: "Show - S01E08-1080p [G].mkv"},
	}}
	if got, want := derivedTitle(&pack, EntryInfo{}), "Show - S01-1080p [G]"; got != want {
		t.Errorf("pack collapse = %q, want %q (no stray residue)", got, want)
	}
	trueRange := seadex.Torrent{Files: []seadex.File{{Name: "Show - S01E01-13 [G].mkv"}}}
	if got, want := episodeMarker(&trueRange, EntryInfo{}), "S01E01-13"; got != want {
		t.Errorf("range marker = %q, want %q (genuine E-less range kept)", got, want)
	}
	underscore := seadex.Torrent{Files: []seadex.File{{Name: "_Show_S02E05_1080p_.mkv"}}}
	if got, want := episodeMarker(&underscore, EntryInfo{}), "S02E05"; got != want {
		t.Errorf("underscore marker = %q, want %q (underscore-delimited names keep matching)", got, want)
	}
}

// TestSingleEpisodeMarkerAbsoluteArmUsesLastToken pins the LAST-token pick
// of singleEpisodeMarker's ABSOLUTE arm (the SxxExx arm has its own
// last-token test): a single file whose title segment is itself " - NN"-
// shaped ("Show - 07 (WEB) - 01") must yield the trailing "- 01" marker,
// never the title's "- 07" - a limit regression in the FindAll call (the
// live INVERT_NEGATIVES mutant class) serves the wrong episode identity to
// the arr.
func TestSingleEpisodeMarkerAbsoluteArmUsesLastToken(t *testing.T) {
	files := []seadex.File{{Name: "Show - 07 (WEB) - 01.mkv"}}
	if got, want := singleEpisodeMarker(files), "- 01"; got != want {
		t.Errorf("singleEpisodeMarker = %q, want %q (the marker is the LAST absolute token, not the title's ' - NN'-shaped segment)", got, want)
	}
}

// TestDerivedTitleRelabelsCourLocalSeason pins the fallback half of the
// season correction (episodeMarker's relabel already covers the assembled
// path): a title-less entry Fribb maps to season 3 whose files use
// cour-local S01 numbering must serve S03 titles - single episodes and the
// pack collapse alike - while an unmapped entry keeps the file's own season.
func TestDerivedTitleRelabelsCourLocalSeason(t *testing.T) {
	single := &seadex.Torrent{Files: []seadex.File{{Name: "Show - S01E07 (1080p) [G].mkv"}}}
	if got, want := synthesizeTitle(single, EntryInfo{Season: 3, SeasonKnown: true}), "Show - S03E07 (1080p) [G]"; got != want {
		t.Errorf("fallback single = %q, want %q", got, want)
	}
	pack := &seadex.Torrent{Files: []seadex.File{
		{Name: "Show - S01E01 (1080p) [G].mkv"},
		{Name: "Show - S01E02 (1080p) [G].mkv"},
	}}
	if got, want := synthesizeTitle(pack, EntryInfo{Season: 3, SeasonKnown: true}), "Show - S03 (1080p) [G]"; got != want {
		t.Errorf("fallback pack = %q, want %q", got, want)
	}
	if got, want := synthesizeTitle(single, EntryInfo{}), "Show - S01E07 (1080p) [G]"; got != want {
		t.Errorf("unmapped fallback = %q, want %q (file's own season kept)", got, want)
	}
	if got, want := synthesizeTitle(pack, EntryInfo{SeasonKnown: true}), "Show - S00 (1080p) [G]"; got != want {
		t.Errorf("fallback special pack = %q, want %q (the special typing outvotes cour-local file seasons, mirroring episodeMarker's pack arm)", got, want)
	}
}

// TestDerivedTitleAbsolutePackLabelsResolvedSeason pins the absolute-episode
// PACK arm of the derived path: collapsing the "- NN" token already claims the
// whole season, so a pack whose entry resolves a season must SAY which season -
// the same label the SxxExx arm and episodeMarker's pack arm emit for the same
// entry. Without a resolved season (and with no SxxExx evidence in the file
// list) the token still just drops, leaving a bare title.
func TestDerivedTitleAbsolutePackLabelsResolvedSeason(t *testing.T) {
	pack := &seadex.Torrent{Files: []seadex.File{
		{Name: "[Grp] Frieren - 07 (1080p).mkv"},
		{Name: "[Grp] Frieren - 08 (1080p).mkv"},
	}}
	if got, want := derivedTitle(pack, EntryInfo{Season: 1, SeasonKnown: true}), "[Grp] Frieren S01 (1080p)"; got != want {
		t.Errorf("derivedTitle(absolute pack, resolved season) = %q, want %q", got, want)
	}
	if got, want := derivedTitle(pack, EntryInfo{SeasonKnown: true}), "[Grp] Frieren S00 (1080p)"; got != want {
		t.Errorf("derivedTitle(absolute pack, specials bucket) = %q, want %q", got, want)
	}
	if got, want := derivedTitle(pack, EntryInfo{}), "[Grp] Frieren (1080p)"; got != want {
		t.Errorf("derivedTitle(absolute pack, no season) = %q, want %q (nothing to label)", got, want)
	}
}

// TestTitleBasePromotesReleaseNameDirectory pins the derived path's headline
// pick: when the file's own base name carries no episode evidence but an
// ancestor directory does AND that directory has text of its own (the top-level
// directory IS the release name, the files under it are bare "01.mkv"), the
// directory headlines the title - an unparseable "01"/"video" makes the curated
// release invisible on RSS. A token-ONLY directory is still not promoted (a
// bare "S01" with no show name is worse than the basename), which is what
// TestDerivedTitlePackWithDirectoryOnlyEpisodeTokens pins.
func TestTitleBasePromotesReleaseNameDirectory(t *testing.T) {
	pack := &seadex.Torrent{Files: []seadex.File{
		{Name: "[Grp] Show S01E01-E12 (1080p)/01.mkv"},
		{Name: "[Grp] Show S01E01-E12 (1080p)/02.mkv"},
	}}
	if got, want := derivedTitle(pack, EntryInfo{}), "[Grp] Show S01 (1080p)"; got != want {
		t.Errorf("derivedTitle(directory-named pack) = %q, want %q", got, want)
	}
	single := &seadex.Torrent{Files: []seadex.File{{Name: "Show S02E05 [Grp]/video.mkv"}}}
	if got, want := derivedTitle(single, EntryInfo{}), "Show S02E05 [Grp]"; got != want {
		t.Errorf("derivedTitle(directory-named single) = %q, want %q", got, want)
	}
	if got, want := titleBase("Show S02E05 [Grp]/extras/video.mkv"), "Show S02E05 [Grp]"; got != want {
		t.Errorf("titleBase = %q, want %q (the NEAREST ancestor carrying episode evidence, skipping evidence-free components)", got, want)
	}
}

// TestDerivedTitleMappedSeasonNeverRelabelsAbsoluteOrMarkerlessNames pins the
// no-token guard of relabelEpisodeSeason (the branch both title paths share): a
// MAPPED season over a single release whose base name carries no
// SxxExx token - an absolute "- NN" name or a marker-less movie-shaped file -
// must pass through unchanged, never gain an invented season label.
func TestDerivedTitleMappedSeasonNeverRelabelsAbsoluteOrMarkerlessNames(t *testing.T) {
	single := &seadex.Torrent{Files: []seadex.File{{Name: "[Grp] Show - 07 (1080p).mkv"}}}
	if got, want := synthesizeTitle(single, EntryInfo{Season: 3, SeasonKnown: true}), "[Grp] Show - 07 (1080p)"; got != want {
		t.Errorf("derived absolute single with a mapped season = %q, want %q (nothing to relabel)", got, want)
	}
	markerless := &seadex.Torrent{Files: []seadex.File{{Name: "Show Movie.mkv"}}}
	if got, want := synthesizeTitle(markerless, EntryInfo{Season: 3, SeasonKnown: true}), "Show Movie"; got != want {
		t.Errorf("derived marker-less single with a mapped season = %q, want %q (nothing to relabel)", got, want)
	}
	special := &seadex.Torrent{Files: []seadex.File{{Name: "[Grp] Show - 07 (1080p).mkv"}}}
	if got, want := synthesizeTitle(special, EntryInfo{SeasonKnown: true}), "[Grp] Show - 07 (1080p)"; got != want {
		t.Errorf("derived absolute single special = %q, want %q (mapped season 0 has no token to relabel)", got, want)
	}
}

// TestSortFeedIsStableForEqualFirstSeen pins sortFeed's documented stability
// contract: items journaled in the same rebuild share a FirstSeen and must
// keep catalogue order (a SortFunc regression reorders them while every
// distinct-timestamp assertion stays green). Equal-timestamp items are
// interleaved with distinct ones so an unstable sort has to move them.
func TestSortFeedIsStableForEqualFirstSeen(t *testing.T) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	const n = 400
	items := make([]journalItem, 0, n)
	for i := range n {
		ts := base
		if i%2 == 0 {
			ts = base.Add(time.Duration(n-i) * time.Minute)
		}
		items = append(items, journalItem{item: item{GUID: strconv.Itoa(i)}, FirstSeen: ts})
	}
	got := sortFeed(items)
	prev := -1
	for i := range got {
		if !got[i].FirstSeen.Equal(base) {
			continue
		}
		id, _ := strconv.Atoi(got[i].GUID)
		if id < prev {
			t.Fatalf("sortFeed reordered equal-FirstSeen items: GUID %d appears after %d (same-rebuild items must keep catalogue order)", id, prev)
		}
		prev = id
	}
}

// TestRepresentativeFileFallsBackToFirstFileWhenNoMediaFileSurvives pins the
// last-resort arm of the title-source pick: when a torrent's file list holds no
// content media file at all - a sidecar-only list, or a creditless-extras-only
// list - the first file still becomes the title source (classify's eligiblePool
// hands the whole list back once the type gate keeps nothing), so the
// synthesized title keeps a parseable episode marker instead of degrading to
// the bare release group.
func TestRepresentativeFileFallsBackToFirstFileWhenNoMediaFileSurvives(t *testing.T) {
	sidecars := []seadex.File{
		{Name: "Show - S01E01 (1080p) [Grp].ass"},
		{Name: "fonts/Some Font.ttf"},
	}
	if got := representativeFile(sidecars); got != sidecars[0].Name {
		t.Errorf("representativeFile(sidecar-only) = %q, want the first file %q", got, sidecars[0].Name)
	}
	got := synthesizeTitle(&seadex.Torrent{Files: sidecars, ReleaseGroup: "Grp"}, EntryInfo{Title: "Show", Season: 1, SeasonKnown: true})
	if want := "Show S01E01 1080p [Grp]"; got != want {
		t.Errorf("synthesizeTitle(sidecar-only) = %q, want %q (the episode marker still derives from the only file present)", got, want)
	}
	creditless := []seadex.File{
		{Name: "[Grp] Show NCED 01 (1080p).mkv"},
		{Name: "[Grp] Show NCOP 01 (1080p).mkv"},
	}
	if got := representativeFile(creditless); got != creditless[0].Name {
		t.Errorf("representativeFile(creditless-only) = %q, want the first file %q", got, creditless[0].Name)
	}
	if got, want := derivedTitle(&seadex.Torrent{Files: creditless}, EntryInfo{}), "[Grp] Show NCED 01 (1080p)"; got != want {
		t.Errorf("derivedTitle(creditless-only) = %q, want %q", got, want)
	}
}

// TestRepresentativeFileFindsAbsoluteEpisodeAbuttingTheExtension pins the
// asymmetric input representativeFile's ABSOLUTE arm needs (the case its own
// "do not unify them onto one input" comment names): absoluteEpisode ends in
// (?:[\s_]|$), so an episode number abutting the extension ("Show - 07.mkv")
// only matches against the extension-stripped name. Fed the raw name the arm
// finds nothing and the pick falls through to the first media file - here a
// token-less extras file, which then headlines the served RSS title with no
// episode marker the arr can parse.
func TestRepresentativeFileFindsAbsoluteEpisodeAbuttingTheExtension(t *testing.T) {
	files := []seadex.File{
		{Name: "Show Extras Menu.mkv"},
		{Name: "Show - 07.mkv"},
	}
	if got, want := representativeFile(files), files[1].Name; got != want {
		t.Errorf("representativeFile = %q, want the absolute-numbered episode %q (the absolute arm matches against the extension-stripped name)", got, want)
	}
	if got, want := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}), "Show - 07"; got != want {
		t.Errorf("derivedTitle = %q, want %q (the title must derive from the episode, not the extras file)", got, want)
	}
}

// TestCoveredEpisodesTreatsAbsoluteVersionRevisionAsOneEpisode pins the vN
// strip on the ABSOLUTE arm of the episode key (the SxxExx arm's v2 case is
// TestDerivedTitle's "a v2 revision of the same episode is one episode, not a
// pack" row, in indexer_test.go): a lone absolute-numbered episode shipped
// beside its v2 re-encode spans ONE episode, so the torrent keeps its "- NN"
// marker instead of reading as a two-episode pack that collapses to a bare
// season-level title - which Sonarr ranks as FullSeason and grabs as a season
// it does not actually have.
func TestCoveredEpisodesTreatsAbsoluteVersionRevisionAsOneEpisode(t *testing.T) {
	files := []seadex.File{
		{Name: "[Grp] Show - 07 (1080p).mkv"},
		{Name: "[Grp] Show - 07v2 (1080p).mkv"},
	}
	if got := coveredEpisodes(files); got != 1 {
		t.Errorf("coveredEpisodes = %d, want 1 (a v2 revision of the same absolute episode is not a second episode)", got)
	}
	if isPack(&seadex.Torrent{Files: files}) {
		t.Error("isPack = true, want false (one absolute episode plus its v2 revision is not a pack)")
	}
	if got, want := derivedTitle(&seadex.Torrent{Files: files}, EntryInfo{}), "[Grp] Show - 07 (1080p)"; got != want {
		t.Errorf("derivedTitle = %q, want %q (the single episode keeps its absolute marker)", got, want)
	}
}

// TestPackFromTitle pins the title-based season-pack verdict against the rule
// Sonarr's own parser applies to whatever title this app serves: a season-only
// match reads as a pack, an episode token reads as a single episode, and
// anything else - including Sonarr's own EXTRAS/SUBPACK extras group and a
// special marker, both of which cancel FullSeason there - answers UNKNOWN so
// packVerdict falls back to the file census.
func TestPackFromTitle(t *testing.T) {
	tests := map[string]struct {
		title     string
		wantPack  bool
		wantKnown bool
	}{
		"season only":               {"Show - S01 [1080p]", true, true},
		"season word":               {"Show Season 2", true, true},
		"bracketed anime season":    {"[Grp] Show [S01][1080p]", true, true},
		"french season word":        {"Show Saison 1", true, true},
		"series season word":        {"Show Series 3", true, true},
		"italian season word":       {"Show Stagione 2", true, true},
		"season plus episode":       {"Show - S01E07", false, true},
		"absolute episode":          {"[Grp] Show - 07 [1080p]", false, true},
		"episode range":             {"Show S01E01-E12", false, true},
		"empty":                     {"", false, false},
		"bare show name":            {"Show", false, false},
		"unparseable":               {"random text", false, false},
		"season extras":             {"Show - S01 EXTRAS", false, false},
		"season subpack":            {"Show - S01 SUBPACK", false, false},
		"special-marked season":     {"Show - S01 [OVA]", false, false},
		"season then episode digit": {"Show - S01 05", false, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pack, known := packFromTitle(tt.title)
			if pack != tt.wantPack || known != tt.wantKnown {
				t.Errorf("packFromTitle(%q) = (%v, %v), want (%v, %v)", tt.title, pack, known, tt.wantPack, tt.wantKnown)
			}
		})
	}
}

// TestPackFromTitleRefusesSeasonFollowedByEpisodeNumber pins the load-bearing
// half of Sonarr's season-only regex separately, because it is the one
// condition RE2 cannot express and this app therefore reimplements: the
// negative lookahead after the season number. "Show - S01 05" names episode 5
// of season 1, so the title must NOT claim a whole season - it answers unknown
// and the file census decides.
func TestPackFromTitleRefusesSeasonFollowedByEpisodeNumber(t *testing.T) {
	for _, title := range []string{"Show - S01 05", "Show S01 5", "Show Season 1 05", "Show.S01.05"} {
		if pack, _ := packFromTitle(title); pack {
			t.Errorf("packFromTitle(%q) read as a season pack; the season number is followed by an episode number", title)
		}
	}
}

// packEvidenceName renders a census grade for a failure message (the production
// type carries no String method - nothing in the app formats one).
func packEvidenceName(e packEvidence) string {
	switch e {
	case packEvidencePack:
		return "pack"
	case packEvidenceSingle:
		return "single"
	case packEvidenceUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("packEvidence(%d)", int(e))
	}
}

// TestPackEvidenceOf pins the three-valued census: what a torrent's FILE LIST
// proves about its episode count. The distinction the old boolean could not make
// is the load-bearing one - zero recognized tokens (absent files, or naming
// outside the two recognized forms) proves NOTHING about the payload and must
// never read as positive single-episode evidence, because that is the evidence a
// served-title correction acts on.
//
// It also pins that isPack is EXACTLY the pack arm, for every row: the boolean
// and the three-valued reading have one source of truth.
func TestPackEvidenceOf(t *testing.T) {
	const gib = 1 << 30
	pack := make([]seadex.File, 0, 12)
	for e := 1; e <= 12; e++ {
		pack = append(pack, seadex.File{Name: fmt.Sprintf("Show S01E%02d [1080p].mkv", e), Length: gib})
	}
	tests := map[string]struct {
		files []seadex.File
		want  packEvidence
	}{
		"twelve-episode pack":      {pack, packEvidencePack},
		"lone SxxExx episode":      {[]seadex.File{{Name: "Show S01E07 [1080p].mkv", Length: gib}}, packEvidenceSingle},
		"lone absolute episode":    {[]seadex.File{{Name: "[Grp] Show - 07 (1080p).mkv", Length: gib}}, packEvidenceSingle},
		"empty file list":          {[]seadex.File{}, packEvidenceUnknown},
		"nil file list":            {nil, packEvidenceUnknown},
		"unrecognized bare number": {[]seadex.File{{Name: "01.mkv", Length: gib}}, packEvidenceUnknown},
		"unrecognized 1x01 form":   {[]seadex.File{{Name: "1x01.mkv", Length: gib}}, packEvidenceUnknown},
		"episode beside a marked sample": {[]seadex.File{
			{Name: "Show S01E00 Sample [480p].mkv", Length: 200 << 20},
			{Name: "Show S01E07 [1080p].mkv", Length: gib},
		}, packEvidenceSingle},
		"episode beside a token-less extra": {[]seadex.File{
			{Name: "Show S01E07 [1080p].mkv", Length: gib},
			{Name: "Show Cast Interview [1080p].mkv", Length: gib},
		}, packEvidenceSingle},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tor := &seadex.Torrent{Files: tt.files}
			got := packEvidenceOf(tor)
			if got != tt.want {
				t.Errorf("packEvidenceOf = %s, want %s", packEvidenceName(got), packEvidenceName(tt.want))
			}
			if want := got == packEvidencePack; isPack(tor) != want {
				t.Errorf("isPack = %v, want %v (isPack must be exactly packEvidenceOf's pack arm)", isPack(tor), want)
			}
		})
	}
}

// TestCorrectSeasonOnlyTitle pins the surgical rewrite: only the title's season
// token changes, so the group, resolution and codec text Sonarr reads for its
// quality and custom-format decisions survives byte for byte. The season number
// stays the TRACKER's claim (the title says which season) while the episode half
// comes from the file census (the files say which episode), and a marker the
// rewrite cannot read refuses rather than guessing.
func TestCorrectSeasonOnlyTitle(t *testing.T) {
	tests := map[string]struct {
		title, marker, want string
		wantOK              bool
	}{
		"season token gains the census episode": {"Show - S01 [1080p][x265]-GRP", "S01E07", "Show - S01E07 [1080p][x265]-GRP", true},
		"season word form":                      {"Show Season 2", "- 07", "Show S02E07", true},
		"bracketed anime season":                {"[Grp] Show [S01][1080p]", "S01E07", "[Grp] Show [S01E07][1080p]", true},
		"versioned absolute marker":             {"Show Season 2", "- 07v2", "Show S02E07", true},
		"range marker keeps its range":          {"Show - S01 [1080p]", "S01E01-E13", "Show - S01E01-E13 [1080p]", true},
		"empty marker refuses":                  {"Show - S01 [1080p]", "", "Show - S01 [1080p]", false},
		"unreadable marker refuses":             {"Show - S01 [1080p]", "07", "Show - S01 [1080p]", false},
		"title without a season token refuses":  {"Show [1080p]", "S01E07", "Show [1080p]", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := correctSeasonOnlyTitle(tt.title, tt.marker)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("correctSeasonOnlyTitle(%q, %q) = (%q, %v), want (%q, %v)", tt.title, tt.marker, got, ok, tt.want, tt.wantOK)
			}
			if !ok {
				return
			}
			// The corrected title must no longer claim a whole season to the
			// parser this app serves - that is the entire point of the rewrite.
			if pack, known := packFromTitle(got); pack || !known {
				t.Errorf("packFromTitle(%q) = (%v, %v), want the corrected title to read as one episode", got, pack, known)
			}
		})
	}
}

// TestEpisodeTokenBoundary pins where the season/episode tokenizer decides a
// token ENDS, now that the boundary is the shared release-name word alphabet
// (nametoken.NonWordEdge) rather than a case-folded [^0-9a-z] class. Dot and
// hyphen are the rows that matter: both are ordinary token boundaries here (a
// dot-delimited scene name and a dash-joined resolution must both yield the bare
// SxxExx), while the ABSOLUTE form's own delimiters deliberately stay narrower
// than the shared edge - "Show.-.07" is not an absolute episode, which is why
// representativeFile strips the extension before running that pattern. A table
// so the two rules are read side by side; conflating them is how the dialects
// drifted apart in the first place.
func TestEpisodeTokenBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		want string
	}{
		// SxxExx: every non-word rune ends the token, underscore included.
		{"dot ends the token", "Show.S01E01.1080p.mkv", "S01E01"},
		{"hyphen ends the token", "Show - S01E07-1080p [G].mkv", "S01E07"},
		{"underscore ends the token", "_Show_S02E05_1080p_.mkv", "S02E05"},
		{"end of name ends the token", "Show S01E01.mkv", "S01E01"},
		{"bracket ends the token", "Show [S01E01][1080p].mkv", "S01E01"},
		{"a letter does not end the token", "Show S01E01p.mkv", ""},
		{"a digit does not end the token", "Show - S01E01-13 [G].mkv", "S01E01-13"},
		// The absolute "- NN" form: space and underscore only, by policy.
		{"space-delimited absolute episode", "Show - 07.mkv", "- 07"},
		{"underscore-delimited absolute episode", "[Grp]_Show_-_07_(1080p).mkv", "- 07"},
		{"dot-delimited absolute episode is not one", "Show.-.07.mkv", ""},
		{"hyphen-delimited absolute episode is not one", "Show---07.mkv", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := singleEpisodeMarker([]seadex.File{{Name: tc.file}}); got != tc.want {
				t.Errorf("singleEpisodeMarker(%q) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

// TestHomographVocabularyAgreesWithTheReleaseClassifier is the convergence pin:
// the season/episode tokenizer here and the marker engine in internal/release now
// read ONE name vocabulary (internal/nametoken), so a name carrying U+017F (ſ) or
// U+0130 (İ) means the same thing to both. It used to not: this package folded
// case with a global (?i) (unicode.SimpleFold) while the classifier folded with
// strings.ToLower, so "ſ01E01" was a season-1 token here - ToUpper even rendered
// the served marker as a confident "S01E01" - and no marker at all there, and
// U+0130 ended a token here while continuing a word there.
//
// Each row states the shared rule, the marker this side reads, and the kind the
// classifier reads from the SAME name. Rows 1-3 are the two-sided ones; row 4
// records the one half only the classifier can observe (its markers contain the
// letters i and k, the episode tokens do not), so a reader does not mistake the
// silence for disagreement.
func TestHomographVocabularyAgreesWithTheReleaseClassifier(t *testing.T) {
	for _, tc := range []struct {
		rule       string
		file       string
		wantMarker string
		wantKind   release.Kind
	}{
		{
			rule:       "U+017F is not the letter s: no season token here, no kbps marker there",
			file:       "Show - \u017f01E01 4500 kbp\u017f [G].mkv",
			wantMarker: "",
			wantKind:   release.KindUnknown,
		},
		{
			rule:       "U+017F ends a token on both sides: the SxxExx stands, the remux marker stands",
			file:       "Show - S01E01\u017f Remux\u017f [G].mkv",
			wantMarker: "S01E01",
			wantKind:   release.KindRemux,
		},
		{
			rule:       "U+0130 continues a word on both sides: neither token can end there",
			file:       "Show - S01E01\u0130 Remux\u0130 [G].mkv",
			wantMarker: "",
			wantKind:   release.KindUnknown,
		},
		{
			rule:       "U+0130 folds onto i for the classifier; the episode tokens carry no i to fold",
			file:       "Show - S01E01 BDR\u0130P [G].mkv",
			wantMarker: "S01E01",
			wantKind:   release.KindEncode,
		},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			if got := singleEpisodeMarker([]seadex.File{{Name: tc.file}}); got != tc.wantMarker {
				t.Errorf("singleEpisodeMarker(%+q) = %q, want %q", tc.file, got, tc.wantMarker)
			}
			if got := release.Classify(&release.Input{Names: []string{tc.file}}); got.Kind != tc.wantKind {
				t.Errorf("release.Classify(%+q).Kind = %q (%s), want %q", tc.file, got.Kind, got.Reason, tc.wantKind)
			}
		})
	}
}

// TestPackFromTitleReadsSonarrCleanedTitles pins the quality-token strip
// (sonarrSimpleNoise) that makes the season-only reading answer for a REAL
// tracker title. Sonarr deletes those tokens (SimpleTitleRegex) before its
// season/episode patterns run, so the lookahead must be applied to the CLEANED
// tail: on a raw title the digits after the season number are the RESOLUTION's,
// the title answers UNKNOWN, and titleAudit.served makes no correction - while
// Sonarr, reading "Show S01", sets FullSeason, grabs the release as a whole
// season and suppresses that season's real episodes.
//
// The shapes TestPackFromTitle covers all separate the season number from the
// resolution with a bracket, which reads as a pack with or without the strip.
// The last row is the companion guarantee: stripping the noise must not defeat
// the season-only lookahead - a real episode number after the quality tokens
// still refuses.
func TestPackFromTitleReadsSonarrCleanedTitles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		title     string
		wantPack  bool
		wantKnown bool
	}{
		{"unbracketed resolution after the season", "Show S01 1080p BluRay [G]", true, true},
		{"720p tail", "Show - S01 720p", true, true},
		{"resolution plus DD5.1", "Show - S01 2160p DD5.1", true, true},
		{"WxH dimensions", "Show - S01 1920x1080", true, true},
		{"bit-depth marker", "Show - S01 10-bit", true, true},
		{"episode number after the quality tokens", "Show S01 1080p 05", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pack, known := packFromTitle(tc.title)
			if pack != tc.wantPack || known != tc.wantKnown {
				t.Errorf("packFromTitle(%q) = (%v, %v), want (%v, %v)", tc.title, pack, known, tc.wantPack, tc.wantKnown)
			}
		})
	}
}
