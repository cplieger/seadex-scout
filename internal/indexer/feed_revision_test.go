package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func episodeFiles(format string, n int) []seadex.File {
	files := make([]seadex.File, 0, n)
	for i := 1; i <= n; i++ {
		files = append(files, seadex.File{Name: fmt.Sprintf(format, i), Length: 1 << 30})
	}
	return files
}

// mixedPackFiles is a 12-episode season pack whose group reissued one episode:
// eleven untokened files and S01E07 as v2.
func mixedPackFiles() []seadex.File {
	files := episodeFiles("[Grp] Show - S01E%02d (BD 1080p).mkv", 12)
	files[6].Name = "[Grp] Show - S01E07v2 (BD 1080p).mkv"
	return files
}

func TestSynthesizeTitleCarriesPayloadRevision(t *testing.T) {
	mixed := mixedPackFiles()
	tests := []struct {
		desc string
		tor  seadex.Torrent
		meta EntryInfo
		want string
	}{
		{
			"repack pack",
			seadex.Torrent{ReleaseGroup: "koala", DualAudio: true, Files: episodeFiles("86.Eighty.Six.S01E%02d.REPACK.1080p.Blu-ray.Opus2.0.x265-koala.mkv", 3)},
			EntryInfo{Title: "86 Eighty Six", Season: 1, SeasonKnown: true},
			"86 Eighty Six S01 REPACK 1080p Dual Audio [koala]",
		},
		{
			"v2 pack",
			seadex.Torrent{ReleaseGroup: "Grp", Files: episodeFiles("[Grp] Show - S01E%02dv2 (BD 1080p).mkv", 12)},
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true},
			"Show S01 [v2] 1080p [Grp]",
		},
		{
			"pack whose one reissued episode is v2",
			seadex.Torrent{ReleaseGroup: "Grp", Files: mixed},
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true},
			"Show S01 [v2] 1080p [Grp]",
		},
		{
			"pack with no reissued episode",
			seadex.Torrent{ReleaseGroup: "Grp", Files: episodeFiles("[Grp] Show - S01E%02d (BD 1080p).mkv", 12)},
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true},
			"Show S01 1080p [Grp]",
		},
		{
			"single v2 episode already reads v2",
			seadex.Torrent{ReleaseGroup: "Grp", Files: []seadex.File{{Name: "[Grp] Show - S01E05v2 (BD 1080p).mkv", Length: 1 << 30}}},
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true},
			"Show S01E05V2 1080p [Grp]",
		},
		{
			"repack film",
			seadex.Torrent{ReleaseGroup: "PTP", Files: []seadex.File{{Name: "Given.2020.REPACK.1080p.BluRay.REMUX.DTS-HD.MA.5.1.AVC-PTP.mkv", Length: 1 << 30}}},
			EntryInfo{Title: "Given", Year: 2020, IsMovie: true},
			"Given (2020) REPACK 1080p [PTP]",
		},
		{
			"no flag to sit before",
			seadex.Torrent{Files: []seadex.File{{Name: "Show.S01E01.REPACK.mkv", Length: 1 << 30}, {Name: "Show.S01E02.REPACK.mkv", Length: 1 << 30}}},
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true},
			"Show S01",
		},
		{
			"show title already reads higher",
			seadex.Torrent{ReleaseGroup: "Grp", Files: episodeFiles("[Grp] Proper Show - S01E%02dv2 (BD 1080p).mkv", 2)},
			EntryInfo{Title: "The Proper Show", Season: 1, SeasonKnown: true},
			"The Proper Show S01 1080p [Grp]",
		},
	}
	for _, tc := range tests {
		got := synthesizeTitle(&tc.tor, &tc.meta)
		if got != tc.want {
			t.Errorf("synthesizeTitle(%s) = %q, want %q", tc.desc, got, tc.want)
			continue
		}
		payloadRev := classify.PayloadRevision(tc.tor.Files)
		if read := release.ParseTitleRevision(got); payloadRev.Explicit() && strings.Contains(got, payloadRev.Token()) && read.Version != payloadRev.Version {
			t.Errorf("synthesizeTitle(%s) = %q reads version %d, want the payload's %d", tc.desc, got, read.Version, payloadRev.Version)
		}
	}
}

