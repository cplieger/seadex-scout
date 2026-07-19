package indexer

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

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
	items := make([]item, n)
	for i := range items {
		items[i] = item{GUID: strconv.Itoa(i), FirstSeen: base.Add(time.Duration(i) * time.Minute)}
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
// releases.moe entry page (the default site base when none is configured, a
// trailing-slash base normalized to the same link) and a zero/negative id
// yields no link at all.
func TestEntryURL(t *testing.T) {
	w := NewFeedWriter(&FeedWriterConfig{}, Deps{})
	if got := w.entryURL(154587); got != "https://releases.moe/154587" {
		t.Errorf("entryURL(154587) = %q, want the releases.moe entry page", got)
	}
	if got := w.entryURL(0); got != "" {
		t.Errorf("entryURL(0) = %q, want empty", got)
	}
	if got := w.entryURL(-3); got != "" {
		t.Errorf("entryURL(-3) = %q, want empty", got)
	}
	slash := NewFeedWriter(&FeedWriterConfig{SeaDexBaseURL: "https://releases.moe/"}, Deps{})
	if got := slash.entryURL(154587); got != "https://releases.moe/154587" {
		t.Errorf("entryURL(154587) with trailing-slash base = %q, want the normalized entry page", got)
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
	if got := feedTitle(&seadex.Torrent{Files: files}); got != "[Grp] Show (1080p)" {
		t.Errorf("feedTitle = %q, want %q (collapsed from the episode, not the NCED)", got, "[Grp] Show (1080p)")
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
	if got := feedTitle(&seadex.Torrent{Files: files}); got != "Show" {
		t.Errorf("feedTitle = %q, want %q (two-episode pack collapses)", got, "Show")
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
	got := synthesizeTitle(&seadex.Torrent{Files: files, ReleaseGroup: "Grp"}, EntryInfo{Title: "Show", SeasonTvdb: 1})
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
	if got := feedTitle(&seadex.Torrent{Files: files}); got != "Show - S01E01 (1080p) [Grp]" {
		t.Errorf("feedTitle = %q, want title derived from the media file", got)
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

// TestFeedTitleMixedSeasonPackLabelsRealSeason pins the S00+S01 fix on the
// file-name-derived path: a pack bundling an S00 special with S01 episodes
// must label S01 (the dominant REAL season across the whole file list), not
// the S00 its representative (first) file happens to carry.
func TestFeedTitleMixedSeasonPackLabelsRealSeason(t *testing.T) {
	files := []seadex.File{
		{Name: "Show - S00E01 (1080p) [Grp].mkv"},
		{Name: "Show - S01E01 (1080p) [Grp].mkv"},
		{Name: "Show - S01E02 (1080p) [Grp].mkv"},
	}
	if got, want := feedTitle(&seadex.Torrent{Files: files}), "Show - S01 (1080p) [Grp]"; got != want {
		t.Errorf("feedTitle(S00+S01 pack) = %q, want %q (labeled by the dominant real season)", got, want)
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
		t    seadex.Torrent
		meta EntryInfo
		want string
	}{
		{
			name: "season pack labels the Fribb season with flags",
			t:    seadex.Torrent{Files: packFiles, ReleaseGroup: "PMR", DualAudio: true},
			meta: EntryInfo{Title: "Frieren: Beyond Journey's End", SeasonTvdb: 1},
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
			meta: EntryInfo{Title: "Scum of the Brave", SeasonTvdb: 1},
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
			meta: EntryInfo{Title: "Show OVA", IsSpecial: true},
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
			meta: EntryInfo{Title: "Show", SeasonTvdb: 1},
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

// TestFeedTitlePackWithDirectoryOnlyEpisodeTokens pins feedTitle's final
// fallback (the one branch its tables missed): coveredEpisodes counts episode
// tokens from the FULL path, but the title derives from path.Base of the
// representative file - so a pack whose SxxExx tokens live only in directory
// components is a pack with a token-less base, and the trimmed basename is
// served rather than an invented marker.
func TestFeedTitlePackWithDirectoryOnlyEpisodeTokens(t *testing.T) {
	files := []seadex.File{
		{Name: "S01E01/Movie Cut A.mkv"},
		{Name: "S01E02/Movie Cut B.mkv"},
	}
	if got := coveredEpisodes(files); got != 2 {
		t.Fatalf("coveredEpisodes = %d, want 2 (tokens counted from the full path)", got)
	}
	if got := feedTitle(&seadex.Torrent{Files: files}); got != "Movie Cut A" {
		t.Errorf("feedTitle = %q, want %q (basename fallback when the base carries no episode token)", got, "Movie Cut A")
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
