package indexer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// sampleFeed is a representative Prowlarr per-indexer Torznab response (one Nyaa
// item), used to verify the parser handles the namespaced torznab:attr elements
// and the enclosure/comments fields.
const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>Nyaa.si</title>
    <item>
      <title>[Group] Some Anime S01 [1080p]</title>
      <guid>https://nyaa.si/view/1234567</guid>
      <comments>https://nyaa.si/view/1234567</comments>
      <pubDate>Mon, 06 Jul 2026 12:00:00 +0000</pubDate>
      <size>14352012572</size>
      <link>http://prowlarr:9696/1/download?apikey=x&amp;link=abc</link>
      <enclosure url="http://prowlarr:9696/1/download?apikey=x&amp;link=abc" length="14352012572" type="application/x-bittorrent"/>
      <torznab:attr name="category" value="5070"/>
      <torznab:attr name="seeders" value="42"/>
      <torznab:attr name="peers" value="50"/>
      <torznab:attr name="infohash" value="ABCDEF1234567890abcdef1234567890abcdef12"/>
      <torznab:attr name="downloadvolumefactor" value="1"/>
    </item>
  </channel>
</rss>`

func TestParseTorznab(t *testing.T) {
	items, err := parseTorznab([]byte(sampleFeed))
	if err != nil {
		t.Fatalf("parseTorznab: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	it := items[0]

	if it.Title != "[Group] Some Anime S01 [1080p]" {
		t.Errorf("title = %q", it.Title)
	}
	if it.InfoURL != "https://nyaa.si/view/1234567" {
		t.Errorf("infoURL = %q", it.InfoURL)
	}
	// The torznab:attr namespaced elements must be captured.
	if it.InfoHash != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Errorf("infohash = %q (torznab:attr not parsed?)", it.InfoHash)
	}
	if it.Seeders != 42 {
		t.Errorf("seeders = %d, want 42", it.Seeders)
	}
	if it.Leechers != 8 { // peers 50 - seeders 42
		t.Errorf("leechers = %d, want 8", it.Leechers)
	}
	if it.Size != 14352012572 {
		t.Errorf("size = %d", it.Size)
	}
	if len(it.Categories) != 1 || it.Categories[0] != 5070 {
		t.Errorf("categories = %v, want [5070]", it.Categories)
	}
	if it.DownloadURL != "http://prowlarr:9696/1/download?apikey=x&link=abc" {
		t.Errorf("downloadURL = %q", it.DownloadURL)
	}
	if it.PubDate.IsZero() {
		t.Error("pubDate not parsed")
	}
}

func TestExtractID(t *testing.T) {
	tests := []struct {
		url, needle, want string
	}{
		{"https://nyaa.si/view/1234567", "/view/", "1234567"},
		{"https://nyaa.si/view/1234567?x=1", "/view/", "1234567"},
		{"https://nyaa.si/view/1234567#c", "/view/", "1234567"},
		{"/torrents.php?id=70543&torrentid=1143533", "torrentid=", "1143533"},
		{"/torrents.php?id=70543&torrentid=1143533&x=1", "torrentid=", "1143533"},
		{"https://nyaa.si/view/abc", "/view/", ""},    // non-numeric rejected
		{"https://nyaa.si/view/12a45", "/view/", ""},  // non-numeric rejected
		{"https://example.com/other/1", "/view/", ""}, // needle absent
		{"", "/view/", ""}, // empty
	}
	for _, tc := range tests {
		if got := extractID(tc.url, tc.needle); got != tc.want {
			t.Errorf("extractID(%q,%q) = %q, want %q", tc.url, tc.needle, got, tc.want)
		}
	}
}

func TestTrackerKey(t *testing.T) {
	if got := trackerKey("Nyaa", "https://nyaa.si/view/1234567"); got != "nyaa:1234567" {
		t.Errorf("nyaa trackerKey = %q", got)
	}
	if got := trackerKey("AB", "/torrents.php?id=70543&torrentid=1143533"); got != "ab:1143533" {
		t.Errorf("ab trackerKey = %q", got)
	}
	if got := trackerKeyFromURL("https://nyaa.si/view/1234567"); got != "nyaa:1234567" {
		t.Errorf("nyaa trackerKeyFromURL = %q", got)
	}
	if got := trackerKeyFromURL("https://animebytes.tv/torrents.php?id=70543&torrentid=1143533"); got != "ab:1143533" {
		t.Errorf("ab trackerKeyFromURL = %q", got)
	}
	if got := trackerKeyFromURL("https://example.com/x/1"); got != "" {
		t.Errorf("unknown host trackerKeyFromURL = %q, want empty", got)
	}
	// Misleading hosts that embed a tracker name as a substring must NOT be
	// keyed: a host-substring match would have accepted these and let a
	// tracker-controlled URL bypass the SeaDex curation gate.
	if got := trackerKeyFromURL("https://notnyaa.example/view/1234567"); got != "" {
		t.Errorf("misleading nyaa host trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://example.com/torrent/1167293/group?tracker=animebytes"); got != "" {
		t.Errorf("misleading animebytes URL trackerKeyFromURL = %q, want empty", got)
	}
}

func TestMarkAndDedupe(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"ab:1143533": false},
	}
	raw := []item{
		{Title: "best by hash", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g1"},
		{Title: "alt by key", InfoURL: "https://animebytes.tv/torrents.php?id=1&torrentid=1143533", GUID: "g2"},
		{Title: "not curated", InfoURL: "https://nyaa.si/view/999", GUID: "g3"},
		{Title: "dup of best", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g1"},
	}
	out := markAndDedupe(raw, set)
	if len(out) != 2 {
		t.Fatalf("got %d items, want 2 (best + alt, dup dropped, uncurated dropped)", len(out))
	}
	if out[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("best marker = %q, want %q", out[0].DownloadVolumeFactor, dvfBest)
	}
	if out[1].DownloadVolumeFactor != dvfAlt {
		t.Errorf("alt marker = %q, want %q", out[1].DownloadVolumeFactor, dvfAlt)
	}
}

// TestIndexerEndToEnd exercises the writer/server split end to end: the compare
// cycle (FeedWriter) builds + persists the feed snapshot from a SeaDex entry,
// and the server loads it and answers both a real search (proxy Prowlarr ->
// parse -> match against the loaded curation set -> mark) and an empty-q RSS
// check (served from the loaded synthesized feed).
func TestIndexerEndToEnd(t *testing.T) {
	// One SeaDex entry with a best Nyaa torrent matching the sample feed's info
	// hash. A multi-episode season pack (two episode files), so the synthesized
	// RSS feed collapses its title to the season.
	entries := []seadex.Entry{{
		AniListID: 123,
		Torrents: []seadex.Torrent{{
			Tracker:      "Nyaa",
			URL:          "https://nyaa.si/view/1234567",
			InfoHash:     "ABCDEF1234567890abcdef1234567890abcdef12",
			IsBest:       true,
			ReleaseGroup: "PMR",
			Files: []seadex.File{
				{Length: 100, Name: "Some Anime - S01E01 (BD Remux 1080p) [PMR].mkv"},
				{Length: 100, Name: "Some Anime - S01E02 (BD Remux 1080p) [PMR].mkv"},
			},
		}},
	}}

	// The compare cycle builds + persists the feed snapshot; the server reads it.
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := NewFeedWriter("", path, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Mock Prowlarr Torznab: returns the sample feed regardless of query.
	var gotAPIKey string
	torznabSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, sampleFeed)
	}))
	defer torznabSrv.Close()

	ix := New(&Config{
		NyaaTorznabURL: torznabSrv.URL,
		ProwlarrAPIKey: "prowlarr-key",
	}, Deps{HTTP: torznabSrv.Client()}, path)

	// A real search (non-empty q) filters to the curation set loaded from the
	// snapshot: the sample item matches by info hash, gets the best marker, and
	// its real seeders pass through.
	items, stats := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Some Anime"}}, "nyaa")
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if !stats.answered || stats.feed || stats.upstream != 1 || stats.curated != 1 {
		t.Errorf("stats = %+v, want answered, not feed, upstream 1, curated 1", stats)
	}
	if items[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best)", items[0].DownloadVolumeFactor, dvfBest)
	}
	if items[0].Seeders != 42 {
		t.Errorf("seeders passed through = %d, want 42", items[0].Seeders)
	}
	if gotAPIKey != "prowlarr-key" {
		t.Errorf("upstream X-Api-Key = %q, want prowlarr-key", gotAPIKey)
	}

	// Per-tracker scoping (real search): the nyaa scope hits the only configured
	// upstream; the ab scope has none, so it serves nothing.
	if got, _ := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Some Anime"}}, "nyaa"); len(got) != 1 {
		t.Errorf("nyaa scope returned %d items, want 1", len(got))
	}
	if got, _ := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Some Anime"}}, "ab"); len(got) != 0 {
		t.Errorf("ab scope returned %d items, want 0 (no ab upstream)", len(got))
	}

	// The synthesized RSS feed is served from the loaded snapshot, independent of
	// the live search path: an empty-q request (an RSS "latest" fetch, or
	// Prowlarr's save test) returns the curated Nyaa release, its title collapsed
	// to the season, a directly-built .torrent link, and the best marker.
	got, st := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa")
	if len(got) != 1 || !st.feed {
		t.Fatalf("empty-q feed returned %d items (feed=%v), want 1 synthesized item", len(got), st.feed)
	}
	if got[0].Title != "Some Anime - S01 (BD Remux 1080p) [PMR]" {
		t.Errorf("synthesized title = %q, want the season-collapsed title", got[0].Title)
	}
	if got[0].DownloadURL != "https://nyaa.si/download/1234567.torrent" {
		t.Errorf("synthesized download URL = %q, want the public Nyaa .torrent link", got[0].DownloadURL)
	}
	if got[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("synthesized marker = %q, want %q (best)", got[0].DownloadVolumeFactor, dvfBest)
	}
	// (Dropping an uncurated Prowlarr result is covered directly by
	// TestMarkAndDedupe; the mock here returns the curated item for any query.)
}

// TestFeedWriterReload verifies the server picks up a newer snapshot the writer
// persists after the server started (the cross-process poll -> resident daemon
// path): an initially-absent snapshot serves an empty feed, and once the writer
// writes one the server reloads it on the next request.
func TestFeedWriterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	ix := New(&Config{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}, Deps{}, path)

	// No snapshot yet: the empty-q feed serves nothing.
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 0 {
		t.Fatalf("pre-write feed = %d items, want 0", len(got))
	}

	// A cycle (here, the writer) persists a snapshot; the next request reloads it.
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: "aa" + strings.Repeat("b", 38),
			IsBest: true, ReleaseGroup: "GRP",
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [GRP].mkv"}},
		}},
	}}
	if err := NewFeedWriter("", path, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got, st := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa")
	if len(got) != 1 || !st.feed {
		t.Fatalf("post-write feed = %d items (feed=%v), want 1 reloaded item", len(got), st.feed)
	}
	if got[0].DownloadURL != "https://nyaa.si/download/42.torrent" {
		t.Errorf("reloaded item download = %q", got[0].DownloadURL)
	}
}

// TestAnimeBytesMatching covers the real AB URL forms: SeaDex stores
// `...torrentid={id}` while Prowlarr's Torznab item uses `/torrent/{id}/group`.
// Both must key to the same "ab:{id}" (validated live: SeaDex torrentid 1167293
// == Prowlarr /torrent/1167293/group).
func TestAnimeBytesMatching(t *testing.T) {
	seadexURL := "/torrents.php?id=86576&torrentid=1167293"
	prowlarrComments := "https://animebytes.tv/torrent/1167293/group"
	prowlarrGUID := "https://animebytes.tv/torrent/1167293/group?nh=709E38EC"

	if got := trackerKey("AB", seadexURL); got != "ab:1167293" {
		t.Errorf("SeaDex AB key = %q, want ab:1167293", got)
	}
	if got := trackerKeyFromURL(prowlarrComments); got != "ab:1167293" {
		t.Errorf("Prowlarr AB comments key = %q, want ab:1167293", got)
	}
	if got := trackerKeyFromURL(prowlarrGUID); got != "ab:1167293" {
		t.Errorf("Prowlarr AB guid key = %q, want ab:1167293", got)
	}

	// End to end: an AB item (no info hash) matches the SeaDex set by tracker key.
	set := &curation{byHash: map[string]bool{}, byKey: map[string]bool{"ab:1167293": true}}
	raw := []item{{Title: "[Momonoki] Frieren S01", InfoURL: prowlarrComments, GUID: prowlarrGUID}}
	out := markAndDedupe(raw, set)
	if len(out) != 1 || out[0].DownloadVolumeFactor != dvfBest {
		t.Fatalf("AB item did not match/mark best: %+v", out)
	}
}

func TestServesQuery(t *testing.T) {
	serves := []url.Values{
		{"t": {"movie"}, "q": {"Totoro"}},                                       // movie
		{"t": {"search"}, "q": {"From Up on Poppy Hill 2011"}, "cat": {"2000"}}, // movie search (Movies cat) ending in a year
		{"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"1"}},                  // season pack search
		{"t": {"tvsearch"}},                     // bare tvsearch / RSS
		{"t": {"search"}},                       // RSS (empty q)
		{"t": {"search"}, "q": {"Frieren"}},     // generic series search
		{"t": {"search"}, "q": {"Frieren OVA"}}, // special
		{"t": {"caps"}},                         // (query() not called for caps, but classifies as serve)
	}
	for _, q := range serves {
		if !servesQuery(q) {
			t.Errorf("servesQuery(%v) = false, want true", q)
		}
	}

	skips := []url.Values{
		{"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"1"}, "ep": {"1"}}, // per-episode (season+ep)
		{"t": {"search"}, "q": {"Frieren 01"}},                              // anime absolute episode
		{"t": {"search"}, "q": {"One Piece 1085"}},                          // 4-digit absolute episode
	}
	for _, q := range skips {
		if servesQuery(q) {
			t.Errorf("servesQuery(%v) = true, want false (per-episode query)", q)
		}
	}
}

func TestScopeFromPath(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/", ""},         // no tracker segment -> 404
		{"/api", ""},      // bare API path, no tracker -> 404
		{"", ""},          // empty
		{"/nyaa", "nyaa"}, // per-tracker base path
		{"/nyaa/api", "nyaa"},
		{"/NYAA", "nyaa"}, // case-insensitive
		{"/ab", "ab"},
		{"/ab/api", "ab"},
		{"/about", ""}, // not the "ab" segment
	}
	for _, tc := range tests {
		if got := scopeFromPath(tc.path); got != tc.want {
			t.Errorf("scopeFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestUpstreamsForScope(t *testing.T) {
	nyaa := &upstream{name: "nyaa"}
	ab := &upstream{name: "ab"}
	all := []*upstream{nyaa, ab}

	if got := upstreamsForScope(all, ""); len(got) != 0 {
		t.Errorf("empty scope: got %d upstreams, want 0 (no combined feed)", len(got))
	}
	if got := upstreamsForScope(all, "nyaa"); len(got) != 1 || got[0] != nyaa {
		t.Errorf("scope nyaa: got %v, want [nyaa]", got)
	}
	if got := upstreamsForScope(all, "ab"); len(got) != 1 || got[0] != ab {
		t.Errorf("scope ab: got %v, want [ab]", got)
	}
}

func TestScopeFromHost(t *testing.T) {
	tests := []struct{ host, want string }{
		{"nyaa.cplieger.com", "nyaa"},
		{"nyaa.cplieger.com:443", "nyaa"}, // port ignored
		{"AB.example.com", "ab"},          // case-insensitive
		{"ab.example.com", "ab"},
		{"seadex.cplieger.com", ""}, // non-tracker subdomain -> 404
		{"seadex-scout:9118", ""},   // internal docker name + port
		{"seadex-scout", ""},        // internal docker name
		{"", ""},
	}
	for _, tc := range tests {
		if got := scopeFromHost(tc.host); got != tc.want {
			t.Errorf("scopeFromHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestScopeFor(t *testing.T) {
	tests := []struct{ host, path, want string }{
		{"seadex-scout:9118", "/nyaa/api", "nyaa"},   // path (internal direct use)
		{"seadex-scout:9118", "/ab", "ab"},           // path
		{"seadex-scout:9118", "/api", ""},            // neither names a tracker -> 404
		{"nyaa.cplieger.com", "/api", "nyaa"},        // host fallback (proxy subdomain)
		{"ab.cplieger.com", "/api", "ab"},            // host fallback
		{"seadex.cplieger.com", "/nyaa/api", "nyaa"}, // path over aggregate host
		{"nyaa.cplieger.com", "/ab/api", "ab"},       // explicit path wins over host
	}
	for _, tc := range tests {
		if got := scopeFor(tc.host, tc.path); got != tc.want {
			t.Errorf("scopeFor(%q,%q) = %q, want %q", tc.host, tc.path, got, tc.want)
		}
	}
}

// findByGUID returns the feed item with the given guid (its tracker page URL),
// or nil. Feed order is by update time, so tests look items up by identity.
func findByGUID(items []item, guid string) *item {
	for i := range items {
		if items[i].GUID == guid {
			return &items[i]
		}
	}
	return nil
}

// TestBuildFeeds synthesizes the per-tracker RSS feeds from a real SeaDex entry
// shape (Frieren, alID 154587: PMR best + LostYears alt, each on Nyaa and AB),
// covering the tracker split, season-title collapse, best/alt markers, direct
// download links (public Nyaa .torrent, AB via passkey), the dropped redacted AB
// info hash, and the missing-passkey skip count.
func TestBuildFeeds(t *testing.T) {
	updated := time.Date(2025, 7, 26, 15, 5, 59, 0, time.UTC)
	pmrFiles := []seadex.File{
		// An extra (creditless) file first, to prove representativeFile skips it
		// for a real episode when deriving the title.
		{Length: 400_000_000, Name: "NCED 01 (BD Remux 1080p AVC FLAC) [PMR].mkv"},
		{Length: 7_500_699_108, Name: "Frieren Beyond Journey's End - S01E01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
		{Length: 7_497_267_058, Name: "Frieren Beyond Journey's End - S01E02 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
	}
	lostYearsFiles := []seadex.File{
		{Length: 3_506_804_569, Name: "[LostYears] Frieren Beyond Journey's End - S01E01 (WEB 1080p x265 10-bit AAC Opus) [0F7F64F6].mkv"},
		{Length: 3_535_154_954, Name: "[LostYears] Frieren Beyond Journey's End - S01E02 (WEB 1080p x265 10-bit AAC Opus) [E5ECA664].mkv"},
	}
	entries := []seadex.Entry{{
		AniListID: 154587,
		Updated:   updated,
		Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1961373", InfoHash: "143ed15e5e3df072ae91adaeb149973a887590dd", IsBest: true, ReleaseGroup: "PMR", Files: pmrFiles},
			{Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>", IsBest: true, ReleaseGroup: "PMR", Files: pmrFiles},
			{Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1162986", InfoHash: "<redacted>", IsBest: false, ReleaseGroup: "LostYears", Files: lostYearsFiles},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1998171", InfoHash: "fb9ce1e001837de7662bd72b3fb79b3fea13d03f", IsBest: false, ReleaseGroup: "LostYears", Files: lostYearsFiles},
		},
	}}

	// Frieren is a TV series, so classify every entry as anime (the category
	// itself is exercised by TestMovieClassifier).
	classifyAnime := func(int) []int { return []int{catAnime} }
	nyaa, ab, abSkipped := buildFeeds(entries, "PASSKEY123", classifyAnime)
	if len(nyaa) != 2 || len(ab) != 2 {
		t.Fatalf("feeds: got nyaa=%d ab=%d, want 2 and 2", len(nyaa), len(ab))
	}
	if abSkipped != 0 {
		t.Errorf("abSkippedNoPasskey = %d, want 0 (passkey provided)", abSkipped)
	}

	// Nyaa best (PMR): season-collapsed title (extras skipped), public .torrent
	// link, best marker, real info hash, anime category, SeaDex entry info URL,
	// summed pack size, entry update time.
	pmrNyaa := findByGUID(nyaa, "https://nyaa.si/view/1961373")
	if pmrNyaa == nil {
		t.Fatal("PMR nyaa item missing")
	}
	if want := "Frieren Beyond Journey's End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]"; pmrNyaa.Title != want {
		t.Errorf("PMR nyaa title = %q, want %q", pmrNyaa.Title, want)
	}
	if pmrNyaa.DownloadURL != "https://nyaa.si/download/1961373.torrent" {
		t.Errorf("PMR nyaa download = %q", pmrNyaa.DownloadURL)
	}
	if pmrNyaa.DownloadVolumeFactor != dvfBest {
		t.Errorf("PMR nyaa dvf = %q, want %q", pmrNyaa.DownloadVolumeFactor, dvfBest)
	}
	if pmrNyaa.InfoHash != "143ed15e5e3df072ae91adaeb149973a887590dd" {
		t.Errorf("PMR nyaa infohash = %q", pmrNyaa.InfoHash)
	}
	if len(pmrNyaa.Categories) != 1 || pmrNyaa.Categories[0] != catAnime {
		t.Errorf("PMR nyaa categories = %v, want [%d]", pmrNyaa.Categories, catAnime)
	}
	if pmrNyaa.InfoURL != "https://releases.moe/154587" {
		t.Errorf("PMR nyaa infoURL = %q", pmrNyaa.InfoURL)
	}
	if pmrNyaa.Size != 400_000_000+7_500_699_108+7_497_267_058 {
		t.Errorf("PMR nyaa size = %d, want summed pack size", pmrNyaa.Size)
	}
	if !pmrNyaa.PubDate.Equal(updated) {
		t.Errorf("PMR nyaa pubDate = %v, want %v", pmrNyaa.PubDate, updated)
	}

	// AB best (PMR): passkey download link, best marker, redacted info hash
	// dropped, guid is the usable (prefixed) AB page URL.
	pmrAB := findByGUID(ab, "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293")
	if pmrAB == nil {
		t.Fatal("PMR ab item missing")
	}
	if pmrAB.DownloadURL != "https://animebytes.tv/torrent/1167293/download/PASSKEY123" {
		t.Errorf("PMR ab download = %q", pmrAB.DownloadURL)
	}
	if pmrAB.InfoHash != "" {
		t.Errorf("PMR ab infohash = %q, want empty (redacted dropped)", pmrAB.InfoHash)
	}

	// AB alt (LostYears): alt marker + its own passkey link.
	lyAB := findByGUID(ab, "https://animebytes.tv/torrents.php?id=86576&torrentid=1162986")
	if lyAB == nil {
		t.Fatal("LostYears ab item missing")
	}
	if lyAB.DownloadVolumeFactor != dvfAlt {
		t.Errorf("LostYears ab dvf = %q, want %q (alt)", lyAB.DownloadVolumeFactor, dvfAlt)
	}
	if lyAB.DownloadURL != "https://animebytes.tv/torrent/1162986/download/PASSKEY123" {
		t.Errorf("LostYears ab download = %q", lyAB.DownloadURL)
	}

	// Without a passkey the AB feed carries nothing grabbable, and both AB
	// releases are counted for the operator nudge; Nyaa is unaffected.
	nyaa2, ab2, abSkipped2 := buildFeeds(entries, "", classifyAnime)
	if len(nyaa2) != 2 {
		t.Errorf("nyaa feed without passkey = %d, want 2", len(nyaa2))
	}
	if len(ab2) != 0 {
		t.Errorf("ab feed without passkey = %d, want 0", len(ab2))
	}
	if abSkipped2 != 2 {
		t.Errorf("abSkippedNoPasskey without passkey = %d, want 2", abSkipped2)
	}
}

func TestFeedTitle(t *testing.T) {
	tests := []struct {
		name  string
		files []seadex.File
		group string
		want  string
	}{
		{
			name: "season pack (multi-file) collapses SxxExx to the season",
			files: []seadex.File{
				{Name: "Frieren Beyond Journey's End - S01E07 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
				{Name: "Frieren Beyond Journey's End - S01E08 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
			},
			want: "Frieren Beyond Journey's End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]",
		},
		{
			name:  "single-episode torrent keeps its SxxExx (complete-but-unpacked season)",
			files: []seadex.File{{Name: "Scum.of.the.Brave.S01E05.A.Brave.Sensei.1080p.CR.WEB-DL.AAC2.0.H.264-VARYG.mkv"}},
			want:  "Scum.of.the.Brave.S01E05.A.Brave.Sensei.1080p.CR.WEB-DL.AAC2.0.H.264-VARYG",
		},
		{
			name: "versioned episode in a pack still collapses to the season",
			files: []seadex.File{
				{Name: "[LostYears] Frieren Beyond Journey's End - S01E15v2 (WEB 1080p x265 10-bit AAC Opus) [3564C0AD].mkv"},
				{Name: "[LostYears] Frieren Beyond Journey's End - S01E16 (WEB 1080p x265 10-bit AAC Opus) [06E8039D].mkv"},
			},
			want: "[LostYears] Frieren Beyond Journey's End - S01 (WEB 1080p x265 10-bit AAC Opus) [3564C0AD]",
		},
		{
			name: "creditless extras skipped; a lone episode keeps its SxxExx",
			files: []seadex.File{
				{Name: "NCED 01 (BD Remux 1080p AVC FLAC) [PMR].mkv"},
				{Name: "Show Title - S02E01 (BD 1080p) [Grp].mkv"},
			},
			want: "Show Title - S02E01 (BD 1080p) [Grp]",
		},
		{
			name:  "single movie file used verbatim",
			files: []seadex.File{{Name: "A Silent Voice (2016) (BD 1080p x264 FLAC) [Group].mkv"}},
			want:  "A Silent Voice (2016) (BD 1080p x264 FLAC) [Group]",
		},
		{
			name: "absolute-numbered pack drops the episode number",
			files: []seadex.File{
				{Name: "[Grp] Some Show - 07 (1080p).mkv"},
				{Name: "[Grp] Some Show - 08 (1080p).mkv"},
			},
			want: "[Grp] Some Show (1080p)",
		},
		{
			name:  "single absolute-numbered episode keeps its number",
			files: []seadex.File{{Name: "[Grp] Some Show - 07 (1080p).mkv"}},
			want:  "[Grp] Some Show - 07 (1080p)",
		},
		{
			name:  "no files falls back to release group",
			files: nil,
			group: "PMR",
			want:  "PMR",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := feedTitle(&seadex.Torrent{Files: tc.files, ReleaseGroup: tc.group})
			if got != tc.want {
				t.Errorf("feedTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMovieClassifier verifies the RSS category comes from the entry's real
// media type (Fribb), not a guess from the file name: a movie routes to Radarr
// (Movies), while a TV series, an OVA, a special, and an unmapped entry all
// route to Sonarr (Anime). The OVA/special cases are the ones a file-name
// heuristic gets wrong - a single-file special is indistinguishable from a film
// by name - so classifying them as anime is the behavior that matters here.
func TestMovieClassifier(t *testing.T) {
	recs := map[int]mapping.Record{
		1: {AniListID: 1, Type: "MOVIE"},
		2: {AniListID: 2, Type: "OVA"},
		3: {AniListID: 3, Type: "SPECIAL"},
		4: {AniListID: 4, Type: "TV"},
	}
	classify := movieClassifier(func(alID int) (mapping.Record, bool) {
		r, ok := recs[alID]
		return r, ok
	})
	tests := []struct {
		name string
		alID int
		want int
	}{
		{"movie routes to Radarr", 1, catMovies},
		{"OVA is not a movie (Sonarr)", 2, catAnime},
		{"special is not a movie (Sonarr)", 3, catAnime},
		{"tv routes to Sonarr", 4, catAnime},
		{"unmapped defaults to Sonarr", 999, catAnime},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.alID)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("classify(%d) = %v, want [%d]", tc.alID, got, tc.want)
			}
		})
	}

	// A nil lookup (no mapping configured) is safe and defaults to anime.
	if got := movieClassifier((*mapping.Index)(nil).Lookup)(1); len(got) != 1 || got[0] != catAnime {
		t.Errorf("nil-mapping classify = %v, want [%d]", got, catAnime)
	}
}

// TestABFeedRequiresPasskey verifies the /ab feed rejects an empty-q request
// (Prowlarr's save-test or an RSS check) with a Torznab <error> when no passkey
// is set, so the AnimeBytes indexer cannot be saved without one; the /nyaa feed
// and an AB request once a passkey is set are unaffected.
func TestABFeedRequiresPasskey(t *testing.T) {
	serve := func(ix *Indexer, target string) string {
		rec := httptest.NewRecorder()
		ix.serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Body.String()
	}

	noKey := New(&Config{}, Deps{}, "")
	if body := serve(noKey, "/ab?t=search"); !strings.Contains(body, "<error") || !strings.Contains(body, "passkey") {
		t.Errorf("ab empty-q without passkey: body = %q, want a Torznab <error> mentioning the passkey", body)
	}
	if body := serve(noKey, "/nyaa?t=search"); strings.Contains(body, "<error") {
		t.Errorf("nyaa empty-q must not error: %q", body)
	}

	withKey := New(&Config{ABPasskey: "PASSKEY"}, Deps{}, "")
	if body := serve(withKey, "/ab?t=search"); strings.Contains(body, "<error") {
		t.Errorf("ab empty-q with passkey must not error: %q", body)
	}
}

// TestRenderSynthesizedItem checks a synthesized RSS item renders in the live
// AnimeBytes Torznab item shape: an enclosure with the direct .torrent link, the
// anime category, the SeaDex freeleech marker (downloadvolumefactor 0.75 +
// uploadvolumefactor 1), a floored seeders count, the SeaDex entry as comments,
// and the info hash.
func TestRenderSynthesizedItem(t *testing.T) {
	out := renderFeed([]item{{
		Title:                "Frieren Beyond Journey's End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]",
		GUID:                 "https://nyaa.si/view/1961373",
		InfoURL:              "https://releases.moe/154587",
		DownloadURL:          "https://nyaa.si/download/1961373.torrent",
		InfoHash:             "143ed15e5e3df072ae91adaeb149973a887590dd",
		DownloadVolumeFactor: dvfBest,
		Categories:           []int{catAnime},
		Size:                 22497965274,
	}})

	want := []string{
		"<title>Frieren Beyond Journey&#39;s End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]</title>",
		`<enclosure url="https://nyaa.si/download/1961373.torrent" length="22497965274" type="application/x-bittorrent"/>`,
		`<comments>https://releases.moe/154587</comments>`,
		`<torznab:attr name="category" value="5070"/>`,
		`<torznab:attr name="infohash" value="143ed15e5e3df072ae91adaeb149973a887590dd"/>`,
		`<torznab:attr name="downloadvolumefactor" value="0.75"/>`,
		`<torznab:attr name="uploadvolumefactor" value="1"/>`,
		`<torznab:attr name="seeders" value="1"/>`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("rendered feed missing %q\nfull output:\n%s", w, out)
		}
	}
}

