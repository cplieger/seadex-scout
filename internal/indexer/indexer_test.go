package indexer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// TestParseTorznabClampsNegativeCounts pins the numeric-domain normalization
// of the untrusted Torznab decode (the sibling of totalSize's guard on the
// SeaDex path): negative size/seeders/leechers values clamp to the feed's
// zero-as-unknown representation, and a negative peers value cannot inflate
// the derived leechers count via an unbounded negative seeders subtraction.
func TestParseTorznabClampsNegativeCounts(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>all negative</title>
      <size>-14352012572</size>
      <torznab:attr name="seeders" value="-42"/>
      <torznab:attr name="leechers" value="-3"/>
    </item>
    <item>
      <title>negative seeders with positive peers</title>
      <torznab:attr name="seeders" value="-5"/>
      <torznab:attr name="peers" value="1"/>
    </item>
    <item>
      <title>negative enclosure length</title>
      <enclosure url="http://prowlarr:9696/1/download" length="-5" type="application/x-bittorrent"/>
      <torznab:attr name="peers" value="-9"/>
    </item>
  </channel>
</rss>`
	items, err := parseTorznab([]byte(feed))
	if err != nil {
		t.Fatalf("parseTorznab: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if it := items[0]; it.Size != 0 || it.Seeders != 0 || it.Leechers != 0 {
		t.Errorf("all-negative item = size %d seeders %d leechers %d, want 0/0/0", it.Size, it.Seeders, it.Leechers)
	}
	// Clamped seeders (0) with peers 1 derives leechers 1, never 1-(-5)=6.
	if it := items[1]; it.Seeders != 0 || it.Leechers != 1 {
		t.Errorf("negative-seeders item = seeders %d leechers %d, want 0/1", it.Seeders, it.Leechers)
	}
	if it := items[2]; it.Size != 0 || it.Leechers != 0 {
		t.Errorf("negative-enclosure item = size %d leechers %d, want 0/0", it.Size, it.Leechers)
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
	if got := trackerKeyFromURL("https://nyaa.si./view/1234567"); got != "nyaa:1234567" {
		t.Errorf("nyaa FQDN trailing-dot trackerKeyFromURL = %q, want nyaa:1234567", got)
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
	// Component-aware extraction: a curated id embedded in a query value or
	// fragment of a trusted host's URL identifies nothing - only the path
	// (Nyaa /view, AB permalink) or the torrentid query parameter may key.
	if got := trackerKeyFromURL("https://nyaa.si/?next=/view/1234567"); got != "" {
		t.Errorf("nyaa query-embedded id trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://animebytes.tv/?next=/torrent/1167293/group"); got != "" {
		t.Errorf("ab query-embedded id trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://nyaa.si/#/view/1234567"); got != "" {
		t.Errorf("nyaa fragment-embedded id trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://animebytes.tv/torrent/1167293/group"); got != "ab:1167293" {
		t.Errorf("ab permalink trackerKeyFromURL = %q, want ab:1167293", got)
	}
}

func TestMarkAndDedupe(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"nyaa:1143533": false},
	}
	raw := []item{
		{Title: "best by hash", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g1"},
		{Title: "alt by key", InfoURL: "https://nyaa.si/view/1143533", GUID: "g2"},
		{Title: "not curated", InfoURL: "https://nyaa.si/view/999", GUID: "g3"},
		{Title: "dup of best", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g1"},
	}
	out := markAndDedupe(raw, set, upstreamNyaa)
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

// TestMarkAndDedupeRejectsConflictingIdentity pins lookup's identity
// consistency rule: every structurally valid identity signal an untrusted
// Torznab item carries must resolve to curated entries agreeing on best/alt.
// An item pairing a curated best info hash with the page URL of a different
// torrent (an alt entry, or a structurally valid but uncurated one) must be
// dropped, never admitted on the first matching signal.
func TestMarkAndDedupeRejectsConflictingIdentity(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"nyaa:1143533": false},
	}
	raw := []item{
		{
			Title: "best hash + alt key", GUID: "g1",
			InfoHash: "abcdef1234567890abcdef1234567890abcdef12",
			InfoURL:  "https://nyaa.si/view/1143533",
		},
		{
			Title: "best hash + uncurated key", GUID: "g2",
			InfoHash: "abcdef1234567890abcdef1234567890abcdef12",
			InfoURL:  "https://nyaa.si/view/999",
		},
	}
	if out := markAndDedupe(raw, set, upstreamNyaa); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (conflicting identity signals must drop the item)", len(out))
	}

	// Two curated keys that AGREE on best/alt but name DIFFERENT releases:
	// healthy Prowlarr emits the same tracker id in comments and guid, so an
	// item whose InfoURL and GUID resolve to distinct curated torrents is an
	// invalid untrusted response and must fail closed - the same-marker
	// coincidence must not admit it.
	bothBest := &curation{
		byHash: map[string]bool{},
		byKey:  map[string]bool{"nyaa:100": true, "nyaa:200": true},
	}
	conflicting := []item{{
		Title:   "two curated best ids",
		InfoURL: "https://nyaa.si/view/100",
		GUID:    "https://nyaa.si/view/200",
	}}
	if out := markAndDedupe(conflicting, bothBest, upstreamNyaa); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (distinct tracker identities must drop the item even when both are best)", len(out))
	}
}

// TestMarkAndDedupeRejectsCrossScopeKey pins lookup's tracker-scope binding: a
// tracker key parsed from an item's page URL must belong to the endpoint being
// served, so a curated Nyaa item is rejected under the /ab scope (a swapped
// upstream or cross-tracker item must not surface under the wrong per-tracker
// indexer). It also pins the AB-specific rule that a scoped tracker key is
// mandatory: AnimeBytes exposes no info hash in Torznab, so a hash-only item
// cannot match under /ab even when its hash is curated.
func TestMarkAndDedupeRejectsCrossScopeKey(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"nyaa:1143533": false, "ab:1143533": false},
	}
	raw := []item{
		{Title: "nyaa key under ab scope", InfoURL: "https://nyaa.si/view/1143533", GUID: "g1"},
		{Title: "curated hash only under ab scope", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g2"},
	}
	if out := markAndDedupe(raw, set, upstreamAB); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (cross-scope key and hash-only items must not match under /ab)", len(out))
	}
	abOnly := []item{{Title: "ab key under nyaa scope", InfoURL: "https://animebytes.tv/torrents.php?id=1&torrentid=1143533", GUID: "g3"}}
	if out := markAndDedupe(abOnly, set, upstreamNyaa); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (an AnimeBytes key must not match under /nyaa)", len(out))
	}
}

// TestMarkAndDedupeRejectsUncuratedHash pins the miss leg of the curation
// gate's info-hash arm: an item carrying a structurally valid 40-hex info hash
// that is NOT in the SeaDex curation set must be dropped, never admitted or marked.
func TestMarkAndDedupeRejectsUncuratedHash(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{},
	}
	raw := []item{{Title: "uncurated hash", InfoHash: "0123456789012345678901234567890123456789", GUID: "g1"}}
	if out := markAndDedupe(raw, set, upstreamNyaa); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (a valid but uncurated info hash must not match)", len(out))
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
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Mock Prowlarr Torznab: returns the sample feed regardless of query.
	var gotAPIKey string
	torznabSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/rss+xml")
		// Rewrite the fixture's download link onto this mock endpoint's own
		// origin: search now drops items whose download URL is not on the
		// configured Prowlarr origin, and a real Prowlarr hands out proxy
		// links on its own host.
		_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
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
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), entries, nil); err != nil {
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
	out := markAndDedupe(raw, set, upstreamAB)
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
		{"t": {"search"}, "q": {"Some Film 2011"}, "cat": {"2999"}}, // top of the Movies range still reads as a film
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
		{"t": {"search"}, "q": {"Frieren 01"}, "cat": {"3000"}},             // 3000 is past the Movies range; the episode skip applies
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

func TestUpstreamForScope(t *testing.T) {
	nyaa := &upstream{name: "nyaa"}
	ab := &upstream{name: "ab"}
	all := []*upstream{nyaa, ab}

	if got := upstreamForScope(all, ""); got != nil {
		t.Errorf("empty scope: got %v, want nil (no combined feed)", got)
	}
	if got := upstreamForScope(all, "nyaa"); got != nyaa {
		t.Errorf("scope nyaa: got %v, want nyaa", got)
	}
	if got := upstreamForScope(all, "ab"); got != ab {
		t.Errorf("scope ab: got %v, want ab", got)
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
	nyaa, ab, abSkipped, _ := buildFeeds(entries, "PASSKEY123", classifyAnime)
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
	nyaa2, ab2, abSkipped2, _ := buildFeeds(entries, "", classifyAnime)
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
		group string
		want  string
		files []seadex.File
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
			name: "torrent directory is not part of the release title",
			files: []seadex.File{
				{Name: "Season 1/Taboo Tattoo S01E01 Tattoo [Bluray-1080p Remux-h264]-LazyRemux.mkv"},
				{Name: "Season 1/Taboo Tattoo S01E02 Surprise Attack [Bluray-1080p Remux-h264]-LazyRemux.mkv"},
			},
			want: "Taboo Tattoo S01 Tattoo [Bluray-1080p Remux-h264]-LazyRemux",
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
			name: "a v2 revision of the same episode is one episode, not a pack",
			files: []seadex.File{
				{Name: "Show - S01E01 (1080p) [G].mkv"},
				{Name: "Show - S01E01v2 (1080p) [G].mkv"},
			},
			want: "Show - S01E01 (1080p) [G]",
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
		{
			name:  "episode-range token in a single file is one release, kept verbatim",
			files: []seadex.File{{Name: "Show - S01E01-E13 (1080p) [G].mkv"}},
			want:  "Show - S01E01-E13 (1080p) [G]",
		},
		{
			name: "pack of episode-range files collapses to the season",
			files: []seadex.File{
				{Name: "Show - S01E01-E02 (1080p) [G].mkv"},
				{Name: "Show - S01E03-E04 (1080p) [G].mkv"},
			},
			want: "Show - S01 (1080p) [G]",
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
	classify := movieClassifier(func(alID int) bool {
		r, ok := recs[alID]
		return ok && r.IsMovie()
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

	// A nil classifier (no mapping configured) is safe and defaults to anime.
	if got := movieClassifier(nil)(1); len(got) != 1 || got[0] != catAnime {
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

	noKey := New(&Config{APIKey: "k"}, Deps{}, "")
	if body := serve(noKey, "/ab?t=search&apikey=k"); !strings.Contains(body, "<error") || !strings.Contains(body, "passkey") {
		t.Errorf("ab empty-q without passkey: body = %q, want a Torznab <error> mentioning the passkey", body)
	}
	if body := serve(noKey, "/nyaa?t=search&apikey=k"); strings.Contains(body, "<error") {
		t.Errorf("nyaa empty-q must not error: %q", body)
	}

	withKey := New(&Config{APIKey: "k", ABPasskey: "PASSKEY"}, Deps{}, "")
	if body := serve(withKey, "/ab?t=search&apikey=k"); strings.Contains(body, "<error") {
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
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), entries, nil); err != nil {
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

// TestReloadKeepsFeedOnZeroSnapshot extends the malformed-snapshot contract to
// syntactically valid but structurally empty JSON: `null` and `{}` decode
// cleanly into a zero snapshot, and installing one would blank both synthesized
// feeds and both curation maps. The writer always emits non-nil by_hash/by_key
// maps (even for an empty catalogue), so nil curation maps identify a
// structurally invalid snapshot the reload must reject, preserving the
// last-good feed.
func TestReloadKeepsFeedOnZeroSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"null document", "null"},
		{"empty object", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), nyaaTestEntries(1), nil); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			ix := New(&Config{}, Deps{}, path)
			if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
				t.Fatalf("initial feed = %d items, want 1", len(got))
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("zero-snapshot write: %v", err)
			}
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, future, future); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
			ix.reload(context.Background())
			if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
				t.Errorf("after %s rewrite feed = %d items, want 1 (a zero snapshot must not blank a live feed)", tc.name, len(got))
			}
		})
	}
}

// TestReloadRebuildsABDownloadURLsFromCurrentPasskey pins the credential
// policy for the persisted AB feed: FeedWriter materializes the passkey into
// each item's DownloadURL, so after an ab_passkey rotation and restart the
// snapshot still embeds the PREVIOUS secret. The reload must re-derive every
// AB download URL from the item's non-secret tracker page URL (GUID) and the
// CURRENT passkey - never serve the persisted credential verbatim - drop an
// item whose URL cannot be re-derived, and clear the AB feed entirely when no
// passkey is configured.
func TestReloadRebuildsABDownloadURLsFromCurrentPasskey(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := NewFeedWriter("OLD_PASSKEY", true, path, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// A restart after rotating the passkey: the loaded AB feed must carry only
	// the NEW credential.
	ix := New(&Config{APIKey: "k", ABPasskey: "NEW_PASSKEY"}, Deps{}, path)
	got := ix.feedFor(upstreamAB)
	if len(got) != 1 {
		t.Fatalf("ab feed = %d items, want 1", len(got))
	}
	if want := "https://animebytes.tv/torrent/1167293/download/NEW_PASSKEY"; got[0].DownloadURL != want {
		t.Errorf("ab download = %q, want %q (rebuilt from the current passkey)", got[0].DownloadURL, want)
	}
	if strings.Contains(got[0].DownloadURL, "OLD_PASSKEY") {
		t.Errorf("ab download still carries the rotated passkey: %q", got[0].DownloadURL)
	}

	// With NO passkey configured the persisted credential-bearing links must
	// not be served at all: the AB feed clears (serve answers the /ab RSS
	// check with a Torznab <error> in that state); Nyaa is untouched.
	none := New(&Config{APIKey: "k"}, Deps{}, path)
	if got := none.feedFor(upstreamAB); len(got) != 0 {
		t.Errorf("ab feed without a configured passkey = %d items, want 0", len(got))
	}

	// An AB item whose page URL yields no torrent id cannot have its URL
	// re-derived: it is dropped rather than served with the stale credential.
	noID := `{"by_hash":{},"by_key":{},"nyaa_feed":[],"ab_feed":[{"Title":"no id","GUID":"https://animebytes.tv/torrents.php?id=1","DownloadURL":"https://animebytes.tv/torrent/1/download/OLD_PASSKEY"}]}`
	noIDPath := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(noIDPath, []byte(noID), 0o600); err != nil {
		t.Fatalf("write no-id snapshot: %v", err)
	}
	dropper := New(&Config{APIKey: "k", ABPasskey: "NEW_PASSKEY"}, Deps{}, noIDPath)
	if got := dropper.feedFor(upstreamAB); len(got) != 0 {
		t.Errorf("ab feed with an underivable item = %d items, want 0 (dropped, never served with the persisted credential)", len(got))
	}
}

// TestReloadRetriesPreservedMtimeReplacementAfterFailure pins the failed-file
// memo to file IDENTITY, not just mtime: after a malformed snapshot fails to
// load at mtime T, a repaired valid snapshot installed on a NEW inode whose
// mtime is reset to the same T (an atomic rename or backup restore preserving
// timestamps) must be retried and installed - a mtime-only watermark would skip
// it and wedge the server on the old feed until restart. Only the unchanged bad
// inode itself stays memoized.
func TestReloadRetriesPreservedMtimeReplacementAfterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("malformed write: %v", err)
	}
	failedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, failedAt, failedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// New's warm-up reload reads the malformed file and memoizes it as failed.
	ix := New(&Config{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}, Deps{}, path)
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 0 {
		t.Fatalf("initial feed = %d items, want 0 (malformed snapshot must not load)", len(got))
	}
	// Repair: a valid snapshot on a NEW inode, renamed over the bad file with
	// the failed mtime preserved.
	repaired := filepath.Join(dir, "feed-repaired.json")
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter("", false, repaired, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := os.Rename(repaired, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Chtimes(path, failedAt, failedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ix.reload(context.Background())
	if got, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 1 {
		t.Errorf("after preserved-mtime repair feed = %d items, want 1 (a new inode at the failed mtime must be retried)", len(got))
	}
}

// nyaaTestEntries builds n distinct single-torrent Nyaa SeaDex entries, the
// minimal input for a synthesized feed of n items in reload tests.
func nyaaTestEntries(n int) []seadex.Entry {
	entries := make([]seadex.Entry, 0, n)
	for i := range n {
		entries = append(entries, seadex.Entry{
			AniListID: 7 + i,
			Torrents: []seadex.Torrent{{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/" + strconv.Itoa(42+i), IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show " + strconv.Itoa(i) + " - S01E01 (1080p) [G].mkv"}},
			}},
		})
	}
	return entries
}

// TestReloadInstallsPreservedMtimeReplacementAfterSuccess pins the last-good
// gate to file IDENTITY, not just mtime: after a snapshot loads successfully at
// mtime T, a DIFFERENT valid snapshot installed on a new inode with its mtime
// reset to the same T (an atomic rename or backup restore preserving
// timestamps) must still install - a mtime-only last-good check would return
// early and leave the old feed served until an unrelated write or a restart.
func TestReloadInstallsPreservedMtimeReplacementAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	loadedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, loadedAt, loadedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ix := New(&Config{}, Deps{}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// A different snapshot on a NEW inode, renamed over the loaded file with
	// the loaded mtime preserved.
	replacement := filepath.Join(dir, "feed-replacement.json")
	if err := NewFeedWriter("", false, replacement, nil).Rebuild(context.Background(), nyaaTestEntries(2), nil); err != nil {
		t.Fatalf("Rebuild replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Chtimes(path, loadedAt, loadedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 2 {
		t.Errorf("after preserved-mtime replacement feed = %d items, want 2 (a new inode at the loaded mtime must install)", len(got))
	}
}

// TestReloadRetriesTransientReadFailureOnSameInode pins the failed-file memo to
// DETERMINISTIC failures only: a snapshot whose read fails (here an oversized
// file the bounded read rejects - a root-safe stand-in for a transient EIO or
// a later-chmodded EACCES) must NOT be memoized, so a subsequent in-place
// repair that changes neither inode nor mtime is still retried and installs.
// Memoizing the read failure would skip the unchanged-identity file forever.
func TestReloadRetriesTransientReadFailureOnSameInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	// A sparse file one byte over the bound: os.Stat succeeds, the bounded
	// read fails.
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxFeedBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	failedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, failedAt, failedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// New's warm-up reload hits the read failure; it must stay retryable.
	ix := New(&Config{}, Deps{}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		t.Fatalf("initial feed = %d items, want 0 (oversized snapshot must not load)", len(got))
	}

	// Repair IN PLACE (same inode: build a valid snapshot beside it, then
	// rewrite the original file's bytes) and restore the failed mtime.
	repaired := filepath.Join(dir, "feed-repaired.json")
	if err := NewFeedWriter("", false, repaired, nil).Rebuild(context.Background(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	valid, err := os.ReadFile(repaired)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatalf("in-place repair: %v", err)
	}
	if err := os.Chtimes(path, failedAt, failedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("after same-inode repair feed = %d items, want 1 (a read failure must stay retryable)", len(got))
	}
}

// TestReloadConcurrentCallers exercises reload's coalescing under concurrency
// (run with -race): many requests observing a rewritten snapshot at once must
// never race on the published snapshot fields, and the new feed must be
// installed once the dust settles.
func TestReloadConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ix := New(&Config{}, Deps{}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), nyaaTestEntries(2), nil); err != nil {
		t.Fatalf("Rebuild newer: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ix.reload(context.Background())
			_ = ix.feedFor(upstreamNyaa)
		})
	}
	wg.Wait()
	// TryLock losers return without installing; one more serial reload
	// guarantees the newer snapshot is in.
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 2 {
		t.Errorf("after concurrent reloads feed = %d items, want 2", len(got))
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
	nyaa, _, _, _ := buildFeeds(entries, "", func(int) []int { return []int{catAnime} })
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

// TestReloadInstallsOlderMtimeSnapshot pins reload's inequality freshness
// guard: an on-disk snapshot whose mtime is OLDER than the loaded copy's still
// installs. A /config volume restored from backup, or a file replaced by an
// atomic rename preserving an older mtime, is the current truth on disk; the
// former strictly-After guard never installed it and wedged the server on the
// stale in-memory snapshot until restart. Any mtime CHANGE reloads; only
// equality skips (TestReloadSkipsUnchangedMtime). Driven single-threaded: the
// pre-install holds the write lock exactly as a real cycle would, and the lone
// reload runs after it, so there is no shared-state access outside the lock.
func TestReloadInstallsOlderMtimeSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	oldTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newerTime := oldTime.Add(time.Hour)
	restoredJSON := `{"by_hash":{},"by_key":{},"nyaa_feed":[{"Title":"restored","GUID":"restored","DownloadURL":"restored"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(restoredJSON), 0o600); err != nil {
		t.Fatalf("write restored snapshot: %v", err)
	}
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("set restored snapshot mtime: %v", err)
	}

	ix := New(&Config{}, Deps{}, "")
	ix.path = path

	// Pre-install a newer-mtime snapshot the way a pre-restore cycle would,
	// holding the write lock exactly as reload's install path does.
	ix.mu.Lock()
	ix.snap = snapshot{
		ByHash:   map[string]bool{},
		ByKey:    map[string]bool{},
		NyaaFeed: []item{{Title: "stale", GUID: "stale", DownloadURL: "stale"}},
	}
	ix.snapMod = newerTime
	ix.mu.Unlock()

	// Reloading against the older-mtime on-disk file must install it: the
	// mtime differs from the loaded snapshot's, and the file is the truth.
	ix.reload(context.Background())

	got := ix.feedFor(upstreamNyaa)
	if len(got) != 1 || got[0].Title != "restored" {
		t.Fatalf("feed after reloading an older-mtime snapshot = %#v, want the restored on-disk snapshot", got)
	}
	if ix.snapMod.Equal(newerTime) {
		t.Fatalf("snapMod after reloading an older-mtime snapshot = %v, want the on-disk mtime, not the stale %v", ix.snapMod, newerTime)
	}
}

// TestReloadSkipsUnchangedMtime pins the equality leg of reload's freshness
// guard: when the on-disk mtime equals the loaded snapshot's, reload leaves the
// served feed untouched - even if the bytes changed - so the per-request mtime
// check stays a cheap stat, never a read/unmarshal.
func TestReloadSkipsUnchangedMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	when := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	firstJSON := `{"by_hash":{},"by_key":{},"nyaa_feed":[{"Title":"first","GUID":"first","DownloadURL":"first"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(firstJSON), 0o600); err != nil {
		t.Fatalf("write first snapshot: %v", err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("set first snapshot mtime: %v", err)
	}
	ix := New(&Config{}, Deps{}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("initial feed = %#v, want the first snapshot", got)
	}

	// Rewrite the content but restore the identical mtime: reload must skip.
	secondJSON := `{"by_hash":{},"by_key":{},"nyaa_feed":[{"Title":"second","GUID":"second","DownloadURL":"second"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(secondJSON), 0o600); err != nil {
		t.Fatalf("write second snapshot: %v", err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("feed after unchanged-mtime rewrite = %#v, want the loaded first snapshot (equality skips)", got)
	}
}

// TestReloadCoalescesConcurrentRefreshes pins reload's coalescing contract:
// while one request holds the refresh (reloadMu, as a winning reload does for
// its whole stat/read/unmarshal), a sibling reload returns immediately without
// duplicating the read - it does not block and does not install the on-disk
// snapshot itself - and feedFor keeps serving the current snapshot unblocked.
// Once the refresh is released, the next reload installs the new snapshot.
func TestReloadCoalescesConcurrentRefreshes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	newJSON := `{"by_hash":{},"by_key":{},"nyaa_feed":[{"Title":"new","GUID":"new","DownloadURL":"new"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(newJSON), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	ix := New(&Config{}, Deps{}, "")
	ix.path = path

	// Simulate a refresh in progress: hold reloadMu exactly as the winning
	// request does across its stat/read/unmarshal.
	ix.reloadMu.Lock()

	// A sibling reload must return immediately rather than queue behind the
	// in-progress refresh or perform a duplicate read.
	done := make(chan struct{})
	go func() {
		ix.reload(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		ix.reloadMu.Unlock()
		t.Fatal("sibling reload blocked behind an in-progress refresh; want an immediate return")
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		ix.reloadMu.Unlock()
		t.Fatalf("sibling reload installed the snapshot itself = %#v; want the install left to the refresh holder", got)
	}

	// Once the winning request releases the refresh, the next reload installs
	// the new snapshot as usual.
	ix.reloadMu.Unlock()
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "new" {
		t.Fatalf("reload after the refresh released = %#v, want the new snapshot installed", got)
	}
}

// TestApplyPaging pins the synthesized feed's Torznab paging contract (t=caps
// advertises limit/offset): limit trims the window, offset advances it, an
// offset past the end yields an empty page, and absent params leave the feed
// untouched.
func TestApplyPaging(t *testing.T) {
	feed := []item{{GUID: "a"}, {GUID: "b"}, {GUID: "c"}}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"no params leaves the feed unpaged", "", []string{"a", "b", "c"}},
		{"limit trims the window", "limit=2", []string{"a", "b"}},
		{"offset advances the window", "offset=2", []string{"c"}},
		{"offset+limit page", "offset=1&limit=1", []string{"b"}},
		{"offset past the end is an empty page", "offset=10", nil},
		{"invalid params are ignored", "offset=x&limit=-1", []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.query, err)
			}
			got := applyPaging(feed, q)
			if len(got) != len(tc.want) {
				t.Fatalf("applyPaging(%q) returned %d items, want %d", tc.query, len(got), len(tc.want))
			}
			for i := range got {
				if got[i].GUID != tc.want[i] {
					t.Errorf("applyPaging(%q)[%d].GUID = %q, want %q", tc.query, i, got[i].GUID, tc.want[i])
				}
			}
		})
	}
}

// TestParsePubDate pins the Torznab <pubDate> parser on the untrusted upstream
// date string: each supported layout parses to the same instant, and any empty,
// whitespace-only, or unparseable value yields the zero time (the failure signal
// writeItem keys on to omit the pubDate element). Today only TestParseTorznab's
// single RFC1123Z sample and the round-trip fuzz seed exercise this, so the
// alternate layouts and the failure branch (the uncovered path, 85.7%) are
// otherwise unpinned.
func TestParsePubDate(t *testing.T) {
	want := time.Date(2026, time.July, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, in string }{
		{"RFC1123Z", "Mon, 06 Jul 2026 12:00:00 +0000"},
		{"RFC1123", "Mon, 06 Jul 2026 12:00:00 GMT"},
		{"RFC822Z", "06 Jul 26 12:00 +0000"},
		{"RFC822", "06 Jul 26 12:00 GMT"},
		{"RFC3339", "2026-07-06T12:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePubDate(tc.in); !got.Equal(want) {
				t.Errorf("parsePubDate(%q) = %v, want %v", tc.in, got, want)
			}
		})
	}
	for _, tc := range []struct{ name, in string }{
		{"empty", ""},
		{"whitespace only", "   "},
		{"unparseable", "not a date"},
		{"wrong shape", "2026/07/06 12:00"},
	} {
		t.Run("zero on "+tc.name, func(t *testing.T) {
			if got := parsePubDate(tc.in); !got.IsZero() {
				t.Errorf("parsePubDate(%q) = %v, want the zero time", tc.in, got)
			}
		})
	}
}

// TestServeFailsClosedWithoutConfiguredAPIKey pins serve's independent
// fail-closed guard for an unconfigured feed_api_key: Run refuses to bind in
// that state, but any other construction path reaching serve must get a 503,
// never a served feed - an absent apikey param also hashes to sha256(""), so
// skipping straight to the constant-time compare would OPEN the gate and serve
// the passkey-bearing feed unauthenticated.
func TestServeFailsClosedWithoutConfiguredAPIKey(t *testing.T) {
	ix := New(&Config{}, Deps{}, "")
	for _, target := range []string{
		"/nyaa?t=caps",
		"/nyaa?t=caps&apikey=",
		"/ab?t=search&apikey=x",
	} {
		rec := httptest.NewRecorder()
		ix.serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("serve(%q) with unconfigured feed_api_key = %d, want 503", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<caps>") {
			t.Errorf("serve(%q) leaked a caps response despite unconfigured feed_api_key", target)
		}
	}
}

// TestInstallSnapshotSkipsAlreadyInstalledFile pins installSnapshot's
// under-lock re-check: re-installing the same unchanged file (equal mtime AND
// os.SameFile identity) returns false and leaves the published snapshot
// untouched. reloadMu already serializes reloads today, but the comment
// declares this defense-in-depth invariant must hold even if the TryLock
// coalescing changes, so it is pinned by direct call.
func TestInstallSnapshotSkipsAlreadyInstalledFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	ix := New(&Config{}, Deps{}, "")
	if !ix.installSnapshot(info1, snapshot{NyaaFeed: []item{{Title: "first"}}}) {
		t.Fatal("first installSnapshot = false, want true")
	}
	if ix.installSnapshot(info2, snapshot{NyaaFeed: []item{{Title: "second"}}}) {
		t.Fatal("second installSnapshot with same unchanged file = true, want false")
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("served feed = %+v, want the originally installed snapshot", got)
	}
}

// TestSearchUsesConfiguredABUpstream is the AB-side behavioral mirror of
// TestIndexerEndToEnd's Nyaa search: an AB-only config must actually wire the
// AnimeBytes upstream in New, so an /ab search proxies Prowlarr, matches the
// curated AB torrent by tracker key (AB exposes no info hash in Torznab), and
// marks it best - while the unconfigured nyaa scope serves nothing without an
// upstream failure. Kills the lived CONDITIONALS_NEGATION mutant on New's AB
// wiring conditional (with the wiring negated, /ab returns 0 items).
func TestSearchUsesConfiguredABUpstream(t *testing.T) {
	// The compare cycle rebuilds the curation set from a SeaDex AB entry
	// (torrentid 1167293, best). No passkey is needed: a search matches by
	// tracker key and rides Prowlarr's own download link.
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Mock Prowlarr AB Torznab: one item whose guid/comments carry the
	// /torrent/1167293/group permalink (the live AB shape), no info hash.
	const abFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>AnimeBytes</title>
    <item>
      <title>[PMR] Frieren S01 [BD Remux 1080p]</title>
      <guid>https://animebytes.tv/torrent/1167293/group?nh=709E38EC</guid>
      <comments>https://animebytes.tv/torrent/1167293/group</comments>
      <size>22497965274</size>
      <link>http://prowlarr:9696/2/download?apikey=x&amp;link=abc</link>
      <enclosure url="http://prowlarr:9696/2/download?apikey=x&amp;link=abc" length="22497965274" type="application/x-bittorrent"/>
      <torznab:attr name="category" value="5070"/>
      <torznab:attr name="seeders" value="7"/>
    </item>
  </channel>
</rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		// Rewrite the fixture's download link onto this mock endpoint's own
		// origin: search drops items whose download URL is off the configured
		// Prowlarr origin, and a real Prowlarr hands out its own proxy links.
		_, _ = io.WriteString(w, strings.ReplaceAll(abFeed, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	ix := New(&Config{ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"}, Deps{HTTP: srv.Client()}, path)

	items, stats := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "ab")
	if len(items) != 1 {
		t.Fatalf("ab search returned %d items, want 1 (the AB upstream must be wired)", len(items))
	}
	if items[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best)", items[0].DownloadVolumeFactor, dvfBest)
	}
	if !stats.answered || stats.upstreamFailed || stats.upstream != 1 || stats.curated != 1 {
		t.Errorf("ab stats = %+v, want answered, no upstream failure, upstream 1, curated 1", stats)
	}

	// The nyaa scope has no configured upstream: an empty result (a standing
	// misconfiguration), never reported as an upstream failure.
	nyaaItems, nyaaStats := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "nyaa")
	if len(nyaaItems) != 0 || nyaaStats.upstreamFailed {
		t.Errorf("nyaa scope = %d items (upstreamFailed=%v), want 0 items and no failure", len(nyaaItems), nyaaStats.upstreamFailed)
	}
}
