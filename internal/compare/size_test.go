package compare

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

const (
	gib       = int64(1) << 30
	heldBytes = gib     // each held episode file
	newBytes  = 2 * gib // each SeaDex episode file
)

// sizedSeries is a Sonarr series holding season 1 as twelve one-episode files,
// file 100+e holding episode e, each heldBytes.
func sizedSeries() *library.Item {
	item := &library.Item{
		Title: "Show", Arr: library.ArrSonarr, ArrID: 7, HasFile: true,
		Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}},
		SeasonEpisodes: map[int]map[int]library.Episode{1: {}},
		FileBytes:      map[int]int64{},
	}
	for e := 1; e <= 12; e++ {
		item.SeasonEpisodes[1][e] = library.Episode{File: 100 + e, Absolute: e}
		item.FileBytes[100+e] = heldBytes
	}
	return item
}

// hash is a valid 40-hex info hash unique per n. Packs take small n and
// singles 500 and up, so a pack is the headline among otherwise equal candidates.
func hash(n int) string { return fmt.Sprintf("%040x", n) }

// single is one SubsPlease single-episode torrent for S01E<e>.
func single(e int, codec string) seadex.Torrent {
	return seadex.Torrent{
		IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa",
		URL: fmt.Sprintf("https://nyaa.si/view/%d", e), InfoHash: hash(500 + e),
		Files: []seadex.File{{Name: fmt.Sprintf("[SubsPlease] Show - S01E%02d (1080p %s).mkv", e, codec), Length: newBytes}},
	}
}

// pack is one SubsPlease torrent holding S01E<first>..S01E<last>.
func pack(first, last, n int) seadex.Torrent {
	t := seadex.Torrent{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: fmt.Sprintf("https://nyaa.si/view/p%d", n), InfoHash: hash(n)}
	for e := first; e <= last; e++ {
		t.Files = append(t.Files, seadex.File{Name: fmt.Sprintf("Show S01 1080p/[SubsPlease] Show - S01E%02d (1080p x264).mkv", e), Length: newBytes})
	}
	return t
}

// absolutePack is one SubsPlease torrent naming absolute episodes first..last.
func absolutePack(first, last, n int) seadex.Torrent {
	t := seadex.Torrent{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: fmt.Sprintf("https://nyaa.si/view/a%d", n), InfoHash: hash(n)}
	for e := first; e <= last; e++ {
		t.Files = append(t.Files, seadex.File{Name: fmt.Sprintf("Show 1080p/[SubsPlease] Show - %02d (1080p x264).mkv", e), Length: newBytes})
	}
	return t
}

// seasonTwoSeries holds only season 2: relative episodes firstRel.. firstRel+11
// numbered absolute firstAbs.., file 200+i holding the i-th of them.
func seasonTwoSeries(firstRel, firstAbs int) *library.Item {
	item := &library.Item{
		Title: "Show", Arr: library.ArrSonarr, ArrID: 7, HasFile: true,
		Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{2: {"erai-raws"}},
		SeasonEpisodes: map[int]map[int]library.Episode{2: {}},
		FileBytes:      map[int]int64{},
	}
	for i := 1; i <= 12; i++ {
		item.SeasonEpisodes[2][firstRel+i-1] = library.Episode{File: 200 + i, Absolute: firstAbs + i - 1}
		item.FileBytes[200+i] = heldBytes
	}
	return item
}

func singles(first, last int) []seadex.Torrent {
	var out []seadex.Torrent
	for e := first; e <= last; e++ {
		out = append(out, single(e, "x264"))
	}
	return out
}

func sizeOne(t *testing.T, item *library.Item, rec mapping.Record, torrents []seadex.Torrent, siblings ...int) Finding {
	t.Helper()
	m := match.Match{Item: item, Arr: item.Arr, Entry: seadex.Entry{AniListID: 1, Torrents: torrents}, Record: rec, SiblingSeasons: siblings}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("Compare = %d findings, want 1: %+v", len(got), got)
	}
	return got[0]
}

