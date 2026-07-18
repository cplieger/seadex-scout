package indexer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// harvestMock is an httptest Prowlarr Torznab endpoint for harvest tests: it
// records every request's query params (under a mutex; the writer queries
// sequentially, but -race must stay clean) and serves per-call bodies with the
// fixture's download origins rewritten onto the mock's own host (the search
// path drops items whose download URL is off the Prowlarr origin).
type harvestMock struct {
	mu       sync.Mutex
	requests []map[string]string
	respond  func(call int) string
}

func newHarvestMock(respond func(call int) string) (*harvestMock, *httptest.Server) {
	m := &harvestMock{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		call := len(m.requests)
		params := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
		m.requests = append(m.requests, params)
		m.mu.Unlock()
		body := m.respond(call)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	return m, srv
}

func (m *harvestMock) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *harvestMock) request(i int) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[i]
}

// torznabItem renders one Torznab <item> whose enclosure sits on the Prowlarr
// origin placeholder (rewritten by the mock) and whose guid/comments carry the
// tracker page URL the harvest matches by.
func torznabItem(title, pageURL string) string {
	return `<item><title>` + title + `</title><guid>` + pageURL + `</guid><comments>` + pageURL + `</comments>` +
		`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>`
}

// torznabBody wraps items in the Torznab RSS envelope.
func torznabBody(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>` +
		strings.Join(items, "") + `</channel></rss>`
}

// emptyTorznab is a valid zero-item response.
func emptyTorznab() string { return torznabBody() }

// TestHarvestMatchesABByTorrentID pins the AnimeBytes harvest end to end: one
// series-level Prowlarr query (t=search, q = the synthesis title source), the
// returned item matched back by the AB torrent id in its permalink page URL
// (AB exposes no info hash), the real title cached in the snapshot and served
// on this rebuild's write.
func TestHarvestMatchesABByTorrentID(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(torznabItem("[PMR] Frieren S01 [BD Remux 1080p]", "https://animebytes.tv/torrent/1167293/group"))
	})
	defer srv.Close()

	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	info := func(int) EntryInfo { return EntryInfo{Title: "Frieren: Beyond Journey's End", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABPasskey: "PK", ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 1 {
		t.Fatalf("harvest queries = %d, want 1 (AB search is series-level)", mock.calls())
	}
	req := mock.request(0)
	if req["t"] != "search" || req["q"] != "Frieren: Beyond Journey's End" {
		t.Errorf("AB harvest params = %v, want a plain series-level search on the synthesis title", req)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 1 {
		t.Fatalf("ab feed = %d items, want 1", len(snap.ABFeed))
	}
	if got, want := snap.ABFeed[0].Title, "[PMR] Frieren S01 [BD Remux 1080p]"; got != want {
		t.Errorf("served title = %q, want the harvested real title %q", got, want)
	}
	if snap.Titles["ab:1167293"] != "[PMR] Frieren S01 [BD Remux 1080p]" {
		t.Errorf("title cache = %v, want the harvested title under ab:1167293", snap.Titles)
	}
	if snap.ABFeed[0].GUID != "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293" {
		t.Errorf("GUID = %q, want the tracker page URL unchanged by the title upgrade", snap.ABFeed[0].GUID)
	}
}

// TestHarvestMatchesNyaaByViewID pins the Nyaa harvest: the season-form query
// (t=tvsearch, q + season, the shape that surfaces packs and SxxExx episodes
// alike) with the advertised page limit, matched back by the /view/{id} in the
// returned item's page URL.
func TestHarvestMatchesNyaaByViewID(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(torznabItem("Frieren S01 1080p BluRay [PMR]", "https://nyaa.si/view/1961373"))
	})
	defer srv.Close()

	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/1961373", IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{
				{Length: 1, Name: "Frieren - S01E01 (1080p) [PMR].mkv"},
				{Length: 1, Name: "Frieren - S01E02 (1080p) [PMR].mkv"},
			},
		}},
	}}
	info := func(int) EntryInfo { return EntryInfo{Title: "Frieren", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 1 {
		t.Fatalf("harvest queries = %d, want 1", mock.calls())
	}
	req := mock.request(0)
	if req["t"] != "tvsearch" || req["q"] != "Frieren" || req["season"] != "1" {
		t.Errorf("Nyaa harvest params = %v, want the season-form query (t=tvsearch, q, season=1)", req)
	}
	if req["limit"] != strconv.Itoa(harvestPageSize) {
		t.Errorf("limit = %q, want %d", req["limit"], harvestPageSize)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "Frieren S01 1080p BluRay [PMR]" {
		t.Errorf("nyaa feed = %+v, want the harvested real title served", snap.NyaaFeed)
	}
}

// TestHarvestCachePersistsAcrossRebuilds pins the harvested-once-ever
// contract: a title cached by one rebuild is served by the next without any
// further Prowlarr query (torrents are immutable), even though the item is
// re-rendered from current catalogue data.
func TestHarvestCachePersistsAcrossRebuilds(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(torznabItem("Frieren S01 1080p BluRay [PMR]", "https://nyaa.si/view/1961373"))
	})
	defer srv.Close()

	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/1961373", IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{
				{Length: 1, Name: "Frieren - S01E01 (1080p) [PMR].mkv"},
				{Length: 1, Name: "Frieren - S01E02 (1080p) [PMR].mkv"},
			},
		}},
	}}
	info := func(int) EntryInfo { return EntryInfo{Title: "Frieren", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if mock.calls() != 1 {
		t.Fatalf("harvest queries after first rebuild = %d, want 1", mock.calls())
	}
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if mock.calls() != 1 {
		t.Errorf("harvest queries after second rebuild = %d, want still 1 (cached title, no re-query)", mock.calls())
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "Frieren S01 1080p BluRay [PMR]" {
		t.Errorf("second-rebuild feed = %+v, want the cached harvested title still served", snap.NyaaFeed)
	}
}

// TestHarvestBudgetCapEnforced pins harvestSearchBudget: with more pending
// shows than the budget, exactly harvestSearchBudget queries are issued and
// the rest stay synthetic (to retry next rebuild); the rebuild still succeeds.
func TestHarvestBudgetCapEnforced(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	entries := make([]seadex.Entry, 0, harvestSearchBudget+5)
	for i := range harvestSearchBudget + 5 {
		entries = append(entries, nyaaEntry(1000+i, 500+i, true, fmt.Sprintf("Show %d - S01E01 (1080p) [G].mkv", i)))
	}
	info := func(alID int) EntryInfo { return EntryInfo{Title: fmt.Sprintf("Show %d", alID)} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != harvestSearchBudget {
		t.Errorf("harvest queries = %d, want the budget cap %d", mock.calls(), harvestSearchBudget)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != harvestSearchBudget+5 {
		t.Errorf("feed = %d items, want %d (over-budget items still serve synthesized titles)", len(snap.NyaaFeed), harvestSearchBudget+5)
	}
	if len(snap.Titles) != 0 {
		t.Errorf("titles = %v, want empty (no query matched)", snap.Titles)
	}
}

// TestHarvestQueryFailureKeepsSynthetic pins the failure posture: a failed
// Prowlarr query warns (kv-only) and the item keeps its synthesized title -
// the rebuild never fails over harvesting.
func TestHarvestQueryFailureKeepsSynthetic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv", "Show - S01E02 (1080p) [G].mkv")}
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client(), Logger: log})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rec.Contains("indexer title harvest query failed; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("harvest failure not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "Show S01 1080p" {
		t.Errorf("feed = %+v, want the synthesized title kept on harvest failure", snap.NyaaFeed)
	}
	if len(snap.Titles) != 0 {
		t.Errorf("titles = %v, want empty after a failed harvest", snap.Titles)
	}
}

// TestHarvestMalformedResponseSkipsOnlyThatShow pins the failure
// classification: a persistently malformed 2xx response for one show is a
// show-local poison item, not a scope-wide outage, so a LATER group on the
// same upstream is still harvested this rebuild instead of the whole tracker
// freezing on synthesized titles indefinitely (the sorted rebuild order would
// otherwise retry the same poisoned show first every cycle).
func TestHarvestMalformedResponseSkipsOnlyThatShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		if r.URL.Query().Get("q") == "Show A" {
			_, _ = io.WriteString(w, "this is not torznab xml <<<")
			return
		}
		body := torznabBody(torznabItem("Show B S01 1080p BluRay [G]", "https://nyaa.si/view/43"))
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	entries := []seadex.Entry{
		nyaaEntry(7, 42, true, "Show A - S01E01 (1080p) [G].mkv", "Show A - S01E02 (1080p) [G].mkv"),
		nyaaEntry(8, 43, true, "Show B - S01E01 (1080p) [G].mkv", "Show B - S01E02 (1080p) [G].mkv"),
	}
	info := func(alID int) EntryInfo {
		if alID == 7 {
			return EntryInfo{Title: "Show A", SeasonTvdb: 1}
		}
		return EntryInfo{Title: "Show B", SeasonTvdb: 1}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client(), Logger: log})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rec.Contains("indexer title harvest response malformed; show keeps its synthesized title this rebuild") {
		t.Errorf("show-local malformed response not warned as such; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	snap := readSnapshotFile(t, path)
	if _, ok := snap.Titles["nyaa:42"]; ok {
		t.Errorf("titles = %v, want no cached title for the malformed show", snap.Titles)
	}
	if snap.Titles["nyaa:43"] != "Show B S01 1080p BluRay [G]" {
		t.Errorf("titles = %v, want the later show on the same upstream still harvested (nyaa:43)", snap.Titles)
	}
}

// TestHarvestUnconfiguredTrackerNeverQueried pins the tracker gate: journal
// items whose tracker has no configured Prowlarr upstream are never harvested
// - no query leaves the process for them - and they serve their synthesized
// titles.
func TestHarvestUnconfiguredTrackerNeverQueried(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	// Nyaa items pending, but only the AB upstream is configured (pointing at
	// the mock): the nyaa group must be skipped without any HTTP call.
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABPasskey: "PK", ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 0 {
		t.Errorf("harvest queries = %d, want 0 (no upstream configured for the nyaa scope)", mock.calls())
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || !strings.HasPrefix(snap.NyaaFeed[0].Title, "Show") {
		t.Errorf("feed = %+v, want the synthesized title served", snap.NyaaFeed)
	}
}

// TestHarvestPagesNyaaByOffset pins the offset paging that reaches older items
// under the indexer's default created/desc ordering: a full first page without
// the target keeps paging (offset advanced by the page size) until the match
// lands, each page costing budget.
func TestHarvestPagesNyaaByOffset(t *testing.T) {
	filler := make([]string, 0, harvestPageSize)
	for i := range harvestPageSize {
		filler = append(filler, torznabItem(fmt.Sprintf("Other %d", i), "https://nyaa.si/view/"+strconv.Itoa(9000+i)))
	}
	mock, srv := newHarvestMock(func(call int) string {
		if call == 0 {
			return torznabBody(filler...)
		}
		return torznabBody(torznabItem("Show S01 1080p BluRay [G]", "https://nyaa.si/view/42"))
	})
	defer srv.Close()

	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv", "Show - S01E02 (1080p) [G].mkv")}
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 2 {
		t.Fatalf("harvest queries = %d, want 2 (a full page pages on)", mock.calls())
	}
	if off := mock.request(0)["offset"]; off != "" {
		t.Errorf("first page offset = %q, want unset (anchored at the newest items)", off)
	}
	if off := mock.request(1)["offset"]; off != strconv.Itoa(harvestPageSize) {
		t.Errorf("second page offset = %q, want %d", off, harvestPageSize)
	}
	snap := readSnapshotFile(t, path)
	if snap.Titles["nyaa:42"] != "Show S01 1080p BluRay [G]" {
		t.Errorf("titles = %v, want the second-page match cached", snap.Titles)
	}
}

// TestHarvestMatchesNyaaByInfoHash pins the info-hash arm of the harvest
// match (the documented secondary identity): a Prowlarr result whose page
// URLs identify no tracker (a mirror/foreign host) still matches the pending
// journal item by its torznab infohash attr - normalized through the same
// validInfoHash the journal side used - and its real title is cached and
// served.
func TestHarvestMatchesNyaaByInfoHash(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	_, srv := newHarvestMock(func(int) string {
		return torznabBody(`<item><title>Show S01 1080p BluRay [G]</title>` +
			`<guid>https://mirror.example/release/999</guid><comments>https://mirror.example/release/999</comments>` +
			`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/>` +
			`<torznab:attr name="infohash" value="` + strings.ToUpper(hash) + `"/></item>`)
	})
	defer srv.Close()

	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: hash, IsBest: true, ReleaseGroup: "G",
			Files: []seadex.File{
				{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"},
				{Length: 1, Name: "Show - S01E02 (1080p) [G].mkv"},
			},
		}},
	}}
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if snap.Titles["nyaa:42"] != "Show S01 1080p BluRay [G]" {
		t.Errorf("titles = %v, want the hash-matched harvested title under nyaa:42", snap.Titles)
	}
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "Show S01 1080p BluRay [G]" {
		t.Errorf("feed = %+v, want the harvested title served", snap.NyaaFeed)
	}
}

