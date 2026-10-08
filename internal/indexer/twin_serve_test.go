package indexer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

const (
	twinFilmHash  = "abcdef1234567890abcdef1234567890abcdef12"
	twinFilmTitle = "Code Geass: Lelouch of the Rebellion S00E04 1080p [PMR]"
	twinOVATitle  = "Code Geass: Lelouch of the Rebellion S00E05 1080p [PMR]"
	twinSeries    = "Code Geass: Lelouch of the Rebellion"
)

func twinFixtureEntries() []seadex.Entry {
	return []seadex.Entry{
		{AniListID: 21519, Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567", InfoHash: twinFilmHash, IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 100, Name: "Lelouch of the Resurrection (1080p) [PMR].mkv"}},
		}}},
		{AniListID: 3000, Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/7654321", ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 100, Name: "Code Geass OVA (1080p) [PMR].mkv"}},
		}}},
	}
}

func twinFixtureInfo(alID int) EntryInfo {
	if alID == 21519 {
		return EntryInfo{
			Title: "Lelouch of the Resurrection", Year: 2019, IsMovie: true, TvdbID: 79525,
			SpecialEpisodes: []int{4}, SpecialsEpisodes: 1, SeriesTitle: twinSeries,
		}
	}
	return EntryInfo{Title: "Code Geass OVA", TvdbID: 79525, SpecialEpisodes: []int{5}, SpecialsEpisodes: 1, SeriesTitle: twinSeries}
}

func twinFixtureIndexer(t *testing.T, entries []seadex.Entry, body string, cfg Config) *Indexer {
	t.Helper()
	return twinIndexer(t, entries, twinFixtureInfo, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}, cfg)
}