func replacedKeys(f *Finding) []string {
	var keys []string
	for _, r := range f.Replaced {
		keys = append(keys, r.Key)
	}
	return keys
}

func fileKeys(item *library.Item, ids ...int) []string {
	var keys []string
	for _, id := range ids {
		keys = append(keys, item.FileKey(id))
	}
	slices.Sort(keys)
	return keys
}

func ids(first, last int) []int {
	var out []int
	for e := first; e <= last; e++ {
		out = append(out, 100+e)
	}
	return out
}

// TestSizeReplacesOnlyTheFilesTheDownloadCovers pins per-file replacement: the
// net library size change subtracts exactly the held files the selected
// download set replaces whole, never the whole season.
func TestSizeReplacesOnlyTheFilesTheDownloadCovers(t *testing.T) {
	season1 := mapping.Record{SeasonTvdb: 1}
	withOdd := singles(1, 10)
	withOdd[4] = single(5, "x265")
	multi := sizedSeries()
	multi.SeasonEpisodes[1][4] = library.Episode{File: 103, Absolute: 4}
	delete(multi.FileBytes, 104)
	tests := []struct {
		item         *library.Item
		name         string
		torrents     []seadex.Torrent
		wantFiles    []int
		wantReleases int64
	}{
		{name: "singles for episodes 1-10 of a 12-file season", item: sizedSeries(), torrents: singles(1, 10), wantFiles: ids(1, 10), wantReleases: 10},
		{name: "an episode filtered out of the family by codec", item: sizedSeries(), torrents: withOdd, wantFiles: slices.Delete(ids(1, 10), 4, 5), wantReleases: 9},
		{name: "a partial pack of episodes 1-6", item: sizedSeries(), torrents: []seadex.Torrent{pack(1, 6, 1)}, wantFiles: ids(1, 6), wantReleases: 6},
		{name: "a complete pack", item: sizedSeries(), torrents: []seadex.Torrent{pack(1, 12, 1)}, wantFiles: ids(1, 12), wantReleases: 12},
		{name: "a two-episode file with one episode covered", item: multi, torrents: singles(1, 3), wantFiles: []int{101, 102}, wantReleases: 3},
		{name: "a two-episode file with both covered", item: multi, torrents: singles(1, 4), wantFiles: []int{101, 102, 103}, wantReleases: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := sizeOne(t, tc.item, season1, tc.torrents)
			if want := tc.wantReleases * newBytes; f.ReleaseBytes != want {
				t.Errorf("ReleaseBytes = %d, want %d", f.ReleaseBytes, want)
			}
			if got, want := replacedKeys(&f), fileKeys(tc.item, tc.wantFiles...); !slices.Equal(got, want) {
				t.Errorf("Replaced = %v, want %v", got, want)
			}
			if want := int64(len(tc.wantFiles)) * heldBytes; f.CurrentBytes != want {
				t.Errorf("CurrentBytes = %d, want %d", f.CurrentBytes, want)
			}
		})
	}
}

