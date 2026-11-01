package indexer

import (
	"cmp"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// routeFacts is the feed metadata internal/scout's TestRoutingByMapping
// expects the producer to hand over for one routing case, written out so each
// half of the routing table fails on its own side.
type routeFacts struct {
	title, series string
	twinEpisodes  []int
	seasons       []SeasonRange
	year          int
	tvdb          int
	season        int
	twinSeason    int
	twinEntry     int
	kind          MediaKind
	seasonKnown   bool
	nonAired      bool
}

// entryInfoOf is the one place this file builds an EntryInfo.
func entryInfoOf(f *routeFacts) EntryInfo {
	return EntryInfo{
		Title: f.title, Year: f.year, Kind: f.kind, TvdbID: f.tvdb, Season: f.season, SeasonKnown: f.seasonKnown,
		Seasons: f.seasons, SeriesTitle: f.series, TwinEpisodes: f.twinEpisodes, TwinEntryEpisodes: f.twinEntry, TwinSeason: f.twinSeason,
		NonAiredOrder: f.nonAired,
	}
}

// wireItem is what a requester sees of one served item.
type wireItem struct {
	title string
	cats  []int
	tvdb  int
	twin  bool
}

func serveWire(t *testing.T, ix *Indexer, target string) []wireItem {
	t.Helper()
	_, body := serveTitles(t, ix, target)
	var feed feedXML
	if err := xml.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatalf("decode serve(%s): %v\n%s", target, err, body)
	}
	out := make([]wireItem, 0, len(feed.Channel.Items))
	for i := range feed.Channel.Items {
		x := &feed.Channel.Items[i]
		w := wireItem{title: strings.TrimSpace(x.Title.Raw()), twin: isTwinGUID(strings.TrimSpace(x.GUID))}
		for _, a := range x.Attrs {
			switch a.Name {
			case "category":
				n, _ := strconv.Atoi(a.Value)
				w.cats = append(w.cats, n)
			case "tvdbid":
				w.tvdb, _ = strconv.Atoi(a.Value)
			}
		}
		slices.Sort(w.cats)
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b wireItem) int { return cmp.Compare(a.title, b.title) })
	return out
}

// sonarrItemToken is the season and episode token a Sonarr item's title must
// carry after the series title: SxxEaa, or SxxEaa-Ebb.
var sonarrItemToken = regexp.MustCompile(`\sS\d{2}E\d{2,3}(-E\d{2,3})?(\s|$)`)

// assertNoBareSonarrItem fails on any #sonarr item whose title names no
// season and episode: the feed offers Sonarr an entry only where the map
// states the TVDB episodes it is.
func assertNoBareSonarrItem(t *testing.T, target string, items []wireItem) {
	t.Helper()
	for _, it := range items {
		if it.twin && !sonarrItemToken.MatchString(it.title) {
			t.Errorf("serve(%s) ships the Sonarr item %q, which names no TVDB season and episode", target, it.title)
		}
	}
}

// want is one expected served item; title is matched as a prefix, so a
// series item's own episode marker is not part of the routing contract.
type want struct {
	title string
	cats  []int
	tvdb  int
	twin  bool
}

var placeholder = want{title: bootstrapItem().Title, cats: []int{catAnime, catMovies}}

func film(title string, tvdb int) want { return want{title: title, cats: []int{catMovies}, tvdb: tvdb} }

func sonarrItem(title string, tvdb int) want {
	return want{title: title, cats: []int{catAnime}, tvdb: tvdb, twin: true}
}

func matchesWant(got []wireItem, wants []want) bool {
	w := slices.Clone(wants)
	slices.SortFunc(w, func(a, b want) int { return cmp.Compare(a.title, b.title) })
	return slices.EqualFunc(got, w, func(g wireItem, w want) bool {
		return strings.HasPrefix(g.title, w.title) && slices.Equal(g.cats, slices.Sorted(slices.Values(w.cats))) && g.tvdb == w.tvdb && g.twin == w.twin
	})
}

