package indexer

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestSortAndCap pins the synthesized feed's ordering + window contract: items
// are sorted newest-first by SeaDex entry update time and the feed is trimmed
// to feedWindow, dropping the oldest entries beyond the cap.
func TestSortAndCap(t *testing.T) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	items := make([]item, feedWindow+2)
	for i := range items {
		items[i] = item{GUID: strconv.Itoa(i), PubDate: base.Add(time.Duration(i) * time.Minute)}
	}
	got := sortAndCap(items)
	if len(got) != feedWindow {
		t.Fatalf("sortAndCap returned %d items, want %d (capped)", len(got), feedWindow)
	}
	newest := base.Add(time.Duration(feedWindow+1) * time.Minute)
	if !got[0].PubDate.Equal(newest) {
		t.Errorf("got[0].PubDate = %v, want %v (newest first)", got[0].PubDate, newest)
	}
	oldestKept := base.Add(2 * time.Minute)
	if !got[len(got)-1].PubDate.Equal(oldestKept) {
		t.Errorf("got[last].PubDate = %v, want %v (the two oldest dropped)", got[len(got)-1].PubDate, oldestKept)
	}
}

// TestBuildFeedsDropsUnknownTracker pins the tail-drop: a SeaDex torrent on a
// tracker other than Nyaa/AB (the negligible AnimeTosho/RuTracker tail) is
// excluded from both synthesized feeds and does not count as an AB passkey skip.
func TestBuildFeedsDropsUnknownTracker(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	nyaa, ab, skipped, _ := buildFeeds(entries, "PK", func(int) []int { return []int{catAnime} })
	if len(nyaa) != 0 || len(ab) != 0 {
		t.Errorf("unknown tracker leaked into a feed: nyaa=%d ab=%d, want 0 and 0", len(nyaa), len(ab))
	}
	if skipped != 0 {
		t.Errorf("abSkippedNoPasskey = %d, want 0 (not an AB passkey skip)", skipped)
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
// releases.moe entry page and a zero/negative id yields no link at all.
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

// TestBuildFeedsIdlessABNotCountedAsPasskeySkip pins the precision of the
// missing-passkey nudge: an AnimeBytes release whose URL carries no parseable
// torrent id is un-grabbable regardless of the passkey, so it is excluded from
// the feed WITHOUT counting toward abSkippedNoPasskey - the operator warning
// must only count releases a passkey would actually make grabbable.
func TestBuildFeedsIdlessABNotCountedAsPasskeySkip(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	nyaa, ab, skipped, _ := buildFeeds(entries, "", func(int) []int { return []int{catAnime} })
	if len(nyaa) != 0 || len(ab) != 0 {
		t.Errorf("id-less AB release leaked into a feed: nyaa=%d ab=%d, want 0 and 0", len(nyaa), len(ab))
	}
	if skipped != 0 {
		t.Errorf("abSkippedNoPasskey = %d, want 0 (no parseable id, so a passkey would not help)", skipped)
	}
}

// TestBuildFeedsDedupesSharedTorrentByGUID pins the feed-side identity merge: a
// torrent attached to two SeaDex entries (same GUID) emits ONE feed item with
// best-wins on the marker (mirroring buildSnapshot's OR-accumulation for the
// search curation set), the categories of both entries unioned, and the newest
// entry update as pubdate. The alt-marked, newer entry is listed FIRST so a
// first-wins or last-wins merge would fail the marker or pubdate assertion.
func TestBuildFeedsDedupesSharedTorrentByGUID(t *testing.T) {
	shared := seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567",
		Files: []seadex.File{{Length: 7, Name: "Show - S01E01 (1080p) [G].mkv"}},
	}
	older := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(48 * time.Hour)
	alt := shared
	best := shared
	best.IsBest = true
	entries := []seadex.Entry{
		{AniListID: 1, Updated: newer, Torrents: []seadex.Torrent{alt}},
		{AniListID: 2, Updated: older, Torrents: []seadex.Torrent{best}},
	}
	classify := func(alID int) []int {
		if alID == 1 {
			return []int{catAnime}
		}
		return []int{catMovies}
	}
	nyaa, ab, skipped, unresolvable := buildFeeds(entries, "", classify)
	if len(ab) != 0 || skipped != 0 || unresolvable != 0 {
		t.Fatalf("ab=%d skipped=%d unresolvable=%d, want all 0", len(ab), skipped, unresolvable)
	}
	if len(nyaa) != 1 {
		t.Fatalf("nyaa feed has %d items, want 1 (same-GUID items merged)", len(nyaa))
	}
	got := nyaa[0]
	if got.DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best-wins even when the alt entry is listed first)", got.DownloadVolumeFactor, dvfBest)
	}
	if len(got.Categories) != 2 || !slices.Contains(got.Categories, catAnime) || !slices.Contains(got.Categories, catMovies) {
		t.Errorf("categories = %v, want the union {%d, %d}", got.Categories, catAnime, catMovies)
	}
	if !got.PubDate.Equal(newer) {
		t.Errorf("pubdate = %v, want %v (newest entry update, independent of which entry is best)", got.PubDate, newer)
	}
}