// TestHarvestSingleShowPagingStopsAtBudget pins the politeness bound on the
// paging leg: ONE show whose Nyaa search keeps returning full, non-matching
// pages drains the whole harvestSearchBudget through offset paging and then
// stops - the global budget bounds pages, not just shows - leaving the item
// synthetic for the next rebuild to retry.
func TestHarvestSingleShowPagingStopsAtBudget(t *testing.T) {
	filler := make([]string, 0, harvestPageSize)
	for i := range harvestPageSize {
		filler = append(filler, torznabItem(fmt.Sprintf("Other %d", i), fmt.Sprintf("https://nyaa.si/view/%d", 9000+i)))
	}
	mock, srv := newHarvestMock(func(int) string { return torznabBody(filler...) })
	defer srv.Close()

	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv", "Show - S01E02 (1080p) [G].mkv")}
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != harvestSearchBudget {
		t.Errorf("harvest queries = %d, want the budget %d (paging one show must respect the global budget)", mock.calls(), harvestSearchBudget)
	}
	if snap := readSnapshotFile(t, path); len(snap.Titles) != 0 {
		t.Errorf("titles = %v, want empty (nothing matched)", snap.Titles)
	}
}

// TestMatchHarvestSkipsEmptyTitlesAndKeepsFirstTitle pins two guards of the
// pure match step: a matched result with an empty/whitespace title caches
// nothing (an empty served title would be worse than the synthesized one),
// and an already-cached key is never overwritten (torrents are immutable, so
// the first harvested title stands).
func TestMatchHarvestSkipsEmptyTitlesAndKeepsFirstTitle(t *testing.T) {
	index := map[string]string{"nyaa:1": "nyaa:1", "nyaa:2": "nyaa:2"}
	titles := map[string]string{"nyaa:2": "First Title"}
	results := []item{
		{Title: "   ", InfoURL: "https://nyaa.si/view/1"},
		{Title: "Second Title", InfoURL: "https://nyaa.si/view/2"},
	}
	if n := matchHarvest(results, "nyaa", index, titles); n != 0 {
		t.Errorf("matchHarvest = %d matches, want 0", n)
	}
	if _, ok := titles["nyaa:1"]; ok {
		t.Errorf("empty-title result cached: %v", titles)
	}
	if titles["nyaa:2"] != "First Title" {
		t.Errorf("cached title overwritten: %v (the first harvested title stands)", titles)
	}
}