func TestTwinTitleCarriesPayloadRevision(t *testing.T) {
	tor := seadex.Torrent{ReleaseGroup: "Grp", Files: []seadex.File{{Name: "[Grp] Show Movie 01v2 [BD 1080p].mkv", Length: 1 << 30}}}
	info := EntryInfo{Title: "Show Movie", SeriesTitle: "Show", SpecialEpisodes: []int{3}, SpecialsEpisodes: 1}
	if got, want := twinTitle(&tor, &info), "Show S00E03 [v2] 1080p [Grp]"; got != want {
		t.Errorf("twinTitle(v2 film) = %q, want %q", got, want)
	}
}

func TestRevisedTitleInsertsTokenAfterSeasonToken(t *testing.T) {
	v2 := release.Revision{Version: 2, Marker: release.RevisionVersion}
	repack := release.Revision{Version: 2, Marker: release.RevisionRepack}
	long := "Show S01 " + strings.Repeat("x", maxPersistedFieldBytes) + " [Blu-ray / MKV / 1080p]"
	tests := []struct {
		desc  string
		title string
		rev   release.Revision
		want  string
	}{
		{"season pack", "Show S01 [Blu-ray / MKV / 1080p]", v2, "Show S01 [v2] [Blu-ray / MKV / 1080p]"},
		{"episode", "Show - S01E05 [1080p][Grp]", repack, "Show - S01E05 REPACK [1080p][Grp]"},
		{"already v2", "Show S01 [v2] [Blu-ray / MKV / 1080p]", v2, "Show S01 [v2] [Blu-ray / MKV / 1080p]"},
		{"no season token", "Show [Blu-ray / MKV / 1080p]", v2, "Show [Blu-ray / MKV / 1080p]"},
		{"token would end the title", "Show S01", v2, "Show S01"},
		{"original payload", "Show S01 [Blu-ray]", release.Revision{Version: 1, Marker: release.RevisionNone}, "Show S01 [Blu-ray]"},
		{"unknown payload", "Show S01 [Blu-ray]", release.Revision{}, "Show S01 [Blu-ray]"},
		{"over the persisted cap", long, v2, long},
	}
	for _, tc := range tests {
		if got := revisedTitle(tc.title, tc.rev); got != tc.want {
			t.Errorf("revisedTitle(%s, %+v) = %q, want %q", tc.desc, tc.rev, got, tc.want)
		}
	}
}

func TestApplyTitlesRevisesHarvestedTitleIdempotently(t *testing.T) {
	census := censusPacks(map[string][]curatedRef{
		"ab:1": {{entry: &seadex.Entry{AniListID: 1}, torrent: &seadex.Torrent{Files: episodeFiles("[Grp] Show - S01E%02dv2 (BD 1080p).mkv", 3)}}},
	})
	titles := map[string]string{"ab:1": "Show S01 [Blu-ray / MKV / 1080p]"}
	items := []journalItem{{Key: "ab:1"}}
	audit := titleAudit{census: census}
	want := "Show S01 [v2] [Blu-ray / MKV / 1080p]"
	applyTitles(items, titles, audit)
	if items[0].Title != want || titles["ab:1"] != want {
		t.Fatalf("applyTitles first pass: item %q, cache %q; want both %q", items[0].Title, titles["ab:1"], want)
	}
	again := []journalItem{{Key: "ab:1"}}
	applyTitles(again, titles, audit)
	if again[0].Title != want {
		t.Errorf("applyTitles second pass = %q, want the unchanged %q", again[0].Title, want)
	}
	mixed := censusPacks(map[string][]curatedRef{
		"ab:2": {{entry: &seadex.Entry{AniListID: 2}, torrent: &seadex.Torrent{Files: mixedPackFiles()}}},
	})
	mixedTitles := map[string]string{"ab:2": "Show S01 [Blu-ray / MKV / 1080p]"}
	mixedItems := []journalItem{{Key: "ab:2"}}
	applyTitles(mixedItems, mixedTitles, titleAudit{census: mixed})
	if mixedItems[0].Title != "Show S01 [v2] [Blu-ray / MKV / 1080p]" {
		t.Errorf("applyTitles(pack whose one reissued episode is v2) = %q, want the harvested title carrying [v2]", mixedItems[0].Title)
	}
	carried := []journalItem{{Key: "ab:1"}}
	plain := map[string]string{"ab:1": "Show S01 [Blu-ray / MKV / 1080p]"}
	applyTitles(carried, plain, titleAudit{})
	if carried[0].Title != plain["ab:1"] {
		t.Errorf("applyTitles without census evidence = %q, want the harvested title unchanged", carried[0].Title)
	}
}