// TestSizeLeavesTheCurrentSideUnknown pins every case where the replaced bytes
// cannot be known, while the download side still is.
func TestSizeLeavesTheCurrentSideUnknown(t *testing.T) {
	noFile := sizedSeries()
	delete(noFile.SeasonEpisodes[1], 7)
	zeroSize := sizedSeries()
	zeroSize.FileBytes[103] = 0
	negative := sizedSeries()
	negative.FileBytes[103] = -5
	overflow := sizedSeries()
	overflow.FileBytes[101], overflow.FileBytes[102] = math.MaxInt64, 1
	unread := sizedSeries()
	unread.SeasonEpisodes = nil
	relative := sizedSeries()
	relative.SeasonEpisodes[2] = map[int]library.Episode{1: {File: 201, Absolute: 13}}
	relative.FileBytes[201] = heldBytes
	twoSeasons := sizedSeries()
	twoSeasons.SeasonEpisodes[2] = map[int]library.Episode{}
	for e := 1; e <= 6; e++ {
		twoSeasons.SeasonEpisodes[2][e] = library.Episode{File: 200 + e, Absolute: 12 + e}
		twoSeasons.FileBytes[200+e] = heldBytes
	}
	twoAbsolute := seasonTwoSeries(1, 13)
	twoAbsolute.SeasonEpisodes[2][13] = library.Episode{File: 213, Absolute: 13}
	twoAbsolute.FileBytes[213] = heldBytes
	absolute := func(e int) seadex.Torrent {
		tr := single(e, "x264")
		tr.Files[0].Name = fmt.Sprintf("[SubsPlease] Show - %02d (1080p x264).mkv", e)
		return tr
	}
	tests := []struct {
		item     *library.Item
		name     string
		torrents []seadex.Torrent
		siblings []int
		record   mapping.Record
	}{
		{name: "a covered episode with no file", item: noFile, torrents: singles(1, 10), record: mapping.Record{SeasonTvdb: 1}},
		{name: "a replaced file of size 0", item: zeroSize, torrents: singles(1, 10), record: mapping.Record{SeasonTvdb: 1}},
		{name: "a replaced file of negative size", item: negative, torrents: singles(1, 10), record: mapping.Record{SeasonTvdb: 1}},
		{name: "replaced files summing past MaxInt64", item: overflow, torrents: singles(1, 2), record: mapping.Record{SeasonTvdb: 1}},
		{name: "an episode list the walk could not read", item: unread, torrents: singles(1, 10), record: mapping.Record{SeasonTvdb: 1}},
		{name: "a season a sibling entry also maps", item: sizedSeries(), torrents: singles(1, 10), record: mapping.Record{SeasonTvdb: 1}, siblings: []int{1}},
		{name: "a whole-series scope", item: sizedSeries(), torrents: singles(1, 10), record: mapping.Record{SeasonKind: mapping.SeasonAbsent}},
		{name: "a token naming another season", item: twoSeasons, torrents: []seadex.Torrent{pack(1, 6, 1)}, record: mapping.Record{SeasonTvdb: 2}},
		{name: "an absolute number another episode holds", item: relative, torrents: []seadex.Torrent{absolute(1)}, record: mapping.Record{SeasonTvdb: 2}},
		{name: "absolute numbers no episode carries, equal to this season's relative ones", item: seasonTwoSeries(13, 25), torrents: []seadex.Torrent{absolutePack(13, 24, 1)}, record: mapping.Record{SeasonTvdb: 2}},
		{name: "an absolute number two episodes carry", item: twoAbsolute, torrents: []seadex.Torrent{absolutePack(13, 24, 1)}, record: mapping.Record{SeasonTvdb: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.item.SeasonGroups[2] = []string{"erai-raws"}
			f := sizeOne(t, tc.item, tc.record, tc.torrents, tc.siblings...)
			if f.ReleaseBytes <= 0 {
				t.Errorf("ReleaseBytes = %d, want the download side known", f.ReleaseBytes)
			}
			if f.CurrentBytes != 0 || f.Replaced != nil {
				t.Errorf("CurrentBytes, Replaced = %d, %v, want unknown (0, nil)", f.CurrentBytes, f.Replaced)
			}
		})
	}
	t.Run("an absolute number that only one episode can mean", func(t *testing.T) {
		f := sizeOne(t, sizedSeries(), mapping.Record{SeasonTvdb: 1}, []seadex.Torrent{absolute(1), absolute(2)})
		if got, want := replacedKeys(&f), fileKeys(sizedSeries(), 101, 102); !slices.Equal(got, want) {
			t.Errorf("Replaced = %v, want %v", got, want)
		}
	})
	t.Run("absolute numbers that continue from an earlier season", func(t *testing.T) {
		item := seasonTwoSeries(1, 13)
		f := sizeOne(t, item, mapping.Record{SeasonTvdb: 2}, []seadex.Torrent{absolutePack(13, 24, 1)})
		want := make([]int, 0, 12)
		for i := 1; i <= 12; i++ {
			want = append(want, 200+i)
		}
		if got, want := replacedKeys(&f), fileKeys(item, want...); !slices.Equal(got, want) {
			t.Errorf("Replaced = %v, want %v", got, want)
		}
		if f.CurrentBytes != 12*heldBytes {
			t.Errorf("CurrentBytes = %d, want %d", f.CurrentBytes, 12*heldBytes)
		}
	})
}