func routeTorrent(id int, files ...string) seadex.Torrent {
	t := seadex.Torrent{Tracker: "Nyaa", URL: fmt.Sprintf("https://nyaa.si/view/%d", id), IsBest: true, ReleaseGroup: "GrpA"}
	for _, f := range files {
		t.Files = append(t.Files, seadex.File{Length: 100, Name: f})
	}
	return t
}

const (
	routeSonarrRSS = "/nyaa?t=tvsearch&cat=5000,5070&apikey=k"
	routeRadarrRSS = "/nyaa?t=movie&cat=2000&apikey=k"
	routeAnyRSS    = "/nyaa?t=search&apikey=k"
)

const (
	routeFMAFilm   = "Fullmetal Alchemist: The Sacred Star of Milos"
	routeFMASeries = "Fullmetal Alchemist: Brotherhood"
	routeGITS      = "Ghost in the Shell (TVDB)"
)

// Feed facts of the routing cases, as the scout half expects them. A
// both-routes entry carries SeriesTitle only beside a run the map states.
var (
	factsPorco    = routeFacts{title: "Porco Rosso", year: 1992, kind: KindMovie}
	factsBlueBox  = routeFacts{title: "Blue Box", year: 2024, kind: KindSeries, season: 1, seasonKnown: true, tvdb: 429934, seasons: []SeasonRange{{Season: 1, First: 1, Last: 25}}}
	factsFMA      = routeFacts{title: routeFMAFilm, year: 2011, kind: KindMovie, tvdb: 85249, twinEpisodes: []int{21}, twinEntry: 3, series: routeFMASeries}
	facts399      = routeFacts{title: "Film 399", year: 1990, kind: KindMovie, tvdb: 72025, twinEpisodes: []int{3}, twinEntry: 1, series: "Series 72025"}
	factsMacross  = routeFacts{title: "Macross: Do You Remember Love?", year: 1984, kind: KindMovie, tvdb: 73038, twinEpisodes: []int{1}, series: "Macross"}
	facts100049   = routeFacts{title: "Film 100049", year: 2018, kind: KindMovie, tvdb: 305089, twinEpisodes: []int{26, 28}, twinEntry: 2, series: "Series 305089"}
	factsDB502    = routeFacts{title: "Dragon Ball: Curse of the Blood Rubies", year: 1986, kind: KindMovie, tvdb: 76666}
	factsInitialD = routeFacts{title: "Initial D Third Stage", year: 2001, kind: KindMovie, tvdb: 70900, twinSeason: 3, twinEpisodes: []int{1}, series: "Initial D"}
	facts619      = routeFacts{title: "OVA 619", year: 1990, kind: KindMovie, tvdb: 398170}
	factsInfinity = routeFacts{title: "Demon Slayer: Kimetsu no Yaiba Infinity Castle", year: 2025, kind: KindMovie, tvdb: 348545}
	factsFMAOrder = routeFacts{title: routeFMAFilm, year: 2011, kind: KindMovie, tvdb: 85249, nonAired: true}
	factsFMAAlone = routeFacts{title: routeFMAFilm, year: 2011, kind: KindMovie, tvdb: 85249}
	factsGITS43   = routeFacts{title: routeGITS, kind: KindSeries, seasonKnown: true, tvdb: 73749, twinEpisodes: []int{1}, twinEntry: 6, series: routeGITS}
	factsPia      = routeFacts{title: "Welcome to Pia Carrot!! Sayaka's Love Story", year: 2002, kind: KindMovie}
	factsGhosts   = routeFacts{title: "Three Little Ghosts", year: 1988, kind: KindSeries}
)

type feedRouteCase struct {
	facts   map[int]routeFacts
	name    string
	entries []seadex.Entry
	sonarr  []want
	radarr  []want
	any     []want
	// persisted is the Sonarr item title the owner record stores, "" for none.
	persisted string
}

func oneEntry(alID int, t seadex.Torrent) []seadex.Entry {
	return []seadex.Entry{{AniListID: alID, Torrents: []seadex.Torrent{t}}}
}

