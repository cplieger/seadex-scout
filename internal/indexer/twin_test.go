package indexer

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func twinFiles(lengths map[string]int64) []seadex.File {
	files := make([]seadex.File, 0, len(lengths))
	for name, length := range lengths {
		files = append(files, seadex.File{Name: name, Length: length})
	}
	return files
}

func numberedFiles(format string, first, last int, length int64) map[string]int64 {
	out := make(map[string]int64, last-first+1)
	for e := first; e <= last; e++ {
		out[fmt.Sprintf(format, e)] = length
	}
	return out
}

func TestTwinTitle(t *testing.T) {
	film := twinFiles(map[string]int64{
		"[G] Lelouch of the Resurrection (BD 1080p).mkv":         8 << 30,
		"[G] Lelouch of the Resurrection (BD 1080p) - NCOP.mkv":  100 << 20,
		"Extras/[G] Lelouch of the Resurrection - Trailer.mkv":   50 << 20,
		"[G] Lelouch of the Resurrection (BD 1080p).ass":         1 << 20,
		"Extras/[G] Lelouch of the Resurrection - Menu (BD).mkv": 10 << 20,
	})
	filmWithSeries := numberedFiles("[Arid] Steins;Gate - %02d (BD 1080p).mkv", 1, 24, 3<<29)
	filmWithSeries["[Arid] Steins;Gate - The Movie - Load Region of Deja Vu (BD 1080p).mkv"] = 8 << 30
	hyoukaPack := numberedFiles("[MTBB] Hyouka - %02d (BD 1080p).mkv", 1, 22, 1<<30)
	hyoukaPack["[MTBB] Hyouka - 11.5 (OVA) (BD 1080p).mkv"] = 1 << 30
	geass := EntryInfo{IsMovie: true, SpecialEpisodes: []int{4}, SpecialsEpisodes: 1, SeriesTitle: "Code Geass"}
	fallback := geass
	fallback.SpecialsEpisodes = 0
	ovaFallback := fallback
	ovaFallback.IsMovie = false
	tests := []struct {
		name      string
		files     []seadex.File
		info      EntryInfo
		want      string
		dualAudio bool
	}{
		{name: "a film with BD extras", files: film, info: geass, dualAudio: true, want: "Code Geass S00E04 1080p Dual Audio [G]"},
		{
			name: "a whole-season pack holding the OVA", files: twinFiles(hyoukaPack),
			info: EntryInfo{SpecialEpisodes: []int{1}, SpecialsEpisodes: 1, SeriesTitle: "Hyouka"},
		},
		{
			name: "a film shipped with the series it follows", files: twinFiles(filmWithSeries),
			info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{2}, SpecialsEpisodes: 1, SeriesTitle: "Steins;Gate"},
		},
		{
			name: "one episode of a four-episode ONA", files: twinFiles(map[string]int64{"[AtlasSubbed] Mini Yuri - 01 1080p.mkv": 1 << 28}),
			info: EntryInfo{SpecialEpisodes: []int{5, 6, 7, 8}, SpecialsEpisodes: 4, SeriesTitle: "Mini Yuri"},
		},
		{
			name: "two of five episodes of an OVA", files: twinFiles(numberedFiles("[G] Food Wars - S00E%02d (BD 1080p).mkv", 2, 3, 1<<30)),
			info: EntryInfo{SpecialEpisodes: []int{2, 3, 4, 5, 6}, SpecialsEpisodes: 5, SeriesTitle: "Food Wars!"},
		},
		{
			name: "two placed episodes beside three unrecognised videos",
			files: twinFiles(map[string]int64{
				"[sam] Kuroshitsuji - Book of Murder - 01 [BD 1080p].mkv":  3 << 30,
				"[sam] Kuroshitsuji - Book of Murder - 02 [BD 1080p].mkv":  3 << 30,
				"[sam] Kuroshitsuji - Book of the Atlantic [BD 1080p].mkv": 3 << 30,
				"[sam] Kuroshitsuji - Book of Circus [BD 1080p].mkv":       3 << 30,
				"[sam] Kuroshitsuji - Lucifer [BD 1080p].mkv":              3 << 30,
			}),
			info: EntryInfo{SpecialEpisodes: []int{9, 10}, SpecialsEpisodes: 2, SeriesTitle: "Black Butler"},
		},
		{
			name: "a two-part special in two files", files: twinFiles(numberedFiles("[sam] Kuroshitsuji - Book of Murder - %02d [BD 1080p FLAC].mkv", 1, 2, 3<<30)),
			info: EntryInfo{SpecialEpisodes: []int{9, 10}, SpecialsEpisodes: 2, SeriesTitle: "Black Butler"},
			want: "Black Butler S00E09-E10 1080p [G]",
		},
		{
			name: "one file answering one AniDB episode over three TVDB specials", files: twinFiles(map[string]int64{"[G] Film (BD 1080p).mkv": 6 << 30}),
			info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{4, 5, 6}, SpecialsEpisodes: 1, SeriesTitle: "Series"},
			want: "Series S00E04-E06 1080p [G]",
		},
		{
			name: "one film of a three-film entry", files: twinFiles(map[string]int64{"Mononoke The Movie The Phantom in the Rain (2024) (1080p).mkv": 6 << 30}),
			info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{4, 5, 6}, SpecialsEpisodes: 3, SeriesTitle: "Mononoke"},
		},
		{
			name: "two placed episodes padded to the run by unrecognised videos",
			files: twinFiles(map[string]int64{
				"[G] Food Wars - S00E02 (BD 1080p).mkv":  1 << 30,
				"[G] Food Wars - S00E03 (BD 1080p).mkv":  1 << 30,
				"[G] Food Wars - The Fifth Plate.mkv":    1 << 30,
				"[G] Food Wars - The Second Plate.mkv":   1 << 30,
				"[G] Food Wars - Totsuki Autumn Fes.mkv": 1 << 30,
			}),
			info: EntryInfo{SpecialEpisodes: []int{2, 3, 4, 5, 6}, SpecialsEpisodes: 5, SeriesTitle: "Food Wars!"},
		},
		{
			name: "two files of a special answering three AniDB episodes", files: twinFiles(numberedFiles("[G] Saiki Kusuo no Psi Nan - Kanketsu-hen - %02d (WEB 1080p).mkv", 1, 2, 1<<30)),
			info: EntryInfo{SpecialEpisodes: []int{2}, SpecialsEpisodes: 3, SeriesTitle: "The Disastrous Life of Saiki K."},
			want: "The Disastrous Life of Saiki K. S00E02 1080p [G]",
		},
		{
			name: "a gapped placement", files: twinFiles(numberedFiles("[G] Overlord Movie - %02d (BD 1080p).mkv", 1, 3, 4<<30)),
			info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{9, 12, 13}, SpecialsEpisodes: 3, SeriesTitle: "Overlord"},
		},
		{name: "episode 999", files: film, info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{999}, SpecialsEpisodes: 1, SeriesTitle: "S"}, dualAudio: true, want: "S S00E999 1080p Dual Audio [G]"},
		{name: "episode 1000", files: film, info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{1000}, SpecialsEpisodes: 1, SeriesTitle: "S"}},
		{name: "the mapping-list fallback on one file", files: film, info: fallback, dualAudio: true, want: "Code Geass S00E04 1080p Dual Audio [G]"},
		{name: "the mapping-list fallback for an OVA on one file", files: film, info: ovaFallback},
		{name: "the mapping-list fallback on two files", files: twinFiles(numberedFiles("[G] Code Geass Akito - %02d (BD 1080p).mkv", 1, 2, 3<<30)), info: fallback},
		{name: "no episode", files: film, info: EntryInfo{IsMovie: true, SeriesTitle: "Code Geass"}},
		{name: "blank series title", files: film, info: EntryInfo{IsMovie: true, SpecialEpisodes: []int{4}, SpecialsEpisodes: 1, SeriesTitle: "  "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tor := &seadex.Torrent{ReleaseGroup: "G", Files: tc.files, DualAudio: tc.dualAudio}
			if got := twinTitle(tor, &tc.info); got != tc.want {
				t.Errorf("twinTitle(%s, %+v) = %q, want %q", tc.name, tc.info, got, tc.want)
			}
		})
	}
}

