package indexer

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// The arrs' own add/test requests (servarr's NewznabRequestGenerator for Sonarr
// and Radarr, Prowlarr's HttpIndexerBase test): the shapes the placeholder
// exists to satisfy.
const (
	sonarrTestQuery   = "t=tvsearch&cat=5000,5070&extended=1&offset=0&limit=100"
	radarrTestQuery   = "t=movie&cat=2000&extended=1&offset=0&limit=100"
	prowlarrTestQuery = "t=search&extended=1"
)

// servarrFeed decodes a served feed the way servarr's TorznabRssParser reads it:
// item children by local name, and every torznab:attr by the Torznab namespace.
type servarrFeed struct {
	Items []servarrItem `xml:"channel>item"`
}

type servarrItem struct {
	Title      string             `xml:"title"`
	GUID       string             `xml:"guid"`
	PubDate    string             `xml:"pubDate"`
	Enclosures []servarrEnclosure `xml:"enclosure"`
	Attrs      []servarrAttr      `xml:"http://torznab.com/schemas/2015/feed attr"`
}

type servarrEnclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type servarrAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (it *servarrItem) attrs(name string) []string {
	var out []string
	for _, a := range it.Attrs {
		if a.Name == name {
			out = append(out, a.Value)
		}
	}
	return out
}

// emptyRSSUpstream is a Prowlarr stand-in answering every search with a valid,
// empty Torznab feed, so a search request is answered rather than faulted.
func emptyRSSUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title></channel></rss>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// freshNyaaIndexer is a warmed Nyaa-only server over an absent snapshot, the
// fresh-install state whose journal is empty by design.
func freshNyaaIndexer(t *testing.T, cfg *Config, log *slog.Logger) *Indexer {
	t.Helper()
	srv := emptyRSSUpstream(t)
	if cfg.SnapshotPath == "" {
		cfg.SnapshotPath = filepath.Join(t.TempDir(), "feed.json")
	}
	if cfg.NyaaTorznabURL == "" {
		cfg.NyaaTorznabURL = srv.URL
	}
	cfg.APIKey, cfg.ProwlarrAPIKey = "k", "pk"
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return warmedIndexer(cfg, log, srv.Client())
}

// serveFeed sends one request through the production handler chain and decodes
// the body as servarr would; a body that is not a feed fails the test.
func serveFeed(t *testing.T, ix *Indexer, path, query string) servarrFeed {
	t.Helper()
	body, code := serveRaw(ix, path, query)
	if code != http.StatusOK {
		t.Fatalf("GET %s?%s = %d, want 200:\n%s", path, query, code, body)
	}
	var feed servarrFeed
	if err := xml.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatalf("decode %s?%s: %v\n%s", path, query, err, body)
	}
	return feed
}

func serveRaw(ix *Indexer, path, query string) (body string, code int) {
	rec := httptest.NewRecorder()
	target := path + "?apikey=k"
	if query != "" {
		target += "&" + query
	}
	ix.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Body.String(), rec.Code
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("url.ParseQuery(%q): %v", raw, err)
	}
	return q
}

func placeholderCount(feed servarrFeed) int {
	n := 0
	for i := range feed.Items {
		if feed.Items[i].GUID == bootstrapGUID {
			n++
		}
	}
	return n
}

// placeholderTitleIdentity matches anything in a title the arrs' parsers read as
// release identity: a digit (episode, season, year, resolution), a bracketed
// group or tag, or a special/OVA token.
var placeholderTitleIdentity = regexp.MustCompile(`(?i)[0-9\[\]()]|\b(specials?|ova|ona|oad)\b`)

func realItem(guid string, cats ...int) journalItem {
	return journalItem{Title: "Show S01E01 1080p [G]", GUID: guid, DownloadURL: "https://nyaa.si/download/" + guid + ".torrent", Categories: cats}
}

