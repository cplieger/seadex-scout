package scout

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// liveFragmentIndex loads testdata/route_by_mapping.animap.json, entries of the
// live v2026.10.21 animap release with their published type kept, through the
// mapping loader as the scout does.
func liveFragmentIndex(t *testing.T) *mapping.Index {
	t.Helper()
	body, err := os.ReadFile("testdata/route_by_mapping.animap.json")
	if err != nil {
		t.Fatal(err)
	}
	const tag = "v2026.10.21"
	sum := sha256.Sum256(body)
	prefix := "/" + mapping.DefaultRepo + "/releases/"
	var ts *httptest.Server
	ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case prefix + "latest":
			w.Header().Set("Location", ts.URL+prefix+"tag/"+tag)
			w.WriteHeader(http.StatusFound)
		case prefix + "download/" + tag + "/animap.json.sha256":
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  animap.json\n"))
		case prefix + "download/" + tag + "/animap.json":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	src := mapread.Source{Repo: mapping.DefaultRepo, Base: ts.URL}
	_, idx, err := mapping.NewLoader(ts.Client(), src, mapping.WithLogger(slog.New(slog.DiscardHandler))).Load(t.Context(), nil)
	if err != nil {
		t.Fatalf("load the live fragment: %v", err)
	}
	return idx
}

// TestLiveFragmentProjectsTheLiveRecords guards the fixture: each entry projects
// to the record and placement the whole v2026.10.21 file gives it.
func TestLiveFragmentProjectsTheLiveRecords(t *testing.T) {
	idx := liveFragmentIndex(t)
	for _, tc := range []struct {
		specials       []int
		episodes       []int
		tmdb           []int
		kind           mapping.SeasonKind
		al             int
		tvdb           int
		season         int
		specialEpisode int
		ranges         int
	}{
		{al: 416, kind: mapping.SeasonAbsent, tmdb: []int{11621}},
		{al: 743, kind: mapping.SeasonAbsent, tmdb: []int{21057}},
		{al: 170942, tvdb: 429934, kind: mapping.SeasonPresent, season: 1, ranges: 1},
		{al: 9135, tvdb: 85249, kind: mapping.SeasonPresent, tmdb: []int{80518}, specials: []int{21}},
		{al: 399, tvdb: 72025, kind: mapping.SeasonPresent, tmdb: []int{89496}, specials: []int{3}},
		{al: 100049, tvdb: 305089, kind: mapping.SeasonPresent, tmdb: []int{532321, 566451}, specials: []int{26, 28}},
		{al: 502, tvdb: 76666, kind: mapping.SeasonPresent, tmdb: []int{39144}},
		{al: 1089, tvdb: 73038, kind: mapping.SeasonPresent, tmdb: []int{14132}, specialEpisode: 1},
		{al: 187, tvdb: 70900, kind: mapping.SeasonPresent, season: 3, tmdb: []int{80741}, episodes: []int{1}, ranges: 1},
		{al: 619, tvdb: 398170, kind: mapping.SeasonPresent, season: 1, tmdb: []int{146006}, ranges: 1},
		{al: 178788, tvdb: 348545, kind: mapping.SeasonAbsent, tmdb: []int{1311031}},
		{al: 43, tvdb: 73749, kind: mapping.SeasonPresent, specials: []int{1}},
		{al: 216, kind: mapping.SeasonAbsent},
		{al: 310, kind: mapping.SeasonAbsent},
		{al: 131942, tvdb: 262954, kind: mapping.SeasonPresent, season: 5, ranges: 1},
		{al: 107351, tvdb: 278157, kind: mapping.SeasonPresent, specials: []int{5}},
	} {
		rec, ok := idx.Lookup(tc.al)
		if !ok {
			t.Errorf("Lookup(%d): no record", tc.al)
			continue
		}
		m, _ := idx.MappingFor(&rec)
		if rec.TvdbID != tc.tvdb || rec.SeasonKind != tc.kind || rec.SeasonTvdb != tc.season || !slices.Equal(rec.TmdbMovies, tc.tmdb) ||
			!slices.Equal(m.Specials, tc.specials) || m.SpecialEpisode != tc.specialEpisode || !slices.Equal(m.Episodes, tc.episodes) || len(m.Seasons) != tc.ranges {
			t.Errorf("AniList %d projects to tvdb %d kind %q season %d tmdb %v specials %v special_episode %d episodes %v ranges %d, want %+v",
				tc.al, rec.TvdbID, rec.SeasonKind, rec.SeasonTvdb, rec.TmdbMovies, m.Specials, m.SpecialEpisode, m.Episodes, len(m.Seasons), tc)
		}
	}
}