// TestMatchHarvestFailsClosedOnContradictoryIdentity pins resolveHarvestKey's
// fail-closed rule (the same one the search curation match applies in
// acceptScopedKeys): a result whose comments and guid page URLs name two
// DIFFERENT curated releases is an untrusted response and must title nothing -
// neither journal item may cache its attacker-chosen title.
func TestMatchHarvestFailsClosedOnContradictoryIdentity(t *testing.T) {
	index := map[string]string{"nyaa:1": "nyaa:1", "nyaa:2": "nyaa:2"}
	titles := map[string]string{}
	results := []item{
		{Title: "Tampered Title", InfoURL: "https://nyaa.si/view/1", GUID: "https://nyaa.si/view/2"},
	}
	if n := matchHarvest(results, "nyaa", index, titles); n != 0 {
		t.Errorf("matchHarvest = %d matches, want 0 (contradictory identity fails closed)", n)
	}
	if len(titles) != 0 {
		t.Errorf("contradictory-identity result cached a title: %v", titles)
	}
}

// TestMatchHarvestFailsClosedWhenURLAndHashResolveToDifferentReleases pins
// the other fail-closed branch of resolveHarvestKey: the page URLs agree with
// each other but the info hash maps to a DIFFERENT curated release, so the
// cross-signal contradiction must title nothing.
func TestMatchHarvestFailsClosedWhenURLAndHashResolveToDifferentReleases(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	index := map[string]string{"nyaa:1": "nyaa:1", hash: "nyaa:2"}
	titles := map[string]string{}
	results := []item{{
		Title: "Tampered Title", InfoURL: "https://nyaa.si/view/1",
		GUID: "https://nyaa.si/view/1", InfoHash: hash,
	}}
	if n := matchHarvest(results, "nyaa", index, titles); n != 0 {
		t.Errorf("matchHarvest = %d matches, want 0 (URL and hash resolving to different releases must fail closed)", n)
	}
	if len(titles) != 0 {
		t.Errorf("conflicting URL/hash identity cached a title: %v", titles)
	}
}