func feedRouteCases() []feedRouteCase {
	const (
		fmaFilmTitle = routeFMAFilm + " (2011) 1080p [GrpA]"
		fmaTwin      = routeFMASeries + " S00E21 1080p [GrpA]"
		initialDTwin = "Initial D S03E01 1080p [GrpA]"
		macrossTwin  = "Macross S00E01 1080p [GrpA]"
		gitsTwin     = routeGITS + " S00E01 1080p [GrpA]"
	)
	fmaFile := routeTorrent(9135, "Fullmetal Alchemist The Sacred Star of Milos (1080p) [GrpA].mkv")
	single := func(alID int, f routeFacts, t seadex.Torrent) (map[int]routeFacts, []seadex.Entry) {
		return map[int]routeFacts{alID: f}, oneEntry(alID, t)
	}
	var cases []feedRouteCase
	add := func(c feedRouteCase, alID int, f routeFacts, t seadex.Torrent) {
		c.facts, c.entries = single(alID, f, t)
		cases = append(cases, c)
	}
	add(feedRouteCase{
		name:   "tmdb only (416)",
		sonarr: []want{placeholder}, radarr: []want{film("Porco Rosso (1992) 1080p [GrpA]", 0)}, any: []want{film("Porco Rosso (1992) 1080p [GrpA]", 0)},
	}, 416, factsPorco, routeTorrent(416, "Porco Rosso (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "tvdb only (170942)",
		sonarr: []want{{title: "Blue Box", cats: []int{catAnime}, tvdb: 429934}}, radarr: []want{placeholder},
		any: []want{{title: "Blue Box", cats: []int{catAnime}, tvdb: 429934}},
	}, 170942, factsBlueBox, routeTorrent(170942, "Blue Box - 01 (1080p) [GrpA].mkv", "Blue Box - 02 (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, season 0 (9135)",
		sonarr: []want{sonarrItem(fmaTwin, 85249)}, radarr: []want{film(fmaFilmTitle, 85249)},
		any: []want{film(fmaFilmTitle, 85249), sonarrItem(fmaTwin, 85249)}, persisted: fmaTwin,
	}, 9135, factsFMA, fmaFile)
	add(feedRouteCase{
		name:   "both, season 0, typed special (399)",
		sonarr: []want{sonarrItem("Series 72025 S00E03 1080p [GrpA]", 72025)}, radarr: []want{film("Film 399 (1990) 1080p [GrpA]", 72025)},
		any:       []want{film("Film 399 (1990) 1080p [GrpA]", 72025), sonarrItem("Series 72025 S00E03 1080p [GrpA]", 72025)},
		persisted: "Series 72025 S00E03 1080p [GrpA]",
	}, 399, facts399, routeTorrent(399, "Film 399 (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, special episode only, one-file torrent (1089)",
		sonarr: []want{sonarrItem(macrossTwin, 73038)}, radarr: []want{film("Macross: Do You Remember Love? (1984) 1080p [GrpA]", 73038)},
		any:       []want{film("Macross: Do You Remember Love? (1984) 1080p [GrpA]", 73038), sonarrItem(macrossTwin, 73038)},
		persisted: macrossTwin,
	}, 1089, factsMacross, routeTorrent(1089, "Macross Do You Remember Love (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, special episode only, two-file torrent (1089)",
		sonarr: []want{placeholder}, radarr: []want{film("Macross: Do You Remember Love? (1984) 1080p [GrpA]", 73038)},
		any: []want{film("Macross: Do You Remember Love? (1984) 1080p [GrpA]", 73038)},
	}, 1089, factsMacross, routeTorrent(1089, "Macross Do You Remember Love (1080p) [GrpA].mkv", "Macross Flash Back 2012 (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, season 0, run not consecutive (100049)",
		sonarr: []want{placeholder}, radarr: []want{film("Film 100049 (2018) 1080p [GrpA]", 305089)},
		any: []want{film("Film 100049 (2018) 1080p [GrpA]", 305089)},
	}, 100049, facts100049, routeTorrent(100049, "Film 100049 (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, season 0, unplaced (502)",
		sonarr: []want{placeholder}, radarr: []want{film("Dragon Ball: Curse of the Blood Rubies (1986) 1080p [GrpA]", 76666)},
		any: []want{film("Dragon Ball: Curse of the Blood Rubies (1986) 1080p [GrpA]", 76666)},
	}, 502, factsDB502, routeTorrent(502, "Dragon Ball Curse of the Blood Rubies (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, positive season (187)",
		sonarr: []want{sonarrItem(initialDTwin, 70900)}, radarr: []want{film("Initial D Third Stage (2001) 1080p [GrpA]", 70900)},
		any:       []want{film("Initial D Third Stage (2001) 1080p [GrpA]", 70900), sonarrItem(initialDTwin, 70900)},
		persisted: initialDTwin,
	}, 187, factsInitialD, routeTorrent(187, "Initial D Third Stage (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, positive season, torrent with two unnumbered files (187)",
		sonarr: []want{placeholder}, radarr: []want{film("Initial D Third Stage (2001) 1080p [GrpA]", 70900)},
		any: []want{film("Initial D Third Stage (2001) 1080p [GrpA]", 70900)},
	}, 187, factsInitialD, routeTorrent(187, "Initial D Third Stage (1080p) [GrpA].mkv", "Initial D Third Stage Extras (1080p) [GrpA].mkv"))
	// 619's map names no season-1 episode, so Sonarr gets nothing until animap states them.
	add(feedRouteCase{
		name:   "both, positive season, ranges only (619)",
		sonarr: []want{placeholder}, radarr: []want{film("OVA 619 (1990) 1080p [GrpA]", 398170)},
		any: []want{film("OVA 619 (1990) 1080p [GrpA]", 398170)},
	}, 619, facts619, routeTorrent(619, "OVA 619 (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, absent (178788)",
		sonarr: []want{placeholder}, radarr: []want{film("Demon Slayer: Kimetsu no Yaiba Infinity Castle (2025) 1080p [GrpA]", 348545)},
		any: []want{film("Demon Slayer: Kimetsu no Yaiba Infinity Castle (2025) 1080p [GrpA]", 348545)},
	}, 178788, factsInfinity, routeTorrent(178788, "Infinity Castle (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "both, series in a non-aired order (9135)",
		sonarr: []want{placeholder}, radarr: []want{film(fmaFilmTitle, 85249)}, any: []want{film(fmaFilmTitle, 85249)},
	}, 9135, factsFMAOrder, fmaFile)
	add(feedRouteCase{
		name:   "both, Sonarr series not in the library (9135)",
		sonarr: []want{placeholder}, radarr: []want{film(fmaFilmTitle, 85249)}, any: []want{film(fmaFilmTitle, 85249)},
	}, 9135, factsFMAAlone, fmaFile)
	add(feedRouteCase{
		name:   "tvdb only, season 0 (43)",
		sonarr: []want{sonarrItem(gitsTwin, 73749)}, radarr: []want{placeholder},
		any:       []want{{title: routeGITS, cats: []int{catAnime}, tvdb: 73749}, sonarrItem(gitsTwin, 73749)},
		persisted: gitsTwin,
	}, 43, factsGITS43, routeTorrent(43, "Ghost in the Shell (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "no route, memo MOVIE (216)",
		sonarr: []want{placeholder}, radarr: []want{film("Welcome to Pia Carrot!! Sayaka's Love Story (2002) 1080p [GrpA]", 0)},
		any: []want{film("Welcome to Pia Carrot!! Sayaka's Love Story (2002) 1080p [GrpA]", 0)},
	}, 216, factsPia, routeTorrent(216, "Pia Carrot (1080p) [GrpA].mkv"))
	add(feedRouteCase{
		name:   "no route, memo TV (310)",
		sonarr: []want{{title: "Three Little Ghosts", cats: []int{catAnime}}}, radarr: []want{placeholder},
		any: []want{{title: "Three Little Ghosts", cats: []int{catAnime}}},
	}, 310, factsGhosts, routeTorrent(310, "Three Little Ghosts - 01 (1080p) [GrpA].mkv", "Three Little Ghosts - 02 (1080p) [GrpA].mkv"))
	shared := routeTorrent(9135, "Fullmetal Alchemist The Sacred Star of Milos (1080p) [GrpA].mkv")
	cases = append(cases, feedRouteCase{
		name:    "both, a torrent a Radarr-only entry also holds (9135 and 416)",
		facts:   map[int]routeFacts{9135: factsFMA, 416: factsPorco},
		entries: []seadex.Entry{{AniListID: 9135, Torrents: []seadex.Torrent{shared}}, {AniListID: 416, Torrents: []seadex.Torrent{shared}}},
		sonarr:  []want{placeholder}, radarr: []want{film("Porco Rosso (1992) 1080p [GrpA]", 85249)},
		any: []want{film("Porco Rosso (1992) 1080p [GrpA]", 85249)},
	})
	return cases
}

