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

// TestFetchEntriesErrorsOnMetadataRegression pins the truncated-view guard
// against pagination-metadata REGRESSION: page 1 promises totalItems=501 over
// totalPages=2 and delivers 500 entries, then page 2 arrives empty and OMITS
// totalItems (which decodes as zero). The retained highest reported total
// (fetchTotals.reportedTotal is never overwritten downward) keeps
// pageComplete's outstanding-items check armed, so the fetch fails rather
// than successfully returning the truncated 500-entry catalogue.
func TestFetchEntriesErrorsOnMetadataRegression(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			items := make([]string, 500)
			for i := range items {
				items[i] = fmt.Sprintf(`{"alID":%d,"expand":{"trs":[]}}`, i+1)
			}
			fmt.Fprintf(w, `{"totalItems":501,"totalPages":2,"items":[%s]}`, strings.Join(items, ","))
		case 2:
			fmt.Fprint(w, `{"totalPages":2,"items":[]}`)
		default:
			t.Errorf("unexpected request for page %d after metadata regression", page)
		}
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want truncated-view error on metadata regression")
	}
	if entries != nil {
		t.Fatalf("len(entries) = %d, want nil on truncated-view error", len(entries))
	}
	if !strings.Contains(err.Error(), "500 of 501 reported entries fetched") {
		t.Errorf("error = %q, want it to name fetched (500) vs reported (501) counts", err.Error())
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

// TestFetchEntriesInconsistentTotalsError pins finishFetch's metadata
// self-consistency guard: a completed catalogue whose retained totalItems
// cannot fit the retained totalPages at perPage (every honest PocketBase
// response satisfies totalItems <= totalPages*perPage, and the
// retained-highest maxima preserve that inequality) proves a single response
// was internally inconsistent - upstream misbehavior, not offset-pagination
// raciness - so the fetch must abort instead of waving the deficit through
// with the count-mismatch WARN.
func TestFetchEntriesInconsistentTotalsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":501,"totalPages":1,"items":[{"alID":1,"expand":{"trs":[]}},{"alID":2,"expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	logger, _ := capture.New()
	entries, err := NewClient(server.Client(), server.URL, 0, logger).FetchEntries(context.Background())
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want an error (totalItems 501 cannot fit 1 page of %d)", len(entries), perPage)
	}
	if !strings.Contains(err.Error(), "cannot fit the reported") {
		t.Errorf("error = %q, want inconsistent-totals context", err.Error())
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

// TestFetchEntriesContinuesPastLoweredTotalPages pins the totalPages arm of
// the metadata-regression guard (fetchTotals.reportedPages, never overwritten
// downward): a later NON-EMPTY page whose currently-valid totalPages regressed
// below an earlier page's promise must not end the walk early - the fetch
// continues to the promised page and returns the full catalogue, instead of
// stopping at the regressed value and returning a truncated view finishFetch
// would wave through with only the count-mismatch WARN.
func TestFetchEntriesContinuesPastLoweredTotalPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			fmt.Fprint(w, `{"totalItems":3,"totalPages":3,"items":[{"alID":1,"expand":{"trs":[]}}]}`)
		case 2:
			// A non-empty page whose CURRENT metadata says the walk is over
			// (totalPages regressed 3 -> 2) while page 1 promised a page 3.
			fmt.Fprint(w, `{"totalItems":3,"totalPages":2,"items":[{"alID":2,"expand":{"trs":[]}}]}`)
		case 3:
			fmt.Fprint(w, `{"totalItems":3,"totalPages":3,"items":[{"alID":3,"expand":{"trs":[]}}]}`)
		default:
			t.Errorf("unexpected request for page %d", page)
		}
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (a lowered-but-valid totalPages must not fail the fetch)", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (the walk must continue to the promised page 3, not stop at the regressed totalPages)", len(entries))
	}
	if entries[2].AniListID != 3 {
		t.Errorf("entries[2].AniListID = %d, want 3 (page 3 fetched after the metadata regression)", entries[2].AniListID)
	}
}