// TestMatchHarvestRejectsCrossScopeKey pins the scope binding matchHarvest
// shares with the search curation match (acceptScopedKeys): a result returned
// by one tracker's upstream whose identity resolves to the OTHER tracker's
// journal key must title nothing - a healthy Prowlarr never emits
// cross-tracker URLs, so such a result is an untrusted response.
func TestMatchHarvestRejectsCrossScopeKey(t *testing.T) {
	index := map[string]string{"ab:300": "ab:300"}
	titles := map[string]string{}
	results := []item{{Title: "AB title from the nyaa upstream", InfoURL: "https://animebytes.tv/torrent/300/group", GUID: "https://animebytes.tv/torrent/300/group"}}
	if n := matchHarvest(results, "nyaa", index, titles); n != 0 {
		t.Errorf("matchHarvest = %d matches, want 0 (a cross-scope key must not title the other tracker's item)", n)
	}
	if len(titles) != 0 {
		t.Errorf("cross-scope result cached a title: %v", titles)
	}
}

// TestMatchHarvestRejectsOversizedTitle pins the title length bound on the
// harvest cache: the titles map is persisted verbatim into the snapshot and
// rendered into every RSS response, so an absurd multi-KB title from a
// tampered/garbled upstream body must never enter the cache, while a normal
// title still caches.
func TestMatchHarvestRejectsOversizedTitle(t *testing.T) {
	index := map[string]string{"nyaa:1": "nyaa:1", "nyaa:2": "nyaa:2"}
	titles := map[string]string{}
	results := []item{
		{Title: strings.Repeat("A", harvestMaxTitleLen+1), InfoURL: "https://nyaa.si/view/1"},
		{Title: "Normal Title - S01 (1080p) [G]", InfoURL: "https://nyaa.si/view/2"},
	}
	if n := matchHarvest(results, "nyaa", index, titles); n != 1 {
		t.Errorf("matchHarvest = %d matches, want 1 (only the normal title caches)", n)
	}
	if _, ok := titles["nyaa:1"]; ok {
		t.Errorf("oversized title cached: %d bytes", len(titles["nyaa:1"]))
	}
	if titles["nyaa:2"] != "Normal Title - S01 (1080p) [G]" {
		t.Errorf("normal title not cached: %v", titles)
	}
}