func twinIndexer(t *testing.T, entries []seadex.Entry, info EntryInfoFunc, upstream http.HandlerFunc, cfg Config) *Indexer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, cfg.ABPasskey, cfg.ABTorznabURL != "").Rebuild(t.Context(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	cfg.APIKey, cfg.SnapshotPath, cfg.ProwlarrAPIKey = "k", path, "pk"
	if cfg.NyaaTorznabURL != "" {
		cfg.NyaaTorznabURL = srv.URL
	}
	if cfg.ABTorznabURL != "" {
		cfg.ABTorznabURL = srv.URL
	}
	return warmedIndexer(&cfg, nil, srv.Client())
}

const emptyUpstreamFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><title>empty</title></channel></rss>`

func serveTitles(t *testing.T, ix *Indexer, target string) ([]string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
	body := rec.Body.String()
	items, err := parseTorznab([]byte(body))
	if err != nil {
		t.Fatalf("parseTorznab(%s): %v\n%s", target, err, body)
	}
	titles := make([]string, 0, len(items))
	for i := range items {
		titles = append(titles, items[i].Title)
	}
	return titles, body
}

// TestServeSplitsTheTwinByRequester: Sonarr gets the twins in place of the
// originals they supersede, Radarr the film under its own title and never a
// twin, and a request naming neither arr (Prowlarr's save test) gets both.
// Every real item keeps the marker and the scene tag.
func TestServeSplitsTheTwinByRequester(t *testing.T) {
	ix := twinFixtureIndexer(t, twinFixtureEntries(), sampleFeed, Config{NyaaTorznabURL: "set"})
	const filmTitle, ovaTitle = "Lelouch of the Resurrection (2019) 1080p [PMR]", "Code Geass OVA 1080p [PMR]"
	const proxiedTitle = "[Group] Some Anime S01 [1080p]"
	tests := map[string]struct {
		target string
		want   []string
	}{
		"Sonarr RSS":     {target: "/nyaa?t=search&cat=5070,5000&apikey=k", want: []string{twinFilmTitle, twinOVATitle}},
		"Radarr RSS":     {target: "/nyaa?t=search&cat=2000&apikey=k", want: []string{filmTitle}},
		"Prowlarr test":  {target: "/nyaa?t=search&extended=1&apikey=k", want: []string{filmTitle, twinFilmTitle, ovaTitle, twinOVATitle}},
		"Sonarr search":  {target: "/nyaa?t=tvsearch&q=Code+Geass&season=1&cat=5070&apikey=k", want: []string{twinFilmTitle}},
		"Radarr search":  {target: "/nyaa?t=search&q=Lelouch+of+the+Resurrection+2019&cat=2000&apikey=k", want: []string{proxiedTitle}},
		"no-cat tv type": {target: "/nyaa?t=tvsearch&apikey=k", want: []string{twinFilmTitle, twinOVATitle}},
		"mixed cats":     {target: "/nyaa?t=search&cat=2000,5070&apikey=k", want: []string{filmTitle, twinFilmTitle, ovaTitle, twinOVATitle}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, body := serveTitles(t, ix, tc.target)
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) {
				t.Errorf("serve(%s) titles = %q, want %q", tc.target, got, want)
			}
			if n := strings.Count(body, "<item>"); strings.Count(body, `name="downloadvolumefactor"`) != n || strings.Count(body, `name="tag" value="scene"`) != n {
				t.Errorf("serve(%s): not every one of %d items carries the marker and the scene tag:\n%s", tc.target, n, body)
			}
		})
	}
}

// TestServePlaceholderFollowsTheRequesterSplit pins that the placeholder is
// decided after the requester split and the category filter: Radarr polling a
// journal whose only release is an OVA served to Sonarr gets the placeholder,
// while Sonarr polling a journal of one twinned film gets the twin.
func TestServePlaceholderFollowsTheRequesterSplit(t *testing.T) {
	entries := twinFixtureEntries()
	ova := twinFixtureIndexer(t, entries[1:], emptyUpstreamFeed, Config{NyaaTorznabURL: "set"})
	if got, _ := serveTitles(t, ova, "/nyaa?t=search&cat=2000&apikey=k"); len(got) != 1 || got[0] != bootstrapItem().Title {
		t.Errorf("Radarr RSS over an OVA-only journal = %q, want the placeholder", got)
	}
	film := twinFixtureIndexer(t, entries[:1], emptyUpstreamFeed, Config{NyaaTorznabURL: "set"})
	if got, _ := serveTitles(t, film, "/nyaa?t=search&cat=5070,5000&apikey=k"); !slices.Equal(got, []string{twinFilmTitle}) {
		t.Errorf("Sonarr RSS over a twinned-film journal = %q, want the twin alone", got)
	}
}

// TestServeAnswersASonarrSpecialSearchFromTheCatalogue pins Sonarr's exact
// special search (season "00", an episode, the series in q) against an upstream
// that finds nothing: the twin whose run holds the episode is answered from the
// curation, with its marker, tvdb id and a working Nyaa link, and nothing else.
func TestServeAnswersASonarrSpecialSearchFromTheCatalogue(t *testing.T) {
	ix := twinFixtureIndexer(t, twinFixtureEntries(), emptyUpstreamFeed, Config{NyaaTorznabURL: "set"})
	const special = "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k"
	got, body := serveTitles(t, ix, special)
	if !slices.Equal(got, []string{twinFilmTitle}) {
		t.Fatalf("serve(special S00E04) = %q, want the film twin alone:\n%s", got, body)
	}
	for _, want := range []string{
		`<guid>https://nyaa.si/view/1234567#sonarr</guid>`,
		`url="https://nyaa.si/download/1234567.torrent"`,
		`name="downloadvolumefactor" value="0.75"`,
		`name="tag" value="scene"`,
		`name="tvdbid" value="79525"`,
		`name="category" value="5070"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("serve(special S00E04) is missing %s:\n%s", want, body)
		}
	}
	for name, target := range map[string]string{
		"an episode outside the run": "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=6&cat=5070&apikey=k",
		"a Radarr search":            "/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=2000&apikey=k",
		"another series":             "/nyaa?t=tvsearch&q=Sailor+Moon&season=00&ep=4&cat=5070&apikey=k",
		"a real season":              "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=1&cat=5070&apikey=k",
		"a later page":               "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&offset=100&apikey=k",
	} {
		if got, _ := serveTitles(t, ix, target); len(got) != 0 {
			t.Errorf("serve(%s) = %q, want nothing", name, got)
		}
	}
	if got, _ := serveTitles(t, ix, "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=0&ep=5&apikey=k"); !slices.Equal(got, []string{twinOVATitle}) {
		t.Errorf("serve(special S00E05, season 0, no cat) = %q, want the OVA twin", got)
	}
}

// TestServeAnswersAnAnimeSpecialsSearchFromTheCatalogue pins the query Sonarr
// sends an Anime-type series for its specials season and for a bulk search of
// two or more specials, "<series> 00" with no season or episode: every placed
// twin of the series is answered from the curation, and the tracker is not
// asked, as for any other per-episode basic search.
func TestServeAnswersAnAnimeSpecialsSearchFromTheCatalogue(t *testing.T) {
	var upstreamHits atomic.Int32
	ix := twinIndexer(t, twinFixtureEntries(), twinFixtureInfo, func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = io.WriteString(w, emptyUpstreamFeed)
	}, Config{NyaaTorznabURL: "set"})
	for target, want := range map[string][]string{
		"/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion+00&cat=5070&apikey=k":            {twinFilmTitle, twinOVATitle},
		"/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion+00&apikey=k":                     {twinFilmTitle, twinOVATitle},
		"/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion+00&cat=5070&offset=100&apikey=k": nil,
		"/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion+01&cat=5070&apikey=k":            nil,
		"/nyaa?t=search&q=Sailor+Moon+00&cat=5070&apikey=k":                                    nil,
	} {
		got, body := serveTitles(t, ix, target)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("serve(%s) = %q, want %q:\n%s", target, got, want, body)
		}
	}
	if n := upstreamHits.Load(); n != 0 {
		t.Errorf("serve(anime specials and episode queries) asked the tracker %d times, want 0", n)
	}
	for _, target := range []string{
		"/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion+00&cat=2000&apikey=k",
		"/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion+0&cat=5070&apikey=k",
		"/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion+00&season=1&cat=5070&apikey=k",
	} {
		if got, body := serveTitles(t, ix, target); len(got) != 0 {
			t.Errorf("serve(%s) = %q, want nothing:\n%s", target, got, body)
		}
	}
}

// TestServeCatalogueDefersToTheProxiedTwin pins the dedupe: when the tracker
// search already returned the release, its proxied twin with live peers is the
// one served, never a second catalogue copy.
func TestServeCatalogueDefersToTheProxiedTwin(t *testing.T) {
	ix := twinFixtureIndexer(t, twinFixtureEntries(), sampleFeed, Config{NyaaTorznabURL: "set"})
	got, body := serveTitles(t, ix, "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k")
	if !slices.Equal(got, []string{twinFilmTitle}) || !strings.Contains(body, `name="seeders" value="42"`) {
		t.Errorf("serve(special with a proxied hit) = %q, want the proxied twin once with its 42 seeders:\n%s", got, body)
	}
}

// TestServeCatalogueNeedsTheABPasskey pins the AnimeBytes gate: without a
// usable passkey the catalogue cannot build a download link, so an /ab special
// search keeps the proxy's answer alone.
func TestServeCatalogueNeedsTheABPasskey(t *testing.T) {
	entries := []seadex.Entry{{AniListID: 21519, Torrents: []seadex.Torrent{{
		Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", IsBest: true, ReleaseGroup: "PMR",
		Files: []seadex.File{{Length: 100, Name: "Lelouch of the Resurrection (1080p) [PMR].mkv"}},
	}}}}
	const special = "/ab?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k"
	for name, tc := range map[string]struct {
		passkey string
		want    []string
	}{
		"with a passkey":    {passkey: "pk-test", want: []string{twinFilmTitle}},
		"without a passkey": {},
	} {
		t.Run(name, func(t *testing.T) {
			ix := twinFixtureIndexer(t, entries, emptyUpstreamFeed, Config{ABTorznabURL: "set", ABPasskey: tc.passkey})
			if got, body := serveTitles(t, ix, special); !slices.Equal(got, tc.want) {
				t.Errorf("serve(/ab special, %s) = %q, want %q:\n%s", name, got, tc.want, body)
			}
		})
	}
}

// TestServeCatalogueStaysOffOnADisabledTracker pins the off switch: with Nyaa's
// Torznab URL empty, a Nyaa special search answers nothing, catalogue included.
func TestServeCatalogueStaysOffOnADisabledTracker(t *testing.T) {
	ix := twinFixtureIndexer(t, twinFixtureEntries(), emptyUpstreamFeed, Config{ABTorznabURL: "set", ABPasskey: "pk-test"})
	special := "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k"
	if got, body := serveTitles(t, ix, special); len(got) != 0 {
		t.Errorf("serve(Nyaa special, Nyaa off) = %q, want nothing:\n%s", got, body)
	}
}

// TestServeCatalogueAnswersThroughAnUpstreamOutage pins that a total Prowlarr
// failure still returns the placed twin the catalogue holds, while a search
// the catalogue cannot answer keeps reporting the outage as a Torznab error.
func TestServeCatalogueAnswersThroughAnUpstreamOutage(t *testing.T) {
	ix := twinIndexer(t, twinFixtureEntries(), twinFixtureInfo, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, Config{NyaaTorznabURL: "set"})
	got, body := serveTitles(t, ix, "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k")
	if !slices.Equal(got, []string{twinFilmTitle}) {
		t.Fatalf("serve(special S00E04, upstream down) = %q, want the film twin:\n%s", got, body)
	}
	for _, want := range []string{
		`url="https://nyaa.si/download/1234567.torrent"`,
		`name="downloadvolumefactor" value="0.75"`,
		`name="tag" value="scene"`,
		`name="tvdbid" value="79525"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("serve(special S00E04, upstream down) is missing %s:\n%s", want, body)
		}
	}
	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?t=search&q=Code+Geass&cat=5070&apikey=k", nil))
	if !strings.Contains(rec.Body.String(), "<error") {
		t.Errorf("serve(generic search, upstream down) = %s, want a Torznab error", rec.Body.String())
	}
}