// libItem is one library item of a routing case.
type libItem struct {
	library.Item
}

type matchView struct {
	arr, source, uncompared string
	inLibrary               bool
}

type rowView struct {
	arr, verdict, scope string
	episodes            []int
	season              int
	special             bool
}

func (r rowView) equal(o rowView) bool {
	return r.arr == o.arr && r.verdict == o.verdict && r.scope == o.scope && r.season == o.season &&
		r.special == o.special && slices.Equal(r.episodes, o.episodes)
}

type feedView struct {
	title, seriesTitle string
	twinEpisodes       []int
	year               int
	season             int
	tvdb               int
	twinSeason         int
	twinEntry          int
	seasons            int
	kind               indexer.MediaKind
	seasonKnown        bool
}

func (f feedView) equal(o feedView) bool {
	return f.title == o.title && f.seriesTitle == o.seriesTitle && slices.Equal(f.twinEpisodes, o.twinEpisodes) &&
		f.year == o.year && f.season == o.season && f.tvdb == o.tvdb && f.twinSeason == o.twinSeason &&
		f.twinEntry == o.twinEntry && f.seasons == o.seasons && f.kind == o.kind && f.seasonKnown == o.seasonKnown
}

// surfaces is what one entry yields on every surface.
type surfaces struct {
	matches     []matchView
	rows        []rowView
	findings    []string
	notOnSeaDex []string
	feed        feedView
}

// refusingAniList fails every lookup: a routing case whose entry needs AniList
// is answered from the memo.
type refusingAniList struct{ calls int }

func (r *refusingAniList) Fetch(context.Context, int) (anilist.Media, error) {
	r.calls++
	return anilist.Media{}, errors.New("unexpected AniList fetch")
}

func (r *refusingAniList) FetchMany(context.Context, []int) (anilist.BatchResult, error) {
	r.calls++
	return anilist.BatchResult{}, errors.New("unexpected AniList batch")
}

// runSurfaces runs one entry through the matcher, the comparer, the auditor and
// the feed's entry metadata, with the matcher, comparer and auditor sharing one
// specials policy. It is the one place the test touches those APIs.
func runSurfaces(t *testing.T, idx *mapping.Index, alID int, lib []libItem, memo match.Memo, excludeSpecials bool) surfaces {
	t.Helper()
	snap := &library.Snapshot{}
	for i := range lib {
		snap.Items = append(snap.Items, lib[i].Item)
	}
	entry := seadex.Entry{AniListID: alID, Torrents: []seadex.Torrent{{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/1", ReleaseGroup: "GrpA", IsBest: true,
		Files: []seadex.File{{Length: 100, Name: "Entry (1080p) [GrpA].mkv"}},
	}}}
	client := &refusingAniList{}
	res := match.New(match.Config{AniList: client, Logger: slog.New(slog.DiscardHandler), ExcludeSpecials: excludeSpecials}).Match(t.Context(), []seadex.Entry{entry}, snap, idx, memo)
	if res.Degraded || client.calls > 0 {
		t.Fatalf("Match(%d) degraded %v after %d AniList calls, want the memo to answer", alID, res.Degraded, client.calls)
	}
	var out surfaces
	for i := range res.Matches {
		m := &res.Matches[i]
		v := matchView{arr: m.Arr, source: string(m.Source), inLibrary: m.InLibrary()}
		if m.Uncompared != nil {
			v.uncompared = m.Uncompared.Arr
		}
		out.matches = append(out.matches, v)
	}
	for _, f := range compare.New(compare.Config{Logger: slog.New(slog.DiscardHandler)}).Compare(res.Matches) {
		out.findings = append(out.findings, f.Arr+" "+string(f.Status))
	}
	report := audit.New(audit.Config{ExcludeSpecials: excludeSpecials}).Audit(res.Matches, snap, idx, nil)
	for i := range report.Rows {
		r := &report.Rows[i]
		if r.Verdict == audit.VerdictNotOnSeaDex {
			out.notOnSeaDex = append(out.notOnSeaDex, r.Arr+" "+r.Title)
			continue
		}
		out.rows = append(out.rows, rowView{arr: r.Arr, verdict: string(r.Verdict), scope: r.Scope.String(), season: r.Season, episodes: r.Episodes, special: r.Special})
	}
	info := entryInfo(idx, snap, map[int]bool{}, memo)(alID)
	out.feed = feedView{
		title: info.Title, seriesTitle: info.SeriesTitle, twinEpisodes: info.TwinEpisodes, twinEntry: info.TwinEntryEpisodes, twinSeason: info.TwinSeason,
		year: info.Year, season: info.Season, seasonKnown: info.SeasonKnown, tvdb: info.TvdbID, seasons: len(info.Seasons), kind: info.Kind,
	}
	slices.SortFunc(out.matches, func(a, b matchView) int { return cmp.Compare(a.arr, b.arr) })
	slices.SortFunc(out.rows, func(a, b rowView) int { return cmp.Or(cmp.Compare(a.arr, b.arr), cmp.Compare(a.verdict, b.verdict)) })
	slices.Sort(out.findings)
	slices.Sort(out.notOnSeaDex)
	return out
}