// TestHarvestScopeWideFailureSkipsRemainingShows pins the scope-wide half of
// the harvest failure classification (the counterpart of
// TestHarvestMalformedResponseSkipsOnlyThatShow): after one show's query fails
// with a status error (upstream down/refusing), the SAME upstream's remaining
// shows are skipped this rebuild - only one show is ever queried, no matter
// how many are pending - all stay on synthesized titles, and the rebuild still
// succeeds. Distinct shows are counted by the q param, not raw HTTP calls,
// because the retry stack may issue several transport attempts per query.
func TestHarvestScopeWideFailureSkipsRemainingShows(t *testing.T) {
	entries := []seadex.Entry{
		nyaaEntry(7, 42, true, "Show A - S01E01 (1080p) [G].mkv", "Show A - S01E02 (1080p) [G].mkv"),
		nyaaEntry(8, 43, true, "Show B - S01E01 (1080p) [G].mkv", "Show B - S01E02 (1080p) [G].mkv"),
		nyaaEntry(9, 44, true, "Show C - S01E01 (1080p) [G].mkv", "Show C - S01E02 (1080p) [G].mkv"),
	}
	var mu sync.Mutex
	queried := map[string]int{}
	countSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queried[r.URL.Query().Get("q")]++
		mu.Unlock()
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer countSrv.Close()
	info := func(alID int) EntryInfo {
		switch alID {
		case 7:
			return EntryInfo{Title: "Show A", SeasonTvdb: 1}
		case 8:
			return EntryInfo{Title: "Show B", SeasonTvdb: 1}
		default:
			return EntryInfo{Title: "Show C", SeasonTvdb: 1}
		}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: countSrv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: countSrv.Client(), Logger: log})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	mu.Lock()
	shows := len(queried)
	mu.Unlock()
	if shows != 1 {
		t.Errorf("shows queried = %d (%v), want 1 (a scope-wide failure must skip the scope's remaining shows this rebuild)", shows, queried)
	}
	if !rec.Contains("indexer title harvest query failed; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("scope-wide failure not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	snap := readSnapshotFile(t, path)
	if len(snap.Titles) != 0 {
		t.Errorf("titles = %v, want empty (no show harvested after the scope failed)", snap.Titles)
	}
	if len(snap.NyaaFeed) != 3 {
		t.Errorf("feed = %d items, want 3 (skipped shows still serve synthesized titles)", len(snap.NyaaFeed))
	}
}