// TestServeNeverOffersSonarrAFilmWithoutItsTwin pins the wrong-grab guard for
// a film whose twin was refused, a gapped placement or a torrent that is not
// the film alone: Sonarr gets neither the film under its own title nor a twin,
// on RSS and on a proxied search, while Radarr and an unattributed request
// still get the film, and an OVA without a twin still reaches Sonarr.
func TestServeNeverOffersSonarrAFilmWithoutItsTwin(t *testing.T) {
	const filmTitle, ovaTitle = "Lelouch of the Resurrection (2019) 1080p [PMR]", "Code Geass OVA 1080p [PMR]"
	const proxiedTitle = "[Group] Some Anime S01 [1080p]"
	gapped := func(alID int) EntryInfo {
		info := twinFixtureInfo(alID)
		info.SpecialEpisodes = []int{4, 6}
		return info
	}
	pack := twinFixtureEntries()
	pack[0].Torrents[0].Files = []seadex.File{
		{Length: 100, Name: "Lelouch of the Resurrection (1080p) [PMR].mkv"},
		{Length: 100, Name: "Code Geass - 01 (1080p) [PMR].mkv"},
	}
	ovaGapped := func(alID int) EntryInfo {
		if alID == 3000 {
			return gapped(alID)
		}
		return twinFixtureInfo(alID)
	}
	for name, tc := range map[string]struct {
		entries []seadex.Entry
		info    EntryInfoFunc
	}{
		"a gapped placement":        {entries: twinFixtureEntries(), info: gapped},
		"a torrent beyond the film": {entries: pack, info: twinFixtureInfo},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
			}
			ix := twinIndexer(t, tc.entries[:1], tc.info, upstream, Config{NyaaTorznabURL: "set"})
			for target, want := range map[string][]string{
				"/nyaa?t=search&cat=5070,5000&apikey=k":                          {bootstrapItem().Title},
				"/nyaa?t=tvsearch&apikey=k":                                      {bootstrapItem().Title},
				"/nyaa?t=tvsearch&q=Code+Geass&season=1&cat=5070&apikey=k":       nil,
				"/nyaa?t=search&cat=2000&apikey=k":                               {filmTitle},
				"/nyaa?t=search&extended=1&apikey=k":                             {filmTitle},
				"/nyaa?t=search&q=Lelouch+of+the+Resurrection&apikey=k":          {proxiedTitle},
				"/nyaa?t=search&q=Lelouch+of+the+Resurrection&cat=2000&apikey=k": {proxiedTitle},
			} {
				if got, body := serveTitles(t, ix, target); !slices.Equal(got, want) {
					t.Errorf("serve(%s) = %q, want %q:\n%s", target, got, want, body)
				}
			}
		})
	}
	ova := twinIndexer(t, twinFixtureEntries()[1:], ovaGapped, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, emptyUpstreamFeed)
	}, Config{NyaaTorznabURL: "set"})
	if got, _ := serveTitles(t, ova, "/nyaa?t=search&cat=5070,5000&apikey=k"); !slices.Equal(got, []string{ovaTitle}) {
		t.Errorf("Sonarr RSS over an OVA without a twin = %q, want the OVA", got)
	}
}