// TestTwinGUIDKeysToTheSameTracker pins the identity rule the twin rests on: the
// fragment GUID passes trackerKeyFromURL to the SAME key as the original for both
// tracker URL shapes (a Nyaa view URL, an AnimeBytes torrentid URL), so the search
// path still recognizes the twin as the curated release while Prowlarr and
// Sonarr, which key on the GUID string, keep the two items apart.
func TestTwinGUIDKeysToTheSameTracker(t *testing.T) {
	for _, guid := range []string{
		"https://nyaa.si/view/1234567",
		"https://animebytes.tv/torrents.php?id=1&torrentid=1167293",
	} {
		twin := twinGUID(guid)
		if twin == guid || twin == "" {
			t.Fatalf("twinGUID(%q) = %q, want a distinct non-empty GUID", guid, twin)
		}
		if got, want := trackerKeyFromURL(twin), trackerKeyFromURL(guid); got != want || want == "" {
			t.Errorf("trackerKeyFromURL(twin %q) = %q, want the original's %q", twin, got, want)
		}
	}
	if twinGUID("") != "" {
		t.Error(`twinGUID("") != "", want empty`)
	}
}

// TestTwinVote pins the unanimous fold on the twin title: agreement resolves
// it, and a disagreement or an abstaining holder vetoes it, through merge too.
func TestTwinVote(t *testing.T) {
	tests := []struct {
		name   string
		titles []string
		want   string
	}{
		{name: "none", titles: nil, want: ""},
		{name: "one", titles: []string{"Series S00E04 [G]"}, want: "Series S00E04 [G]"},
		{name: "agree", titles: []string{"Series S00E04 [G]", "Series S00E04 [G]"}, want: "Series S00E04 [G]"},
		{name: "one abstains", titles: []string{"", "Series S00E04 [G]"}, want: ""},
		{name: "disagree", titles: []string{"Series S00E02 [G]", "Series S00E03 [G]"}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v twinVote
			for _, title := range tc.titles {
				v.add(title)
			}
			if got := v.resolve(); got != tc.want {
				t.Errorf("twinVote over %q resolve() = %q, want %q", tc.titles, got, tc.want)
			}
		})
	}
	var a, b twinVote
	a.add("Series S00E02 [G]")
	b.add("Series S00E03 [G]")
	a.merge(b)
	if a.resolve() != "" {
		t.Errorf("merge of two different titles resolve() = %q, want a veto", a.resolve())
	}
	var c, d, abstain twinVote
	c.add("Series S00E02 [G]")
	d.merge(c)
	if d.resolve() != "Series S00E02 [G]" {
		t.Errorf("merge into an empty vote resolve() = %q, want the merged title", d.resolve())
	}
	abstain.add("")
	d.merge(abstain)
	if d.resolve() != "" {
		t.Errorf("merge of an abstention resolve() = %q, want a veto", d.resolve())
	}
}

// TestTwinTokenSpellsTheRunAsTheReportDoes pins one spelling for a season-0 run
// across the feed token and the report's Scope cell (align.EpisodeLabel).
func TestTwinTokenSpellsTheRunAsTheReportDoes(t *testing.T) {
	for _, run := range [][]int{{1}, {4}, {9, 10}, {4, 5, 6}, {21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}, {999}} {
		if got, want := twinToken(run[0], run[len(run)-1]), align.EpisodeLabel(run); got != want {
			t.Errorf("twinToken(%d, %d) = %q, want align.EpisodeLabel(%v) = %q", run[0], run[len(run)-1], got, run, want)
		}
	}
}
