package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
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
//
// The fixture is also the class the title audit CORRECTS end to end: the
// harvested title claims a whole season while the release ships exactly one
// proven episode, so the served title keeps every byte of the tracker's own name
// except its season token, which becomes the season+episode form the file census
// names (titleAudit.served). The CACHE holds that SERVED title, not the raw
// harvested claim: the correction is derived from a file census a later pass may
// not hold (a tick's census covers its window only), so caching the raw claim let
// every such pass re-serve the whole-season title over the correction. Caching
// what is served is what a pass with no census evidence carries.
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
	info := func(int) EntryInfo {
		return EntryInfo{Title: "Frieren: Beyond Journey's End", Season: 1, SeasonKnown: true}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, ABPasskey: "PK", ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
	if got, want := snap.ABFeed[0].Title, "[PMR] Frieren S01E01 [BD Remux 1080p]"; got != want {
		t.Errorf("served title = %q, want the harvested real title with its provably-wrong season token corrected %q", got, want)
	}
	if snap.Titles["ab:1167293"] != "[PMR] Frieren S01E01 [BD Remux 1080p]" {
		t.Errorf("title cache = %v, want the SERVED (season-corrected) title under ab:1167293 so a pass holding no file census carries it instead of re-serving the whole-season claim", snap.Titles)
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
	info := func(int) EntryInfo { return EntryInfo{Title: "Frieren", Season: 1, SeasonKnown: true} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 1 {
		t.Fatalf("harvest queries = %d, want 1", mock.calls())
	}
	req := mock.request(0)
	if req["t"] != "tvsearch" || req["q"] != "Frieren" || req["season"] != "1" {
		t.Errorf("Nyaa harvest params = %v, want the season-form query (t=tvsearch, q, season=1)", req)
	}
	if _, ok := req["limit"]; ok {
		t.Errorf("limit = %q, want no limit sent (AnimeBytes ignores it and Nyaa caps below it)", req["limit"])
	}
	if _, ok := req["offset"]; ok {
		t.Errorf("offset = %q, want no offset sent (neither upstream honours it through Prowlarr)", req["offset"])
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
	info := func(int) EntryInfo { return EntryInfo{Title: "Frieren", Season: 1, SeasonKnown: true} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if mock.calls() != 1 {
		t.Errorf("harvest queries after first rebuild = %d, want 1", mock.calls())
	}
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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

// TestMain replaces the two wall-clock waits the whole package would otherwise
// spend in real time. The harvest's pacing gap is politeness toward the
// trackers, not logic under test, and the suite must not spend 2s per simulated
// query. The Prowlarr retry backoff is the same kind of value, and the
// retry-exhaustion tests pay it two attempts deep per failed query - about 50s
// of this package's wall clock, re-paid by every PR run and by every gremlins
// mutant that re-executes it. Tests that assert on the retry BUDGET (attempt
// counts, Retry-After hints) are unaffected: none of them measures elapsed
// time. Tests that exercise the pacer's deadline install their own
// clock-advancing harvestWait (serially - nothing here runs t.Parallel).
func TestMain(m *testing.M) {
	harvestWait = func(context.Context, time.Duration) error { return nil }
	upstreamBaseDelay = time.Millisecond
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
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	clock := time.Unix(1700000000, 0)
	w.now = func() time.Time { return clock }
	prevWait := harvestWait
	harvestWait = func(context.Context, time.Duration) error {
		clock = clock.Add(harvestTimeBudget / 4)
		return nil
	}
	t.Cleanup(func() { harvestWait = prevWait })

	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", Season: 1, SeasonKnown: true} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		log, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
			return EntryInfo{Title: "Show A", Season: 1, SeasonKnown: true}
		}
		return EntryInfo{Title: "Show B", Season: 1, SeasonKnown: true}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		log, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
			return EntryInfo{Title: "Show A", Season: 1, SeasonKnown: true}
		}
		return EntryInfo{Title: "Show B", Season: 1, SeasonKnown: true}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		log, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
// the process for it - while its identities still fold into the publication log,
// so enabling the tracker later starts from current novelty.
func TestHarvestUnconfiguredTrackerNeverQueried(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	// A Nyaa entry, but only the AB upstream is configured (pointing at
	// the mock): the nyaa scope must journal nothing and trigger no HTTP call.
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", Season: 1, SeasonKnown: true} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, ABPasskey: "PK", ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if mock.calls() != 0 {
		t.Errorf("harvest queries = %d, want 0 (no upstream configured for the nyaa scope)", mock.calls())
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %+v, want empty (an unconfigured tracker journals nothing)", snap.NyaaFeed)
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing the skipped Nyaa identity (it must not journal later as new): %v", snap.Published)
	}
}

// TestHarvestSpendsOneQueryPerTitleCandidate pins the post-paging-removal
// query budget: exactly ONE query per (show, title candidate), whatever the
// response size. Offset paging was removed because neither upstream honours
// `offset` through Prowlarr (measured 2026-07-29; see the note in harvest.go),
// and the cost it left behind was a wasted paced query per unsatisfied
// AnimeBytes candidate - AB returns the show's whole set in one response, so a
// full response always read as "there is more". A satisfied show must still
// cost exactly one query, and an unsatisfied two-candidate ladder must cost
// exactly two rather than one per candidate per page.
func TestHarvestSpendsOneQueryPerTitleCandidate(t *testing.T) {
	// A response at least as large as the retired page stride: under offset
	// paging this was the shape that kept a show paging, so it is the shape
	// that must NOT cost a second query now.
	const fullOldPage = 100
	filler := make([]string, 0, fullOldPage)
	for i := range fullOldPage {
		filler = append(filler, torznabItem(fmt.Sprintf("Other %d", i), "https://nyaa.si/view/"+strconv.Itoa(9000+i)))
	}
	tests := map[string]struct {
		title      string
		match      bool
		wantCalls  int
		wantTitled bool
	}{
		"a satisfied single-candidate show costs one query": {
			title: "Show", match: true, wantCalls: 1, wantTitled: true,
		},
		"an unsatisfied two-candidate ladder costs two queries": {
			title: "Show (2023)", match: false, wantCalls: 2, wantTitled: false,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			mock, srv := newHarvestMock(func(int) string {
				if tc.match {
					return torznabBody(torznabItem("Show S01 1080p BluRay [G]", "https://nyaa.si/view/42"))
				}
				return torznabBody(filler...)
			})
			defer srv.Close()

			feeds := map[string][]journalItem{
				upstreamNyaa: {{Title: "Show S01", Key: "nyaa:42", AniListID: 7}},
			}
			w := NewFeedWriter(&FeedWriterConfig{
				NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
			}, nil, srv.Client())
			titles := map[string]string{}
			w.harvest.harvestTitles(t.Context(), feeds, titles,
				func(int) EntryInfo { return EntryInfo{Title: tc.title, Season: 1, SeasonKnown: true} }, "")

			if got := mock.calls(); got != tc.wantCalls {
				t.Errorf("harvest queries = %d, want %d (one per title candidate, never a second page)", got, tc.wantCalls)
			}
			for i := range mock.calls() {
				if off, ok := mock.request(i)["offset"]; ok {
					t.Errorf("query %d sent offset = %q, want no offset at all", i, off)
				}
			}
			if _, titled := titles["nyaa:42"]; titled != tc.wantTitled {
				t.Errorf("titled = %v, want %v", titled, tc.wantTitled)
			}
		})
	}
}

// TestHarvestConsumesTheWholeSingleResponse is the evidence h-f50 was dismissed
// on: AnimeBytes returns a show's entire torrent set in ONE response (725 items
// for the largest measured show, well inside maxUpstreamItems), and the app
// decodes all of it, so an adjacent alias run can never be split across a page
// boundary - there are no pages. The fixture puts the target torrent's two
// aliases at the very END of a 725-item response, the position a boundary split
// would have truncated, and requires the alias the COMPLETE set yields (the
// arr-vocabulary Romaji one, which loses the most-parseable fallback the
// English alias alone would win).
func TestHarvestConsumesTheWholeSingleResponse(t *testing.T) {
	const (
		responseItems = 725
		romaji        = "[PMR] Sousou no Frieren - S01 (BD Remux 1080p)"
		english       = "[PMR] Frieren Beyond Journeys End Extended Edition - S01 (BD Remux 1080p)"
	)
	items := make([]string, 0, responseItems)
	for i := range responseItems - 2 {
		items = append(items, torznabItem(fmt.Sprintf("Other %d", i), "https://animebytes.tv/torrent/"+strconv.Itoa(900000+i)+"/group"))
	}
	items = append(items,
		torznabItem(english, "https://animebytes.tv/torrent/1167293/group?nh=a"),
		torznabItem(romaji, "https://animebytes.tv/torrent/1167293/group?nh=b"),
	)
	mock, srv := newHarvestMock(func(int) string { return torznabBody(items...) })
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamAB: {{Title: "Frieren S01", Key: "ab:1167293", AniListID: 154587}},
	}
	w := NewFeedWriter(&FeedWriterConfig{
		ABTorznabURL: srv.URL, ABPasskey: "PK", ProwlarrAPIKey: "k",
	}, nil, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles,
		func(int) EntryInfo { return EntryInfo{Title: "Sousou no Frieren"} }, "")

	if mock.calls() != 1 || stats.matched != 1 {
		t.Errorf("harvest queries = %d, matched = %d; want 1 and 1 (the whole set arrives in one response)", mock.calls(), stats.matched)
	}
	if got := titles["ab:1167293"]; got != romaji {
		t.Errorf("cached title = %q, want %q - the alias the complete trailing run yields", got, romaji)
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
	info := func(int) EntryInfo { return EntryInfo{Title: "Show", Season: 1, SeasonKnown: true} }
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"},
		nil, srv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
	if n, _, _, _ := matchHarvest(results, "nyaa", index, titles, nil, nil); n != 0 {
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
	if n, _, _, _ := matchHarvest(results, "nyaa", index, titles, nil, nil); n != 0 {
		t.Errorf("matchHarvest = %d matches, want 0 (contradictory identity fails closed)", n)
	}
	if len(titles) != 0 {
		t.Errorf("contradictory-identity result cached a title: %v", titles)
	}
}

// TestMatchHarvestGradesRejectionsAgainstTheGroupsPendingKeys pins the pending
// grade the fruitless backstop reads (d-gpt-u8-1, refined by h-f35). A
// contradictory result is always refused and always counted in
// harvest_rejected, but only a refusal that touched one of THIS GROUP's pending
// releases means this show harvested nothing: a result whose comments and guid
// disagree with each other is refused before either signal is looked up, and
// AnimeBytes answers the SAME broad series-level corpus to every query, so one
// unrelated malformed item in that corpus repeats across every show. Grading it
// as no-progress - whether it belongs to no pending show at all, or to a
// DIFFERENT pending show than the one queried - would let it condemn the scope
// after consecutiveFruitlessLatch otherwise-clean shows.
func TestMatchHarvestGradesRejectionsAgainstTheGroupsPendingKeys(t *testing.T) {
	index := map[string]string{"nyaa:1": "nyaa:1", "nyaa:2": "nyaa:2", "nyaa:7": "nyaa:7"}
	titles := map[string]string{}
	results := []item{
		// Both signals name this group's releases: one of ours was refused.
		{Title: "Tampered Ours", InfoURL: "https://nyaa.si/view/1", GUID: "https://nyaa.si/view/2"},
		// Self-contradictory, but neither id is pending: not ours at all.
		{Title: "Tampered Stranger", InfoURL: "https://nyaa.si/view/900", GUID: "https://nyaa.si/view/901"},
		// Self-contradictory and globally pending, but it belongs to a
		// DIFFERENT group: this show refused nothing of its own.
		{Title: "Tampered Other Show", InfoURL: "https://nyaa.si/view/7", GUID: "https://nyaa.si/view/902"},
	}
	matched, rejected, pendingRejected, unusable := matchHarvest(results, upstreamNyaa, index, titles, nil, []string{"nyaa:1", "nyaa:2"})
	if matched != 0 || rejected != 3 || pendingRejected != 1 {
		t.Errorf("matchHarvest = %d matched / %d rejected / %d pendingRejected, want 0/3/1 (all refused, only one of them this group's)",
			matched, rejected, pendingRejected)
	}
	if unusable != 0 {
		t.Errorf("unusable = %d, want 0 (every result carried an admissible title)", unusable)
	}
	if len(titles) != 0 {
		t.Errorf("contradictory-identity results cached a title: %v", titles)
	}
}

// TestMatchHarvestCountsUnusableTitlesNamingThisGroup pins the OTHER silent
// drop's grade: a result whose page URL names one of this group's still-pending
// releases but whose title cannot enter the persisted cache (blank or over
// harvestMaxTitleLen) strands that release on its synthesized title, charges no
// rejection, and does not mark the show contradicted - so it must be counted
// separately or an upstream emitting unusable titles for our items burns every
// rebuild's slice with no signal.
func TestMatchHarvestCountsUnusableTitlesNamingThisGroup(t *testing.T) {
	index := map[string]string{"nyaa:1": "nyaa:1", "nyaa:2": "nyaa:2"}
	titles := map[string]string{}
	results := []item{
		{Title: strings.Repeat("x", harvestMaxTitleLen+1), InfoURL: "https://nyaa.si/view/1"},
		// Not one of ours: an unusable title on an unrelated item carries no
		// signal about this show.
		{Title: "   ", InfoURL: "https://nyaa.si/view/900"},
	}
	matched, rejected, pendingRejected, unusable := matchHarvest(results, upstreamNyaa, index, titles, nil, []string{"nyaa:1", "nyaa:2"})
	if matched != 0 || rejected != 0 || pendingRejected != 0 || unusable != 1 {
		t.Errorf("matchHarvest = %d matched / %d rejected / %d pendingRejected / %d unusable, want 0/0/0/1",
			matched, rejected, pendingRejected, unusable)
	}
	if len(titles) != 0 {
		t.Errorf("an inadmissible title entered the cache: %v", titles)
	}
}

// TestHarvestReportsADegradedCheckpoint pins the operator-facing half of the
// cursor rebaseline: decodeHarvestCursor reporting WHY it degraded only
// helps if harvestTitles actually says so, and a clean cursor must stay silent
// so the signal is not noise on every rebuild.
func TestHarvestReportsADegradedCheckpoint(t *testing.T) {
	const warnMsg = "indexer title harvest checkpoint degraded"
	tests := []struct {
		name   string
		cursor string
		want   bool
	}{
		{name: "truncated JSON warns", cursor: `{"pages": {"nyaa:7": `, want: true},
		{name: "unparseable rotation cursor warns", cursor: "bogus:5", want: true},
		// The JSON object form no released binary can write is now simply an
		// invalid rotation cursor, so it rebaselines and says so rather than
		// being read as a cursor.
		{name: "a JSON cursor warns", cursor: `{"last":"nyaa:7","pages":{"nyaa:7":3}}`, want: true},
		{name: "clean bare cursor stays silent", cursor: "nyaa:7", want: false},
		{name: "absent cursor stays silent", cursor: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newHarvestMock(func(int) string { return emptyTorznab() })
			defer srv.Close()
			log, rec := capture.New()
			w := NewFeedWriter(&FeedWriterConfig{
				Path:           filepath.Join(t.TempDir(), "feed.json"),
				NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
			}, log, srv.Client())
			w.harvest.harvestTitles(t.Context(), map[string][]journalItem{}, map[string]string{},
				func(int) EntryInfo { return EntryInfo{} }, tc.cursor)
			if got := rec.Contains(warnMsg); got != tc.want {
				t.Errorf("checkpoint WARN emitted = %v, want %v; log output:\n%s",
					got, tc.want, strings.Join(rec.Messages(), "\n"))
			}
		})
	}
}

// TestHarvestReportsStrandedReleases pins the WARN that carries matchHarvest's
// pendingRejected/unusable grades to an operator: a result naming this show's
// own still-untitled release whose title cannot enter the cache may strand that
// release on its synthesized title for the whole journal window, and unusable
// rides no stat at all, so this line is its only report. One line per show per
// rebuild, never per candidate.
//
// The ladder is what makes the once-per-show half falsifiable, and it starts
// CLEAN: the first candidate's response strands nothing, so the second
// candidate's is the one that must open the line and the third's the one that
// must not repeat it. A single-candidate show cannot tell that from a line
// emitted per candidate.
func TestHarvestReportsStrandedReleases(t *testing.T) {
	const warnMsg = "indexer title harvest encountered results it could not use for this show's releases"
	mock, srv := newHarvestMock(func(call int) string {
		if call == 0 {
			return emptyTorznab()
		}
		return torznabBody(torznabItem("   ", "https://nyaa.si/view/42"))
	})
	defer srv.Close()
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		Path:           filepath.Join(t.TempDir(), "feed.json"),
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	feeds := map[string][]journalItem{
		upstreamNyaa: {{Title: "Show S01", Key: "nyaa:42", AniListID: 7}},
	}
	titles := map[string]string{}
	w.harvest.harvestTitles(t.Context(), feeds, titles, func(int) EntryInfo {
		return EntryInfo{Title: "Show (2023) (Remux)", Season: 1, SeasonKnown: true}
	}, "")
	if got := mock.calls(); got != 3 {
		t.Fatalf("harvest queries = %d, want 3 (one per title candidate); a shorter ladder cannot observe the once-per-show latch", got)
	}
	if got := rec.Count(warnMsg); got != 1 {
		t.Errorf("stranded WARN emitted %d times, want exactly one line per show per rebuild; log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
	if len(titles) != 0 {
		t.Errorf("titles = %v, want the unusable title kept out of the cache", titles)
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
	if n, _, _, _ := matchHarvest(results, "nyaa", index, titles, nil, nil); n != 0 {
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
	if n, _, _, _ := matchHarvest(results, "nyaa", index, titles, nil, nil); n != 0 {
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
// title - and one of exactly harvestMaxTitleLen bytes, the INCLUSIVE upper
// bound - still caches.
func TestMatchHarvestRejectsOversizedTitle(t *testing.T) {
	index := map[string]string{"nyaa:1": "nyaa:1", "nyaa:2": "nyaa:2", "nyaa:3": "nyaa:3"}
	titles := map[string]string{}
	atCap := strings.Repeat("A", harvestMaxTitleLen)
	results := []item{
		{Title: strings.Repeat("A", harvestMaxTitleLen+1), InfoURL: "https://nyaa.si/view/1"},
		{Title: "Normal Title - S01 (1080p) [G]", InfoURL: "https://nyaa.si/view/2"},
		{Title: atCap, InfoURL: "https://nyaa.si/view/3"},
	}
	if n, _, _, _ := matchHarvest(results, "nyaa", index, titles, nil, nil); n != 2 {
		t.Errorf("matchHarvest = %d matches, want 2 (the normal and the exactly-at-cap titles cache)", n)
	}
	if titles["nyaa:3"] != atCap {
		t.Errorf("title of exactly harvestMaxTitleLen bytes not cached (len %d): %v", len(titles["nyaa:3"]), titles)
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
			return EntryInfo{Title: "Show A", Season: 1, SeasonKnown: true}
		case 8:
			return EntryInfo{Title: "Show B", Season: 1, SeasonKnown: true}
		default:
			return EntryInfo{Title: "Show C", Season: 1, SeasonKnown: true}
		}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, NyaaTorznabURL: countSrv.URL, ProwlarrAPIKey: "k"},
		log, countSrv.Client())
	if err := w.Rebuild(t.Context(), entries, info); err != nil {
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
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()

	log, rec := capture.New()
	cfg := &FeedWriterConfig{
		Path:           filepath.Join(t.TempDir(), "feed.json"),
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}
	w := NewFeedWriter(cfg, log, srv.Client())
	feeds := map[string][]journalItem{
		upstreamNyaa: {{Title: "Show S01", Key: "nyaa:42", AniListID: 7}},
	}
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(ctx, feeds, titles, func(int) EntryInfo { return EntryInfo{Title: "Show", Season: 1, SeasonKnown: true} }, "")
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
// journal item that carries its bookkeeping (positive AniList id), has
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

// TestHarvestParams pins the per-tracker query form the title harvest sends:
// Nyaa uses the season form (t=tvsearch, q + season) only for a non-movie
// with a mapped POSITIVE season - a seasonless show, a specials-bucket entry
// (season 0) and a movie stay a plain search - while AnimeBytes is always a
// plain series-level search, and the q value is the title candidate it was
// given (harvestTitleCandidates trims; the ladder varies the title only, never
// the mode or the season).
func TestHarvestParams(t *testing.T) {
	tests := []struct {
		name       string
		meta       EntryInfo
		scope      string
		wantT      string
		wantSeason string
	}{
		{"nyaa series with a mapped season uses the season form", EntryInfo{Title: "Frieren", Season: 1, SeasonKnown: true}, upstreamNyaa, "tvsearch", "1"},
		{"nyaa seasonless series stays a plain search", EntryInfo{Title: "One Piece"}, upstreamNyaa, "search", ""},
		{"nyaa specials bucket stays a plain search", EntryInfo{Title: "Frieren OVA", Season: 0, SeasonKnown: true}, upstreamNyaa, "search", ""},
		{"nyaa movie stays a plain search even with a mapped season", EntryInfo{Title: "A Silent Voice", Season: 1, SeasonKnown: true, IsMovie: true}, upstreamNyaa, "search", ""},
		{"ab is always a plain series-level search", EntryInfo{Title: "Frieren", Season: 1, SeasonKnown: true}, upstreamAB, "search", ""},
		{"q is the trimmed synthesis title", EntryInfo{Title: "  Frieren  ", Season: 2, SeasonKnown: true}, upstreamNyaa, "tvsearch", "2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := harvestParams(tc.meta, tc.scope, strings.TrimSpace(tc.meta.Title))
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
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
			{Title: "Show C", Key: "nyaa:44", AniListID: 9},
			{Title: "Show D", Key: "nyaa:45", AniListID: 10},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"},
		9: {Title: "Show C"}, 10: {Title: "Show D"},
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

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

// TestHarvestMatchesHashlessRecordAgainstHashBearingResult pins the end-to-end
// half of d-u5-c2-2: SeaDex's record for a curated Nyaa torrent carries no
// usable info hash (the field is absent, or AB's "<redacted>" form which
// validInfoHash drops), so the pending index holds only the item's tracker key
// - while Prowlarr's Nyaa result ALWAYS reports a hash. That hash is unknown to
// the partial index, and treating the absence as an identity contradiction
// rejected the result outright. Because the index is rebuilt from the same
// journal every rebuild the rejection was permanent: the item served its
// synthesized heuristic title forever, with no diagnostic at all. The page URL
// resolves the identity on its own, so the harvest must cache the real title.
func TestHarvestMatchesHashlessRecordAgainstHashBearingResult(t *testing.T) {
	const prowlarrHash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		body := torznabBody(`<item><title>[SubsPlease] Show - S01 (1080p)</title>` +
			`<guid>https://nyaa.si/view/42</guid><comments>https://nyaa.si/view/42</comments>` +
			`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/>` +
			`<torznab:attr name="infohash" value="` + prowlarrHash + `"/></item>`)
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	// The journal item carries NO info hash, so pendingHarvest indexes its key
	// alone - exactly the shape a SeaDex record without the field produces.
	feeds := map[string][]journalItem{
		upstreamNyaa: {{Title: "Show S01", Key: "nyaa:42", AniListID: 7}},
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles,
		func(int) EntryInfo { return EntryInfo{Title: "Show", Season: 1, SeasonKnown: true} }, "")

	if got, want := titles["nyaa:42"], "[SubsPlease] Show - S01 (1080p)"; got != want {
		t.Errorf("harvested title = %q, want %q (an unknown hash must not veto the page URL's identity)", got, want)
	}
	if stats.matched != 1 || stats.rejected != 0 {
		t.Errorf("stats.matched = %d, stats.rejected = %d; want 1 and 0", stats.matched, stats.rejected)
	}
	if stats.pending != 0 {
		t.Errorf("stats.pending = %d, want 0 (the item now serves a real title)", stats.pending)
	}
	if rec.Contains("indexer title harvest results rejected: contradictory identity signals") {
		t.Errorf("an unknown hash was reported as a contradiction; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestHarvestReportsContradictoryResults pins the diagnostic half: a result
// whose own page URLs name two DIFFERENT pending releases still fails closed,
// and that rejection is now observable - a silently dropped result left its
// journal item on the synthesized title with nothing in the logs or stats to
// explain it. Debug plus the harvest_rejected stat, not WARN: a systematically
// tampered feed would otherwise warn once per page.
func TestHarvestReportsContradictoryResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		body := torznabBody(`<item><title>Tampered</title>` +
			`<guid>https://nyaa.si/view/43</guid><comments>https://nyaa.si/view/42</comments>` +
			`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>`)
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "Show S01", Key: "nyaa:42", AniListID: 7},
			{Title: "Show S02", Key: "nyaa:43", AniListID: 7},
		},
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles,
		func(int) EntryInfo { return EntryInfo{Title: "Show", Season: 1, SeasonKnown: true} }, "")

	if len(titles) != 0 {
		t.Errorf("titles = %v, want none (contradictory identity must title nothing)", titles)
	}
	if stats.rejected == 0 {
		t.Error("stats.rejected = 0, want the contradictory result counted")
	}
	if !rec.Contains("indexer title harvest results rejected: contradictory identity signals") {
		t.Errorf("contradictory result not reported; log output:\n%s", strings.Join(rec.Messages(), "\n"))
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
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
			{Title: "Show C", Key: "nyaa:44", AniListID: 9},
			{Title: "Show D", Key: "nyaa:45", AniListID: 10},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"},
		9: {Title: "Show C"}, 10: {Title: "Show D"},
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

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
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
			{Title: "Show C", Key: "nyaa:44", AniListID: 9},
			{Title: "Show D", Key: "nyaa:45", AniListID: 10},
			{Title: "Show E", Key: "nyaa:46", AniListID: 11},
			{Title: "Show F", Key: "nyaa:47", AniListID: 12},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"}, 9: {Title: "Show C"},
		10: {Title: "Show D"}, 11: {Title: "Show E"}, 12: {Title: "Show F"},
	}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

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
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
		},
	}
	info := map[int]EntryInfo{7: {Title: "Show A"}, 8: {Title: "Show B"}}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

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

// TestHarvestOpportunisticMatchUsesTheOtherShowsVocabulary pins h-f37: the
// identity index is GLOBAL, so one show's page routinely resolves ANOTHER
// pending show's items too - and that show's alias choice must be made against
// its OWN trusted title, not against the title of whichever show happened to be
// queried. Show A's page carries two aliases of show B's torrent; the English
// one wins the ASCII fallback, so a per-query show title (A's, which neither
// alias contains) would cache the English alias for an arr that carries B under
// its Romaji name - a title Sonarr cannot match, worse than the synthesized one
// it replaced.
func TestHarvestOpportunisticMatchUsesTheOtherShowsVocabulary(t *testing.T) {
	const (
		english = "Show B English Longer Title S01 1080p BluRay [G]"
		romaji  = "Shou Bii S01 1080p [G]"
	)
	if asciiAlnums(english) <= asciiAlnums(romaji) {
		t.Fatalf("premise broken: the English alias must win the ASCII fallback (%d vs %d)",
			asciiAlnums(english), asciiAlnums(romaji))
	}
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(
			torznabItem("Show A S01 1080p BluRay [G]", "https://nyaa.si/view/42"),
			torznabItem(english, "https://nyaa.si/view/43"),
			torznabItem(romaji, "https://nyaa.si/view/43"),
		)
	})
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
		},
	}
	// B's arr carries the Romaji title; A's does not appear in either alias.
	info := map[int]EntryInfo{7: {Title: "Show A"}, 8: {Title: "Shou Bii"}}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if mock.calls() != 1 || stats.queries != 1 {
		t.Errorf("harvest queries = %d (HTTP calls %d), want 1 (B is titled opportunistically)", stats.queries, mock.calls())
	}
	if got := titles["nyaa:43"]; got != romaji {
		t.Errorf("titles[nyaa:43] = %q, want B's own arr vocabulary %q", got, romaji)
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
				"http://prowlarr:9696", "http://"+r.Host,
			)
		default:
			body = "this is not torznab xml <<<"
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
			{Title: "Show C", Key: "nyaa:44", AniListID: 9},
			{Title: "Show D", Key: "nyaa:45", AniListID: 10},
			{Title: "Show E", Key: "nyaa:46", AniListID: 11},
			{Title: "Show F", Key: "nyaa:47", AniListID: 12},
		},
	}
	info := map[int]EntryInfo{
		7: {Title: "Show A"}, 8: {Title: "Show B"}, 9: {Title: "Show C"},
		10: {Title: "Show D"}, 11: {Title: "Show E"}, 12: {Title: "Show F"},
	}
	log, _ := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

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
			w := NewFeedWriter(&FeedWriterConfig{}, log, nil)
			l := newHarvestLatches(1)
			w.harvest.updateHarvestScopeState(upstreamNyaa, harvestShowFailed, false, l)
			w.harvest.updateHarvestScopeState(upstreamNyaa, harvestShowFailed, false, l)
			w.harvest.updateHarvestScopeState(upstreamNyaa, tc.reset, false, l)
			w.harvest.updateHarvestScopeState(upstreamNyaa, harvestShowFailed, false, l)
			if l.blocked(upstreamNyaa) {
				t.Fatal("scope latched after a non-consecutive third rejection; the intervening outcome must reset the run")
			}
			if got := l.rejected[upstreamNyaa]; got != 1 {
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
//
// The scope-latching rows additionally pin the LEVEL split (l-f75): 401/403 are
// the credentials class and log at ERROR naming the remedy, because they cannot
// clear without an operator and the same rejection on the search path makes an
// arr disable this indexer. A 404 stays a WARN - an endpoint answering not-found
// may be a removed Prowlarr indexer, which is a config question but not provably
// a credential one. Scoping is identical for all three; only the level differs.
func TestHarvestHTTPStatusFailureScoping(t *testing.T) {
	const (
		showLocalMsg   = "indexer title harvest request rejected; show keeps its synthesized title this rebuild"
		scopeWarnMsg   = "indexer title harvest query failed; skipping this upstream's remaining shows this rebuild"
		credentialsMsg = "indexer title harvest rejected the credentials; this upstream is unusable until an operator fixes it, " +
			"and the same rejection on the search path makes every query answer an error the arr counts toward disabling this indexer - " +
			"check indexer.prowlarr_api_key and the per-tracker Torznab URL"
	)
	tests := []struct {
		name      string
		wantMsg   string
		status    int
		showLocal bool
	}{
		{name: "400 bad request stays show-local", status: http.StatusBadRequest, showLocal: true, wantMsg: showLocalMsg},
		{name: "414 URI too long stays show-local", status: http.StatusRequestURITooLong, showLocal: true, wantMsg: showLocalMsg},
		{name: "422 unprocessable entity stays show-local", status: http.StatusUnprocessableEntity, showLocal: true, wantMsg: showLocalMsg},
		{name: "401 unauthorized latches the scope at ERROR", status: http.StatusUnauthorized, wantMsg: credentialsMsg},
		{name: "403 forbidden latches the scope at ERROR", status: http.StatusForbidden, wantMsg: credentialsMsg},
		{name: "404 not found latches the scope at WARN", status: http.StatusNotFound, wantMsg: scopeWarnMsg},
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
					{Title: "Show A", Key: "nyaa:42", AniListID: 7},
					{Title: "Show B", Key: "nyaa:43", AniListID: 8},
				},
			}
			info := map[int]EntryInfo{7: {Title: "Show A"}, 8: {Title: "Show B"}}
			log, rec := capture.New()
			w := NewFeedWriter(&FeedWriterConfig{
				NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
			}, log, srv.Client())
			titles := map[string]string{}
			stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

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
			} else {
				if stats.queries != 1 {
					t.Errorf("harvest queries = %d, want 1 (an auth/config status must latch the scope)", stats.queries)
				}
				if len(titles) != 0 {
					t.Errorf("titles = %v, want empty (no show harvested after the scope latched)", titles)
				}
			}
			if !rec.Contains(tc.wantMsg) {
				t.Errorf("expected diagnostic not emitted for status %d; want %q; log output:\n%s",
					tc.status, tc.wantMsg, strings.Join(rec.Messages(), "\n"))
			}
			// The three scope-latching statuses must not share one message: a
			// credentials rejection needs an operator, a 404 may not, and
			// collapsing them is what left a dead feed un-alerted.
			for _, other := range []string{showLocalMsg, scopeWarnMsg, credentialsMsg} {
				if other != tc.wantMsg && rec.Contains(other) {
					t.Errorf("status %d also logged %q; the classes must stay distinct", tc.status, other)
				}
			}
		})
	}
}