func routeIndexer(t *testing.T, entries []seadex.Entry, facts map[int]routeFacts, upstream string) *Indexer {
	t.Helper()
	info := func(alID int) EntryInfo {
		f := facts[alID]
		return entryInfoOf(&f)
	}
	return twinIndexer(t, entries, info, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(upstream, "http://prowlarr:9696", "http://"+r.Host))
	}, Config{NyaaTorznabURL: "set"})
}

// TestFeedRoutesByMapping is the feed half of the routing table: an entry with
// a TMDB link is the film for Radarr, a TVDB link reaches Sonarr, and a
// both-routes entry gets a Sonarr item only where the map states its TVDB run,
// so Sonarr RSS otherwise gets nothing and a request naming no arr gets the
// film alone.
func TestFeedRoutesByMapping(t *testing.T) {
	for _, tc := range feedRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			ix := routeIndexer(t, tc.entries, tc.facts, emptyUpstreamFeed)
			for _, req := range []struct {
				target string
				want   []want
			}{{routeSonarrRSS, tc.sonarr}, {routeRadarrRSS, tc.radarr}, {routeAnyRSS, tc.any}} {
				got := serveWire(t, ix, req.target)
				assertNoBareSonarrItem(t, req.target, got)
				if !matchesWant(got, req.want) {
					t.Errorf("serve(%s) = %+v, want %+v", req.target, got, req.want)
				}
			}
		})
	}
}