// TestServePlaceholderParsesLikeServarr pins the wire shape the placeholder's
// whole purpose depends on, decoded the way every servarr parser decodes it: an
// item without an absolute enclosure URL is dropped by the parser (so the add/test
// sees zero releases), a category outside the request's is filtered away, and a
// parseable title, an id, a marker or a positive seeder count would let an arr
// map or grab it.
func TestServePlaceholderParsesLikeServarr(t *testing.T) {
	tests := map[string]struct {
		path, query string
		cfg         Config
	}{
		"sonarr add test":       {path: "/nyaa", query: sonarrTestQuery},
		"radarr add test":       {path: "/nyaa", query: radarrTestQuery},
		"prowlarr add test":     {path: "/nyaa", query: prowlarrTestQuery},
		"animebytes with a key": {path: "/ab", query: prowlarrTestQuery, cfg: Config{ABPasskey: "pk"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := tc.cfg
			if tc.path == "/ab" {
				cfg.ABTorznabURL = "http://prowlarr.invalid/2/api"
			}
			feed := serveFeed(t, freshNyaaIndexer(t, &cfg, nil), tc.path, tc.query)
			if len(feed.Items) != 1 {
				t.Fatalf("%s served %d items, want exactly the placeholder", tc.query, len(feed.Items))
			}
			it := feed.Items[0]
			if it.GUID != bootstrapGUID {
				t.Errorf("guid = %q, want %q", it.GUID, bootstrapGUID)
			}
			if it.Title == "" || placeholderTitleIdentity.MatchString(it.Title) {
				t.Errorf("title = %q, want a non-empty title with no digit, bracket or special/OVA token an arr could parse into a release", it.Title)
			}
			if pub, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil || !pub.Equal(time.Unix(0, 0)) {
				t.Errorf("pubDate = %q (parse error %v), want the RFC1123Z epoch", it.PubDate, err)
			}
			if len(it.Enclosures) != 1 {
				t.Fatalf("enclosures = %+v, want exactly one: servarr drops an item without one", it.Enclosures)
			}
			enc := it.Enclosures[0]
			u, err := url.Parse(enc.URL)
			if err != nil || !u.IsAbs() || u.Host == "" || !strings.HasSuffix(u.Hostname(), ".invalid") {
				t.Errorf("enclosure url = %q (parse error %v), want an absolute URL on a .invalid host", enc.URL, err)
			}
			if enc.Type != "application/x-bittorrent" {
				t.Errorf("enclosure type = %q, want application/x-bittorrent", enc.Type)
			}
			if _, err := strconv.ParseInt(enc.Length, 10, 64); err != nil {
				t.Errorf("enclosure length = %q, want an int64: servarr drops an enclosure whose length does not parse: %v", enc.Length, err)
			}
			cats := it.attrs("category")
			if len(cats) == 0 {
				t.Errorf("no category attr, want at least one: Prowlarr refuses a category-less release")
			}
			requested := parseCats(mustQuery(t, tc.query).Get("cat"))
			matched := len(requested) == 0
			for _, c := range cats {
				if c != strconv.Itoa(catAnime) && c != strconv.Itoa(catMovies) {
					t.Errorf("category %q, want only the caps-served %d and %d", c, catAnime, catMovies)
				}
				// Prowlarr's relay keeps a result only when a category matches the
				// request's, directly or through its parent.
				if n, err := strconv.Atoi(c); err == nil && (requested[n] || requested[n-n%1000]) {
					matched = true
				}
			}
			if !matched {
				t.Errorf("categories %v match none of the requested %v, so a category-filtering relay drops the placeholder", cats, requested)
			}
			if size := it.attrs("size"); len(size) != 1 {
				t.Errorf("size attrs = %v, want one", size)
			} else if _, err := strconv.ParseInt(size[0], 10, 64); err != nil {
				t.Errorf("size = %q, want an int64: %v", size[0], err)
			}
			for _, name := range []string{"seeders", "peers"} {
				if got := it.attrs(name); !slices.Equal(got, []string{"0"}) {
					t.Errorf("%s = %v, want [0] so the minimum-seeders rule rejects it", name, got)
				}
			}
			for _, name := range []string{"tvdbid", "imdbid", "infohash", "downloadvolumefactor", "uploadvolumefactor", "tag"} {
				if got := it.attrs(name); len(got) != 0 {
					t.Errorf("%s attr = %v, want none on the placeholder", name, got)
				}
			}
		})
	}
}

