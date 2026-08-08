package seadexapi

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

// TestFetchEntriesPaginatesAndDecodes pins the request contract and the
// decoded shape across a two-chunk keyset walk: every request asks for page 1
// of the sorted remainder (the cursor filter, absent on the first chunk, is
// what advances the walk), the politeness headers and expand/perPage/sort
// params are set, and one rich record decodes fully (identity, timestamp,
// torrent with files and tags).
func TestFetchEntriesPaginatesAndDecodes(t *testing.T) {
	rich := `{"alID":154587,"id":"rec000000","created":"2026-01-02 03:04:05.000Z","updated":"2026-01-02 03:04:05.000Z","notes":"note","comparison":"cmp","theoreticalBest":"","incomplete":true,"expand":{"trs":[{"releaseGroup":"SubsPlease","tracker":"Nyaa","infoHash":"abc","url":"https://nyaa.si/view/1","isBest":true,"dualAudio":true,"tags":["best"],"files":[{"name":"Frieren.mkv","length":123}] }]}}`
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
		if got := q.Get("sort"); got != "created,id" {
			t.Errorf("sort query = %q, want created,id (the immutable keyset order)", got)
		}
		if got := q.Get("page"); got != "1" {
			t.Errorf("page query = %q, want 1 (a keyset walk always reads the first page of the remainder)", got)
		}
		if q.Get("filter") == "" {
			// The first chunk is unfiltered and FULL, so the walk continues:
			// the rich record plus filler up to perPage.
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s,%s]}`, perPage+1, rich, keysetRecords(1, perPage-1))
			return
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1,
			`{"alID":900,"id":"rec000900","created":"2026-01-03 04:05:06.000Z","updated":"2026-01-03T04:05:06Z","expand":{"trs":[]}}`)
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL, 0, nil)
	entries, err := client.FetchEntries(context.Background(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != perPage+1 {
		t.Fatalf("entries = %d, want %d", len(entries), perPage+1)
	}
	if entries[perPage].AniListID != 900 {
		t.Errorf("last entry alID = %d, want the second chunk's 900", entries[perPage].AniListID)
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

// TestFetchEntriesPaginationCapErrors pins the maxPages walk bound: an upstream
// that keeps serving FULL chunks (so the keyset walk never sees a short one)
// must stop at maxPages requests with the truncated-view error and a nil slice,
// never loop forever.
func TestFetchEntriesPaginationCapErrors(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		fmt.Fprintf(w, `{"totalItems":%d,"items":[%s]}`, maxPages*perPage,
			fullKeysetChunk(requests*perPage+1))
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL, 0, nil)
	entries, err := client.FetchEntries(context.Background(), Options{})
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want pagination cap error")
	}
	if entries != nil {
		t.Fatalf("entries = %d, want nil on cap error", len(entries))
	}
	want := fmt.Sprintf("pagination exceeded max %d pages", maxPages)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
	if requests != maxPages {
		t.Errorf("requests = %d, want %d", requests, maxPages)
	}
}

// TestFetchEntriesDecodesEveryPublishedField pins the wire contract for every
// entry/torrent field downstream consumers read (matching, feed construction,
// link rendering, classification, theoretical-best reporting): a mistyped JSON
// tag or an omission in pbEntry.toEntry must fail here rather than silently
// zeroing a field.
func TestFetchEntriesDecodesEveryPublishedField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":1,"totalPages":1,"items":[{"alID":154587,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","notes":"curator note","theoreticalBest":"ideal remux","updated":"2026-01-02T03:04:05Z","incomplete":true,"expand":{"trs":[{"releaseGroup":"SubsPlease","tracker":"Nyaa","infoHash":"abc123","url":"https://nyaa.si/view/1","files":[{"name":"Frieren.mkv","length":123}],"tags":["best","dual"],"isBest":true,"dualAudio":true}]}}]}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.AniListID != 154587 || entry.Notes != "curator note" || entry.TheoreticalBest != "ideal remux" || !entry.Incomplete {
		t.Errorf("entry = %+v, want every published entry field decoded", entry)
	}
	if len(entry.Torrents) != 1 {
		t.Fatalf("torrents = %d, want 1", len(entry.Torrents))
	}
	tor := entry.Torrents[0]
	if tor.ReleaseGroup != "SubsPlease" || tor.Tracker != "Nyaa" || tor.InfoHash != "abc123" || tor.URL != "https://nyaa.si/view/1" || !tor.IsBest || !tor.DualAudio {
		t.Errorf("torrent = %+v, want every published torrent field decoded", tor)
	}
	if len(tor.Tags) != 2 || tor.Tags[0] != "best" || tor.Tags[1] != "dual" {
		t.Errorf("torrent tags = %v, want [best dual]", tor.Tags)
	}
	if len(tor.Files) != 1 || tor.Files[0].Name != "Frieren.mkv" || tor.Files[0].Length != 123 {
		t.Errorf("torrent files = %+v, want Frieren.mkv length 123", tor.Files)
	}
}
