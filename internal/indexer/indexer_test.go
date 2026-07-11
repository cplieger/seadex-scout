package indexer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
}

func TestMarkAndDedupe(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"ab:1143533": false},
	}
	raw := []Item{
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

// TestIndexerEndToEnd wires the Indexer against a mock SeaDex API and a mock
// Prowlarr Torznab endpoint, exercising the full path: SeaDex fetch -> curation
// set -> upstream query -> parse -> match -> mark -> query result.
func TestIndexerEndToEnd(t *testing.T) {
	// Mock SeaDex: one entry with a best Nyaa torrent matching the sample feed.
	seadexBody := `{"items":[{"alID":123,"incomplete":false,"expand":{"trs":[` +
		`{"tracker":"Nyaa","url":"https://nyaa.si/view/1234567",` +
		`"infoHash":"ABCDEF1234567890abcdef1234567890abcdef12","isBest":true}]}}],"totalPages":1}`
	seadexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, seadexBody)
	}))
	defer seadexSrv.Close()

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
	}, Deps{
		SeaDex: seadex.NewClient(seadexSrv.Client(), seadexSrv.URL, 0, nil),
		HTTP:   torznabSrv.Client(),
	})

	if err := ix.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	items := ix.query(context.Background(), url.Values{"t": {"search"}}, "")
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
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

	// Per-tracker scoping: the nyaa scope hits the (only) configured upstream;
	// the ab scope has no upstream here, so it serves nothing.
	if got := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Errorf("nyaa scope returned %d items, want 1", len(got))
	}
	if got := ix.query(context.Background(), url.Values{"t": {"search"}}, "ab"); len(got) != 0 {
		t.Errorf("ab scope returned %d items, want 0 (no ab upstream)", len(got))
	}

	// A query for a series not curated by SeaDex still returns a valid (empty)
	// result once we point the curation set elsewhere.
	ix.set = curation{byHash: map[string]bool{}, byKey: map[string]bool{}}
	if got := ix.query(context.Background(), url.Values{"t": {"tvsearch"}}, ""); len(got) != 0 {
		t.Errorf("uncurated query returned %d items, want 0", len(got))
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
	raw := []Item{{Title: "[Momonoki] Frieren S01", InfoURL: prowlarrComments, GUID: prowlarrGUID}}
	out := markAndDedupe(raw, set)
	if len(out) != 1 || out[0].DownloadVolumeFactor != dvfBest {
		t.Fatalf("AB item did not match/mark best: %+v", out)
	}
}

func TestServesQuery(t *testing.T) {
	serves := []url.Values{
		{"t": {"movie"}, "q": {"Totoro"}},                      // movie
		{"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"1"}}, // season pack search
		{"t": {"tvsearch"}},                                    // bare tvsearch / RSS
		{"t": {"search"}},                                      // RSS (empty q)
		{"t": {"search"}, "q": {"Frieren"}},                    // generic series search
		{"t": {"search"}, "q": {"Frieren OVA"}},                // special
		{"t": {"caps"}},                                        // (query() not called for caps, but classifies as serve)
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
		{"/", ""},         // aggregate (both trackers)
		{"/api", ""},      // default arr API path -> aggregate
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

	if got := upstreamsForScope(all, ""); len(got) != 2 {
		t.Errorf("scope all: got %d upstreams, want 2", len(got))
	}
	if got := upstreamsForScope(all, "nyaa"); len(got) != 1 || got[0] != nyaa {
		t.Errorf("scope nyaa: got %v, want [nyaa]", got)
	}
	if got := upstreamsForScope(all, "ab"); len(got) != 1 || got[0] != ab {
		t.Errorf("scope ab: got %v, want [ab]", got)
	}
}