// pagedRecordingTransport serves a fixed two-page catalogue and records the
// virtual time of each page request relative to the transport's start.
type pagedRecordingTransport struct {
	started time.Time
	times   []time.Duration
}

func (tr *pagedRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.times = append(tr.times, time.Since(tr.started))
	body := `{"totalItems":2,"totalPages":2,"items":[{"alID":1,"expand":{"trs":[]}}]}`
	if req.URL.Query().Get("page") == "2" {
		body = `{"totalItems":2,"totalPages":2,"items":[{"alID":2,"expand":{"trs":[]}}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// TestFetchEntriesSleepsOnlyBetweenPages pins WHERE the politeness sleep
// lands, not just that cancellation during it aborts: page 1 must be fetched
// immediately (no delay before the first page of a cycle), and exactly one
// pageDelay must elapse before page 2. A guard drift that also sleeps before
// page 1 (or stops sleeping between pages) shifts every cycle's first fetch
// by a full pageDelay without failing any existing test.
func TestFetchEntriesSleepsOnlyBetweenPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &pagedRecordingTransport{started: time.Now()}
		client := NewClient(&http.Client{Transport: tr}, "https://example.test", time.Minute, nil)
		entries, err := client.FetchEntries(t.Context())
		if err != nil {
			t.Fatalf("FetchEntries returned error: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("entries = %d, want 2", len(entries))
		}
		if len(tr.times) != 2 {
			t.Fatalf("requests = %d, want 2", len(tr.times))
		}
		if tr.times[0] != 0 {
			t.Errorf("page 1 fetched after %s of delay, want immediately (no politeness sleep before the first page)", tr.times[0])
		}
		if tr.times[1] != time.Minute {
			t.Errorf("page 2 fetched after %s, want exactly one pageDelay (1m0s)", tr.times[1])
		}
	})
}

// TestFetchEntriesCleanFetchEmitsNoWarnings pins the OFF state of the three
// aggregate degradation gates (count mismatch, unparseable timestamps,
// unusable torrent URLs): a fully healthy fetch - counts agreeing, a
// parseable updated timestamp, a usable canonical-host torrent URL - must
// emit none of the alert-stable WARN lines, so the Loki alerts keyed on them
// can never fire on a clean cycle.
func TestFetchEntriesCleanFetchEmitsNoWarnings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":1,"totalPages":1,"items":[{"alID":1,"updated":"2026-01-02 03:04:05.000Z","expand":{"trs":[{"tracker":"Nyaa","url":"https://nyaa.si/view/1"}]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, 0, logger).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	for _, msg := range []string{
		"seadex catalogue count mismatch",
		"seadex updated timestamps unparseable; feed newest-first ordering degraded",
		"seadex torrent URLs unusable; affected findings and feed items carry no release link",
	} {
		if got := recorder.CountExact(msg); got != 0 {
			t.Errorf("clean fetch logged %q %d times, want 0", msg, got)
		}
	}
}

// TestFetchEntriesExactlyFullPagesSucceed pins the consistency guard's
// boundary: a catalogue whose reported totalItems exactly fills the reported
// pages (totalItems == totalPages*perPage) is the honest maximally-full
// PocketBase shape and must succeed - the guard fires only when totalItems
// STRICTLY exceeds what the reported pages can carry.
func TestFetchEntriesExactlyFullPagesSucceed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		items := make([]string, perPage)
		for i := range items {
			items[i] = fmt.Sprintf(`{"alID":%d,"expand":{"trs":[]}}`, i+1)
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":1,"items":[%s]}`, perPage, strings.Join(items, ","))
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (totalItems == totalPages*perPage is the honest full-page shape and must not trip the consistency guard)", err)
	}
	if len(entries) != perPage {
		t.Fatalf("entries = %d, want %d", len(entries), perPage)
	}
}
