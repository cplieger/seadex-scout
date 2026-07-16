package seadex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/appinfo"
)

func TestFetchEntriesPaginatesAndDecodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != entriesPath {
			t.Errorf("path = %q, want %q", r.URL.Path, entriesPath)
		}
		if got := r.Header.Get("User-Agent"); got != appinfo.UserAgent {
			t.Errorf("User-Agent = %q, want %q", got, appinfo.UserAgent)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		q := r.URL.Query()
		if got := q.Get("expand"); got != "trs" {
			t.Errorf("expand query = %q, want trs", got)
		}
		if got := q.Get("perPage"); got != strconv.Itoa(perPage) {
			t.Errorf("perPage query = %q, want %d", got, perPage)
		}
		page, err := strconv.Atoi(q.Get("page"))
		if err != nil {
			t.Errorf("page query is not an int: %v", err)
		}
		switch page {
		case 1:
			fmt.Fprint(w, `{"totalItems":2,"totalPages":2,"items":[{"alID":154587,"updated":"2026-01-02 03:04:05.000Z","notes":"note","comparison":"cmp","theoreticalBest":"","incomplete":true,"expand":{"trs":[{"releaseGroup":"SubsPlease","tracker":"Nyaa","infoHash":"abc","url":"https://nyaa.si/view/1","isBest":true,"dualAudio":true,"tags":["best"],"files":[{"name":"Frieren.mkv","length":123}] }]}}]}`)
		case 2:
			fmt.Fprint(w, `{"totalItems":2,"totalPages":2,"items":[{"alID":200,"updated":"2026-01-03T04:05:06Z","expand":{"trs":[]}}]}`)
		default:
			t.Errorf("unexpected page %d", page)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL, 0, nil)
	entries, err := client.FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].AniListID != 154587 || !entries[0].Incomplete {
		t.Errorf("first entry identity = alID %d incomplete %v, want 154587 true", entries[0].AniListID, entries[0].Incomplete)
	}
	wantUpdated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !entries[0].Updated.Equal(wantUpdated) {
		t.Errorf("updated = %s, want %s", entries[0].Updated, wantUpdated)
	}
	if len(entries[0].Torrents) != 1 {
		t.Fatalf("torrents = %d, want 1", len(entries[0].Torrents))
	}
	gotTorrent := entries[0].Torrents[0]
	if gotTorrent.ReleaseGroup != "SubsPlease" || gotTorrent.Tracker != "Nyaa" || !gotTorrent.IsBest || !gotTorrent.DualAudio {
		t.Errorf("torrent = %+v, want SubsPlease/Nyaa best dual-audio", gotTorrent)
	}
	if len(gotTorrent.Files) != 1 || gotTorrent.Files[0].Name != "Frieren.mkv" || gotTorrent.Files[0].Length != 123 {
		t.Errorf("torrent files = %+v, want Frieren.mkv length 123", gotTorrent.Files)
	}
}

func TestFetchEntriesPaginationCapErrors(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Errorf("page query is not an int: %v", err)
		}
		fmt.Fprintf(w, `{"totalPages":%d,"items":[{"alID":%d,"expand":{"trs":[]}}]}`, maxPages+1, page)
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL, 0, nil)
	entries, err := client.FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want pagination cap error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on cap error", entries)
	}
	want := fmt.Sprintf("pagination exceeded max %d pages", maxPages)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
	if requests != maxPages {
		t.Errorf("requests = %d, want %d", requests, maxPages)
	}
}

func TestTorrentUsableURL(t *testing.T) {
	tests := []struct {
		name string
		want string
		in   Torrent
	}{
		{name: "blank", in: Torrent{Tracker: "Nyaa", URL: "   "}, want: ""},
		{name: "absolute", in: Torrent{Tracker: "AB", URL: " https://example.test/t/1 "}, want: "https://example.test/t/1"},
		{name: "animebytes relative", in: Torrent{Tracker: "AB", URL: "/torrents.php?id=1"}, want: "https://animebytes.tv/torrents.php?id=1"},
		{name: "relative without slash", in: Torrent{Tracker: "Nyaa", URL: "view/1"}, want: "https://nyaa.si/view/1"},
		{name: "unknown tracker relative drops", in: Torrent{Tracker: "unknown", URL: "/local/path"}, want: ""},
		{name: "unknown tracker absolute passes through", in: Torrent{Tracker: "unknown", URL: "https://example.test/t/9"}, want: "https://example.test/t/9"},
		{name: "stripped tracker relative drops", in: Torrent{Tracker: "beyondhd", URL: "/torrents/1"}, want: ""},
		{name: "rutracker relative", in: Torrent{Tracker: "RuTracker", URL: "forum/viewtopic.php?t=1"}, want: "https://rutracker.org/forum/viewtopic.php?t=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.UsableURL(); got != tc.want {
				t.Errorf("UsableURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFetchEntriesUsesStableSort pins the immutable-field pagination ordering
// (sort=created,id): with offset pagination over a live collection, sorting on
// a mutable field lets a mid-pagination update shift records across pages (one
// entry missed, another duplicated), so losing this query silently reopens that
// truncation class.
func TestFetchEntriesUsesStableSort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("sort"); got != "created,id" {
			t.Errorf("sort query = %q, want created,id", got)
		}
		fmt.Fprint(w, `{"totalItems":1,"totalPages":1,"items":[{"alID":1,"expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1", len(entries))
	}
}

// TestTorrentUsableURLRejectsUnsafeSchemes pins the unsafe-scheme and
// malformed-URL gate on the untrusted upstream URL: javascript:, data:, and
// file: values must never be converted into clickable tracker links, and a
// malformed absolute value (hostless, unparseable escape, whitespace in the
// host, backslash authority) must drop to the empty-URL case rather than be
// published as a link a human cannot follow.
func TestTorrentUsableURLRejectsUnsafeSchemes(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "javascript", url: "javascript:alert(1)"},
		{name: "data", url: "data:text/html,<script>alert(1)</script>"},
		{name: "file", url: "file:///etc/passwd"},
		{name: "hostless https", url: "https://"},
		{name: "invalid escape", url: "https://example.test/%zz"},
		{name: "whitespace in host", url: "https://bad host/path"},
		{name: "backslash authority", url: `\\evil.example/path`},
		{name: "userinfo authority confusion", url: "https://animebytes.tv@evil.example/torrent"},
		{name: "query-only with colon", url: "?x:y"},
		{name: "fragment-only with colon", url: "#a:b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := (&Torrent{Tracker: "Nyaa", URL: tc.url}).UsableURL()
			if got != "" {
				t.Errorf("UsableURL(%q) = %q, want empty for unsafe scheme", tc.url, got)
			}
		})
	}
}