func TestSizeDownloadSet(t *testing.T) {
	v2 := single(3, "x264")
	v2.InfoHash = hash(99)
	v2.Files[0].Name = "[SubsPlease] Show - S01E03v2 (1080p x264).mkv"
	negative := single(2, "x264")
	negative.Files[0].Length = -1
	tokenless := single(4, "x264")
	tokenless.Files[0].Name = "[SubsPlease] Show (1080p x264).mkv"
	sharedName := single(4, "x264")
	sharedName.InfoHash, sharedName.URL = hash(98), "https://nyaa.si/view/98"
	withExtras := pack(1, 12, 1)
	withExtras.Files = append(withExtras.Files, seadex.File{Name: "Show S01 1080p/Extras/NCOP (1080p).mkv", Length: 100})
	altPack := pack(1, 12, 2)
	altPack.ReleaseGroup = "Zero"
	altPack.Files = altPack.Files[:1]
	onAB := single(1, "x264")
	onAB.Tracker, onAB.URL, onAB.InfoHash = "AB", "/torrents.php?id=1&torrentid=1", ""
	lateHashPack := pack(5, 6, 1)
	lateHashPack.InfoHash = hash(999)
	many := singles(1, 65)
	sameHash := single(2, "x264")
	sameHash.InfoHash = hash(501)
	sameHashLarger := single(2, "x264")
	sameHashLarger.InfoHash, sameHashLarger.Files[0].Length = hash(501), 3*gib
	sameSource := singles(1, 2)
	for i := range sameSource {
		sameSource[i].InfoHash, sameSource[i].URL = "", "https://nyaa.si/view/1"
	}
	manyItem := sizedSeries()
	for e := 13; e <= 65; e++ {
		manyItem.SeasonEpisodes[1][e] = library.Episode{File: 100 + e}
		manyItem.FileBytes[100+e] = heldBytes
	}
	tests := []struct {
		item     *library.Item
		name     string
		torrents []seadex.Torrent
		want     int64
	}{
		{name: "a pack headline counts alone", torrents: []seadex.Torrent{pack(1, 12, 1), single(1, "x264")}, want: 12 * newBytes},
		{name: "a pack beside an alternative pack counts one", torrents: []seadex.Torrent{pack(1, 12, 1), altPack}, want: 12 * newBytes},
		{name: "every file of a pack counts, extras included", torrents: []seadex.Torrent{withExtras}, want: 12*newBytes + 100},
		{name: "singles for episodes 1-12 sum once each", torrents: singles(1, 12), want: 12 * newBytes},
		{name: "the same release on another tracker is not counted", torrents: append(singles(1, 2), onAB), want: 2 * newBytes},
		{name: "two candidates for one episode", torrents: append(singles(1, 4), v2), want: 0},
		{name: "a pack among the family's singles", torrents: append(singles(1, 4), lateHashPack), want: 0},
		{name: "a single with no readable token", torrents: append(singles(1, 3), tokenless), want: 0},
		{name: "two torrents sharing a file name", torrents: append(singles(1, 4), sharedName), want: 0},
		{name: "a torrent with a negative length", torrents: []seadex.Torrent{single(1, "x264"), negative}, want: 0},
		{name: "two episodes under one info hash", torrents: []seadex.Torrent{single(1, "x264"), sameHash}, want: 0},
		{name: "two episodes under one info hash at different sizes", torrents: []seadex.Torrent{single(1, "x264"), sameHashLarger}, want: 0},
		{name: "two episodes under one tracker and URL", torrents: sameSource, want: 0},
		{name: "more than 64 downloads", item: manyItem, torrents: many, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := tc.item
			if item == nil {
				item = sizedSeries()
			}
			f := sizeOne(t, item, mapping.Record{SeasonTvdb: 1}, tc.torrents)
			if f.ReleaseBytes != tc.want {
				t.Errorf("ReleaseBytes = %d, want %d", f.ReleaseBytes, tc.want)
			}
			if tc.want == 0 && (f.Downloads != nil || f.Replaced != nil || f.CurrentBytes != 0) {
				t.Errorf("an unknown download set must leave every size unknown, got Downloads %v Replaced %v CurrentBytes %d", f.Downloads, f.Replaced, f.CurrentBytes)
			}
			var sum int64
			for _, d := range f.Downloads {
				sum += d.Bytes
			}
			if sum != f.ReleaseBytes {
				t.Errorf("Downloads sum to %d, ReleaseBytes is %d", sum, f.ReleaseBytes)
			}
		})
	}
}