// TestServeGivesSonarrACoHeldReleaseOnlyUnderANonFilmTitle pins the RSS split
// for a release a film and an OVA both hold, their twins vetoing each other:
// the original carries the lowest AniList id's title, and Sonarr gets it only
// when that title is the OVA's, never the film's, which Sonarr could attribute
// only by guessing an episode from it.
func TestServeGivesSonarrACoHeldReleaseOnlyUnderANonFilmTitle(t *testing.T) {
	const filmTitle, ovaTitle = "Lelouch of the Resurrection (2019) 1080p [PMR]", "Code Geass OVA 1080p [PMR]"
	held := twinFixtureEntries()[0].Torrents
	for name, tc := range map[string]struct {
		ovaID      int
		title      string
		sonarrWant []string
	}{
		"the film holds the lowest id": {ovaID: 30000, title: filmTitle, sonarrWant: []string{bootstrapItem().Title}},
		"the OVA holds the lowest id":  {ovaID: 3000, title: ovaTitle, sonarrWant: []string{ovaTitle}},
	} {
		t.Run(name, func(t *testing.T) {
			entries := []seadex.Entry{{AniListID: 21519, Torrents: held}, {AniListID: tc.ovaID, Torrents: held}}
			ix := twinIndexer(t, entries, twinFixtureInfo, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, emptyUpstreamFeed)
			}, Config{NyaaTorznabURL: "set"})
			for target, want := range map[string][]string{
				"/nyaa?t=search&cat=5070,5000&apikey=k": tc.sonarrWant,
				"/nyaa?t=tvsearch&apikey=k":             tc.sonarrWant,
				"/nyaa?t=search&cat=2000&apikey=k":      {tc.title},
				"/nyaa?t=search&extended=1&apikey=k":    {tc.title},
			} {
				if got, body := serveTitles(t, ix, target); !slices.Equal(got, want) {
					t.Errorf("serve(%s) = %q, want %q:\n%s", target, got, want, body)
				}
			}
		})
	}
}