// TestResolveHarvestKeyPartialSignals pins the identity-resolution table of
// resolveHarvestKey across its three outcomes: a resolved key, a plain
// non-match (not one of ours), and a CONFLICT (the signals contradict each
// other, which fails closed and is counted). The distinction matters because a
// conflict is an untrusted-response signal worth reporting while a non-match is
// the ordinary fate of most of a season query's page.
//
// The row that changed with d-u5-c4-1's sibling d-u5-c2-2: a known page URL
// carrying a hash the PARTIAL index does not hold now resolves. The index holds
// only pending items and only the hashes SeaDex published, so a Prowlarr Nyaa
// result (always hash-bearing) routinely carries an unknown one; rejecting it
// permanently stranded the item on its synthesized title.
func TestResolveHarvestKeyPartialSignals(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	index := map[string]string{"nyaa:42": "nyaa:42", hash: "nyaa:42"}
	tests := []struct {
		name         string
		it           item
		want         string
		wantConflict bool
	}{
		{name: "guid alone resolves when comments URL is foreign", it: item{InfoURL: "https://mirror.example/x", GUID: "https://nyaa.si/view/42"}, want: "nyaa:42"},
		{name: "comments alone resolves when guid URL is foreign", it: item{InfoURL: "https://nyaa.si/view/42", GUID: "https://mirror.example/x"}, want: "nyaa:42"},
		{name: "url and hash agreeing on one release resolve it", it: item{InfoURL: "https://nyaa.si/view/42", GUID: "https://nyaa.si/view/42", InfoHash: hash}, want: "nyaa:42"},
		{name: "unknown id is a non-match, not a conflict", it: item{InfoURL: "https://nyaa.si/view/999", GUID: "https://nyaa.si/view/999"}},
		{
			name: "known url with an unindexed hash still resolves",
			it:   item{InfoURL: "https://nyaa.si/view/42", GUID: "https://nyaa.si/view/42", InfoHash: strings.Repeat("a", 40)},
			want: "nyaa:42",
		},
		{name: "known hash with an unindexed url is a non-match", it: item{InfoURL: "https://nyaa.si/view/999", GUID: "https://nyaa.si/view/999", InfoHash: hash}},
		{name: "no identity signals resolve nothing", it: item{InfoURL: "https://mirror.example/x", GUID: "https://mirror.example/x"}},
		{
			name:         "page URLs naming different releases conflict",
			it:           item{InfoURL: "https://nyaa.si/view/42", GUID: "https://nyaa.si/view/43"},
			wantConflict: true,
		},
		{
			name:         "hash naming a different indexed release conflicts",
			it:           item{InfoURL: "https://nyaa.si/view/43", GUID: "https://nyaa.si/view/43", InfoHash: hash},
			wantConflict: true,
		},
	}
	// nyaa:43 must be resolvable for the cross-signal conflict rows to reach
	// the disagreement instead of exiting as an unindexed non-match.
	index["nyaa:43"] = "nyaa:43"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, conflict := resolveHarvestKey(&tc.it, index)
			if got != tc.want {
				t.Errorf("resolveHarvestKey(%+v) key = %q, want %q", tc.it, got, tc.want)
			}
			if conflict != tc.wantConflict {
				t.Errorf("resolveHarvestKey(%+v) conflict = %v, want %v", tc.it, conflict, tc.wantConflict)
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
		if p.next(t.Context()) {
			t.Error("next = true with the slice already spent, want false")
		}
	})
	t.Run("cancelled pacing gap admits no query", func(t *testing.T) {
		prev := harvestWait
		harvestWait = func(context.Context, time.Duration) error { return context.Canceled }
		t.Cleanup(func() { harvestWait = prev })
		p := &harvestPacer{now: func() time.Time { return base }, deadline: base.Add(time.Hour), started: true}
		if p.next(t.Context()) {
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

// TestHarvestCheckpointCodec pins the persisted harvest_cursor decoder's
// degradation contract directly: an honest rotation cursor survives verbatim,
// and anything else - a JSON object no released binary can write, an
// out-of-domain id, an unknown tracker scope - is dropped to the baseline with a
// reason, so a garbage value can never be carried forward into every future
// snapshot.
func TestHarvestCheckpointCodec(t *testing.T) {
	t.Run("bare cursor survives verbatim", func(t *testing.T) {
		cursor, degraded := decodeHarvestCursor("nyaa:1500")
		if cursor != "nyaa:1500" {
			t.Errorf("decode bare cursor = %q, want nyaa:1500", cursor)
		}
		if degraded != "" {
			t.Errorf("degraded reason = %q, want none for an honest cursor", degraded)
		}
	})
	t.Run("truncated JSON degrades to the baseline", func(t *testing.T) {
		cursor, degraded := decodeHarvestCursor(`{"pages": {"nyaa:7": `)
		if cursor != "" {
			t.Errorf("decode malformed = %q, want the empty baseline", cursor)
		}
		if degraded == "" {
			// The rebaseline is only reportable if the decoder says WHY it
			// happened; a silent one restarts the rotation at the head with
			// no signal to the operator.
			t.Error("degraded reason = \"\", want a non-empty reason for a malformed cursor")
		}
	})
	t.Run("the JSON object form is not a cursor", func(t *testing.T) {
		// No released binary can write it (the writer has only ever emitted the
		// bare cursor), so it is an invalid rotation cursor like any other.
		cursor, degraded := decodeHarvestCursor(`{"last":"nyaa:7"}`)
		if cursor != "" || degraded == "" {
			t.Errorf("decode JSON object = %q (degraded %q), want the baseline with a reason", cursor, degraded)
		}
	})
	t.Run("non-positive cursor ids are discarded", func(t *testing.T) {
		for _, raw := range []string{"nyaa:0", "nyaa:-1", "ab:0", "ab:-12"} {
			if cursor, _ := decodeHarvestCursor(raw); cursor != "" {
				t.Errorf("decode %q = %q, want it discarded (outside harvestCursorKey's domain)", raw, cursor)
			}
		}
		for _, raw := range []string{"nyaa:1", "ab:154587"} {
			if cursor, _ := decodeHarvestCursor(raw); cursor != raw {
				t.Errorf("decode %q = %q, want it kept", raw, cursor)
			}
		}
	})
	t.Run("a cursor naming no known tracker scope is discarded", func(t *testing.T) {
		// The cursor is carried into every future snapshot verbatim, so a
		// scope no upstream serves must be dropped at decode rather than
		// re-persisted forever; the existing rows only cover the id half.
		for _, raw := range []string{"bogus:5", "NYAA:5", "ab :5", ":5", "nyaa"} {
			if cursor, _ := decodeHarvestCursor(raw); cursor != "" {
				t.Errorf("decode %q = %q, want it discarded (no such upstream scope)", raw, cursor)
			}
		}
		for _, raw := range []string{"nyaa:5", "ab:154587"} {
			if cursor, _ := decodeHarvestCursor(raw); cursor != raw {
				t.Errorf("decode %q = %q, want it kept", raw, cursor)
			}
		}
	})
	t.Run("decode is its own fixpoint", func(t *testing.T) {
		cursor, _ := decodeHarvestCursor("nyaa:1500")
		if again, _ := decodeHarvestCursor(cursor); again != "nyaa:1500" {
			t.Errorf("re-decode = %q, want byte-identical %q", again, "nyaa:1500")
		}
	})
}

// TestPendingHarvestSkipsCrossScopeJournalKey pins the collection-side half of
// the cross-scope guard (indexHarvestItem): a journal item whose Key names a
// DIFFERENT tracker than the feed it sits in can never satisfy matchHarvest's
// scope binding, so it must be left out of both the per-show query group and
// the identity index - otherwise it burns harvest queries of every rebuild's
// time slice forever with no reachable outcome. The package's existing
// cross-scope test pins the MATCH side only.
func TestPendingHarvestSkipsCrossScopeJournalKey(t *testing.T) {
	info := func(int) EntryInfo { return EntryInfo{Title: "Show"} }
	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "synthetic", Key: "ab:300", AniListID: 7},
			{Title: "synthetic", Key: "nyaa:42", AniListID: 7},
		},
	}
	groups, index, _ := pendingHarvest(feeds, map[string]string{}, info)
	if len(groups) != 1 || groups[0].scope != upstreamNyaa || len(groups[0].keys) != 1 || groups[0].keys[0] != "nyaa:42" {
		t.Errorf("pendingHarvest groups = %+v, want only nyaa:42 grouped under the nyaa feed", groups)
	}
	if _, ok := index["ab:300"]; ok {
		t.Errorf("index = %v, indexed the cross-scope key ab:300 that matchHarvest can never satisfy", index)
	}
}