// Library items of the routing cases. Titles are the arrs' own; every file is
// grpb, so a judged copy is behind SeaDex's best, GrpA.
func radarrMovie(arrID, tmdb int, title string, year int, hasFile bool) library.Item {
	it := library.Item{Arr: library.ArrRadarr, ArrID: arrID, TmdbID: tmdb, Title: title, Year: year, HasFile: hasFile}
	if hasFile {
		it.Groups = []string{"grpb"}
	}
	return it
}

func sonarrSpecials(arrID, tvdb int, title string, specials map[int]bool) library.Item {
	it := library.Item{Arr: library.ArrSonarr, ArrID: arrID, TvdbID: tvdb, Title: title, Specials: map[int]library.ListedEpisode{}}
	for ep, held := range specials {
		if held {
			it.Specials[ep] = library.ListedEpisode{Group: "grpb", HasFile: true}
			it.SeasonGroups = map[int][]string{0: {"grpb"}}
		} else {
			it.Specials[ep] = library.ListedEpisode{}
		}
	}
	return it
}

func sonarrSeason(arrID, tvdb int, title string, season int) library.Item {
	return library.Item{Arr: library.ArrSonarr, ArrID: arrID, TvdbID: tvdb, Title: title, SeasonGroups: map[int][]string{season: {"grpb"}}}
}

const (
	behind      = "have_unlisted"
	betterFound = " better_release"
)

const (
	fmaFilm     = "Fullmetal Alchemist: The Sacred Star of Milos"
	fmaSeries   = "Fullmetal Alchemist: Brotherhood"
	gitsFilm    = "Ghost in the Shell"
	gitsSeries  = "Ghost in the Shell (TVDB)"
	initialD    = "Initial D"
	infinity    = "Demon Slayer: Kimetsu no Yaiba Infinity Castle"
	demonSlayer = "Demon Slayer: Kimetsu no Yaiba"
	piaCarrot   = "Welcome to Pia Carrot!! Sayaka's Love Story"
)

type routeCase struct {
	memo match.Memo
	name string
	lib  []libItem
	on   surfaces
	off  surfaces
	alID int
}

func memoOf(alID int, format, title string, year int) match.Memo {
	return match.Memo{Entries: map[int]match.MemoEntry{alID: {Expiry: time.Now().Add(240 * time.Hour), Format: format, Titles: []string{title}, Year: year}}}
}