// TestRebuildPersistsTheTwinCatalogue pins the five catalogue fields on disk
// under their snake_case keys, and that the snapshot decode keeps them.
func TestRebuildPersistsTheTwinCatalogue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "", false).Rebuild(t.Context(), twinFixtureEntries()[:1], twinFixtureInfo); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	for _, key := range []string{`"twin_series":`, `"url":`, `"size":100`, `"twin_first":4`, `"twin_last":4`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("persisted owners lack %s: %s", key, data)
		}
	}
	if strings.Contains(string(data), `non_film`) {
		t.Errorf("persisted film owner carries non_film: %s", data)
	}
	snap, _, reason, err := decodeSnapshot(data)
	if err != nil || reason != "" {
		t.Fatalf("decodeSnapshot: %v %q", err, reason)
	}
	want := ownedRelease{
		Key: "nyaa:1234567", Hash: twinFilmHash, SonarrTitle: twinFilmTitle, TwinSeries: twinSeries,
		URL: "https://nyaa.si/view/1234567", Size: 100, TwinFirst: 4, TwinLast: 4, TvdbID: 79525, IsBest: true,
	}
	if got := snap.Owners[ownerKey(21519)]; len(got) != 1 || got[0] != want {
		t.Errorf("decoded owners[21519] = %+v, want [%+v]", got, want)
	}
}