// TestPendingHarvestRetiresAmbiguousInfoHash pins the collision arm of the
// identity index: a hash names BYTES, not a journal item, so two pending items
// publishing the same hash (the same bytes curated under two tracker ids, or
// listed on both trackers) must make the hash inconclusive rather than let one
// item win the slot last-write-wins. Under last-write-wins the loser's own
// honest Prowlarr result read as a contradictory identity and was rejected
// permanently (the index is rebuilt from the same journal every rebuild),
// pinning the harvest_rejected tamper counter non-zero on benign data. Which
// item won was map-iteration-dependent, so this asserts on BOTH results: it
// fails whichever way the old code happened to order them.
func TestPendingHarvestRetiresAmbiguousInfoHash(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	info := func(int) EntryInfo { return EntryInfo{Title: "Show"} }
	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "synthetic", InfoHash: hash, Key: "nyaa:1", AniListID: 7},
			{Title: "synthetic", InfoHash: hash, Key: "nyaa:2", AniListID: 7},
		},
	}
	_, index, showTitles := pendingHarvest(feeds, map[string]string{}, info)
	if owner, ok := index[hash]; ok {
		t.Errorf("index[hash] = %q, want the shared hash retired (it names neither item)", owner)
	}
	titles := map[string]string{}
	results := []item{
		{Title: "Real Title 1", InfoURL: "https://nyaa.si/view/1", GUID: "https://nyaa.si/view/1", InfoHash: hash},
		{Title: "Real Title 2", InfoURL: "https://nyaa.si/view/2", GUID: "https://nyaa.si/view/2", InfoHash: hash},
	}
	matched, rejected, _, _ := matchHarvest(results, upstreamNyaa, index, titles, showTitles, []string{"nyaa:1", "nyaa:2"})
	if matched != 2 || rejected != 0 {
		t.Errorf("matchHarvest = %d matched / %d rejected, want 2/0 (a shared hash corroborates neither item, it contradicts nothing)", matched, rejected)
	}
	if titles["nyaa:1"] != "Real Title 1" || titles["nyaa:2"] != "Real Title 2" {
		t.Errorf("titles = %v, want both items titled from their own page URLs", titles)
	}
}