// TestServePlaceholderRequestMatrix walks every request shape an empty journal
// can meet through the production handler, and the request log's placeholder
// flag beside each: only an answered feed request whose category filter leaves
// nothing gets the placeholder, and only on the first page.
func TestServePlaceholderRequestMatrix(t *testing.T) {
	tests := map[string]struct {
		path, query     string
		wantPlaceholder bool
	}{
		"anime category":                    {path: "/nyaa", query: "cat=5070", wantPlaceholder: true},
		"movies category":                   {path: "/nyaa", query: "cat=2000", wantPlaceholder: true},
		"tv parent category":                {path: "/nyaa", query: "cat=5000", wantPlaceholder: true},
		"a category the app never serves":   {path: "/nyaa", query: "cat=5030,5040"},
		"first page":                        {path: "/nyaa", query: "offset=0&limit=100", wantPlaceholder: true},
		"second page":                       {path: "/nyaa", query: "offset=1&limit=100"},
		"far page":                          {path: "/nyaa", query: "offset=100&limit=100"},
		"a search":                          {path: "/nyaa", query: "t=tvsearch&q=Frieren&season=1"},
		"a whitespace-only query is a feed": {path: "/nyaa", query: "q=%20", wantPlaceholder: true},
		"a skipped per-episode search":      {path: "/nyaa", query: "t=tvsearch&ep=1"},
		"a disabled scope stays silent":     {path: "/ab", query: prowlarrTestQuery},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log, rec := capture.New()
			ix := freshNyaaIndexer(t, &Config{}, log)
			body, code := serveRaw(ix, tc.path, tc.query)
			if code != http.StatusOK {
				t.Fatalf("GET %s?%s = %d, want 200:\n%s", tc.path, tc.query, code, body)
			}
			var feed servarrFeed
			if err := xml.Unmarshal([]byte(body), &feed); err != nil {
				t.Fatalf("decode: %v\n%s", err, body)
			}
			want := 0
			if tc.wantPlaceholder {
				want = 1
			}
			if len(feed.Items) != want || placeholderCount(feed) != want {
				t.Errorf("GET %s?%s = %d items (%d placeholder), want %d placeholder only", tc.path, tc.query, len(feed.Items), placeholderCount(feed), want)
			}
			if !rec.HasAttr("indexer request", "placeholder", strconv.FormatBool(tc.wantPlaceholder)) {
				got, _ := rec.AttrValue("indexer request", "placeholder")
				t.Errorf("request log placeholder = %q, want %t", got, tc.wantPlaceholder)
			}
			if !rec.HasAttr("indexer request", "curated", "0") {
				got, _ := rec.AttrValue("indexer request", "curated")
				t.Errorf("request log curated = %q, want 0: the placeholder is not a curated release", got)
			}
		})
	}
}

// TestServePlaceholderGates pins the three non-feed answers an empty journal
// must keep: a wrong apikey is refused before any feed is built, t=caps answers
// the caps document alone, and an enabled AnimeBytes scope with no passkey
// answers a Torznab error with its reason rather than a placeholder that would
// let the save-test pass on a feed that can never carry a link.
func TestServePlaceholderGates(t *testing.T) {
	ix := freshNyaaIndexer(t, &Config{ABTorznabURL: "http://prowlarr.invalid/2/api"}, nil)

	rec := httptest.NewRecorder()
	ix.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nyaa?apikey=wrong&"+prowlarrTestQuery, nil))
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), bootstrapGUID) {
		t.Errorf("wrong apikey = %d %s, want 401 with no placeholder", rec.Code, rec.Body.String())
	}

	body, code := serveRaw(ix, "/nyaa", "t=caps")
	if code != http.StatusOK || !strings.Contains(body, "<caps>") || strings.Contains(body, "<item>") {
		t.Errorf("t=caps = %d %s, want 200 with a caps document and no item", code, body)
	}

	body, code = serveRaw(ix, "/ab", prowlarrTestQuery)
	if code != http.StatusOK || !strings.Contains(body, "<error") || !strings.Contains(body, "passkey") || strings.Contains(body, bootstrapGUID) {
		t.Errorf("/ab without a passkey = %d %s, want a Torznab passkey error and no placeholder", code, body)
	}
}