func routeCases() []routeCase {
	m := func(arr, source, uncompared string) matchView {
		return matchView{arr: arr, source: source, uncompared: uncompared, inLibrary: source != "unmapped"}
	}
	movieRow := func(verdict string) rowView {
		return rowView{arr: library.ArrRadarr, verdict: verdict, scope: "movie"}
	}
	s0 := func(verdict string, special bool, eps ...int) rowView {
		return rowView{arr: library.ArrSonarr, verdict: verdict, scope: "episodes", episodes: eps, special: special}
	}
	radarr, sonarr := library.ArrRadarr+betterFound, library.ArrSonarr+betterFound

	fmaLib := func(filmFile bool) []libItem {
		return []libItem{
			{Item: radarrMovie(4, 80518, fmaFilm, 2011, filmFile)},
			{Item: sonarrSpecials(5, 85249, fmaSeries, map[int]bool{21: true})},
		}
	}
	fmaFeed := feedView{kind: indexer.KindMovie, title: fmaFilm, year: 2011, tvdb: 85249, twinEpisodes: []int{21}, twinEntry: 3, seriesTitle: fmaSeries}
	porco := surfaces{
		matches:  []matchView{m(library.ArrRadarr, "id", "")},
		rows:     []rowView{movieRow(behind)},
		findings: []string{radarr},
		feed:     feedView{kind: indexer.KindMovie, title: "Porco Rosso", year: 1992},
	}
	ocean := surfaces{
		matches:  []matchView{m(library.ArrRadarr, "id", "")},
		rows:     []rowView{movieRow(behind)},
		findings: []string{radarr},
		feed:     feedView{kind: indexer.KindMovie, title: "Ocean Waves", year: 1993},
	}
	blueBox := surfaces{
		matches:  []matchView{m(library.ArrSonarr, "id", "")},
		rows:     []rowView{{arr: library.ArrSonarr, verdict: behind, scope: "season", season: 1}},
		findings: []string{sonarr},
		feed:     feedView{kind: indexer.KindSeries, title: "Blue Box", year: 2024, season: 1, seasonKnown: true, tvdb: 429934, seasons: 1},
	}
	stoneOcean := surfaces{
		matches:  []matchView{m(library.ArrSonarr, "id", "")},
		rows:     []rowView{{arr: library.ArrSonarr, verdict: behind, scope: "season", season: 5}},
		findings: []string{sonarr},
		feed:     feedView{kind: indexer.KindSeries, title: "JoJo's Bizarre Adventure", season: 5, seasonKnown: true, tvdb: 262954, seasons: 1},
	}
	pia := surfaces{
		matches:  []matchView{m(library.ArrRadarr, "title", "")},
		rows:     []rowView{movieRow(behind)},
		findings: []string{radarr},
		feed:     feedView{kind: indexer.KindMovie, title: piaCarrot, year: 2002},
	}
	ghosts := surfaces{
		matches: []matchView{m(library.ArrSonarr, "unmapped", "")},
		feed:    feedView{kind: indexer.KindSeries, title: "Three Little Ghosts", year: 1988},
	}
	initialDFeed := feedView{kind: indexer.KindMovie, title: "Initial D Third Stage", year: 2001, tvdb: 70900, twinSeason: 3, twinEpisodes: []int{1}, seriesTitle: initialD}
	initialDBoth := surfaces{
		matches: []matchView{m(library.ArrRadarr, "id", ""), m(library.ArrSonarr, "id", "")},
		rows: []rowView{
			movieRow(behind),
			{arr: library.ArrSonarr, verdict: behind, scope: "episodes", season: 3, episodes: []int{1}},
		},
		findings: []string{radarr, sonarr},
		feed:     initialDFeed,
	}
	ovaBoth := surfaces{
		matches:  []matchView{m(library.ArrRadarr, "id", ""), m(library.ArrSonarr, "id", "")},
		rows:     []rowView{movieRow(behind), {arr: library.ArrSonarr, verdict: behind, scope: "season", season: 1}},
		findings: []string{radarr, sonarr},
		feed:     feedView{kind: indexer.KindMovie, title: "OVA 619", year: 1990, tvdb: 398170},
	}
	infinityFeed := feedView{kind: indexer.KindMovie, title: infinity, year: 2025, tvdb: 348545}
	return []routeCase{
		{name: "tmdb only", alID: 416, lib: []libItem{{Item: radarrMovie(1, 11621, "Porco Rosso", 1992, true)}}, on: porco, off: porco},
		{name: "tmdb only, typed special", alID: 743, lib: []libItem{{Item: radarrMovie(2, 21057, "Ocean Waves", 1993, true)}}, on: ocean, off: ocean},
		{name: "tvdb only", alID: 170942, lib: []libItem{{Item: func() library.Item {
			it := sonarrSeason(3, 429934, "Blue Box", 1)
			it.Year = 2024
			return it
		}()}}, on: blueBox, off: blueBox},
		{
			name: "both, a film in season 0", alID: 9135, lib: fmaLib(true),
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", ""), m(library.ArrSonarr, "id", "")},
				rows:        []rowView{movieRow(behind), s0(behind, true, 21)},
				findings:    []string{radarr, sonarr},
				notOnSeaDex: []string{library.ArrSonarr + " " + fmaSeries},
				feed:        fmaFeed,
			},
			off: surfaces{
				matches:  []matchView{m(library.ArrRadarr, "id", "")},
				rows:     []rowView{movieRow(behind)},
				findings: []string{radarr},
				feed:     fmaFeed,
			},
		},
		{
			name: "both, a film in season 0, file in Sonarr only", alID: 9135, lib: fmaLib(false),
			on: surfaces{
				matches:     []matchView{m(library.ArrSonarr, "id", library.ArrRadarr)},
				rows:        []rowView{s0(behind, true, 21)},
				findings:    []string{sonarr},
				notOnSeaDex: []string{library.ArrSonarr + " " + fmaSeries},
				feed:        fmaFeed,
			},
			off: surfaces{
				matches: []matchView{m(library.ArrRadarr, "id", "")},
				rows:    []rowView{movieRow("no_file")},
				feed:    fmaFeed,
			},
		},
		{
			name: "both, typed special, in season 0", alID: 399,
			lib: []libItem{
				{Item: radarrMovie(6, 89496, "Film 399", 1990, true)},
				{Item: sonarrSpecials(7, 72025, "Series 72025", map[int]bool{3: true})},
			},
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", ""), m(library.ArrSonarr, "id", "")},
				rows:        []rowView{movieRow(behind), s0(behind, true, 3)},
				findings:    []string{radarr, sonarr},
				notOnSeaDex: []string{library.ArrSonarr + " Series 72025"},
				feed:        feedView{kind: indexer.KindMovie, title: "Film 399", year: 1990, tvdb: 72025, twinEpisodes: []int{3}, twinEntry: 1, seriesTitle: "Series 72025"},
			},
			off: surfaces{
				matches:  []matchView{m(library.ArrRadarr, "id", "")},
				rows:     []rowView{movieRow(behind)},
				findings: []string{radarr},
				feed:     feedView{kind: indexer.KindMovie, title: "Film 399", year: 1990, tvdb: 72025, twinEpisodes: []int{3}, twinEntry: 1, seriesTitle: "Series 72025"},
			},
		},
		{
			name: "both, season 0, run not consecutive", alID: 100049,
			lib: []libItem{
				{Item: radarrMovie(8, 532321, "Film 100049", 2018, true)},
				{Item: sonarrSpecials(9, 305089, "Series 305089", map[int]bool{26: true, 28: true})},
			},
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", ""), m(library.ArrSonarr, "id", "")},
				rows:        []rowView{movieRow(behind), s0(behind, true, 26, 28)},
				findings:    []string{radarr, sonarr},
				notOnSeaDex: []string{library.ArrSonarr + " Series 305089"},
				feed:        feedView{kind: indexer.KindMovie, title: "Film 100049", year: 2018, tvdb: 305089, twinEpisodes: []int{26, 28}, twinEntry: 2, seriesTitle: "Series 305089"},
			},
			off: surfaces{
				matches:  []matchView{m(library.ArrRadarr, "id", "")},
				rows:     []rowView{movieRow(behind)},
				findings: []string{radarr},
				feed:     feedView{kind: indexer.KindMovie, title: "Film 100049", year: 2018, tvdb: 305089, twinEpisodes: []int{26, 28}, twinEntry: 2, seriesTitle: "Series 305089"},
			},
		},
		{
			name: "both, season 0, unplaced", alID: 502,
			lib: []libItem{
				{Item: radarrMovie(10, 39144, "Dragon Ball: Curse of the Blood Rubies", 1986, true)},
				{Item: sonarrSeason(11, 76666, "Dragon Ball", 1)},
			},
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", "")},
				rows:        []rowView{movieRow(behind)},
				findings:    []string{radarr},
				notOnSeaDex: []string{library.ArrSonarr + " Dragon Ball"},
				feed:        feedView{kind: indexer.KindMovie, title: "Dragon Ball: Curse of the Blood Rubies", year: 1986, tvdb: 76666},
			},
			off: surfaces{
				matches:  []matchView{m(library.ArrRadarr, "id", "")},
				rows:     []rowView{movieRow(behind)},
				findings: []string{radarr},
				feed:     feedView{kind: indexer.KindMovie, title: "Dragon Ball: Curse of the Blood Rubies", year: 1986, tvdb: 76666},
			},
		},
		{
			name: "both, season 0, special episode only", alID: 1089,
			lib: []libItem{
				{Item: radarrMovie(12, 14132, "Macross: Do You Remember Love?", 1984, true)},
				{Item: sonarrSpecials(13, 73038, "Macross", map[int]bool{1: true})},
			},
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", "")},
				rows:        []rowView{movieRow(behind)},
				findings:    []string{radarr},
				notOnSeaDex: []string{library.ArrSonarr + " Macross"},
				feed:        feedView{kind: indexer.KindMovie, title: "Macross: Do You Remember Love?", year: 1984, tvdb: 73038, twinEpisodes: []int{1}, seriesTitle: "Macross"},
			},
			off: surfaces{
				matches:  []matchView{m(library.ArrRadarr, "id", "")},
				rows:     []rowView{movieRow(behind)},
				findings: []string{radarr},
				feed:     feedView{kind: indexer.KindMovie, title: "Macross: Do You Remember Love?", year: 1984, tvdb: 73038, twinEpisodes: []int{1}, seriesTitle: "Macross"},
			},
		},
		{
			name: "both, positive season", alID: 187,
			lib: []libItem{
				{Item: radarrMovie(14, 80741, "Initial D Third Stage", 2001, true)},
				{
					Arr: library.ArrSonarr, ArrID: 15, TvdbID: 70900, Title: initialD, SeasonGroups: map[int][]string{3: {"grpb"}},
					Listed: map[int]map[int]library.ListedEpisode{3: {1: {Group: "grpb", HasFile: true}, 2: {}}},
				},
			},
			on: initialDBoth, off: initialDBoth,
		},
		{
			name: "both, positive season, ranges only", alID: 619,
			lib: []libItem{
				{Item: radarrMovie(16, 146006, "OVA 619", 1990, true)},
				{Item: sonarrSeason(17, 398170, "Series 398170", 1)},
			},
			on: ovaBoth, off: ovaBoth,
		},
		{
			name: "both, absent", alID: 178788,
			lib: []libItem{
				{Item: radarrMovie(18, 1311031, infinity, 2025, true)},
				{Item: sonarrSeason(19, 348545, demonSlayer, 1)},
			},
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", "")},
				rows:        []rowView{movieRow(behind)},
				findings:    []string{radarr},
				notOnSeaDex: []string{library.ArrSonarr + " " + demonSlayer},
				feed:        infinityFeed,
			},
			off: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "id", "")},
				rows:        []rowView{movieRow(behind)},
				findings:    []string{radarr},
				notOnSeaDex: []string{library.ArrSonarr + " " + demonSlayer},
				feed:        infinityFeed,
			},
		},
		{
			name: "both, absent, film not in Radarr", alID: 178788,
			lib: []libItem{{Item: sonarrSeason(19, 348545, demonSlayer, 1)}},
			on: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "unmapped", "")},
				notOnSeaDex: []string{library.ArrSonarr + " " + demonSlayer},
				feed:        feedView{kind: indexer.KindMovie, tvdb: 348545},
			},
			off: surfaces{
				matches:     []matchView{m(library.ArrRadarr, "unmapped", "")},
				notOnSeaDex: []string{library.ArrSonarr + " " + demonSlayer},
				feed:        feedView{kind: indexer.KindMovie, tvdb: 348545},
			},
		},
		{
			name: "film with a tvdb link only, Sonarr only", alID: 43,
			lib: []libItem{
				{Item: radarrMovie(20, 9323, gitsFilm, 1995, true)},
				{Item: sonarrSpecials(21, 73749, gitsSeries, map[int]bool{1: false})},
			},
			on: surfaces{
				matches:     []matchView{m(library.ArrSonarr, "id", "")},
				rows:        []rowView{s0("no_file", true, 1)},
				notOnSeaDex: []string{library.ArrSonarr + " " + gitsSeries},
				feed:        feedView{kind: indexer.KindSeries, title: gitsSeries, season: 0, seasonKnown: true, tvdb: 73749, twinEpisodes: []int{1}, twinEntry: 6, seriesTitle: gitsSeries},
			},
			off: surfaces{
				feed: feedView{kind: indexer.KindSeries, title: gitsSeries, season: 0, seasonKnown: true, tvdb: 73749, twinEpisodes: []int{1}, twinEntry: 6, seriesTitle: gitsSeries},
			},
		},
		{
			name: "film with no link, no route", alID: 216, memo: memoOf(216, "MOVIE", piaCarrot, 2002),
			lib: []libItem{{Item: radarrMovie(22, 99216, piaCarrot, 2002, true)}},
			on:  pia, off: pia,
		},
		{name: "no route", alID: 310, memo: memoOf(310, "TV", "Three Little Ghosts", 1988), on: ghosts, off: ghosts},
		{
			name: "ONA series", alID: 131942,
			lib: []libItem{{Item: sonarrSeason(23, 262954, "JoJo's Bizarre Adventure", 5)}},
			on:  stoneOcean, off: stoneOcean,
		},
		{
			name: "Sonarr-only special", alID: 107351,
			lib: []libItem{{Item: sonarrSpecials(24, 278157, "HAIKYU!!", map[int]bool{5: true})}},
			on: surfaces{
				matches:     []matchView{m(library.ArrSonarr, "id", "")},
				rows:        []rowView{s0(behind, true, 5)},
				findings:    []string{sonarr},
				notOnSeaDex: []string{library.ArrSonarr + " HAIKYU!!"},
				feed:        feedView{kind: indexer.KindSeries, title: "HAIKYU!!", seasonKnown: true, tvdb: 278157, twinEpisodes: []int{5}, twinEntry: 1, seriesTitle: "HAIKYU!!"},
			},
			off: surfaces{
				feed: feedView{kind: indexer.KindSeries, title: "HAIKYU!!", seasonKnown: true, tvdb: 278157, twinEpisodes: []int{5}, twinEntry: 1, seriesTitle: "HAIKYU!!"},
			},
		},
	}
}