// TestPendingHarvestKeepsAnAmbiguousInfoHashRetired pins the third-occurrence
// arm of the identity index: once a hash is proven to name more than one
// pending item it stays retired, so a THIRD item publishing it cannot
// re-register the slot. Without that memory the third item OWNS the hash, and
// the first item's own honest Prowlarr result then reads as a contradictory
// identity - permanently, since the index is rebuilt from the same journal
// every rebuild.
func TestPendingHarvestKeepsAnAmbiguousInfoHashRetired(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	info := func(int) EntryInfo { return EntryInfo{Title: "Show"} }
	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "synthetic", InfoHash: hash, Key: "nyaa:1", AniListID: 7},
			{Title: "synthetic", InfoHash: hash, Key: "nyaa:2", AniListID: 7},
			{Title: "synthetic", InfoHash: hash, Key: "nyaa:3", AniListID: 7},
		},
	}
	_, index, showTitles := pendingHarvest(feeds, map[string]string{}, info)
	if owner, ok := index[hash]; ok {
		t.Errorf("index[hash] = %q, want the hash still retired after a third pending item published it", owner)
	}
	titles := map[string]string{}
	results := []item{{
		Title: "Real Title 1", InfoURL: "https://nyaa.si/view/1",
		GUID: "https://nyaa.si/view/1", InfoHash: hash,
	}}
	matched, rejected, _, _ := matchHarvest(results, upstreamNyaa, index, titles, showTitles,
		[]string{"nyaa:1", "nyaa:2", "nyaa:3"})
	if matched != 1 || rejected != 0 {
		t.Errorf("matchHarvest = %d matched / %d rejected, want 1/0 (a retired hash corroborates nothing, it contradicts nothing)", matched, rejected)
	}
	if titles["nyaa:1"] != "Real Title 1" || len(titles) != 1 {
		t.Errorf("titles = %v, want only nyaa:1 titled from its own page URL", titles)
	}
}