// TestFeedPersistsASonarrTitleOnlyBesideAStatedRun pins the owner record: a
// release stores the Sonarr item's title only when it has one, and only a
// season-0 run joins the special-search catalogue.
func TestFeedPersistsASonarrTitleOnlyBesideAStatedRun(t *testing.T) {
	for _, tc := range feedRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			seedEmptyFeed(t, path)
			info := func(alID int) EntryInfo {
				f := tc.facts[alID]
				return entryInfoOf(&f)
			}
			if err := newTestWriter(path, "", false).Rebuild(t.Context(), tc.entries, FeedInfo{Entry: info}); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			snap := readSnapshotFile(t, path)
			for alID, f := range tc.facts {
				for _, r := range snap.Owners[ownerKey(alID)] {
					if len(tc.facts) == 1 && r.SonarrTitle != tc.persisted {
						t.Errorf("owners[%d].SonarrTitle = %q, want %q", alID, r.SonarrTitle, tc.persisted)
					}
					if r.SonarrTitle != "" && !sonarrItemToken.MatchString(r.SonarrTitle) {
						t.Errorf("owners[%d].SonarrTitle = %q names no TVDB season and episode", alID, r.SonarrTitle)
					}
					if f.twinSeason > 0 && (r.TwinSeries != "" || r.URL != "" || r.Size != 0 || r.TwinFirst != 0 || r.TwinLast != 0) {
						t.Errorf("owners[%d] = %+v: a positive-season Sonarr item joined the special-search catalogue", alID, r)
					}
				}
			}
		})
	}
}