// TestDecodeSnapshotDropsAnUnservableTwin pins the decode bounds on the
// catalogue fields: a record failing one loses only those fields.
func TestDecodeSnapshotDropsAnUnservableTwin(t *testing.T) {
	valid := ownedRelease{
		Key: "nyaa:1", SonarrTitle: "S S00E04 [G]", TwinSeries: "S", URL: "https://nyaa.si/view/1", Size: 1, TwinFirst: 4, TwinLast: 4,
	}
	for name, tc := range map[string]struct {
		mutate func(r *ownedRelease)
		keep   bool
	}{
		"a servable record":          {mutate: func(*ownedRelease) {}, keep: true},
		"episode 0":                  {mutate: func(r *ownedRelease) { r.TwinFirst = 0 }},
		"episode 1000":               {mutate: func(r *ownedRelease) { r.TwinLast = 1000 }},
		"a run ending before it":     {mutate: func(r *ownedRelease) { r.TwinFirst, r.TwinLast = 5, 4 }},
		"another torrent's URL":      {mutate: func(r *ownedRelease) { r.URL = "https://nyaa.si/view/2" }},
		"a foreign host":             {mutate: func(r *ownedRelease) { r.URL = "https://evil.example/view/1" }},
		"a negative size":            {mutate: func(r *ownedRelease) { r.Size = -1 }},
		"a blank series":             {mutate: func(r *ownedRelease) { r.TwinSeries = " " }},
		"an oversized series":        {mutate: func(r *ownedRelease) { r.TwinSeries = strings.Repeat("s", maxPersistedFieldBytes+1) }},
		"no key to prove the URL by": {mutate: func(r *ownedRelease) { r.Key = "" }},
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			tc.mutate(&r)
			want := r
			if !tc.keep {
				want.TwinSeries, want.URL, want.Size, want.TwinFirst, want.TwinLast = "", "", 0, 0, 0
			}
			path := filepath.Join(t.TempDir(), "feed.json")
			writeSnapshotFile(t, path, &snapshot{Owners: ownsBy(1, r), NyaaFeed: []journalItem{}, ABFeed: []journalItem{}})
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read snapshot: %v", err)
			}
			snap, _, _, err := decodeSnapshot(data)
			if err != nil {
				t.Fatalf("decodeSnapshot: %v", err)
			}
			if got := snap.Owners[ownerKey(1)]; len(got) != 1 || got[0] != want {
				t.Errorf("decoded %s = %+v, want %+v", name, got, want)
			}
		})
	}
}