// TestRoutingByMapping pins the user's rule on every surface: a TMDB movie id
// routes to Radarr, a TVDB id to Sonarr, both to both, each copy judged on its
// own. A Sonarr copy is a special when the map offers the entry on season 0;
// a Radarr copy never is, so with specials off only the Sonarr copy is hidden.
// The feed names the Sonarr series only beside a run the map states. The
// fixture keeps each entry's published type, which decides nothing.
func TestRoutingByMapping(t *testing.T) {
	idx := liveFragmentIndex(t)
	for _, tc := range routeCases() {
		for _, specials := range []struct {
			want    surfaces
			name    string
			exclude bool
		}{{name: "specials on", want: tc.on}, {name: "specials off", want: tc.off, exclude: true}} {
			t.Run(tc.name+", "+specials.name, func(t *testing.T) {
				got := runSurfaces(t, idx, tc.alID, tc.lib, tc.memo, specials.exclude)
				want := specials.want
				if !slices.Equal(got.matches, want.matches) {
					t.Errorf("matches = %+v, want %+v", got.matches, want.matches)
				}
				if !slices.EqualFunc(got.rows, want.rows, rowView.equal) {
					t.Errorf("report rows = %+v, want %+v", got.rows, want.rows)
				}
				if !slices.Equal(got.findings, want.findings) {
					t.Errorf("findings = %q, want %q", got.findings, want.findings)
				}
				if !slices.Equal(got.notOnSeaDex, want.notOnSeaDex) {
					t.Errorf("not_on_seadex rows = %q, want %q", got.notOnSeaDex, want.notOnSeaDex)
				}
				if !got.feed.equal(want.feed) {
					t.Errorf("feed facts = %+v, want %+v", got.feed, want.feed)
				}
				if got.feed.seriesTitle != "" && len(got.feed.twinEpisodes) == 0 {
					t.Errorf("feed facts = %+v: a Sonarr series title with no run the map states", got.feed)
				}
			})
		}
	}
}
