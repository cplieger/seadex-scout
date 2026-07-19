package seadex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestFetchEntriesDiscardsPartialOnMidPaginationError pins the "never compare
// against a truncated view" contract: when a later page fails after earlier
// pages accumulated entries, FetchEntries returns a nil slice and an error that
// names the failed page, discarding the partial result rather than returning it.
func TestFetchEntriesDiscardsPartialOnMidPaginationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			fmt.Fprint(w, `{"totalPages":2,"items":[{"alID":1,"expand":{"trs":[]}}]}`)
			return
		}
		fmt.Fprint(w, `{`)
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL, 0, nil)
	entries, err := client.FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want a page-2 fetch error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil (partial results discarded, never a truncated view)", entries)
	}
	if !strings.Contains(err.Error(), "fetch page 2") {
		t.Errorf("error = %q, want it to name the failed page 2", err.Error())
	}
}

// TestFetchEntriesErrorsOnEmptyIntermediatePage pins the truncated-view guard
// for the empty-page case: an empty page reported before the final page is an
// error (a bad intermediate page must not falsely resolve findings), so
// FetchEntries returns a nil slice and an error naming the empty page.
func TestFetchEntriesErrorsOnEmptyIntermediatePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			fmt.Fprint(w, `{"totalPages":3,"items":[{"alID":1,"expand":{"trs":[]}}]}`)
		case 2:
			fmt.Fprint(w, `{"totalPages":3,"items":[]}`)
		default:
			t.Errorf("unexpected request for page %d after empty intermediate page", page)
		}
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want empty-intermediate-page error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on truncated-view error", entries)
	}
	if !strings.Contains(err.Error(), "empty before reported total") {
		t.Errorf("error = %q, want empty-intermediate-page context", err.Error())
	}
}

// TestFetchEntriesErrorsOnEmptyTerminalPageWithOutstandingItems pins the
// truncated-view guard for the empty FINAL page: a page equal to the reported
// totalPages returning zero items while the collected count is still below
// the reported totalItems must fail the fetch (the fail-safe direction — a
// degraded cycle preserves existing findings, while accepting the truncated
// view would falsely resolve them), with an error naming the fetched vs
// reported counts, never finishFetch's lenient count-mismatch WARN (which is
// reserved for terminal pages that carried entries).
func TestFetchEntriesErrorsOnEmptyTerminalPageWithOutstandingItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			fmt.Fprint(w, `{"totalItems":2,"totalPages":2,"items":[{"alID":1,"expand":{"trs":[]}}]}`)
		case 2:
			fmt.Fprint(w, `{"totalItems":2,"totalPages":2,"items":[]}`)
		default:
			t.Errorf("unexpected request for page %d after empty terminal page", page)
		}
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want empty-terminal-page error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on truncated-view error", entries)
	}
	if !strings.Contains(err.Error(), "page 2 empty") {
		t.Errorf("error = %q, want it to name the empty page 2", err.Error())
	}
	if !strings.Contains(err.Error(), "1 of 2 reported entries fetched") {
		t.Errorf("error = %q, want it to name fetched (1) vs reported (2) counts", err.Error())
	}
}

// staticPageTransport serves a fixed two-page envelope's first page for every
// request, keeping the unmanaged SeaDex boundary hermetic inside the synctest
// bubble (a real httptest socket would block virtual time).
type staticPageTransport struct{}