// TestSeriesQueryKeyMatchesSonarrsCleanTitle pins the catalogue key to
// Sonarr's GetCleanSceneTitle, case-folded: each want is that function's output
// for the title, so a stored series title, the clean q Sonarr sends for it and
// a raw-title q all key the same, and a word boundary keeps its "+".
func TestSeriesQueryKeyMatchesSonarrsCleanTitle(t *testing.T) {
	tests := []struct {
		title, query, want string
	}{
		{title: "Code Geass: Lelouch of the Rebellion", query: "Code Geass Lelouch of the Rebellion", want: "code+geass+lelouch+of+the+rebellion"},
		{title: "The Seven Deadly Sins", query: "Seven Deadly Sins", want: "seven+deadly+sins"},
		{title: "Kaguya-sama: Love Is War", query: "Kaguya sama Love Is War", want: "kaguya+sama+love+is+war"},
		{title: "Haikyu!!", query: "Haikyu", want: "haikyu"},
		{title: "Fruits & Vegetables", query: "Fruits and Vegetables", want: "fruits+and+vegetables"},
		{title: "JoJo's Bizarre Adventure", query: "JoJos Bizarre Adventure", want: "jojos+bizarre+adventure"},
		{title: "K-On!", query: "K On", want: "k+on"},
		{title: "Kon", query: "Kon", want: "kon"},
		{title: "Snake_Case", query: "Snake_Case", want: "snake_case"},
		{title: "Pokémon", query: "Pokemon", want: "pokemon"},
		{title: "Shūmatsu Train Doko e Iku?", query: "Shumatsu Train Doko e Iku", want: "shumatsu+train+doko+e+iku"},
		{title: "進撃の巨人", query: "進撃の巨人", want: "進撃の巨人"},
		{title: "ダンジョン飯", query: "タンション飯", want: "タンション飯"},
		{title: "Ёжик в тумане", query: "Ежик в тумане", want: "ежик+в+тумане"},
		{title: "나 혼자만 레벨업", query: "나 혼자만 레벨업", want: "나+혼자만+레벨업"},
		{title: "𠮷野家", query: "野家", want: "野家"},
	}
	for _, tc := range tests {
		if got := seriesQueryKey(tc.title); got != tc.want {
			t.Errorf("seriesQueryKey(series %q) = %q, want %q", tc.title, got, tc.want)
		}
		if got := seriesQueryKey(tc.query); got != tc.want {
			t.Errorf("seriesQueryKey(query %q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// TestServeSpecialSearchKeepsSeriesApartByTheirWordBoundary pins the wrong-series
// guard: "K-On!" and "Kon" each place a film on S00E04, and Sonarr's search for
// one gets that series' twin alone.
func TestServeSpecialSearchKeepsSeriesApartByTheirWordBoundary(t *testing.T) {
	const kOn, kon = "K-On! S00E04 1080p [PMR]", "Kon S00E04 1080p [PMR]"
	entries := twinFixtureEntries()
	entries[1].Torrents[0].Files = []seadex.File{{Length: 100, Name: "Kon the Movie (1080p) [PMR].mkv"}}
	info := func(alID int) EntryInfo {
		series := "K-On!"
		if alID == 3000 {
			series = "Kon"
		}
		return EntryInfo{Title: "The Movie", IsMovie: true, TvdbID: 79525, SpecialEpisodes: []int{4}, SpecialsEpisodes: 1, SeriesTitle: series}
	}
	ix := twinIndexer(t, entries, info, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, emptyUpstreamFeed)
	}, Config{NyaaTorznabURL: "set"})
	for q, want := range map[string]string{"K+On": kOn, "Kon": kon} {
		target := "/nyaa?t=tvsearch&q=" + q + "&season=00&ep=4&cat=5070&apikey=k"
		if got, body := serveTitles(t, ix, target); !slices.Equal(got, []string{want}) {
			t.Errorf("serve(%s) = %q, want %q alone:\n%s", target, got, want, body)
		}
	}
}

// TestServeALegacySnapshotGivesSonarrNoFilm pins the upgrade window: a snapshot
// written before the non-film fact existed, served before any pass rewrites
// it, offers its film to Radarr alone, on RSS though its journal item still
// carries Anime, and on a proxied search though its owner names no media type.
func TestServeALegacySnapshotGivesSonarrNoFilm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	twinless := func(alID int) EntryInfo {
		info := twinFixtureInfo(alID)
		info.SpecialEpisodes = nil
		return info
	}
	if err := newTestWriter(path, "", false).Rebuild(t.Context(), twinFixtureEntries()[:1], twinless); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	legacyFeedFile(t, path, func(item map[string]any) { item["Categories"] = []int{catMovies, catAnime} })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
	}))
	t.Cleanup(srv.Close)
	ix := warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "pk"}, nil, srv.Client())
	for target, want := range map[string][]string{
		"/nyaa?t=search&cat=5070,5000&apikey=k":                            {bootstrapItem().Title},
		"/nyaa?t=tvsearch&q=Lelouch+of+the+Resurrection&cat=5070&apikey=k": nil,
		"/nyaa?t=search&cat=2000&apikey=k":                                 {"Lelouch of the Resurrection (2019) 1080p [PMR]"},
		"/nyaa?t=search&q=Lelouch+of+the+Resurrection&cat=2000&apikey=k":   {"[Group] Some Anime S01 [1080p]"},
	} {
		if got, body := serveTitles(t, ix, target); !slices.Equal(got, want) {
			t.Errorf("serve(%s) over a legacy snapshot = %q, want %q:\n%s", target, got, want, body)
		}
	}
}