// TestServePlaceholderCoversAnEmptyCategory pins that the placeholder answers
// per category, not per journal: a journal holding only one arr's category
// still has to let the other arr save the indexer, and a request the real items
// satisfy must never see it.
func TestServePlaceholderCoversAnEmptyCategory(t *testing.T) {
	tests := map[string]struct {
		journal         []int
		query           string
		wantPlaceholder bool
		wantReal        int
	}{
		"radarr against a series-only journal":   {journal: []int{catAnime}, query: radarrTestQuery, wantPlaceholder: true},
		"sonarr against a series-only journal":   {journal: []int{catAnime}, query: sonarrTestQuery, wantReal: 2},
		"sonarr against a films-only journal":    {journal: []int{catMovies}, query: sonarrTestQuery, wantPlaceholder: true},
		"radarr against a films-only journal":    {journal: []int{catMovies}, query: radarrTestQuery, wantReal: 2},
		"prowlarr against a series-only journal": {journal: []int{catAnime}, query: prowlarrTestQuery, wantReal: 2},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ix := freshNyaaIndexer(t, &Config{}, nil)
			seedJournal(t, ix, upstreamNyaa, realItem("https://nyaa.si/view/1", tc.journal...), realItem("https://nyaa.si/view/2", tc.journal...))
			feed := serveFeed(t, ix, "/nyaa", tc.query)
			wantPlaceholders := 0
			if tc.wantPlaceholder {
				wantPlaceholders = 1
			}
			if got := placeholderCount(feed); got != wantPlaceholders || len(feed.Items)-got != tc.wantReal {
				t.Errorf("%s = %d real + %d placeholder, want %d real + %d placeholder", tc.query, len(feed.Items)-got, got, tc.wantReal, wantPlaceholders)
			}
		})
	}
}

// TestServePlaceholderNeverMixesWithRealItems pins that the decision reads the
// list the request would otherwise get, after the film twin expansion and the
// category filter: one stored film expands to two wire items, each surviving a
// different category, and none of the three requests may append the placeholder
// beside a real item.
func TestServePlaceholderNeverMixesWithRealItems(t *testing.T) {
	film := realItem("https://nyaa.si/view/9", catMovies)
	film.SonarrTitle, film.SonarrGUID = "Show S00E01 1080p [G]", "https://nyaa.si/view/9#sonarr"
	tests := map[string]struct {
		query    string
		wantGUID []string
	}{
		"no category":    {query: "", wantGUID: []string{"https://nyaa.si/view/9", "https://nyaa.si/view/9#sonarr"}},
		"anime category": {query: "cat=5070", wantGUID: []string{"https://nyaa.si/view/9#sonarr"}},
		"movie category": {query: "cat=2000", wantGUID: []string{"https://nyaa.si/view/9"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ix := freshNyaaIndexer(t, &Config{}, nil)
			seedJournal(t, ix, upstreamNyaa, film)
			feed := serveFeed(t, ix, "/nyaa", tc.query)
			var got []string
			for i := range feed.Items {
				got = append(got, feed.Items[i].GUID)
			}
			if !slices.Equal(got, tc.wantGUID) {
				t.Errorf("cat=%q served %v, want %v", tc.query, got, tc.wantGUID)
			}
		})
	}
}

// TestServePlaceholderRearmsAfterTheJournalExpires drives the writer and the
// reload path end to end: the placeholder leaves as soon as a real item is
// journaled and returns when that item ages out, because the state that decides
// it is the served list and nothing remembered.
func TestServePlaceholderRearmsAfterTheJournalExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	ix := freshNyaaIndexer(t, &Config{SnapshotPath: path}, nil)
	if got := placeholderCount(serveFeed(t, ix, "/nyaa", sonarrTestQuery)); got != 1 {
		t.Fatalf("seeded empty journal served %d placeholders, want 1", got)
	}

	w := newTestWriter(path, "", false)
	t0 := time.Now().UTC()
	w.now = func() time.Time { return t0 }
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	tick(ix)
	feed := serveFeed(t, ix, "/nyaa", sonarrTestQuery)
	if len(feed.Items) != 1 || placeholderCount(feed) != 0 {
		t.Fatalf("after journaling one release served %+v, want that release and no placeholder", feed.Items)
	}

	w.now = func() time.Time { return t0.Add(feedJournalMaxAge + time.Hour) }
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("expiry Rebuild: %v", err)
	}
	tick(ix)
	feed = serveFeed(t, ix, "/nyaa", sonarrTestQuery)
	if len(feed.Items) != 1 || placeholderCount(feed) != 1 {
		t.Errorf("after the release aged out served %+v, want the lone placeholder back", feed.Items)
	}
}