// TestPreferredHarvestTitlePicksTheArrsVocabulary pins the alias policy
// (l-f142). AnimeBytes lists ONE torrent three times - English, Japanese and
// Romaji titles, distinct ?nh= GUIDs, the same torrent id - so all three resolve
// to one journal key. Caching whichever Prowlarr listed first made the served
// title a coin flip, and a JP or Romaji alias the operator's Sonarr series does
// not carry makes the RSS item LESS matchable than the synthesized title it
// replaced (synthesizeTitle builds from the arr's own vocabulary on purpose).
//
// Torznab carries no language marker, so "prefer English" is expressed as
// "prefer the alias in the arr's vocabulary": the one whose text contains the
// show title the synthesis already trusts. For an English-titled series that is
// the English alias; for a library whose arr carries the Romaji title it is
// Romaji, which is correct for THAT library. A native-script alias cannot
// contain a Latin show title and so never wins.
func TestPreferredHarvestTitlePicksTheArrsVocabulary(t *testing.T) {
	const (
		jp     = "[SubsPlease] 葬送のフリーレン - S01 (BD 1080p)"
		romaji = "[SubsPlease] Sousou no Frieren - S01 (BD 1080p)"
		en     = "[SubsPlease] Frieren Beyond Journeys End - S01 (BD 1080p)"
	)
	tests := map[string]struct {
		candidates []string
		showTitle  string
		want       string
	}{
		"english show title picks the english alias": {
			candidates: []string{jp, romaji, en},
			showTitle:  "Frieren: Beyond Journey's End",
			want:       en,
		},
		"romaji-titled arr picks the romaji alias": {
			candidates: []string{jp, en, romaji},
			showTitle:  "Sousou no Frieren",
			want:       romaji,
		},
		"a native-script alias listed first never wins": {
			candidates: []string{jp, en},
			showTitle:  "Frieren: Beyond Journey's End",
			want:       en,
		},
		"no matching alias keeps the most parseable one": {
			candidates: []string{jp, romaji},
			showTitle:  "Something Else Entirely",
			want:       romaji, // a native-script title carries almost no parseable text
		},
		"no show title keeps the most parseable one": {
			candidates: []string{en, romaji},
			showTitle:  "",
			want:       en, // most ASCII release text, and deterministic where Prowlarr is not
		},
		"the fallback always yields a title, never empty": {
			candidates: []string{jp},
			showTitle:  "No Match At All",
			want:       jp,
		},
		"a single candidate is returned as-is": {
			candidates: []string{jp},
			showTitle:  "Frieren: Beyond Journey's End",
			want:       jp,
		},
		// The cached title is served for the item's whole journal window, so an
		// equal-score tie has to resolve the same way on every rebuild. Keeping
		// the FIRST alias inherits the tracker's own ordering instead of
		// re-deciding it, which is what makes the pick reproducible.
		"an equally parseable tie keeps the first alias": {
			candidates: []string{
				"[Group] Alpha - S01 (BD 1080p)",
				"[Group] Bravo - S01 (BD 1080p)",
			},
			showTitle: "",
			want:      "[Group] Alpha - S01 (BD 1080p)",
		},
		// h-f36: a one- to three-character title key occurs inside ordinary
		// release metadata ("x" in Remux/x265), so a normalized substring test
		// admits the FIRST alias whatever its vocabulary. A short key needs
		// token-boundary evidence, which the normalized form threw away.
		"a short title key is not matched inside release metadata": {
			candidates: []string{
				"[Group] エックス - S01 (BD Remux 1080p x265)",
				"[Group] X - S01 (BD Remux 1080p x265)",
			},
			showTitle: "X",
			want:      "[Group] X - S01 (BD Remux 1080p x265)",
		},
		"a short title key still matches its own token": {
			candidates: []string{
				"[Group] 케이온 - S01 (BD 1080p)",
				"[Group] K-On! - S01 (BD 1080p)",
			},
			showTitle: "K-On!",
			want:      "[Group] K-On! - S01 (BD 1080p)",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := preferredHarvestTitle(tc.candidates, tc.showTitle); got != tc.want {
				t.Errorf("preferredHarvestTitle(%q) = %q, want %q", tc.showTitle, got, tc.want)
			}
		})
	}
}

// TestMatchHarvestChoosesAmongABAliasesOnOnePage pins the policy end to end
// through matchHarvest: three AB aliases of one torrent on a single page resolve
// to the same journal key, and the arr-vocabulary alias is the one cached -
// counted as exactly ONE match, not three.
func TestMatchHarvestChoosesAmongABAliasesOnOnePage(t *testing.T) {
	const en = "[PMR] Frieren Beyond Journeys End - S01 (BD Remux 1080p)"
	// Same torrent id (1167293) under three ?nh= GUIDs, AB's documented shape.
	results := []item{
		{Title: "[PMR] 葬送のフリーレン - S01 (BD Remux 1080p)", InfoURL: "https://animebytes.tv/torrent/1167293/group?nh=a"},
		{Title: "[PMR] Sousou no Frieren - S01 (BD Remux 1080p)", InfoURL: "https://animebytes.tv/torrent/1167293/group?nh=b"},
		{Title: en, InfoURL: "https://animebytes.tv/torrent/1167293/group?nh=c"},
	}
	index := map[string]string{"ab:1167293": "ab:1167293"}
	titles := map[string]string{}

	matched, rejected, _, _ := matchHarvest(results, upstreamAB, index, titles,
		map[string]string{"ab:1167293": "Frieren: Beyond Journey's End"}, []string{"ab:1167293"})

	if matched != 1 || rejected != 0 {
		t.Errorf("matched = %d, rejected = %d; want 1 and 0 (three aliases are ONE torrent)", matched, rejected)
	}
	if got := titles["ab:1167293"]; got != en {
		t.Errorf("cached title = %q, want the arr-vocabulary alias %q", got, en)
	}
	if len(titles) != 1 {
		t.Errorf("titles = %v, want exactly one entry", titles)
	}
}