func legacyFeedFile(t *testing.T, path string, edit func(item map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	for _, releases := range snap["owners"].(map[string]any) {
		for _, r := range releases.([]any) {
			delete(r.(map[string]any), "non_film")
		}
	}
	for _, feed := range []string{"nyaa_feed", "ab_feed"} {
		for _, it := range snap[feed].([]any) {
			delete(it.(map[string]any), "NonFilm")
			edit(it.(map[string]any))
		}
	}
	out, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

// TestServeCatalogueTwinKeepsTheEpochAcrossSnapshots pins the catalogue twin's
// pubDate to the epoch: the snapshot is rewritten every pass, so a date taken
// from it would republish an old release as new on each one.
func TestServeCatalogueTwinKeepsTheEpochAcrossSnapshots(t *testing.T) {
	ix := twinFixtureIndexer(t, twinFixtureEntries(), emptyUpstreamFeed, Config{NyaaTorznabURL: "set"})
	const special = "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k"
	const epoch = "<pubDate>Thu, 01 Jan 1970 00:00:00 +0000</pubDate>"
	for generation := range 2 {
		if generation > 0 {
			if err := newTestWriter(ix.cache.path, "", false).Rebuild(t.Context(), twinFixtureEntries(), twinFixtureInfo); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(ix.cache.path, later, later); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
			tick(ix)
		}
		got, body := serveTitles(t, ix, special)
		if !slices.Equal(got, []string{twinFilmTitle}) || !strings.Contains(body, epoch) {
			t.Errorf("serve(special S00E04) on snapshot %d = %q, want the film twin dated %s:\n%s", generation, got, epoch, body)
		}
	}
}

// TestServeSpecialAnswerSurvivesTheItemCap pins that a full page of unrelated
// results cannot crowd the placed twin out of a special search, whether the
// catalogue supplies it or the tracker returned it last.
func TestServeSpecialAnswerSurvivesTheItemCap(t *testing.T) {
	series := seadex.Entry{AniListID: 4000, Torrents: []seadex.Torrent{{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/5555555", ReleaseGroup: "G",
		Files: []seadex.File{{Length: 100, Name: "[G] Some Anime - 01 (1080p).mkv"}},
	}}}
	info := func(alID int) EntryInfo {
		if alID == series.AniListID {
			return EntryInfo{Title: "Some Anime", TvdbID: 1}
		}
		return twinFixtureInfo(alID)
	}
	entries := append(twinFixtureEntries()[:1], series)
	for name, tc := range map[string]struct {
		target    string
		unrelated int
		withFilm  bool
	}{
		"from the catalogue":      {target: "/nyaa?t=tvsearch&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&cat=5070&apikey=k", unrelated: maxItems},
		"from the tracker's tail": {target: "/nyaa?t=search&q=Code+Geass+Lelouch+of+the+Rebellion&season=00&ep=4&apikey=k", unrelated: maxItems - 1, withFilm: true},
	} {
		t.Run(name, func(t *testing.T) {
			ix := twinIndexer(t, entries, info, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w, cappedUpstreamFeed("http://"+r.Host, tc.unrelated, tc.withFilm))
			}, Config{NyaaTorznabURL: "set"})
			rec := httptest.NewRecorder()
			ix.serve(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			items, err := parseTorznab(rec.Body.Bytes())
			if err != nil {
				t.Fatalf("parseTorznab(%s): %v", tc.target, err)
			}
			if len(items) != maxItems {
				t.Errorf("serve(%s) returned %d items, want the cap %d", tc.target, len(items), maxItems)
			}
			i := slices.IndexFunc(items, func(it item) bool { return it.Title == twinFilmTitle })
			if i < 0 {
				t.Fatalf("serve(%s) dropped the placed twin %q", tc.target, twinFilmTitle)
			}
			if tc.withFilm && items[i].Seeders != 42 {
				t.Errorf("serve(%s) twin seeders = %d, want the tracker copy's 42", tc.target, items[i].Seeders)
			}
		})
	}
}

func cappedUpstreamFeed(host string, n int, film bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><title>Nyaa.si</title>`)
	for i := range n {
		fmt.Fprintf(&b, `<item><title>[G] Some Anime - 01 alias %[1]d</title><guid>https://nyaa.si/view/5555555?alias=%[1]d</guid>`+
			`<comments>https://nyaa.si/view/5555555</comments><pubDate>Mon, 06 Jul 2026 12:00:00 +0000</pubDate><size>100</size>`+
			`<enclosure url="%[2]s/1/download?link=%[1]d" length="100" type="application/x-bittorrent"/>`+
			`<torznab:attr name="category" value="5070"/><torznab:attr name="seeders" value="1"/></item>`, i, host)
	}
	if film {
		s := strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", host)
		b.WriteString(s[strings.Index(s, "<item>"):strings.Index(s, "</channel>")])
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// TestServeAnswersASpecialSearchForANonLatinSeries pins a series titled with
// no Latin letter at all, which Sonarr sends as is.
func TestServeAnswersASpecialSearchForANonLatinSeries(t *testing.T) {
	info := func(alID int) EntryInfo {
		e := twinFixtureInfo(alID)
		e.SeriesTitle = "進撃の巨人"
		return e
	}
	ix := twinIndexer(t, twinFixtureEntries()[:1], info, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, emptyUpstreamFeed)
	}, Config{NyaaTorznabURL: "set"})
	target := "/nyaa?t=tvsearch&q=" + url.QueryEscape("進撃の巨人") + "&season=00&ep=4&cat=5070&apikey=k"
	if got, body := serveTitles(t, ix, target); !slices.Equal(got, []string{"進撃の巨人 S00E04 1080p [PMR]"}) {
		t.Errorf("serve(%s) = %q, want the film twin:\n%s", target, got, body)
	}
}