// TestServePlaceholderUnderConcurrentReload races feed requests against a reload
// clock that flips the journal between empty and populated. Every response must
// come from ONE generation of the snapshot: the lone placeholder, or real items
// with no placeholder. An empty response or a mixed one means the decision read
// the journal twice.
func TestServePlaceholderUnderConcurrentReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	seedEmptyFeed(t, path)
	w := newTestWriter(path, "", false)
	entries := []seadex.Entry{
		nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv"),
		nyaaEntry(8, 43, true, "Other - S01E01 (1080p) [G].mkv"),
	}
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	populated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read populated snapshot: %v", err)
	}
	install := func(data []byte) {
		tmp := filepath.Join(dir, "feed.json.tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			t.Errorf("stage snapshot: %v", err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Errorf("install snapshot: %v", err)
		}
	}
	install([]byte(emptyFeedJSON))
	ix := freshNyaaIndexer(t, &Config{SnapshotPath: path}, nil)

	done := make(chan struct{})
	var reloads sync.WaitGroup
	reloads.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			if i%2 == 0 {
				install(populated)
			} else {
				install([]byte(emptyFeedJSON))
			}
			tick(ix)
		}
	})

	var requests sync.WaitGroup
	var placeholders, reals atomic.Bool
	for g := range 8 {
		requests.Go(func() {
			for r := range 200 {
				body, code := serveRaw(ix, "/nyaa", sonarrTestQuery)
				var feed servarrFeed
				if code != http.StatusOK || xml.Unmarshal([]byte(body), &feed) != nil {
					t.Errorf("request %d/%d = %d, want a decodable feed:\n%s", g, r, code, body)
					return
				}
				n := placeholderCount(feed)
				switch {
				case len(feed.Items) == 0:
					t.Errorf("request %d/%d served an empty feed, want the placeholder or real items", g, r)
					return
				case n == 0:
					reals.Store(true)
				case n == 1 && len(feed.Items) == 1:
					placeholders.Store(true)
				default:
					t.Errorf("request %d/%d mixed %d placeholder(s) into %d items", g, r, n, len(feed.Items))
					return
				}
			}
		})
	}
	requests.Wait()
	close(done)
	reloads.Wait()

	if !placeholders.Load() || !reals.Load() {
		t.Errorf("responses saw placeholder=%t real=%t, want both generations served, or the race was never exercised", placeholders.Load(), reals.Load())
	}
}

// TestPlaceholderNeverReachesPersistence pins that serving the placeholder
// writes nothing: after it is served and a rebuild runs, no persisted fact
// (journal, ownership, publication log, title cache) carries its GUID or its
// download host.
func TestPlaceholderNeverReachesPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	ix := freshNyaaIndexer(t, &Config{SnapshotPath: path}, nil)
	if got := placeholderCount(serveFeed(t, ix, "/nyaa", sonarrTestQuery)); got != 1 {
		t.Fatalf("served %d placeholders, want 1 before the rebuild", got)
	}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: "http://prowlarr/1/api", Server: ix}, nil, nil)
	if err := w.Rebuild(t.Context(), []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	for _, leak := range []string{bootstrapGUID, "seadex-scout.invalid"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("persisted snapshot carries %q:\n%s", leak, raw)
		}
	}
	tick(ix)
	if got := placeholderCount(serveFeed(t, ix, "/nyaa", sonarrTestQuery)); got != 0 {
		t.Errorf("after the rebuild served %d placeholders, want 0", got)
	}
}

// TestServeSummaryLineReportsThePlaceholder pins the request log on a feed path
// that holds real items: the placeholder is reported only when it is served, and
// curated counts the real items, never the placeholder.
func TestServeSummaryLineReportsThePlaceholder(t *testing.T) {
	tests := map[string]struct {
		journal []journalItem
		want    map[string]string
	}{
		"real items": {
			journal: []journalItem{realItem("https://nyaa.si/view/1", catAnime), realItem("https://nyaa.si/view/2", catAnime)},
			want:    map[string]string{"feed": "true", "curated": "2", "placeholder": "false", "returned": "2"},
		},
		"real items in the other category": {
			journal: []journalItem{realItem("https://nyaa.si/view/1", catMovies)},
			want:    map[string]string{"feed": "true", "curated": "1", "placeholder": "true", "returned": "1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log, rec := capture.New()
			ix := freshNyaaIndexer(t, &Config{}, log)
			seedJournal(t, ix, upstreamNyaa, tc.journal...)
			serveFeed(t, ix, "/nyaa", sonarrTestQuery)
			for key, want := range tc.want {
				if !rec.HasAttr("indexer request", key, want) {
					got, _ := rec.AttrValue("indexer request", key)
					t.Errorf("request summary %s = %q, want %q", key, got, want)
				}
			}
		})
	}
}