// TestUpdateHarvestScopeStateLatchesAlternatingFailures pins the backstop the two
// per-kind latches cannot be (l-f91). Each of them resets the OTHER's counter -
// deliberately, since a definitive request rejection falsifies the
// answers-garbage-to-everything hypothesis - so an upstream ALTERNATING between a
// garbled 2xx body and a request rejection tripped NEITHER however long it ran:
// the full harvestTimeBudget burned with zero title progress on every rebuild and
// one WARN fired per failed show (up to ~300) instead of the <=3-then-latch bound
// the homogeneous case gets. A misbehaving reverse proxy answering HTML garbage to
// one query shape and 400/422 to another produces exactly that, while the pending
// set interleaves mapped-season groups (tvsearch) with unmapped ones (search).
//
// The fruitless counter states the purpose directly - consecutive shows with NO
// progress of any kind, reset only by a success - and latches at twice the
// per-kind threshold, so it never preempts the more specific diagnostics.
func TestUpdateHarvestScopeStateLatchesAlternatingFailures(t *testing.T) {
	const msg = "indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild"

	t.Run("perfectly alternating failures latch", func(t *testing.T) {
		log, rec := capture.New()
		w := NewFeedWriter(&FeedWriterConfig{}, log, nil)
		l := newHarvestLatches(1)
		alternating := []harvestOutcome{
			harvestShowMalformed, harvestShowFailed,
			harvestShowMalformed, harvestShowFailed,
			harvestShowMalformed, harvestShowFailed,
		}
		for i, outcome := range alternating {
			if l.blocked(upstreamNyaa) {
				t.Fatalf("scope latched after %d shows, want it to survive to the fruitless threshold", i)
			}
			w.harvest.updateHarvestScopeState(upstreamNyaa, outcome, false, l)
		}
		if !l.blocked(upstreamNyaa) {
			t.Errorf("scope not latched after %d alternating failures with zero progress", len(alternating))
		}
		if !rec.Contains(msg) {
			t.Errorf("no-progress latch not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
		}
		// Neither per-kind counter ever reached its own threshold - which is
		// exactly why the backstop is needed.
		if l.malformed[upstreamNyaa] >= consecutiveMalformedLatch || l.rejected[upstreamNyaa] >= consecutiveRejectedLatch {
			t.Errorf("a per-kind latch also tripped (malformed=%d rejected=%d); the fixture no longer isolates the mixed case",
				l.malformed[upstreamNyaa], l.rejected[upstreamNyaa])
		}
	})

	t.Run("a success resets the no-progress run", func(t *testing.T) {
		log, rec := capture.New()
		w := NewFeedWriter(&FeedWriterConfig{}, log, nil)
		l := newHarvestLatches(1)
		// Five alternating failures, a success, then five more: no run of
		// consecutiveFruitlessLatch ever completes, so the scope keeps working.
		for range 2 {
			for _, outcome := range []harvestOutcome{
				harvestShowMalformed, harvestShowFailed, harvestShowMalformed, harvestShowFailed, harvestShowMalformed,
			} {
				w.harvest.updateHarvestScopeState(upstreamNyaa, outcome, false, l)
			}
			w.harvest.updateHarvestScopeState(upstreamNyaa, harvestOK, false, l)
		}
		if l.blocked(upstreamNyaa) {
			t.Error("scope latched despite a successful show between the failure runs; progress must reset the run")
		}
		if rec.Contains(msg) {
			t.Errorf("no-progress latch warned despite progress; log output:\n%s", strings.Join(rec.Messages(), "\n"))
		}
	})

	t.Run("a homogeneous run still latches on its own diagnostic", func(t *testing.T) {
		log, rec := capture.New()
		w := NewFeedWriter(&FeedWriterConfig{}, log, nil)
		l := newHarvestLatches(1)
		for range consecutiveMalformedLatch {
			w.harvest.updateHarvestScopeState(upstreamNyaa, harvestShowMalformed, false, l)
		}
		if !l.blocked(upstreamNyaa) {
			t.Fatal("homogeneous malformed run did not latch")
		}
		// The specific diagnostic must win: the backstop threshold is higher, and
		// it is skipped once a per-kind latch has fired, so the operator gets one
		// actionable line rather than two.
		if !rec.Contains("repeated malformed responses") {
			t.Errorf("malformed latch lost its own diagnostic; log output:\n%s", strings.Join(rec.Messages(), "\n"))
		}
		if rec.Contains(msg) {
			t.Errorf("the generic no-progress WARN also fired; log output:\n%s", strings.Join(rec.Messages(), "\n"))
		}
	})
}

// TestUpdateHarvestScopeStateLatchesContradictedSuccesses pins h-f52's arm: a
// show whose query SUCCEEDED but whose every candidate result was refused as
// contradictory resolved nothing, so it keeps the fruitless run charged and a
// run of them condemns the scope - an upstream returning our releases with
// contradictory identity signals answers cleanly forever while harvesting
// nothing. A show that actually resolved something still clears the run.
func TestUpdateHarvestScopeStateLatchesContradictedSuccesses(t *testing.T) {
	const msg = "indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild"
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{}, log, nil)

	l := newHarvestLatches(1)
	for i := range consecutiveFruitlessLatch {
		if l.blocked(upstreamNyaa) {
			t.Fatalf("scope latched after %d contradicted shows, want it to survive to the threshold", i)
		}
		w.harvest.updateHarvestScopeState(upstreamNyaa, harvestOK, true, l)
	}
	if !l.blocked(upstreamNyaa) {
		t.Errorf("scope not latched after %d successful shows that resolved nothing", consecutiveFruitlessLatch)
	}
	if !rec.Contains(msg) {
		t.Errorf("no-progress latch not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	if l.malformed[upstreamNyaa] != 0 || l.rejected[upstreamNyaa] != 0 {
		t.Errorf("per-kind runs = malformed %d / rejected %d, want both reset by the successful query",
			l.malformed[upstreamNyaa], l.rejected[upstreamNyaa])
	}

	// A show that RESOLVED something clears the run, so no threshold completes.
	clean := newHarvestLatches(1)
	for range consecutiveFruitlessLatch - 1 {
		w.harvest.updateHarvestScopeState(upstreamNyaa, harvestOK, true, clean)
	}
	w.harvest.updateHarvestScopeState(upstreamNyaa, harvestOK, false, clean)
	for range consecutiveFruitlessLatch - 1 {
		w.harvest.updateHarvestScopeState(upstreamNyaa, harvestOK, true, clean)
	}
	if clean.blocked(upstreamNyaa) {
		t.Error("scope latched despite a show that resolved something between the contradicted runs")
	}
}

// TestUpstreamFailureWarnsOnce pins the once-per-failure cadence on both callers
// of upstream.search. httpx's retry loop publishes its own terminal "retries
// exhausted" line, and this app publishes a WARN for the same failed query with
// strictly more context (the show, the query shape and the page on the harvest
// path; the scope on the request path). Leaving both at Warn produced two
// terminal WARNs per failure and doubled the log volume of exactly the incident
// the once-per-onset latch cadence exists to keep readable, so httpx's verdict is
// demoted to Debug (l-f20). The per-attempt retry diagnostics are deliberately
// kept, which is why the logger is demoted rather than dropped.
func TestUpstreamFailureWarnsOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())

	u := upstreamForScope(w.harvest.upstreams, upstreamNyaa)
	if u == nil {
		t.Fatal("no nyaa upstream wired")
	}
	if _, _, err := u.search(t.Context(), url.Values{"t": {"search"}}); err == nil {
		t.Fatal("search against a 503 upstream = nil error")
	}

	// The library's terminal verdict must not be a WARN: exactly one layer
	// warns, and it is the one with the app context.
	if n := rec.CountLevel(slog.LevelWarn, "retries exhausted"); n != 0 {
		t.Errorf("httpx terminal line logged at WARN %d times, want 0 (demoted to Debug): %v", n, rec.Messages())
	}
	// It is demoted, not suppressed: the diagnosis is still available.
	if !rec.Contains("retries exhausted") {
		t.Errorf("httpx terminal line missing entirely, want it kept at Debug: %v", rec.Messages())
	}
}

// TestHarvestServesTheArrsVocabularyAlias pins the WIRING of the alias policy
// (l-f142) into the harvest itself: harvestShow must hand matchHarvest the
// show title the synthesis trusts, not a blank. preferredHarvestTitle's own
// table proves the policy; nothing proved the harvest actually feeds it, so a
// blank showTitle silently reverts the served title to the
// most-parseable-alias fallback - for a Romaji-titled library that is the
// English alias its Sonarr series does not carry, exactly the coin flip the
// policy replaced. The fixture is chosen so the fallback disagrees with the
// policy: the English alias carries MORE ASCII release text than the Romaji
// one, so only correct wiring can pick Romaji.
func TestHarvestServesTheArrsVocabularyAlias(t *testing.T) {
	const (
		romaji  = "[PMR] Sousou no Frieren - S01 (BD Remux 1080p)"
		english = "[PMR] Frieren Beyond Journeys End Extended Edition - S01 (BD Remux 1080p)"
	)
	// One AB torrent (id 1167293) under two ?nh= aliases, AB's documented shape.
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(
			torznabItem(english, "https://animebytes.tv/torrent/1167293/group?nh=a"),
			torznabItem(romaji, "https://animebytes.tv/torrent/1167293/group?nh=b"),
		)
	})
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamAB: {{Title: "Frieren S01", Key: "ab:1167293", AniListID: 154587}},
	}
	w := NewFeedWriter(&FeedWriterConfig{
		ABTorznabURL: srv.URL, ABPasskey: "PK", ProwlarrAPIKey: "k",
	}, nil, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles,
		func(int) EntryInfo { return EntryInfo{Title: "Sousou no Frieren"} }, "")

	if mock.calls() != 1 || stats.matched != 1 {
		t.Errorf("harvest queries = %d, matched = %d; want 1 and 1 (two aliases are ONE torrent)", mock.calls(), stats.matched)
	}
	if got := titles["ab:1167293"]; got != romaji {
		t.Errorf("cached title = %q, want the arr-vocabulary alias %q", got, romaji)
	}
}