// TestServe_requiresAPIKeyBeforeServingCaps verifies the API-key gate rejects a
// missing or wrong apikey before any capabilities document is served, and that a
// correct key yields the exact caps shape the arrs expect.
func TestServe_requiresAPIKeyBeforeServingCaps(t *testing.T) {
	ix := New(&Config{APIKey: "secret"}, Deps{}, "")

	bad := httptest.NewRecorder()
	ix.serve(bad, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=wrong", nil))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad apikey status = %d, want %d", bad.Code, http.StatusUnauthorized)
	}
	if strings.Contains(bad.Body.String(), "<caps>") {
		t.Errorf("bad apikey body contains caps response: %q", bad.Body.String())
	}

	missing := httptest.NewRecorder()
	ix.serve(missing, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing apikey status = %d, want %d", missing.Code, http.StatusUnauthorized)
	}

	good := httptest.NewRecorder()
	ix.serve(good, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=secret", nil))
	if good.Code != http.StatusOK {
		t.Fatalf("good apikey status = %d, want %d; body=%q", good.Code, http.StatusOK, good.Body.String())
	}
	if ct := good.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("caps content type = %q, want application/xml; charset=utf-8", ct)
	}
	body := good.Body.String()
	for _, want := range []string{
		"<caps>",
		`<search available="yes" supportedParams="q"/>`,
		`<tv-search available="yes" supportedParams="q,season,ep"/>`,
		`<movie-search available="yes" supportedParams="q"/>`,
		`<category id="5000" name="TV"><subcat id="5070" name="Anime"/></category>`,
		`<category id="2000" name="Movies"/>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("caps response missing %q\nfull body:\n%s", want, body)
		}
	}
}

// TestFilterByCats_appliesTorznabCategorySemantics pins the Torznab category
// filter contract: an Anime item satisfies a TV-parent request, Movies excludes
// Anime, and an uncategorized item always passes through (Prowlarr already
// applied the upstream category filter).
func TestFilterByCats_appliesTorznabCategorySemantics(t *testing.T) {
	items := []item{
		{Title: "anime", Categories: []int{catAnime}},
		{Title: "movie", Categories: []int{catMovies}},
		{Title: "uncategorized"},
	}

	if got := filterByCats(items, nil); len(got) != 3 {
		t.Fatalf("empty category filter returned %d items, want 3", len(got))
	}

	anime := filterByCats(items, map[int]bool{catAnime: true})
	if len(anime) != 2 || anime[0].Title != "anime" || anime[1].Title != "uncategorized" {
		t.Fatalf("anime filter returned %#v, want anime plus uncategorized passthrough", anime)
	}

	tv := filterByCats(items, map[int]bool{catTV: true})
	if len(tv) != 2 || tv[0].Title != "anime" || tv[1].Title != "uncategorized" {
		t.Fatalf("TV parent filter returned %#v, want anime subcategory plus uncategorized passthrough", tv)
	}

	movies := filterByCats(items, map[int]bool{catMovies: true})
	if len(movies) != 2 || movies[0].Title != "movie" || movies[1].Title != "uncategorized" {
		t.Fatalf("movies filter returned %#v, want movie plus uncategorized passthrough", movies)
	}
}

// TestReloadKeepsFeedOnMalformedSnapshot verifies reload's resilience contract: once a
// good feed is loaded, a later malformed snapshot write (a partial/corrupt cycle write) is
// logged and ignored, never blanking the live feed. A cross-process poll writes the file
// non-atomically only in the failure case; the server must not serve an empty feed then.
func TestReloadKeepsFeedOnMalformedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter("", path, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ix := New(&Config{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}, Deps{}, path)
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Errorf("after malformed rewrite feed = %d items, want 1 (a bad write must not blank a live feed)", len(got))
	}
}

// TestBuildFeedsCompleteUnpackedSeason pins the v1.7.2 behavior at the buildFeeds level: a
// season SeaDex tracks as one torrent PER episode (each a single-file release) yields one
// feed item per episode, each keeping its SxxExx - never collapsed to the season (which
// would let the arr grab a single episode believing it was the whole season) and never
// deduped away.
func TestBuildFeedsCompleteUnpackedSeason(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 187989,
		Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1", IsBest: true, Files: []seadex.File{{Length: 1, Name: "Scum of the Brave - S01E01 (WEB 1080p) [G].mkv"}}},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/2", IsBest: true, Files: []seadex.File{{Length: 1, Name: "Scum of the Brave - S01E02 (WEB 1080p) [G].mkv"}}},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/3", IsBest: true, Files: []seadex.File{{Length: 1, Name: "Scum of the Brave - S01E03 (WEB 1080p) [G].mkv"}}},
		},
	}}
	nyaa, _, _ := buildFeeds(entries, "", func(int) []int { return []int{catAnime} })
	if len(nyaa) != 3 {
		t.Fatalf("got %d items, want 3 (one per episode torrent, not collapsed/deduped)", len(nyaa))
	}
	titles := map[string]bool{}
	for i := range nyaa {
		titles[nyaa[i].Title] = true
	}
	for _, want := range []string{
		"Scum of the Brave - S01E01 (WEB 1080p) [G]",
		"Scum of the Brave - S01E02 (WEB 1080p) [G]",
		"Scum of the Brave - S01E03 (WEB 1080p) [G]",
	} {
		if !titles[want] {
			t.Errorf("missing per-episode title %q; got %v", want, titles)
		}
	}
}

// TestDownloadURL pins the download-link builder that produces the AnimeBytes secret link:
// Nyaa builds a public .torrent, AB embeds the operator passkey, and every un-grabbable
// case (unknown tracker, missing id, AB without a passkey) is rejected with ok=false so no
// bogus or link-less item is emitted.
func TestDownloadURL(t *testing.T) {
	tests := []struct {
		name, tracker, src, passkey, wantURL string
		wantOK                               bool
	}{
		{"nyaa builds public torrent link", "Nyaa", "https://nyaa.si/view/1961373", "", "https://nyaa.si/download/1961373.torrent", true},
		{"nyaa missing id rejected", "Nyaa", "https://nyaa.si/view/abc", "", "", false},
		{"ab embeds passkey", "AB", "/torrents.php?id=1&torrentid=1167293", "PK", "https://animebytes.tv/torrent/1167293/download/PK", true},
		{"ab without passkey rejected", "AB", "/torrents.php?id=1&torrentid=1167293", "", "", false},
		{"ab missing id rejected", "AB", "/torrents.php?id=1", "PK", "", false},
		{"unknown tracker rejected", "AnimeTosho", "https://animetosho.org/view/1", "PK", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotOK := downloadURL(tc.tracker, tc.src, tc.passkey)
			if gotURL != tc.wantURL || gotOK != tc.wantOK {
				t.Errorf("downloadURL(%q, %q, passkey) = (%q, %v), want (%q, %v)", tc.tracker, tc.src, gotURL, gotOK, tc.wantURL, tc.wantOK)
			}
		})
	}
}

// TestValidInfoHash pins the info-hash gate that keeps a bogus value out of the feed's
// infohash attr: a real 40-char SHA-1 hex is lowercased and trimmed, and anything else -
// SeaDex's literal "<redacted>" for private trackers, a wrong length, or a 40-char string
// with a non-hex byte - is dropped.
func TestValidInfoHash(t *testing.T) {
	const valid = "143ed15e5e3df072ae91adaeb149973a887590dd"
	tests := []struct{ name, in, want string }{
		{"valid lowercase kept", valid, valid},
		{"uppercase normalized", "143ED15E5E3DF072AE91ADAEB149973A887590DD", valid},
		{"whitespace trimmed", "  " + valid + "  ", valid},
		{"redacted dropped", "<redacted>", ""},
		{"wrong length dropped", "abc", ""},
		{"forty chars with a non-hex byte dropped", "g43ed15e5e3df072ae91adaeb149973a887590dd", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validInfoHash(tc.in); got != tc.want {
				t.Errorf("validInfoHash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReloadDoesNotRewindToOlderSnapshot pins reload's anti-rewind invariant:
// a newer snapshot already served in memory is never overwritten by an older
// snapshot on disk. reload's freshness guard installs only when the file's
// mtime is strictly newer than the loaded snapshot, so pointing the server at a
// strictly-older on-disk file and reloading must leave the newer in-memory feed
// and its mtime untouched. Driven single-threaded: the pre-install holds the
// write lock exactly as a real cycle would, and the lone reload runs after it,
// so there is no shared-state access outside the lock.
func TestReloadDoesNotRewindToOlderSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	oldTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newerTime := oldTime.Add(time.Hour)
	oldJSON := `{"by_hash":{},"by_key":{},"nyaa_feed":[{"Title":"old","GUID":"old","DownloadURL":"old"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(oldJSON), 0o600); err != nil {
		t.Fatalf("write old snapshot: %v", err)
	}
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("set old snapshot mtime: %v", err)
	}

	ix := New(&Config{}, Deps{}, "")
	ix.path = path

	// Pre-install a newer snapshot the way a fresher cycle would, holding the
	// write lock exactly as reload's install path does.
	ix.mu.Lock()
	ix.snap = snapshot{
		ByHash:   map[string]bool{},
		ByKey:    map[string]bool{},
		NyaaFeed: []item{{Title: "new", GUID: "new", DownloadURL: "new"}},
	}
	ix.snapMod = newerTime
	ix.mu.Unlock()

	// Reloading against a strictly-older on-disk file must not rewind: the
	// freshness guard skips the install because the file is not newer than the
	// loaded snapshot.
	ix.reload(context.Background())

	got := ix.feedFor(upstreamNyaa)
	if len(got) != 1 || got[0].Title != "new" {
		t.Fatalf("feed after reloading an older snapshot = %#v, want the newer in-memory snapshot", got)
	}
	if !ix.snapMod.Equal(newerTime) {
		t.Fatalf("snapMod after reloading an older snapshot = %v, want %v", ix.snapMod, newerTime)
	}
}