func (staticPageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"totalPages":2,"items":[{"alID":1,"expand":{"trs":[]}}]}`)),
		Request:    req,
	}, nil
}

// TestFetchEntriesCancelledBetweenPagesAborts pins the shutdown arm of the
// "never compare against a truncated view" contract: a context that expires
// during the inter-page politeness sleep must abort the fetch with an
// interrupted error and a nil slice, never return the pages accumulated so
// far. synctest advances exactly 500ms of virtual time, so the timer branch
// is exercised without real wall-clock waiting.
func TestFetchEntriesCancelledBetweenPagesAborts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		// The page delay far exceeds the context deadline, so the sleep
		// between page 1 and page 2 is where the cancellation lands.
		client := NewClient(&http.Client{Transport: staticPageTransport{}}, "https://example.test", time.Minute, nil)
		started := time.Now()
		entries, err := client.FetchEntries(ctx)
		if err == nil {
			t.Fatal("FetchEntries returned nil error, want interrupted-between-pages error")
		}
		if entries != nil {
			t.Fatalf("entries = %+v, want nil (partial pages discarded on interruption)", entries)
		}
		if !strings.Contains(err.Error(), "interrupted between pages") {
			t.Errorf("error = %q, want interrupted-between-pages context", err.Error())
		}
		if elapsed := time.Since(started); elapsed != 500*time.Millisecond {
			t.Errorf("elapsed = %s, want virtual 500ms", elapsed)
		}
	})
}

// TestFetchEntriesHTTPStatusErrorAborts pins the transport arm of the "never
// compare against a truncated view" contract: an HTTP status failure from the
// page fetch (a non-retryable 404 here) must abort FetchEntries with an error
// naming the failed page and a nil slice, never be treated as a complete
// (empty) catalogue.
func TestFetchEntriesHTTPStatusErrorAborts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want HTTP status error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on HTTP failure", entries)
	}
	if !strings.Contains(err.Error(), "fetch page 1") {
		t.Errorf("error = %q, want it to name the failed page 1", err.Error())
	}
}

// TestFetchEntriesEmptyCatalogueErrors pins the empty-catalogue guard: a first
// response reporting zero items ({"totalItems":0,"totalPages":0,"items":[]})
// completes pagination but must surface as an ERROR, never a successful empty
// slice - SeaDex is never legitimately empty, and accepting an empty catalogue
// would make the caller report every library item as having no SeaDex coverage.
func TestFetchEntriesEmptyCatalogueErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":0,"totalPages":0,"items":[]}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want empty-catalogue error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on an empty catalogue", entries)
	}
	if !strings.Contains(err.Error(), "empty catalogue") {
		t.Errorf("error = %q, want empty-catalogue context", err.Error())
	}
}

// TestFetchEntriesCountMismatchWarnsButSucceeds pins the count-mismatch
// contract: a completed catalogue whose collected entry count disagrees with
// the API's reported totalItems logs the single alert-stable WARN line but
// still returns the entries, since offset pagination over a live collection
// can legitimately shift counts mid-fetch.
func TestFetchEntriesCountMismatchWarnsButSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":5,"totalPages":1,"items":[{"alID":1,"expand":{"trs":[]}},{"alID":2,"expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, 0, logger).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (a count mismatch must not fail the fetch)", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if got := recorder.CountExact("seadex catalogue count mismatch"); got != 1 {
		t.Errorf("count-mismatch WARN count = %d, want 1", got)
	}
}

// TestFetchEntriesUnparseableUpdatedWarnsOnce pins the timestamp-drift signal:
// entries whose non-empty updated value fails every known PocketBase layout
// are zeroed (sorting to the feed's tail), and the fetch surfaces ONE
// aggregate WARN carrying the failure count - an upstream format drift that
// zeroes the whole catalogue must be alertable from Loki without per-record
// noise, while the fetch itself still succeeds.
func TestFetchEntriesUnparseableUpdatedWarnsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":3,"totalPages":1,"items":[`+
			`{"alID":1,"updated":"not-a-timestamp","expand":{"trs":[]}},`+
			`{"alID":2,"updated":"31/12/2025","expand":{"trs":[]}},`+
			`{"alID":3,"updated":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, 0, logger).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (unparseable timestamps must not fail the fetch)", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if got := recorder.CountExact("seadex updated timestamps unparseable; feed newest-first ordering degraded"); got != 1 {
		t.Errorf("unparseable-updated WARN count = %d, want 1 aggregate line", got)
	}
	warned := false
	for _, r := range recorder.Records() {
		if r.Message != "seadex updated timestamps unparseable; feed newest-first ordering degraded" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "count" && a.Value.Int64() == 2 {
				warned = true
				return false
			}
			return true
		})
	}
	if !warned {
		t.Error("unparseable-updated WARN does not carry count=2 (only the two bogus timestamps; the empty/valid ones must not count)")
	}
}

// TestFetchEntriesUnusableTorrentURLWarnsOnce pins the link-drop signal: a
// torrent whose URL is unusable — omitted/empty, a foreign host under a
// trusted tracker label, or an unknown tracker — is counted (filter.Obtainable
// treats every UsableURL()=="" torrent as unobtainable), and the fetch
// surfaces ONE aggregate WARN carrying the count - a tracker host migration or
// schema drift that strips every release link must be alertable from Loki -
// while the fetch itself still succeeds. A usable canonical-host URL must not
// count.
func TestFetchEntriesUnusableTorrentURLWarnsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":2,"totalPages":1,"items":[`+
			`{"alID":1,"expand":{"trs":[`+
			`{"tracker":"Nyaa","url":"https://evil.example/view/1"},`+
			`{"tracker":"SomeRandomTracker","url":"https://example.com/x"},`+
			`{"tracker":"Nyaa","url":""}]}},`+
			`{"alID":2,"expand":{"trs":[`+
			`{"tracker":"Nyaa","url":"https://nyaa.si/view/123"}]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, 0, logger).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (unusable torrent URLs must not fail the fetch)", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	const msg = "seadex torrent URLs unusable; affected findings and feed items carry no release link"
	if got := recorder.CountExact(msg); got != 1 {
		t.Errorf("unusable-URL WARN count = %d, want 1 aggregate line", got)
	}
	warned := false
	for _, r := range recorder.Records() {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "count" && a.Value.Int64() == 3 {
				warned = true
				return false
			}
			return true
		})
	}
	if !warned {
		t.Error("unusable-URL WARN does not carry count=3 (the foreign-host, unknown-tracker, and omitted-URL torrents; the usable one must not count)")
	}
}