// TestHarvestRefusalsThatAreNotThisShowsDoNotLatchTheScope pins both halves of
// the group-local pending grade: a contradictory result pending in NO group (the
// original unrelated case, d-gpt-u8-1) and one pending only for a LATER group
// (h-f35) are both ordinary misses for the show being queried, so neither may
// charge the fruitless run. AnimeBytes answers the same broad series-level
// corpus to every query, so such an item repeats across every otherwise
// ordinary miss; charging it latched the scope after consecutiveFruitlessLatch
// clean shows and left the one show whose real title was on offer unqueried,
// with time and rotation still to spend.
func TestHarvestRefusalsThatAreNotThisShowsDoNotLatchTheScope(t *testing.T) {
	const (
		latchMsg = "indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild"
		lastShow = "Show G"
	)
	tests := []struct {
		name         string
		strangerGUID string
	}{
		{name: "refusal pending in no group", strangerGUID: "https://nyaa.si/view/900"},
		{name: "refusal pending only for a later group", strangerGUID: "https://nyaa.si/view/16"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Self-contradictory: guid and comments name two DIFFERENT releases.
			stranger := `<item><title>Contradictory Stranger</title><guid>` + tc.strangerGUID + `</guid>` +
				`<comments>https://nyaa.si/view/901</comments>` +
				`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				items := []string{stranger}
				if r.URL.Query().Get("q") == lastShow {
					items = append(items, torznabItem("Show G Real Title", "https://nyaa.si/view/16"))
				}
				_, _ = io.WriteString(w, strings.ReplaceAll(torznabBody(items...), "http://prowlarr:9696", "http://"+r.Host))
			}))
			defer srv.Close()

			// Seven shows (one more than consecutiveFruitlessLatch), the LAST of
			// them - groups run in AniList-ID order - the one whose real title is
			// on offer.
			names := []string{"Show A", "Show B", "Show C", "Show D", "Show E", "Show F", lastShow}
			feeds := map[string][]journalItem{upstreamNyaa: {}}
			info := map[int]EntryInfo{}
			for i, name := range names {
				feeds[upstreamNyaa] = append(feeds[upstreamNyaa],
					journalItem{Title: "synthetic", Key: "nyaa:" + strconv.Itoa(10+i), AniListID: 7 + i})
				info[7+i] = EntryInfo{Title: name}
			}
			log, rec := capture.New()
			w := NewFeedWriter(&FeedWriterConfig{
				NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
			}, log, srv.Client())
			titles := map[string]string{}
			stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

			if stats.queries != len(names) {
				t.Errorf("harvest queries = %d, want %d: a refusal naming none of THIS show's pending releases is not no-progress",
					stats.queries, len(names))
			}
			if got := titles["nyaa:16"]; got != "Show G Real Title" {
				t.Errorf("titles[nyaa:16] = %q, want the last show harvested (%v)", got, titles)
			}
			if rec.Contains(latchMsg) {
				t.Errorf("scope latched on a refusal that was not this show's; log output:\n%s", strings.Join(rec.Messages(), "\n"))
			}
			// The refusals are still counted, so a tampered feed stays observable.
			if stats.rejected != len(names) {
				t.Errorf("harvest_rejected = %d, want %d (every page refused the stranger)", stats.rejected, len(names))
			}
		})
	}
}

// TestHarvestConflictsNamingAlreadyTitledKeysDoNotLatchTheScope pins the
// still-pending half of the pending grade (d-gpt-u8c2-1). groupKeys is the
// immutable start-of-run list, so it keeps naming keys an EARLIER group's broad
// page titled opportunistically. A contradiction touching only such a key
// refused nothing this rebuild still wants, so it must not charge the fruitless
// run: grading it as no-progress condemned the scope after
// consecutiveFruitlessLatch partially satisfied shows and left the last show -
// the one whose real title was on offer - unqueried, with time and rotation
// still to spend.
func TestHarvestConflictsNamingAlreadyTitledKeysDoNotLatchTheScope(t *testing.T) {
	const (
		latchMsg  = "indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild"
		firstShow = "Show A"
		lastShow  = "Show H"
	)
	// Eight shows of two keys each (two more than consecutiveFruitlessLatch);
	// groups run in AniList-ID order.
	names := []string{firstShow, "Show B", "Show C", "Show D", "Show E", "Show F", "Show G", lastShow}
	// Show A's broad page resolves the SECOND key of every middle show, so each
	// of them is PARTIALLY satisfied - one key titled, one still pending -
	// before its own query runs.
	opportunistic := make([]string, 0, len(names))
	for i := 1; i < len(names)-1; i++ {
		opportunistic = append(opportunistic,
			torznabItem(names[i]+" Second Key Title", "https://nyaa.si/view/"+strconv.Itoa(20+i)))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		var items []string
		switch q := r.URL.Query().Get("q"); q {
		case firstShow:
			items = opportunistic
		case lastShow:
			items = []string{torznabItem("Show H Real Title", "https://nyaa.si/view/17")}
		default:
			// Self-contradictory (guid and comments name two different
			// releases) and the only pending identity it names is the key Show
			// A's page ALREADY titled for this very show: this show's own
			// pending release was never refused.
			items = []string{`<item><title>Contradictory Already-Titled Item</title>` +
				`<guid>https://nyaa.si/view/` + strconv.Itoa(20+slices.Index(names, q)) + `</guid>` +
				`<comments>https://nyaa.si/view/901</comments>` +
				`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>`}
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(torznabBody(items...), "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{upstreamNyaa: {}}
	info := map[int]EntryInfo{}
	for i, name := range names {
		feeds[upstreamNyaa] = append(feeds[upstreamNyaa],
			journalItem{Title: "synthetic", Key: "nyaa:" + strconv.Itoa(10+i), AniListID: 7 + i},
			journalItem{Title: "synthetic", Key: "nyaa:" + strconv.Itoa(20+i), AniListID: 7 + i})
		info[7+i] = EntryInfo{Title: name}
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != len(names) {
		t.Errorf("harvest queries = %d, want %d: a refusal naming a key already titled THIS run is not this show's no-progress",
			stats.queries, len(names))
	}
	if got := titles["nyaa:17"]; got != "Show H Real Title" {
		t.Errorf("titles[nyaa:17] = %q, want the last show harvested (%v)", got, titles)
	}
	if rec.Contains(latchMsg) {
		t.Errorf("scope latched on conflicts naming already-titled keys; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestHarvestPartialProgressDoesNotLatchTheScope pins h-f51: a show that cached
// a real title before a LATER query failed show-locally made progress, so it must
// not charge a consecutive-failure run. Each of the four shows answers its as-is
// title with one of its two keys' titles, stays pending, and gets garbage for the
// widened candidate; charging the malformed run would condemn the scope on the
// third show and leave the fourth unqueried, even though every show harvested a
// title.
func TestHarvestPartialProgressDoesNotLatchTheScope(t *testing.T) {
	const shows = 4
	// Keyed on the REQUEST, not the call index: a malformed body is retried, so
	// call order says nothing about which candidate a response answers.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		q := r.URL.Query().Get("q")
		if !strings.HasSuffix(q, ")") {
			// The widened title candidate is answered with garbage:
			// show-local malformed, after the show already made progress.
			_, _ = io.WriteString(w, "this is not torznab xml <<<")
			return
		}
		show := strings.TrimSuffix(strings.TrimPrefix(q, "Show "), " (2020)")
		// One of the show's TWO keys, so the show stays pending and the ladder
		// widens onto the failing candidate.
		body := torznabBody(torznabItem("Real Title "+show, "https://nyaa.si/view/10"+show))
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	feeds := map[string][]journalItem{upstreamNyaa: {}}
	info := map[int]EntryInfo{}
	for i := range shows {
		feeds[upstreamNyaa] = append(feeds[upstreamNyaa],
			journalItem{Title: "Show S01", Key: "nyaa:10" + strconv.Itoa(i), AniListID: 7 + i},
			journalItem{Title: "Show S01", Key: "nyaa:20" + strconv.Itoa(i), AniListID: 7 + i})
		info[7+i] = EntryInfo{Title: "Show " + strconv.Itoa(i) + " (2020)"}
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != 2*shows {
		t.Errorf("harvest queries = %d, want %d: a show that cached a title must not condemn the scope",
			stats.queries, 2*shows)
	}
	if len(titles) != shows {
		t.Errorf("titles = %v, want one harvested title per show", titles)
	}
	if rec.Contains("indexer title harvest: repeated malformed responses; skipping this upstream's remaining shows this rebuild") {
		t.Error("scope latched on malformed widened-candidate queries although every show harvested a real title")
	}
}

// TestHarvestTitleCandidates pins the harvest's title ladder on the operator's
// own library titles: a TRAILING parenthetical qualifier is stripped (the arrs
// add "(YYYY)" whenever a title collides, and no tracker's release naming
// carries it - measured, "Frieren (2023)" returns zero AnimeBytes items where
// "Frieren" returns 145), one whole balanced group at a time, while an interior
// or leading parenthetical is part of the NAME and is never touched. The as-is
// title is always first, candidates are deduplicated, and no candidate is empty.
func TestHarvestTitleCandidates(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  []string
	}{
		{"trailing year qualifier is stripped", "Hunter x Hunter (2011)", []string{"Hunter x Hunter (2011)", "Hunter x Hunter"}},
		{"trailing region qualifier is stripped", "The Office (US)", []string{"The Office (US)", "The Office"}},
		{"interior parenthetical is part of the name", "Evangelion: 1.0 You Are (Not) Alone", []string{"Evangelion: 1.0 You Are (Not) Alone"}},
		// A trailing parenthetical that IS part of the real name: it is
		// indistinguishable from a qualifier without a title database, and the
		// as-is form is tried FIRST, so the stripped candidate is only ever
		// queried for a show that already harvested nothing. A broader query can
		// only widen the result set, and identity - the tracker id and info hash
		// against this show's pending journal keys - is what admits a result, so
		// a looser title can never mistitle anything.
		{"trailing parenthetical that is part of the name still ladders", "Manda Bala (Send a Bullet)", []string{"Manda Bala (Send a Bullet)", "Manda Bala"}},
		{"leading parenthetical is part of the name", "(A)Torsion", []string{"(A)Torsion"}},
		{"no parenthetical is one candidate", "Frieren", []string{"Frieren"}},
		{"stacked trailing groups strip one at a time", "A (B) (C)", []string{"A (B) (C)", "A (B)", "A"}},
		{"a title that is entirely a parenthetical never strips to empty", "(2023)", []string{"(2023)"}},
		{"a nested trailing group is removed whole", "A (B (C))", []string{"A (B (C))", "A"}},
		{"the as-is candidate is trimmed", "  Frieren  ", []string{"Frieren"}},
		{"an empty title yields no candidate", "   ", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := harvestTitleCandidates(tc.title); !slices.Equal(got, tc.want) {
				t.Errorf("harvestTitleCandidates(%q) = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}

// TestHarvestLadderStopsAtTheFirstSatisfyingCandidate pins the ladder's cost
// floor: a show whose as-is title finds its release spends exactly ONE query,
// so adding the ladder charges a healthy show nothing.
func TestHarvestLadderStopsAtTheFirstSatisfyingCandidate(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string {
		return torznabBody(torznabItem("Hunter x Hunter S01 1080p [G]", "https://nyaa.si/view/42"))
	})
	defer srv.Close()

	feeds := map[string][]journalItem{upstreamNyaa: {
		{Title: "Show S01", Key: "nyaa:42", AniListID: 7},
	}}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles,
		func(int) EntryInfo { return EntryInfo{Title: "Hunter x Hunter (2011)", Season: 1, SeasonKnown: true} }, "")

	if stats.queries != 1 {
		t.Errorf("harvest queries = %d, want 1: a satisfied show must not walk the rest of its ladder", stats.queries)
	}
	if got := mock.request(0)["q"]; got != "Hunter x Hunter (2011)" {
		t.Errorf("first query q = %q, want the title as-is", got)
	}
	if titles["nyaa:42"] != "Hunter x Hunter S01 1080p [G]" {
		t.Errorf("titles = %v, want the harvested real title", titles)
	}
	if rec.Contains("indexer title harvest exhausted its title candidates") {
		t.Error("a satisfied show reported an exhausted ladder")
	}
}

// TestHarvestLadderAdvancesToTheStrippedTitle pins the fix itself: a show whose
// arr title carries a "(YYYY)" qualifier the tracker's naming does not know
// harvests NOTHING on its as-is title (both trackers answer an unresolvable
// title with an empty 200, indistinguishable from a real no-match), so the
// harvest retries on the stripped title and resolves there. The Nyaa season
// form rides every candidate: the ladder varies the title only.
func TestHarvestLadderAdvancesToTheStrippedTitle(t *testing.T) {
	var mock *harvestMock
	mock, srv := newHarvestMock(func(call int) string {
		if strings.Contains(mock.request(call)["q"], "(2011)") {
			return emptyTorznab()
		}
		return torznabBody(torznabItem("Hunter x Hunter S01 1080p [G]", "https://nyaa.si/view/42"))
	})
	defer srv.Close()

	feeds := map[string][]journalItem{upstreamNyaa: {
		{Title: "Show S01", Key: "nyaa:42", AniListID: 7},
	}}
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, nil, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles,
		func(int) EntryInfo { return EntryInfo{Title: "Hunter x Hunter (2011)", Season: 1, SeasonKnown: true} }, "")

	if stats.queries != 2 {
		t.Fatalf("harvest queries = %d, want 2 (as-is, then the stripped title)", stats.queries)
	}
	first, second := mock.request(0), mock.request(1)
	if first["q"] != "Hunter x Hunter (2011)" || second["q"] != "Hunter x Hunter" {
		t.Errorf("ladder queries = %q then %q, want the as-is title then the stripped one", first["q"], second["q"])
	}
	for i, req := range []map[string]string{first, second} {
		if req["t"] != "tvsearch" || req["season"] != "1" {
			t.Errorf("candidate %d params = %v, want the Nyaa season form on every candidate", i, req)
		}
	}
	if titles["nyaa:42"] != "Hunter x Hunter S01 1080p [G]" {
		t.Errorf("titles = %v, want the second candidate's match to satisfy the show", titles)
	}
}

// TestHarvestExhaustedLadderLogsOnceAtDebug pins the diagnostic the ladder
// replaces a latch with: a show that tried every candidate and resolved nothing
// says so ONCE, at Debug, with the number of candidates tried. Debug is the
// honest level - with the query shape fixed, a persistent zero-match is an
// AnimeBytes deletion between the SeaDex posting and the scan, or a release
// genuinely absent; neither is a fault.
func TestHarvestExhaustedLadderLogsOnceAtDebug(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	feeds := map[string][]journalItem{upstreamNyaa: {
		{Title: "Show S01", Key: "nyaa:42", AniListID: 7},
	}}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, map[string]string{},
		func(int) EntryInfo { return EntryInfo{Title: "A (B) (C)"} }, "")

	if stats.queries != 3 {
		t.Errorf("harvest queries = %d, want 3 (one per candidate of \"A (B) (C)\")", stats.queries)
	}
	const msg = "indexer title harvest exhausted its title candidates; show keeps its synthesized title this rebuild"
	if got := rec.CountExact(msg); got != 1 {
		t.Fatalf("exhausted-ladder lines = %d, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	if rec.CountLevel(slog.LevelDebug, msg) != 1 {
		t.Errorf("exhausted-ladder line not at Debug: an expected deletion or absence is not a fault")
	}
	if got, ok := rec.AttrValue(msg, "candidates"); !ok || got != "3" {
		t.Errorf("candidates attr = %q (present=%v), want \"3\"", got, ok)
	}
	if mock.calls() != 3 {
		t.Errorf("upstream calls = %d, want 3", mock.calls())
	}
}

// TestHarvestCleanZeroMatchesNeverLatchTheScope pins the deliberate hole in the
// fruitless latch: an upstream answering CLEANLY with zero items charges no
// latch, however long the run. A zero-match is not evidence about the upstream -
// when SeaDex carries a link to a tracker the release is on that tracker with
// ~99% probability, so an empty clean answer is the app's QUESTION being wrong
// (the ladder's job) or the ~1% deletion, never a broken tracker. So more than
// consecutiveFruitlessLatch such shows in a row are ALL still queried.
func TestHarvestCleanZeroMatchesNeverLatchTheScope(t *testing.T) {
	mock, srv := newHarvestMock(func(int) string { return emptyTorznab() })
	defer srv.Close()

	shows := consecutiveFruitlessLatch + 1
	feeds := map[string][]journalItem{upstreamNyaa: {}}
	info := map[int]EntryInfo{}
	for i := range shows {
		feeds[upstreamNyaa] = append(feeds[upstreamNyaa],
			journalItem{Title: "Show S01", Key: "nyaa:" + strconv.Itoa(100+i), AniListID: 7 + i})
		// Single-candidate titles, so one query per show: the assertion is
		// about the latch, not about the ladder's length.
		info[7+i] = EntryInfo{Title: "Show " + strconv.Itoa(i)}
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, map[string]string{},
		func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != shows {
		t.Errorf("harvest queries = %d, want %d: a clean empty answer must not condemn the scope", stats.queries, shows)
	}
	if mock.calls() != shows {
		t.Errorf("upstream calls = %d, want %d", mock.calls(), shows)
	}
	for _, latch := range []string{
		"indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild",
		"indexer title harvest: repeated malformed responses; skipping this upstream's remaining shows this rebuild",
		"indexer title harvest: repeated request rejections; skipping this upstream's remaining shows this rebuild",
	} {
		if rec.Contains(latch) {
			t.Errorf("scope latched on clean zero-match shows: %q", latch)
		}
	}
	// A clean empty answer carried no results at all, so nothing of this show's
	// was stranded either: the stranding report names results the harvest could
	// not use, and one per query on an ordinary empty answer would make the
	// operator's view of a genuinely stranded show worthless.
	if got := rec.Count("indexer title harvest encountered results it could not use for this show's releases"); got != 0 {
		t.Errorf("clean zero-match shows reported stranding %d times, want 0:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
}

// TestPermanentUpstreamCredentialErrorBandEdges pins both edges of the
// credential band on a Torznab <error> document's code. The band is 100-199
// (wrong or revoked API key, suspended account) and it latches the whole scope
// at ERROR, while 200-299 is the request-scoped band that fails one show only.
// Code 200 sits directly on the seam: classifying it as a credential failure
// would condemn every remaining show on the tracker for what is really one
// malformed query, and the end-to-end credential test only exercises 100.
func TestPermanentUpstreamCredentialErrorBandEdges(t *testing.T) {
	tests := map[int]bool{
		-1: false, 0: false, 99: false,
		100: true, 101: true, 199: true,
		200: false, 201: false, 300: false, 900: false,
	}
	for code, want := range tests {
		err := newUpstreamDocError(strconv.Itoa(code), "upstream said so")
		if got := permanentUpstreamCredentialError(err); got != want {
			t.Errorf("permanentUpstreamCredentialError(code=%d) = %v, want %v", code, got, want)
		}
	}
}

// TestHarvestCredentialErrorDocumentLatchesTheScopeAtError pins the harvest's
// credential classification for the shape it actually arrives in over a healthy
// HTTP hop: Prowlarr answers 200 with a Torznab <error> document, and codes
// 100-199 are its auth/credential band (a wrong or revoked
// indexer.prowlarr_api_key, a suspended account). That band is decided by
// permanentUpstreamCredentialError's DOCUMENT arm, whose status twin (401/403)
// TestHarvestHTTPStatusFailureScoping already pins - so the document arm could be
// deleted, or its range typed as 200-299, and the suite would stay green while
// the rejection fell through to the generic scope WARN.
//
// Two things this shape alone reaches. The LEVEL is the alert contract:
// alerts/logql.yaml keys SeadexScoutCycleError on level=ERROR and on no message at all,
// so a re-level to WARN silences the one signal that says the feed is dying while
// the container stays healthy and the compare loop keeps logging cycle complete.
// And the scope must LATCH: a credential rejection fails every show identically,
// so the second show must never be queried.
func TestHarvestCredentialErrorDocumentLatchesTheScopeAtError(t *testing.T) {
	const (
		credentialsMsg = "indexer title harvest rejected the credentials"
		scopeWarnMsg   = "indexer title harvest query failed; skipping this upstream's remaining shows this rebuild"
		showLocalMsg   = "indexer title harvest request rejected; show keeps its synthesized title this rebuild"
	)
	mock, srv := newHarvestMock(func(int) string {
		return `<?xml version="1.0" encoding="UTF-8"?><error code="100" description="Incorrect user credentials"/>`
	})
	defer srv.Close()

	feeds := map[string][]journalItem{
		upstreamNyaa: {
			{Title: "Show A", Key: "nyaa:42", AniListID: 7},
			{Title: "Show B", Key: "nyaa:43", AniListID: 8},
		},
	}
	info := map[int]EntryInfo{7: {Title: "Show A"}, 8: {Title: "Show B"}}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), feeds, titles, func(alID int) EntryInfo { return info[alID] }, "")

	if got := rec.CountLevel(slog.LevelError, credentialsMsg); got != 1 {
		t.Errorf("credential rejection logged at ERROR %d times, want 1 (alerts/logql.yaml keys on the level, not the message); log output:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
	for _, other := range []string{scopeWarnMsg, showLocalMsg} {
		if rec.Contains(other) {
			t.Errorf("a credential error document also logged %q; the classes must stay distinct", other)
		}
	}
	if stats.queries != 1 || mock.calls() != 1 {
		t.Errorf("harvest queries = %d, upstream calls = %d, want 1 each (a credential rejection fails every show, so the scope must latch)",
			stats.queries, mock.calls())
	}
	if len(titles) != 0 {
		t.Errorf("titles = %v, want empty (no show harvested after the scope latched)", titles)
	}
}

// TestHarvestFruitlessBackstopLatchesOnCleanlyRefusedShows pins the no-progress
// backstop's one genuinely non-obvious input: a show whose query SUCCEEDED.
//
// The two per-kind latches count failures, and each resets the other's run, so an
// upstream that answers 200 with a well-formed body forever trips neither however
// long it runs. An upstream returning this app's own releases with contradictory
// identity signals is exactly that shape: every result is refused, nothing is
// titled, and the whole harvest budget burns with zero progress on every rebuild.
// So a clean answer that resolved nothing is charged to the no-progress run, and
// the scope is condemned once even a mixed sequence has produced nothing.
func TestHarvestFruitlessBackstopLatchesOnCleanlyRefusedShows(t *testing.T) {
	// One more show than the backstop tolerates, so the latch is observable as a
	// query the run never spends.
	const shows = consecutiveFruitlessLatch + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		// Every show's own release is named by a result whose two page URLs
		// disagree with each other: a contradiction resolveHarvestKey refuses,
		// against a key this rebuild is still trying to title.
		show := strings.TrimPrefix(r.URL.Query().Get("q"), "Show ")
		body := torznabBody(`<item><title>Tampered</title>` +
			`<guid>https://nyaa.si/view/9` + show + `</guid>` +
			`<comments>https://nyaa.si/view/4` + show + `</comments>` +
			`<enclosure url="http://prowlarr:9696/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>`)
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	feed := make([]journalItem, 0, shows)
	info := map[int]EntryInfo{}
	for i := range shows {
		feed = append(feed, journalItem{
			Title: "Synthesized S01", Key: "nyaa:4" + strconv.Itoa(i), AniListID: 7 + i,
		})
		info[7+i] = EntryInfo{Title: "Show " + strconv.Itoa(i)}
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), map[string][]journalItem{upstreamNyaa: feed},
		titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != consecutiveFruitlessLatch {
		t.Errorf("harvest queries = %d, want %d (the run of cleanly-refused shows condemns the scope, so the last show is never queried)",
			stats.queries, consecutiveFruitlessLatch)
	}
	if len(titles) != 0 {
		t.Errorf("titles = %v, want none (every result was refused for contradictory identity)", titles)
	}
	if !rec.Contains("indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("no-progress latch not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestHarvestShowThatTitledSomethingIsNotChargedForALaterRejection pins the
// inverse of the backstop: a show that HARVESTED real titles before a later
// title candidate was rejected made progress, so neither the request-rejection
// run nor the no-progress run may be charged for it.
//
// The ladder widens only while the show is still unsatisfied, so a partially
// titled show reaching a rejection on its second candidate is the ordinary shape
// of a multi-release show, not an upstream fault. Charging it would let a run of
// perfectly productive shows condemn the scope and skip every remaining show's
// harvest for the rest of the rebuild.
func TestHarvestShowThatTitledSomethingIsNotChargedForALaterRejection(t *testing.T) {
	// One more show than the request-rejection latch tolerates, so a wrongly
	// charged run would condemn the scope before the last show is queried.
	const shows = consecutiveRejectedLatch + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		q := r.URL.Query().Get("q")
		if !strings.HasSuffix(q, "(2023)") {
			// The WIDENED candidate: the upstream rejects this query shape.
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><error code="201" description="Incorrect parameter"/>`)
			return
		}
		// The first candidate titles ONE of the show's two releases, which
		// leaves the show pending and the ladder free to widen.
		show := strings.TrimSuffix(strings.TrimPrefix(q, "Show "), " (2023)")
		body := torznabBody(torznabItem("Show "+show+" Real Title S01E01",
			"https://nyaa.si/view/1"+show+"0"))
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	feed := make([]journalItem, 0, 2*shows)
	info := map[int]EntryInfo{}
	for i := range shows {
		show := strconv.Itoa(i)
		feed = append(feed,
			journalItem{Title: "Synthesized E01", Key: "nyaa:1" + show + "0", AniListID: 7 + i},
			journalItem{Title: "Synthesized E02", Key: "nyaa:1" + show + "1", AniListID: 7 + i},
		)
		info[7+i] = EntryInfo{Title: "Show " + show + " (2023)"}
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{
		NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
	}, log, srv.Client())
	titles := map[string]string{}
	stats, _ := w.harvest.harvestTitles(t.Context(), map[string][]journalItem{upstreamNyaa: feed},
		titles, func(alID int) EntryInfo { return info[alID] }, "")

	if stats.queries != 2*shows {
		t.Errorf("harvest queries = %d, want %d (two candidates for every show; a productive show must not condemn its scope)",
			stats.queries, 2*shows)
	}
	if len(titles) != shows {
		t.Errorf("titles = %v, want %d (one per show, from each show's first candidate)", titles, shows)
	}
	if rec.Contains("indexer title harvest: repeated request rejections; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("shows that harvested real titles were charged to the request-rejection run; log output:\n%s",
			strings.Join(rec.Messages(), "\n"))
	}
	if rec.Contains("indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild") {
		t.Errorf("shows that harvested real titles were charged to the no-progress run; log output:\n%s",
			strings.Join(rec.Messages(), "\n"))
	}
}