func TestSizeMovieReplacesItsOneFile(t *testing.T) {
	item := &library.Item{
		Title: "Film", Arr: library.ArrRadarr, ArrID: 3, HasFile: true,
		Groups: []string{"erai-raws"}, FileBytes: map[int]int64{9: 30 * gib},
	}
	movie := seadex.Torrent{
		IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/9", InfoHash: hash(9),
		Files: []seadex.File{{Name: "[SubsPlease] Film (1080p).mkv", Length: 40 * gib}},
	}
	f := sizeOne(t, item, mapping.Record{Type: "MOVIE"}, []seadex.Torrent{movie})
	if f.ReleaseBytes != 40*gib || f.CurrentBytes != 30*gib {
		t.Errorf("ReleaseBytes, CurrentBytes = %d, %d, want %d, %d", f.ReleaseBytes, f.CurrentBytes, 40*gib, 30*gib)
	}
	if got, want := replacedKeys(&f), []string{item.FileKey(9)}; !slices.Equal(got, want) {
		t.Errorf("Replaced = %v, want %v", got, want)
	}
}

// TestSizeOnlyUpgradesCarrySizes pins that a manual-review finding and an
// incomplete entry carry no size, while a newer revision of a held group does,
// sized from that group's own releases.
func TestSizeOnlyUpgradesCarrySizes(t *testing.T) {
	mixed := sizedSeries()
	mixed.SeasonGroups[1] = []string{"erai-raws", "commie"}
	f := sizeOne(t, mixed, mapping.Record{SeasonTvdb: 1}, singles(1, 2))
	if f.Status != StatusMixedGroup || f.ReleaseBytes != 0 || f.Downloads != nil {
		t.Errorf("mixed-group finding: status %q, ReleaseBytes %d, Downloads %v, want %q and no size", f.Status, f.ReleaseBytes, f.Downloads, StatusMixedGroup)
	}
	m := match.Match{Item: sizedSeries(), Arr: library.ArrSonarr, Entry: seadex.Entry{AniListID: 1, Incomplete: true, Torrents: singles(1, 2)}, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 || got[0].Status != StatusIncomplete || got[0].ReleaseBytes != 0 || got[0].Downloads != nil {
		t.Errorf("incomplete entry: %+v, want one incomplete finding with no size", got)
	}

	held := sizedSeries()
	held.Groups, held.SeasonGroups[1] = []string{"subsplease"}, []string{"subsplease"}
	held.SeasonRevisions = map[int]map[string]release.Revision{1: {"subsplease": release.ArrRevision(1, false)}}
	revised := []seadex.Torrent{pack(1, 12, 1)}
	for i := range revised[0].Files {
		revised[0].Files[i].Name = fmt.Sprintf("Show S01 1080p/[SubsPlease] Show - S01E%02dv2 (1080p x264).mkv", i+1)
	}
	other := pack(1, 3, 2)
	other.ReleaseGroup = "Alpha"
	f = sizeOne(t, held, mapping.Record{SeasonTvdb: 1}, append(revised, other))
	if f.Status != StatusNewerRevision {
		t.Fatalf("status = %q, want %q", f.Status, StatusNewerRevision)
	}
	if f.ReleaseBytes != 12*newBytes || f.CurrentBytes != 12*heldBytes {
		t.Errorf("newer revision: ReleaseBytes, CurrentBytes = %d, %d, want %d, %d", f.ReleaseBytes, f.CurrentBytes, 12*newBytes, 12*heldBytes)
	}
}

// TestSizeOfANewerRevisionIsTheNewerRelease pins that a newer-revision finding
// recommends and sizes the release at the listed revision, not an older one of
// the same group SeaDex still lists, and that it carries no size when no
// obtainable release is at that revision.
func TestSizeOfANewerRevisionIsTheNewerRelease(t *testing.T) {
	film := &library.Item{
		Title: "Show OVA", Arr: library.ArrRadarr, ArrID: 3, HasFile: true,
		Groups: []string{"mtbb"}, Revisions: map[string]release.Revision{"mtbb": release.ArrRevision(1, false)},
		FileBytes: map[int]int64{9: heldBytes},
	}
	original := seadex.Torrent{
		IsBest: true, ReleaseGroup: "MTBB", Tracker: "Nyaa", URL: "https://nyaa.si/view/1", InfoHash: hash(1),
		Files: []seadex.File{{Name: "[MTBB] Show OVA (1080p).mkv", Length: newBytes}},
	}
	reissue := seadex.Torrent{
		IsBest: true, ReleaseGroup: "MTBB", Tracker: "Nyaa", URL: "https://nyaa.si/view/2", InfoHash: hash(2),
		Files: []seadex.File{{Name: "[MTBB] Show OVA [v2] (1080p).mkv", Length: newBytes + gib}},
	}
	rec := mapping.Record{Type: "MOVIE"}

	f := sizeOne(t, film, rec, []seadex.Torrent{original, reissue})
	if f.Status != StatusNewerRevision {
		t.Fatalf("Compare(held v1, listed v1 and v2) status = %q, want %q", f.Status, StatusNewerRevision)
	}
	if f.ReleaseURL != reissue.URL || len(f.Links) != 1 || f.Links[0].URL != reissue.URL {
		t.Errorf("Compare(held v1, listed v1 and v2) recommends %q with links %+v, want only the v2 release %q", f.ReleaseURL, f.Links, reissue.URL)
	}
	if f.ReleaseBytes != newBytes+gib || f.CurrentBytes != heldBytes {
		t.Errorf("Compare(held v1, listed v1 and v2) ReleaseBytes, CurrentBytes = %d, %d, want %d, %d", f.ReleaseBytes, f.CurrentBytes, newBytes+gib, heldBytes)
	}

	hidden := reissue
	hidden.Tracker, hidden.URL, hidden.InfoHash = "AB", "/torrents.php?id=1&torrentid=2", ""
	f = sizeOne(t, film, rec, []seadex.Torrent{original, hidden})
	if f.Status != StatusNewerRevision {
		t.Fatalf("Compare(v2 only on AnimeBytes, toggle off) status = %q, want %q", f.Status, StatusNewerRevision)
	}
	if f.ReleaseBytes != 0 || f.Downloads != nil || f.CurrentBytes != 0 || f.Replaced != nil {
		t.Errorf("Compare(v2 only on AnimeBytes, toggle off) sizes = ReleaseBytes %d Downloads %v CurrentBytes %d Replaced %v, want none",
			f.ReleaseBytes, f.Downloads, f.CurrentBytes, f.Replaced)
	}
}

// TestSizeOfAFilmOrSpecialInSeasonZero pins the season-0 paths: a film or
// special the map places on Sonarr's specials knows its download but not the
// files it replaces, its Radarr copy replaces the film's one file, and one the
// map does not place yields no upgrade at all.
func TestSizeOfAFilmOrSpecialInSeasonZero(t *testing.T) {
	special := seadex.Torrent{
		IsBest: true, ReleaseGroup: "MTBB", Tracker: "Nyaa", URL: "https://nyaa.si/view/960", InfoHash: hash(960),
		Files: []seadex.File{
			{Name: "[MTBB] Show - S00E09 (1080p).mkv", Length: newBytes},
			{Name: "[MTBB] Show - S00E10 (1080p).mkv", Length: newBytes},
		},
	}
	rec := mapping.Record{Type: "OVA", TvdbID: 1, SeasonKind: mapping.SeasonPresent}
	series := &library.Item{
		Title: "Show", Arr: library.ArrSonarr, ArrID: 7, HasFile: true,
		Groups: []string{"oz"}, SeasonGroups: map[int][]string{0: {"oz"}},
		Specials:       map[int]library.SpecialEpisode{9: {Group: "oz", HasFile: true}, 10: {Group: "oz", HasFile: true}},
		SeasonEpisodes: map[int]map[int]library.Episode{0: {9: {File: 300}, 10: {File: 300}}},
		FileBytes:      map[int]int64{300: heldBytes},
	}
	film := &library.Item{
		Title: "Show OVA", Arr: library.ArrRadarr, ArrID: 3, HasFile: true,
		Groups: []string{"oz"}, FileBytes: map[int]int64{9: heldBytes},
	}
	compareAll := func(it *library.Item, specials []int) []Finding {
		m := match.Match{Item: it, Arr: it.Arr, Entry: seadex.Entry{AniListID: 960, Torrents: []seadex.Torrent{special}}, Record: rec, Specials: specials}
		return comparer(filter.Options{}, false).Compare([]match.Match{m})
	}
	compareOne := func(it *library.Item, specials []int) Finding {
		t.Helper()
		got := compareAll(it, specials)
		if len(got) != 1 {
			t.Fatalf("Compare(%s copy, specials %v) = %d findings, want 1: %+v", it.Arr, specials, len(got), got)
		}
		return got[0]
	}

	f := compareOne(series, []int{9, 10})
	if f.Status != StatusBetter || f.Scope != "episodes" {
		t.Fatalf("placed special: status %q scope %q, want %q on episodes", f.Status, f.Scope, StatusBetter)
	}
	if f.ReleaseBytes != 2*newBytes || f.CurrentBytes != 0 || f.Replaced != nil {
		t.Errorf("placed special: ReleaseBytes, CurrentBytes, Replaced = %d, %d, %v, want %d, 0, nil (replaced side unknown)",
			f.ReleaseBytes, f.CurrentBytes, f.Replaced, 2*newBytes)
	}

	f = compareOne(film, []int{9, 10})
	if f.Status != StatusBetter || f.ReleaseBytes != 2*newBytes || f.CurrentBytes != heldBytes {
		t.Errorf("Radarr copy: status %q, ReleaseBytes, CurrentBytes = %d, %d, want %q, %d, %d",
			f.Status, f.ReleaseBytes, f.CurrentBytes, StatusBetter, 2*newBytes, heldBytes)
	}
	if got, want := replacedKeys(&f), []string{film.FileKey(9)}; !slices.Equal(got, want) {
		t.Errorf("Radarr copy: Replaced = %v, want %v", got, want)
	}

	for _, f := range compareAll(series, nil) {
		if f.Status == StatusBetter || f.Status == StatusNewerRevision || f.ReleaseBytes != 0 || f.Downloads != nil {
			t.Errorf("unplaced special: status %q, ReleaseBytes %d, Downloads %v, want no upgrade and no size", f.Status, f.ReleaseBytes, f.Downloads)
		}
	}
}