// TestFeedSearchPairsOnlyAStatedRun pins the proxied search: a tracker result
// for a curated both-routes film is paired with its Sonarr item on Sonarr's
// season search when the map states its run, Sonarr gets nothing otherwise,
// and Radarr's search gets the tracker's own item alone.
func TestFeedSearchPairsOnlyAStatedRun(t *testing.T) {
	const proxied = "[Group] Some Anime S01 [1080p]"
	torrent := func(name string) seadex.Torrent {
		t := routeTorrent(1234567, name)
		t.InfoHash = twinFilmHash
		return t
	}
	for name, tc := range map[string]struct {
		facts  routeFacts
		file   string
		sonarr string
		radarr string
		want   []want
	}{
		"a positive-season run (187)": {
			facts: factsInitialD, file: "Initial D Third Stage (1080p) [GrpA].mkv",
			sonarr: "/nyaa?t=tvsearch&q=Initial+D&season=3&cat=5070&apikey=k", radarr: "/nyaa?t=movie&q=Initial+D+Third+Stage&cat=2000&apikey=k",
			want: []want{sonarrItem("Initial D S03E01 1080p [GrpA]", 70900)},
		},
		"no stated run (502)": {
			facts: factsDB502, file: "Dragon Ball Curse of the Blood Rubies (1080p) [GrpA].mkv",
			sonarr: "/nyaa?t=tvsearch&q=Dragon+Ball&season=1&cat=5070&apikey=k", radarr: "/nyaa?t=movie&q=Dragon+Ball+Curse+of+the+Blood+Rubies&cat=2000&apikey=k",
		},
		"no stated run (619)": {
			facts: facts619, file: "OVA 619 (1080p) [GrpA].mkv",
			sonarr: "/nyaa?t=tvsearch&q=Series&season=1&cat=5070&apikey=k", radarr: "/nyaa?t=movie&q=OVA+619&cat=2000&apikey=k",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ix := routeIndexer(t, oneEntry(1, torrent(tc.file)), map[int]routeFacts{1: tc.facts}, sampleFeed)
			got := serveWire(t, ix, tc.sonarr)
			assertNoBareSonarrItem(t, tc.sonarr, got)
			if len(got) != len(tc.want) || len(got) == 1 && (got[0].title != tc.want[0].title || !got[0].twin) {
				t.Errorf("serve(%s) = %+v, want %+v", tc.sonarr, got, tc.want)
			}
			if got := serveWire(t, ix, tc.radarr); len(got) != 1 || got[0].title != proxied || got[0].twin {
				t.Errorf("serve(%s) = %+v, want the tracker's own item alone", tc.radarr, got)
			}
		})
	}
}

// TestFeedSpecialSearchAnswersOnlySeasonZeroRuns pins the special-search
// catalogue: a season-0 search finds the season-0 twin, and never a
// positive-season Sonarr item, whose episode is not a special.
func TestFeedSpecialSearchAnswersOnlySeasonZeroRuns(t *testing.T) {
	entries := []seadex.Entry{
		{AniListID: 9135, Torrents: []seadex.Torrent{routeTorrent(9135, "Fullmetal Alchemist The Sacred Star of Milos (1080p) [GrpA].mkv")}},
		{AniListID: 187, Torrents: []seadex.Torrent{routeTorrent(187, "Initial D Third Stage (1080p) [GrpA].mkv")}},
	}
	ix := routeIndexer(t, entries, map[int]routeFacts{9135: factsFMA, 187: factsInitialD}, emptyUpstreamFeed)
	for target, want := range map[string][]string{
		"/nyaa?t=tvsearch&q=Fullmetal+Alchemist+Brotherhood&season=00&ep=21&cat=5070&apikey=k": {routeFMASeries + " S00E21 1080p [GrpA]"},
		"/nyaa?t=tvsearch&q=Initial+D&season=00&ep=1&cat=5070&apikey=k":                        nil,
		"/nyaa?t=search&q=Initial+D+00&cat=5070&apikey=k":                                      nil,
	} {
		if got, body := serveTitles(t, ix, target); !slices.Equal(got, want) {
			t.Errorf("serve(%s) = %q, want %q:\n%s", target, got, want, body)
		}
	}
}