// TestHarvestCancellationMidQueryIsNotWarnedAsUpstreamFault pins harvest
// shutdown observability (the writer-side mirror of
// TestQueryCallerCancellationIsNotWarnedAsUpstreamFault): when the cycle
// context is cancelled while a harvest query is in flight (a daemon redeploy
// SIGTERM), the failed query is NOT logged as a harvest fault - neither the
// scope-wide nor the malformed WARN fires - nothing is cached, and the item
// stays pending for the next rebuild.
func TestHarvestCancellationMidQueryIsNotWarnedAsUpstreamFault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()

	log, rec := capture.New()
	cfg := &FeedWriterConfig{
		Path:           filepath.Join(t.TempDir(), "feed.json"),
		UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
	}
	w := NewFeedWriter(cfg, Deps{HTTP: srv.Client(), Logger: log})
	feeds := map[string][]item{upstreamNyaa: {{Key: "nyaa:42", AniListID: 7, Title: "Show S01"}}}
	titles := map[string]string{}
	stats := w.harvestTitles(ctx, feeds, titles, func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} })
	if len(titles) != 0 {
		t.Errorf("titles = %v, want empty (cancelled harvest must cache nothing)", titles)
	}
	if stats.pending != 1 {
		t.Errorf("stats.pending = %d, want 1 (the item stays synthetic for the next rebuild)", stats.pending)
	}
	if rec.Contains("indexer title harvest query failed; skipping this upstream's remaining shows this rebuild") ||
		rec.Contains("indexer title harvest response malformed; show keeps its synthesized title this rebuild") {
		t.Errorf("shutdown cancellation logged as a harvest fault; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestHarvestableGuards pins harvestable's admission guards directly: only a
// journal item that carries its bookkeeping (key + positive AniList id), has
// no cached real title yet, and whose show has a non-blank synthesis title
// source is due a harvest query.
func TestHarvestableGuards(t *testing.T) {
	title := func(int) EntryInfo { return EntryInfo{Title: "Show"} }
	noTitle := func(int) EntryInfo { return EntryInfo{} }
	tests := []struct {
		name   string
		it     item
		titles map[string]string
		info   func(int) EntryInfo
		want   bool
	}{
		{"pending journal item is harvestable", item{Key: "nyaa:42", AniListID: 7}, map[string]string{}, title, true},
		{"missing journal key", item{AniListID: 7}, map[string]string{}, title, false},
		{"non-positive AniList id", item{Key: "nyaa:42"}, map[string]string{}, title, false},
		{"already-cached title", item{Key: "nyaa:42", AniListID: 7}, map[string]string{"nyaa:42": "Real"}, title, false},
		{"no synthesis title source", item{Key: "nyaa:42", AniListID: 7}, map[string]string{}, noTitle, false},
		{"whitespace-only title source", item{Key: "nyaa:42", AniListID: 7}, map[string]string{}, func(int) EntryInfo { return EntryInfo{Title: "   "} }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := harvestable(&tc.it, tc.titles, tc.info); got != tc.want {
				t.Errorf("harvestable(%+v) = %v, want %v", tc.it, got, tc.want)
			}
		})
	}
}

// TestSyntheticCountSkipsKeylessItems pins the harvest_pending stat's
// domain: only journal-tracked items (non-empty Key) lacking a cached title
// count as pending; a key-less item (no journal bookkeeping, e.g. a
// search-shaped entry in a hand-edited or legacy snapshot) never counts.
func TestSyntheticCountSkipsKeylessItems(t *testing.T) {
	feeds := map[string][]item{
		upstreamNyaa: {
			{Key: "nyaa:1", Title: "synthetic"},
			{Key: "nyaa:2", Title: "harvested"},
			{Title: "keyless search-shaped item"},
		},
		upstreamAB: {
			{Key: "ab:3", Title: "synthetic"},
		},
	}
	titles := map[string]string{"nyaa:2": "Real Title"}
	if got := syntheticCount(feeds, titles); got != 2 {
		t.Errorf("syntheticCount = %d, want 2 (one keyed-untitled per feed; the key-less item never counts)", got)
	}
}
