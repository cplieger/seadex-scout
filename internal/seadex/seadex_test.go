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
			fmt.Fprint(w, `{"totalPages":2,"items":[{"alID":154587,"updated":"2026-01-02 03:04:05.000Z","notes":"note","comparison":"cmp","theoreticalBest":"","incomplete":true,"expand":{"trs":[{"releaseGroup":"SubsPlease","tracker":"Nyaa","infoHash":"abc","url":"https://nyaa.si/view/1","isBest":true,"dualAudio":true,"tags":["best"],"files":[{"name":"Frieren.mkv","length":123}] }]}}]}`)
		case 2:
			fmt.Fprint(w, `{"totalPages":2,"items":[{"alID":200,"updated":"2026-01-03T04:05:06Z","expand":{"trs":[]}}]}`)
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
		in   Torrent
		want string
	}{
		{name: "blank", in: Torrent{Tracker: "Nyaa", URL: "   "}, want: ""},
		{name: "absolute", in: Torrent{Tracker: "AB", URL: " https://example.test/t/1 "}, want: "https://example.test/t/1"},
		{name: "animebytes relative", in: Torrent{Tracker: "AB", URL: "/torrents.php?id=1"}, want: "https://animebytes.tv/torrents.php?id=1"},
		{name: "relative without slash", in: Torrent{Tracker: "Nyaa", URL: "view/1"}, want: "https://nyaa.si/view/1"},
		{name: "unknown tracker relative", in: Torrent{Tracker: "unknown", URL: "/local/path"}, want: "/local/path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.UsableURL(); got != tc.want {
				t.Errorf("UsableURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
