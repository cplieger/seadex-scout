package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v3"
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

// TestMain replaces the harvest's politeness sleep for the whole package: the
// pacing gap is wall-clock politeness toward the trackers, not logic under
// test, and the suite must not spend 2s per simulated query. Tests that
// exercise the pacer's deadline install their own clock-advancing harvestWait
// (serially - nothing here runs t.Parallel).
func TestMain(m *testing.M) {
	harvestWait = func(context.Context, time.Duration) error { return nil }
	os.Exit(m.Run())
}

// TestHarvestTimeSliceEnforced pins the per-rebuild wall-clock slice: with a
// harvestWait that advances a fake clock by a quarter of the slice per pacing
// gap, only the queries fitting the slice run (the first query waits for no
// gap, so 1 + 4 gaps = 5 queries; the 5th gap crosses the deadline), the
// remaining shows keep their synthesized titles, and the persisted rotation
// cursor points at the last show that consumed a query so the NEXT rebuild
// resumes there instead of restarting at the head.
func TestHarvestTimeSliceEnforced(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	const shows = 12
	entries := make([]seadex.Entry, 0, shows)
	for i := range shows {
		entries = append(entries, nyaaEntry(1000+i, 500+i, true, fmt.Sprintf("Show %d - S01E01 (1080p) [G].mkv", i)))
	}
	info := func(alID int) EntryInfo { return EntryInfo{Title: fmt.Sprintf("Show %d", alID)} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	clock := time.Unix(1700000000, 0)
	w.now = func() time.Time { return clock }
	prevWait := harvestWait
	harvestWait = func(context.Context, time.Duration) error {
		clock = clock.Add(harvestTimeBudget / 4)
		return nil
	}
	t.Cleanup(func() { harvestWait = prevWait })

	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 5 {
		t.Errorf("harvest queries = %d, want 5 (1 gap-free + 4 quarter-slice gaps; the 5th gap crosses the deadline)", mock.calls())
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != shows {
		t.Errorf("feed = %d items, want %d (out-of-slice items still serve synthesized titles)", len(snap.NyaaFeed), shows)
	}
	if got, want := snap.HarvestCursor, "nyaa:1004"; got != want {
		t.Errorf("harvest cursor = %q, want %q (the last show that consumed a query)", got, want)
	}
}

// TestHarvestRotationResumesAfterCursor pins the anti-starvation rotation: a
// persisted cursor between two pending shows makes the rebuild query the
// LATER show first (wrapping to the earlier one afterwards), so a rebuild cut
// short never restarts at the head and a deep early show cannot starve its
// successors across rebuilds.
func TestHarvestRotationResumesAfterCursor(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	entries := []seadex.Entry{
		nyaaEntry(1000, 500, true, "Show A - S01E01 (1080p) [G].mkv"),
		nyaaEntry(2000, 600, true, "Show B - S01E01 (1080p) [G].mkv"),
	}
	info := func(alID int) EntryInfo {
		if alID == 1000 {
			return EntryInfo{Title: "Show A"}
		}
		return EntryInfo{Title: "Show B"}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedLedgerWithCursor(t, path, "nyaa:1500")
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}},
		Deps{HTTP: srv.Client()})
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 2 {
		t.Fatalf("harvest queries = %d, want 2 (both shows, one page each)", mock.calls())
	}
	if got := mock.request(0)["q"]; got != "Show B" {
		t.Errorf("first query q = %q, want %q (rotation starts after the cursor nyaa:1500)", got, "Show B")
	}
	if got := mock.request(1)["q"]; got != "Show A" {
		t.Errorf("second query q = %q, want %q (rotation wraps to the head)", got, "Show A")
	}
	if got, want := readSnapshotFile(t, path).HarvestCursor, "nyaa:1000"; got != want {
		t.Errorf("harvest cursor = %q, want %q (the last show that consumed a query)", got, want)
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

// TestHarvestRequestErrorSkipsOnlyThatShow pins the request-scoped half of the
// failure classification: a Torznab <error> document naming a
// request/parameter code (200-299) means the upstream deliberately rejected
// ONE show's query, so that show keeps its synthesized title this rebuild
// while a LATER group on the same upstream is still harvested - a
// deterministic bad request must never condemn the whole scope the way an
// auth (100-199) or status failure does.
func TestHarvestRequestErrorSkipsOnlyThatShow(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/rss+xml")
		if q == "Show A" {
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><error code="201" description="Incorrect parameter"/>`)
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
	if !rec.Contains("indexer title harvest request rejected; show keeps its synthesized title this rebuild") {
		t.Errorf("request-scoped rejection not warned as such; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	mu.Lock()
	gotQueries := slices.Clone(queries)
	mu.Unlock()
	if !slices.Contains(gotQueries, "Show A") || !slices.Contains(gotQueries, "Show B") {
		t.Errorf("queries = %v, want both shows queried (the rejection must stay show-local)", gotQueries)
	}
	snap := readSnapshotFile(t, path)
	if _, ok := snap.Titles["nyaa:42"]; ok {
		t.Errorf("titles = %v, want no cached title for the rejected show", snap.Titles)
	}
	if snap.Titles["nyaa:43"] != "Show B S01 1080p BluRay [G]" {
		t.Errorf("titles = %v, want the later show on the same upstream still harvested (nyaa:43)", snap.Titles)
	}
}

// TestHarvestUnconfiguredTrackerNeverQueried pins the tracker gate: a tracker
// with no configured Prowlarr upstream journals nothing (its Torznab URL is
// the off switch, so no items ever pend for it) and no harvest query leaves
// the process for it - while its identities still fold into the seen ledger,
// so enabling the tracker later starts from current novelty.
func TestHarvestUnconfiguredTrackerNeverQueried(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	// A Nyaa entry, but only the AB upstream is configured (pointing at
	// the mock): the nyaa scope must journal nothing and trigger no HTTP call.
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
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %+v, want empty (an unconfigured tracker journals nothing)", snap.NyaaFeed)
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing the skipped Nyaa identity (it must not journal later as new): %v", snap.Seen)
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

// TestHarvestResumesPagingAcrossRebuilds pins the checkpoint's per-group page
// state (the deep-paging contract): a show whose curated torrent sits beyond
// the first harvestShowPageCap full pages is cut off at the cap on rebuild
// one - which must persist the next page in the harvest cursor - and rebuild
// two must resume that group at the checkpointed offset (300, not a restart
// at zero) and cache the title, clearing the page state once satisfied.
func TestHarvestResumesPagingAcrossRebuilds(t *testing.T) {
	filler := make([]string, 0, harvestPageSize)
	for i := range harvestPageSize {
		filler = append(filler, torznabItem(fmt.Sprintf("Other %d", i), "https://nyaa.si/view/"+strconv.Itoa(9000+i)))
	}
	mock, srv := newHarvestMock(func(call int) string {
		if call < harvestShowPageCap {
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
		t.Fatalf("first Rebuild: %v", err)
	}
	if mock.calls() != harvestShowPageCap {
		t.Fatalf("first-rebuild queries = %d, want %d (offsets 0/100/200)", mock.calls(), harvestShowPageCap)
	}
	snap := readSnapshotFile(t, path)
	cp := decodeHarvestCheckpoint(snap.HarvestCursor)
	if got := cp.Pages["nyaa:7"]; got != harvestShowPageCap {
		t.Fatalf("checkpointed page = %d (cursor %q), want %d preserved for the cut-off group", got, snap.HarvestCursor, harvestShowPageCap)
	}
	if len(snap.Titles) != 0 {
		t.Fatalf("titles = %v, want empty after the capped first rebuild", snap.Titles)
	}

	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if mock.calls() != harvestShowPageCap+1 {
		t.Fatalf("total queries = %d, want %d (rebuild two resumes with one deeper page)", mock.calls(), harvestShowPageCap+1)
	}
	if off, want := mock.request(harvestShowPageCap)["offset"], strconv.Itoa(harvestShowPageCap*harvestPageSize); off != want {
		t.Errorf("resumed page offset = %q, want %q (resume deeper, not a restart at zero)", off, want)
	}
	snap = readSnapshotFile(t, path)
	if snap.Titles["nyaa:42"] != "Show S01 1080p BluRay [G]" {
		t.Errorf("titles = %v, want the deep-page match cached on rebuild two", snap.Titles)
	}
	if cp := decodeHarvestCheckpoint(snap.HarvestCursor); len(cp.Pages) != 0 {
		t.Errorf("checkpoint pages = %v, want empty once the group is satisfied", cp.Pages)
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

// TestHarvestSingleShowPagingStopsAtPageCap pins the anti-hog bound on the
// paging leg: ONE show whose Nyaa search keeps returning full, non-matching
// pages spends exactly harvestShowPageCap offset pages this rebuild and then
// stops - it can no longer monopolize the rebuild's time slice, the flaw that
// used to starve every show sorted after it - leaving the item synthetic to
// page deeper on later rebuilds.
func TestHarvestSingleShowPagingStopsAtPageCap(t *testing.T) {
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
	if mock.calls() != harvestShowPageCap {
		t.Errorf("harvest queries = %d, want the per-show page cap %d (one show must not monopolize the slice)", mock.calls(), harvestShowPageCap)
	}
	if got, want := mock.request(harvestShowPageCap - 1)["offset"], strconv.Itoa((harvestShowPageCap-1)*harvestPageSize); got != want {
		t.Errorf("last page offset = %q, want %q (offset paging advanced page by page)", got, want)
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
	feeds := map[string][]journalItem{
		upstreamNyaa: {{item: item{Title: "Show S01"}, Key: "nyaa:42", AniListID: 7}},
	}
	titles := map[string]string{}
	stats, _ := w.harvestTitles(ctx, feeds, titles, func(int) EntryInfo { return EntryInfo{Title: "Show", SeasonTvdb: 1} }, "")
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
		it     journalItem
		titles map[string]string
		info   func(int) EntryInfo
		want   bool
	}{
		{"pending journal item is harvestable", journalItem{item: item{}, Key: "nyaa:42", AniListID: 7}, map[string]string{}, title, true},
		{"missing journal key", journalItem{item: item{}, AniListID: 7}, map[string]string{}, title, false},
		{"non-positive AniList id", journalItem{item: item{}, Key: "nyaa:42"}, map[string]string{}, title, false},
		{"already-cached title", journalItem{item: item{}, Key: "nyaa:42", AniListID: 7}, map[string]string{"nyaa:42": "Real"}, title, false},
		{"no synthesis title source", journalItem{item: item{}, Key: "nyaa:42", AniListID: 7}, map[string]string{}, noTitle, false},
		{"whitespace-only title source", journalItem{item: item{}, Key: "nyaa:42", AniListID: 7}, map[string]string{}, func(int) EntryInfo { return EntryInfo{Title: "   "} }, false},
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
	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{item: item{Title: "synthetic"}, Key: "nyaa:1"},
			{item: item{Title: "harvested"}, Key: "nyaa:2"},
			{item: item{Title: "keyless search-shaped item"}},
		},
		upstreamAB: {
			{item: item{Title: "synthetic"}, Key: "ab:3"},
		},
	}
	titles := map[string]string{"nyaa:2": "Real Title"}
	if got := syntheticCount(feeds, titles); got != 2 {
		t.Errorf("syntheticCount = %d, want 2 (one keyed-untitled per feed; the key-less item never counts)", got)
	}
}

// TestHarvestParams pins the per-tracker query form the title harvest sends:
// Nyaa uses the season form (t=tvsearch, q + season) only for a non-movie
// with a mapped season - a seasonless show and a movie stay a plain search -
// while AnimeBytes is always a plain series-level search, and the q value is
// the trimmed synthesis title.
func TestHarvestParams(t *testing.T) {
	tests := []struct {
		name       string
		meta       EntryInfo
		scope      string
		wantT      string
		wantSeason string
	}{
		{"nyaa series with a mapped season uses the season form", EntryInfo{Title: "Frieren", SeasonTvdb: 1}, upstreamNyaa, "tvsearch", "1"},
		{"nyaa seasonless series stays a plain search", EntryInfo{Title: "One Piece"}, upstreamNyaa, "search", ""},
		{"nyaa movie stays a plain search even with a mapped season", EntryInfo{Title: "A Silent Voice", SeasonTvdb: 1, IsMovie: true}, upstreamNyaa, "search", ""},
		{"ab is always a plain series-level search", EntryInfo{Title: "Frieren", SeasonTvdb: 1}, upstreamAB, "search", ""},
		{"q is the trimmed synthesis title", EntryInfo{Title: "  Frieren  ", SeasonTvdb: 2}, upstreamNyaa, "tvsearch", "2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := harvestParams(tc.meta, tc.scope)
			if got.Get("t") != tc.wantT {
				t.Errorf("harvestParams(%+v, %q) t = %q, want %q", tc.meta, tc.scope, got.Get("t"), tc.wantT)
			}
			if got.Get("season") != tc.wantSeason {
				t.Errorf("harvestParams(%+v, %q) season = %q, want %q", tc.meta, tc.scope, got.Get("season"), tc.wantSeason)
			}
			if want := strings.TrimSpace(tc.meta.Title); got.Get("q") != want {
				t.Errorf("harvestParams(%+v, %q) q = %q, want %q", tc.meta, tc.scope, got.Get("q"), want)
			}
		})
	}
}

// TestHarvestMalformedResponsesLatchAtThreshold pins the latch boundary: the
// THIRD consecutive malformed show (consecutiveMalformedLatch) condemns the
// scope, so the fourth show is never queried - a >= to > regression makes a
// fourth query and caches its title instead.
func TestHarvestMalformedResponsesLatchAtThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		body := "this is not torznab xml <<<"
		if r.URL.Query().Get("q") == "Show D" {
			body = strings.ReplaceAll(
				torznabBody(torznabItem("Show D Real Title", "https://nyaa.si/view/45")),
				"http://prowlarr:9696", "http://"+r.Host,
			)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{item: item{Title: "Show A"}, Key: "nyaa:42", AniListID: 7},
			{item: item{Title: "Show B"}, Key: "nyaa:43", AniListID: 8},
			{item: item{Title: "Show C"}, Key: "nyaa:44", AniListID: 9},
			{item: item{Title: "Show D"}, Key: "nyaa:45", AniListID: 10},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"},
		9: {Title: "Show C"}, 10: {Title: "Show D"},
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}}, Deps{HTTP: srv.Client(), Logger: log})
	titles := map[string]string{}
	stats, _ := w.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != 3 {
		t.Errorf("harvest queries = %d, want 3 (the third consecutive malformed show latches the scope)", stats.queries)
	}
	if len(titles) != 0 || stats.pending != 4 {
		t.Errorf("titles = %v, pending = %d; want no titles and 4 pending after the latch", titles, stats.pending)
	}
	if !rec.Contains("indexer title harvest: repeated malformed responses; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("malformed-response latch not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestHarvestRejectedResponsesLatchAtThreshold pins the request-rejection
// twin of the malformed latch: the THIRD consecutive request-scoped Torznab
// rejection (consecutiveRejectedLatch, e.g. an indexer definition without
// tvsearch caps answering 201/203 to every season-form query) condemns the
// scope, so the fourth show is never queried and a deterministically-
// rejecting upstream cannot re-burn the whole budget with zero progress on
// every rebuild.
func TestHarvestRejectedResponsesLatchAtThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		body := `<?xml version="1.0" encoding="UTF-8"?><error code="201" description="Incorrect parameter"/>`
		if r.URL.Query().Get("q") == "Show D" {
			body = strings.ReplaceAll(
				torznabBody(torznabItem("Show D Real Title", "https://nyaa.si/view/45")),
				"http://prowlarr:9696", "http://"+r.Host,
			)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{item: item{Title: "Show A"}, Key: "nyaa:42", AniListID: 7},
			{item: item{Title: "Show B"}, Key: "nyaa:43", AniListID: 8},
			{item: item{Title: "Show C"}, Key: "nyaa:44", AniListID: 9},
			{item: item{Title: "Show D"}, Key: "nyaa:45", AniListID: 10},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"},
		9: {Title: "Show C"}, 10: {Title: "Show D"},
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}}, Deps{HTTP: srv.Client(), Logger: log})
	titles := map[string]string{}
	stats, _ := w.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != 3 {
		t.Errorf("harvest queries = %d, want 3 (the third consecutive rejected show latches the scope)", stats.queries)
	}
	if len(titles) != 0 || stats.pending != 4 {
		t.Errorf("titles = %v, pending = %d; want no titles and 4 pending after the latch", titles, stats.pending)
	}
	if !rec.Contains("indexer title harvest: repeated request rejections; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("request-rejection latch not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestHarvestMalformedResponseRunResetsAfterSuccessfulPage pins the
// CONSECUTIVE semantics of the malformed-show latch: a successful (even
// empty) page resets the run, so two separated malformed pairs never latch
// and a later healthy show is still harvested - removing the reset latches on
// the fourth show and leaves every title pending.
func TestHarvestMalformedResponseRunResetsAfterSuccessfulPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		var body string
		switch r.URL.Query().Get("q") {
		case "Show C":
			body = emptyTorznab()
		case "Show F":
			body = torznabBody(torznabItem("Show F Real Title", "https://nyaa.si/view/47"))
		default:
			body = "this is not torznab xml <<<"
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{item: item{Title: "Show A"}, Key: "nyaa:42", AniListID: 7},
			{item: item{Title: "Show B"}, Key: "nyaa:43", AniListID: 8},
			{item: item{Title: "Show C"}, Key: "nyaa:44", AniListID: 9},
			{item: item{Title: "Show D"}, Key: "nyaa:45", AniListID: 10},
			{item: item{Title: "Show E"}, Key: "nyaa:46", AniListID: 11},
			{item: item{Title: "Show F"}, Key: "nyaa:47", AniListID: 12},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"}, 9: {Title: "Show C"},
		10: {Title: "Show D"}, 11: {Title: "Show E"}, 12: {Title: "Show F"},
	}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}}, Deps{HTTP: srv.Client(), Logger: log})
	titles := map[string]string{}
	stats, _ := w.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != 6 {
		t.Errorf("harvest queries = %d, want 6 (a successful empty page resets the malformed run)", stats.queries)
	}
	if got := titles["nyaa:47"]; got != "Show F Real Title" {
		t.Errorf("titles[nyaa:47] = %q, want the post-reset show harvested", got)
	}
	if len(titles) != 1 || stats.pending != 5 {
		t.Errorf("titles = %v, pending = %d; want one harvested title and 5 pending", titles, stats.pending)
	}
}

// TestHarvestOpportunisticMatchSkipsSatisfiedGroup pins the satisfied-group
// skip in harvestTitles: matchHarvest matches against the GLOBAL identity
// index, so one show's page can title a LATER group's items opportunistically
// - and that group must then spend no query of the budget (the skip branch),
// with both titles cached from the single page.
func TestHarvestOpportunisticMatchSkipsSatisfiedGroup(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(
			torznabItem("Show A S01 1080p BluRay [G]", "https://nyaa.si/view/42"),
			torznabItem("Show B S01 1080p BluRay [G]", "https://nyaa.si/view/43"),
		)
	})
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{item: item{Title: "Show A"}, Key: "nyaa:42", AniListID: 7},
			{item: item{Title: "Show B"}, Key: "nyaa:43", AniListID: 8},
		},
	}
	info := map[int]EntryInfo{7: {Title: "Show A"}, 8: {Title: "Show B"}}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}}, Deps{HTTP: srv.Client(), Logger: log})
	titles := map[string]string{}
	stats, _ := w.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if mock.calls() != 1 || stats.queries != 1 {
		t.Errorf("harvest queries = %d (HTTP calls %d), want 1 (the satisfied group must be skipped without a query)", stats.queries, mock.calls())
	}
	if titles["nyaa:42"] != "Show A S01 1080p BluRay [G]" || titles["nyaa:43"] != "Show B S01 1080p BluRay [G]" {
		t.Errorf("titles = %v, want both shows titled from the single page", titles)
	}
	if stats.matched != 2 || stats.pending != 0 {
		t.Errorf("stats = %+v, want matched=2 pending=0", stats)
	}
}

// TestHarvestRequestRejectionResetsMalformedRun pins the harvestShowFailed
// arm of updateHarvestScopeState: a request-scoped Torznab rejection resets
// the consecutive-malformed run like a success, so two separated malformed
// pairs never latch and a later healthy show is still harvested.
func TestHarvestRequestRejectionResetsMalformedRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		var body string
		switch r.URL.Query().Get("q") {
		case "Show C":
			body = `<?xml version="1.0" encoding="UTF-8"?><error code="201" description="Incorrect parameter"/>`
		case "Show F":
			body = strings.ReplaceAll(
				torznabBody(torznabItem("Show F Real Title", "https://nyaa.si/view/47")),
				"http://prowlarr:9696", "http://"+r.Host)
		default:
			body = "this is not torznab xml <<<"
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{item: item{Title: "Show A"}, Key: "nyaa:42", AniListID: 7},
			{item: item{Title: "Show B"}, Key: "nyaa:43", AniListID: 8},
			{item: item{Title: "Show C"}, Key: "nyaa:44", AniListID: 9},
			{item: item{Title: "Show D"}, Key: "nyaa:45", AniListID: 10},
			{item: item{Title: "Show E"}, Key: "nyaa:46", AniListID: 11},
			{item: item{Title: "Show F"}, Key: "nyaa:47", AniListID: 12},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"}, 9: {Title: "Show C"},
		10: {Title: "Show D"}, 11: {Title: "Show E"}, 12: {Title: "Show F"},
	}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}}, Deps{HTTP: srv.Client(), Logger: log})
	titles := map[string]string{}
	stats, _ := w.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != 6 {
		t.Errorf("harvest queries = %d, want 6 (a request-scoped rejection must reset the malformed run like a success)", stats.queries)
	}
	if got := titles["nyaa:47"]; got != "Show F Real Title" {
		t.Errorf("titles[nyaa:47] = %q, want the post-rejection show harvested", got)
	}
	if len(titles) != 1 || stats.pending != 5 {
		t.Errorf("titles = %v, pending = %d; want one harvested title and 5 pending", titles, stats.pending)
	}
}

// TestUpdateHarvestScopeState_resetsRejectedRun pins the inverse reset
// direction of the rejected latch: a successful or malformed show resets the
// consecutive-rejected run, so two rejections, an intervening non-rejection,
// and a later rejection never latch the scope.
func TestUpdateHarvestScopeState_resetsRejectedRun(t *testing.T) {
	tests := []struct {
		name  string
		reset harvestOutcome
	}{
		{name: "successful show", reset: harvestOK},
		{name: "malformed show", reset: harvestShowMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := capture.New()
			w := NewFeedWriter(&FeedWriterConfig{}, Deps{Logger: log})
			failed := map[string]bool{}
			malformed := map[string]int{}
			rejected := map[string]int{}
			w.updateHarvestScopeState(upstreamNyaa, harvestShowFailed, failed, malformed, rejected)
			w.updateHarvestScopeState(upstreamNyaa, harvestShowFailed, failed, malformed, rejected)
			w.updateHarvestScopeState(upstreamNyaa, tc.reset, failed, malformed, rejected)
			w.updateHarvestScopeState(upstreamNyaa, harvestShowFailed, failed, malformed, rejected)
			if failed[upstreamNyaa] {
				t.Fatal("scope latched after a non-consecutive third rejection; the intervening outcome must reset the run")
			}
			if got := rejected[upstreamNyaa]; got != 1 {
				t.Errorf("rejected run after reset = %d, want 1", got)
			}
		})
	}
}

// TestRequestScopedHarvestError pins the boundaries of the show-local
// classification directly, across both failure shapes. Torznab documents:
// 200-299 is request-scoped, 100-199 (auth) and anything else stays
// scope-wide, a non-numeric code never classifies, and the document error is
// found through a wrap. HTTP statuses: only the request-specific 400/414/422
// are show-local; auth/config (401/403/404), timeout (408), rate-limit (429),
// and server (5xx) statuses stay scope-wide.
func TestRequestScopedHarvestError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"code 200 lower bound is request-scoped", newUpstreamDocError("200", ""), true},
		{"code 299 upper bound is request-scoped", newUpstreamDocError("299", ""), true},
		{"code 199 auth code stays scope-wide", newUpstreamDocError("199", ""), false},
		{"code 300 stays scope-wide", newUpstreamDocError("300", ""), false},
		{"non-numeric code stays scope-wide", newUpstreamDocError("20x", ""), false},
		{"empty code stays scope-wide", newUpstreamDocError("", ""), false},
		{"non-document error stays scope-wide", fmt.Errorf("connection refused"), false},
		{"wrapped document error still classifies", fmt.Errorf("search %q: %w", "Show", newUpstreamDocError("201", "")), true},
		{"HTTP 400 bad request is request-scoped", &httpx.StatusError{Code: http.StatusBadRequest}, true},
		{"HTTP 414 URI too long is request-scoped", &httpx.StatusError{Code: http.StatusRequestURITooLong}, true},
		{"HTTP 422 unprocessable entity is request-scoped", &httpx.StatusError{Code: http.StatusUnprocessableEntity}, true},
		{"HTTP 401 unauthorized stays scope-wide", &httpx.StatusError{Code: http.StatusUnauthorized}, false},
		{"HTTP 403 forbidden stays scope-wide", &httpx.StatusError{Code: http.StatusForbidden}, false},
		{"HTTP 404 not found stays scope-wide", &httpx.StatusError{Code: http.StatusNotFound}, false},
		{"HTTP 408 request timeout stays scope-wide", &httpx.StatusError{Code: http.StatusRequestTimeout}, false},
		{"HTTP 429 rate limit stays scope-wide", &httpx.StatusError{Code: http.StatusTooManyRequests}, false},
		{"HTTP 500 server error stays scope-wide", &httpx.StatusError{Code: http.StatusInternalServerError}, false},
		{"HTTP 503 unavailable stays scope-wide", &httpx.StatusError{Code: http.StatusServiceUnavailable}, false},
		{"wrapped status error still classifies", fmt.Errorf("search %q: %w", "Show", &httpx.StatusError{Code: http.StatusBadRequest}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestScopedHarvestError(tc.err); got != tc.want {
				t.Errorf("requestScopedHarvestError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRequestScopedClassificationSurvivesKeyRedaction pins the parse-before-
// redact contract: classifyParseError scrubs a reflected Prowlarr API key
// from the document's DISPLAY strings, and with a short all-digit key ("2")
// that scrub rewrites a valid request code ("201" -> "REDACTED01") - but the
// show-vs-scope classification must still read the parse-time codeNum and
// classify the rejection show-local, never re-parsing the redacted string
// (the c2 audit regression: a one-show rejection wrongly condemned the whole
// scope).
func TestRequestScopedClassificationSurvivesKeyRedaction(t *testing.T) {
	u := &upstream{name: upstreamNyaa, apiKey: "2"}
	err := u.classifyParseError(newUpstreamDocError("201", "missing parameter (apikey=2)"))
	docErr, ok := errors.AsType[*upstreamDocError](err)
	if !ok {
		t.Fatalf("classifyParseError = %T (%v), want the terminal *upstreamDocError", err, err)
	}
	if docErr.code != "REDACTED01" {
		t.Errorf("redacted code string = %q, want %q (display text scrubbed)", docErr.code, "REDACTED01")
	}
	if !strings.Contains(docErr.description, "REDACTED") {
		t.Errorf("redacted description = %q, want the key scrubbed", docErr.description)
	}
	if !requestScopedHarvestError(err) {
		t.Error("requestScopedHarvestError = false after redaction rewrote the code string; classification must read the parse-time codeNum")
	}
}

// TestHarvestHTTPStatusFailureScoping pins the HTTP-status sibling of the
// Torznab-document classification end to end: a request-specific status
// (400/414/422) answered to ONE show's query consumes only that show's
// budget - the SAME upstream's next show is still queried and harvested -
// while an auth/config status (401/403/404) latches the whole scope, so the
// next show is never queried. Without the status arm of
// requestScopedHarvestError, a single title whose encoded query the upstream
// rejects with 400 would condemn every later healthy show on the tracker to
// synthesized titles.
func TestHarvestHTTPStatusFailureScoping(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		showLocal bool
	}{
		{"400 bad request stays show-local", http.StatusBadRequest, true},
		{"414 URI too long stays show-local", http.StatusRequestURITooLong, true},
		{"422 unprocessable entity stays show-local", http.StatusUnprocessableEntity, true},
		{"401 unauthorized latches the scope", http.StatusUnauthorized, false},
		{"403 forbidden latches the scope", http.StatusForbidden, false},
		{"404 not found latches the scope", http.StatusNotFound, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("q") == "Show A" {
					http.Error(w, "rejected", tc.status)
					return
				}
				w.Header().Set("Content-Type", "application/rss+xml")
				body := torznabBody(torznabItem("Show B S01 1080p BluRay [G]", "https://nyaa.si/view/43"))
				_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
			}))
			defer srv.Close()

			feeds := map[string][]journalItem{
				upstreamNyaa: {
					{item: item{Title: "Show A"}, Key: "nyaa:42", AniListID: 7},
					{item: item{Title: "Show B"}, Key: "nyaa:43", AniListID: 8},
				},
			}
			info := map[int]EntryInfo{7: {Title: "Show A"}, 8: {Title: "Show B"}}
			log, rec := capture.New()
			w := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: UpstreamConfig{
				NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
			}}, Deps{HTTP: srv.Client(), Logger: log})
			titles := map[string]string{}
			stats, _ := w.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

			if _, ok := titles["nyaa:42"]; ok {
				t.Errorf("titles = %v, want no cached title for the rejected show", titles)
			}
			if tc.showLocal {
				if stats.queries != 2 {
					t.Errorf("harvest queries = %d, want 2 (a request-specific status must consume only one show's budget)", stats.queries)
				}
				if titles["nyaa:43"] != "Show B S01 1080p BluRay [G]" {
					t.Errorf("titles = %v, want the later show on the same upstream still harvested (nyaa:43)", titles)
				}
				if !rec.Contains("indexer title harvest request rejected; show keeps its synthesized title this rebuild") {
					t.Errorf("request-specific status not warned as a show-local rejection; log output:\n%s", strings.Join(rec.Messages(), "\n"))
				}
			} else {
				if stats.queries != 1 {
					t.Errorf("harvest queries = %d, want 1 (an auth/config status must latch the scope)", stats.queries)
				}
				if len(titles) != 0 {
					t.Errorf("titles = %v, want empty (no show harvested after the scope latched)", titles)
				}
				if !rec.Contains("indexer title harvest query failed; skipping this upstream's remaining shows this rebuild") {
					t.Errorf("scope-wide status not warned as such; log output:\n%s", strings.Join(rec.Messages(), "\n"))
				}
			}
		})
	}
}

// TestResolveHarvestKeyPartialSignals pins the partial-identity resolution
// table of resolveHarvestKey: one tracker page URL (guid OR comments) alone
// resolves, agreeing URL+hash resolve, an unknown id or a signal-less result
// resolves nothing - the accepting side of the fail-closed contract the
// contradictory-identity tests pin from the rejecting side.
func TestResolveHarvestKeyPartialSignals(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	index := map[string]string{"nyaa:42": "nyaa:42", hash: "nyaa:42"}
	tests := []struct {
		name string
		it   item
		want string
	}{
		{"guid alone resolves when comments URL is foreign", item{InfoURL: "https://mirror.example/x", GUID: "https://nyaa.si/view/42"}, "nyaa:42"},
		{"comments alone resolves when guid URL is foreign", item{InfoURL: "https://nyaa.si/view/42", GUID: "https://mirror.example/x"}, "nyaa:42"},
		{"url and hash agreeing on one release resolve it", item{InfoURL: "https://nyaa.si/view/42", GUID: "https://nyaa.si/view/42", InfoHash: hash}, "nyaa:42"},
		{"unknown id resolves nothing", item{InfoURL: "https://nyaa.si/view/999", GUID: "https://nyaa.si/view/999"}, ""},
		{"no identity signals resolve nothing", item{InfoURL: "https://mirror.example/x", GUID: "https://mirror.example/x"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveHarvestKey(&tc.it, index); got != tc.want {
				t.Errorf("resolveHarvestKey(%+v) = %q, want %q", tc.it, got, tc.want)
			}
		})
	}
}

// TestHarvestPacerNextDeniedBranches pins the two refusal paths of the pacer
// no end-to-end test reaches: a slice already spent at entry admits no query
// (harvestShow's inner page loop calls next directly, so page 2+ of a show
// can arrive with the slice expired and no outer pre-check), and a pacing
// gap cut short by cancellation (harvestWait returning the context error)
// admits no query rather than letting a shutdown leak one last request.
func TestHarvestPacerNextDeniedBranches(t *testing.T) {
	base := time.Unix(1700000000, 0)
	t.Run("spent slice admits no query at entry", func(t *testing.T) {
		p := &harvestPacer{now: func() time.Time { return base }, deadline: base.Add(-time.Second)}
		if p.next(context.Background()) {
			t.Error("next = true with the slice already spent, want false")
		}
	})
	t.Run("cancelled pacing gap admits no query", func(t *testing.T) {
		prev := harvestWait
		harvestWait = func(context.Context, time.Duration) error { return context.Canceled }
		t.Cleanup(func() { harvestWait = prev })
		p := &harvestPacer{now: func() time.Time { return base }, deadline: base.Add(time.Hour), started: true}
		if p.next(context.Background()) {
			t.Error("next = true when the pacing gap was cancelled, want false")
		}
	})
}

// TestRotationStart pins the cursor-resolution table directly (the
// end-to-end rotation test covers only a cursor strictly between two
// groups): the group AFTER the cursor is picked, a vanished cursor group
// lands on its order-successor, a cursor on or past the LAST group wraps to
// the head - the steady-state case every rebuild whose final query hit the
// tail-ordered show produces - and an unparseable cursor (hand-edited or
// legacy snapshot: non-numeric id, no colon) starts at the head.
func TestRotationStart(t *testing.T) {
	groups := []harvestGroup{
		{scope: "ab", alID: 10},
		{scope: "nyaa", alID: 5},
		{scope: "nyaa", alID: 9},
	}
	tests := []struct {
		name   string
		cursor string
		want   int
	}{
		{"cursor on the first group resumes at the second", "ab:10", 1},
		{"cursor on a vanished group lands on its successor", "nyaa:7", 2},
		{"cursor on the last group wraps to the head", "nyaa:9", 0},
		{"cursor past every group wraps to the head", "nyaa:9999", 0},
		{"non-numeric id starts at the head", "nyaa:abc", 0},
		{"colon-less cursor starts at the head", "garbage", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rotationStart(groups, tc.cursor); got != tc.want {
				t.Errorf("rotationStart(%q) = %d, want %d", tc.cursor, got, tc.want)
			}
		})
	}
}
