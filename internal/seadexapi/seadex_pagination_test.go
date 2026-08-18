package seadexapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// keysetRecords renders count entries records numbered from start, each
// carrying the immutable (created, id) pair the keyset walk pages on. Every
// record shares one created value, so the walk can only advance through the
// composite cursor's id tie-break.
func keysetRecords(start, count int) string {
	items := make([]string, count)
	for i := range items {
		n := start + i
		items[i] = fmt.Sprintf(`{"alID":%d,"id":"rec%06d","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}`, n, n)
	}
	return strings.Join(items, ",")
}

// fullKeysetChunk renders a FULL chunk (perPage records) numbered from start;
// a full chunk is what keeps the keyset walk going.
func fullKeysetChunk(start int) string { return keysetRecords(start, perPage) }

// cursorIDFromFilter reads the keyset id predicate out of a request's filter
// param, standing in for PocketBase evaluating it. It reports a missing
// predicate as a test failure and returns "" (which surfaces as a duplicate or
// skipped record in the caller's assertions).
func cursorIDFromFilter(t *testing.T, filter string) string {
	t.Helper()
	const marker = `id>"`
	_, rest, found := strings.Cut(filter, marker)
	if !found {
		t.Errorf("filter %q carries no keyset id predicate", filter)
		return ""
	}
	id, _, terminated := strings.Cut(rest, `"`)
	if !terminated {
		t.Errorf("filter %q has an unterminated keyset id predicate", filter)
		return ""
	}
	return id
}

// TestFetchEntriesDiscardsPartialOnMidPaginationError pins the "never compare
// against a truncated view" contract: when a later chunk fails after earlier
// chunks accumulated entries, FetchEntries returns a nil slice and an error that
// names the failed chunk, discarding the partial result rather than returning it.
func TestFetchEntriesDiscardsPartialOnMidPaginationError(t *testing.T) {
	var reqs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs++
		if reqs == 1 {
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		fmt.Fprint(w, `{`)
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL)
	entries, err := client.FetchEntries(t.Context(), Options{})
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

// TestFetchEntriesKeysetSurvivesPrefixDeletion pins WHY the walk is keyset-
// paged rather than offset-paged: a record deleted from an already-read prefix
// shifts every later record one slot forward. With numbered offsets, chunk 2
// (offset 500) would start past the record that moved into the consumed range,
// silently dropping a still-existing entry while the counts, page shape, and
// every completeness guard still agreed. The cursor asks for the records after
// the last one consumed, so the boundary record is delivered exactly once.
func TestFetchEntriesKeysetSurvivesPrefixDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("filter")
		if filter == "" {
			// The pre-deletion prefix: records 1..perPage.
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		// Between the chunks a prefix record was deleted and a new record
		// appended at the tail, so the collection now holds perPage+1 records
		// ending at perPage+2. Everything after the cursor is what remains.
		got := cursorIDFromFilter(t, filter)
		if want := fmt.Sprintf("rec%06d", perPage); got != want {
			t.Errorf("cursor id = %q, want %q (the last record of chunk 1)", got, want)
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s,%s]}`, perPage+1,
			keysetRecords(perPage+1, 1), keysetRecords(perPage+2, 1))
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	seen := make(map[int]int, len(entries))
	for i := range entries {
		seen[entries[i].AniListID]++
	}
	if got := seen[perPage+1]; got != 1 {
		t.Errorf("boundary record alID %d appeared %d times, want exactly 1 (offset pagination would skip it)", perPage+1, got)
	}
	if got := seen[perPage+2]; got != 1 {
		t.Errorf("tail record alID %d appeared %d times, want exactly 1", perPage+2, got)
	}
	if len(entries) != perPage+2 {
		t.Errorf("entries = %d, want %d (the prefix chunk plus both remaining records)", len(entries), perPage+2)
	}
}

// TestFetchEntriesKeysetWalksEqualCreatedRecords pins the composite cursor's
// tie-break over a multi-chunk catalogue whose records share one created
// timestamp: the walk must advance by id through every chunk, delivering each
// record exactly once and never stalling on the equal-created boundary (a
// created-only cursor would either loop on the same chunk forever or skip the
// rest of the timestamp's records).
func TestFetchEntriesKeysetWalksEqualCreatedRecords(t *testing.T) {
	const total = 2*perPage + 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := 1
		if filter := r.URL.Query().Get("filter"); filter != "" {
			id := cursorIDFromFilter(t, filter)
			var last int
			if _, err := fmt.Sscanf(id, "rec%d", &last); err != nil {
				t.Errorf("cursor id %q is not a record id: %v", id, err)
			}
			next = last + 1
		}
		count := min(perPage, total-next+1)
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":3,"items":[%s]}`, total, keysetRecords(next, count))
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != total {
		t.Fatalf("entries = %d, want %d", len(entries), total)
	}
	for i := range entries {
		if want := i + 1; entries[i].AniListID != want {
			t.Fatalf("entries[%d].AniListID = %d, want %d (every record delivered once, in order)", i, entries[i].AniListID, want)
		}
	}
}

// TestFetchEntriesErrorsOnEmptyChunkWithOutstandingItems pins the truncated-view
// guard for an EMPTY follow-up chunk: a chunk returning zero items after a full
// one, while the collected count is still below the reported totalItems, must
// fail the fetch (the fail-safe direction — a degraded cycle preserves existing
// findings, while accepting the truncated view would falsely resolve them), with
// an error naming the fetched vs reported counts, never finishFetch's lenient
// count-mismatch WARN (which is reserved for walks that ended on a SHORT chunk
// carrying entries).
func TestFetchEntriesErrorsOnEmptyChunkWithOutstandingItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "" {
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[]}`, perPage+1)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want empty-chunk error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on truncated-view error", entries)
	}
	if !strings.Contains(err.Error(), "page 2 empty") {
		t.Errorf("error = %q, want it to name the empty page 2", err.Error())
	}
	if want := fmt.Sprintf("%d of %d reported entries fetched", perPage, perPage+1); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %q", err.Error(), want)
	}
}

// TestFetchEntriesErrorsOnMetadataRegression pins the truncated-view guard
// against pagination-metadata REGRESSION: chunk 1 promises totalItems=501 and
// delivers a full chunk of 500, then the follow-up chunk arrives empty and OMITS
// totalItems (which decodes as zero). The retained highest reported total
// (fetchTotals.reportedTotal is never overwritten downward) keeps
// chunkComplete's outstanding-items check armed, so the fetch fails rather
// than successfully returning the truncated 500-entry catalogue.
func TestFetchEntriesErrorsOnMetadataRegression(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "" {
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		fmt.Fprint(w, `{"totalPages":2,"items":[]}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want truncated-view error on metadata regression")
	}
	if entries != nil {
		t.Fatalf("len(entries) = %d, want nil on truncated-view error", len(entries))
	}
	if want := fmt.Sprintf("%d of %d reported entries fetched", perPage, perPage+1); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %q", err.Error(), want)
	}
}

// TestFetchEntriesRejectsNonEmptyCatalogueWithoutReportedTotal pins the
// completeness arm finishFetch applies when the walk ended cleanly but NO
// response ever stated a totalItems: nothing vouches for the walk having read
// the whole collection, so a non-empty catalogue is still refused rather than
// returned as complete. The empty-catalogue and metadata-regression tests exit
// through different guards, so this is the only oracle for this arm.
func TestFetchEntriesRejectsNonEmptyCatalogueWithoutReportedTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalPages":1,"items":[{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want missing-total completeness error")
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil on an unverifiable catalogue", entries)
	}
	if !strings.Contains(err.Error(), "no reported total to vouch for completeness") {
		t.Errorf("error = %q, want missing-total completeness context", err.Error())
	}
}

// staticPageTransport serves a FULL chunk (perPage records, so the keyset walk
// continues into the politeness sleep) for every request, keeping the unmanaged
// SeaDex boundary hermetic inside the synctest bubble (a real httptest socket
// would block virtual time).
type staticPageTransport struct{ body string }

func newStaticPageTransport() *staticPageTransport {
	return &staticPageTransport{body: fmt.Sprintf(`{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))}
}

func (tr *staticPageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(tr.body)),
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
		// between chunk 1 and chunk 2 is where the cancellation lands.
		client := NewClient(&http.Client{Transport: newStaticPageTransport()}, "https://example.test", WithPageDelay(time.Minute))
		started := time.Now()
		entries, err := client.FetchEntries(ctx, Options{})
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

// TestFetchEntriesWholeWalkDeadlineAborts pins the INTERNAL whole-walk deadline
// (maxFetchDuration): a slow-but-responsive upstream must not hold the cycle
// lock indefinitely, so the fetch aborts on the client's own deadline even when
// the caller supplied none. The caller here has no deadline at all and the page
// delay exceeds the internal one, so only the wrapper can end the walk.
func TestFetchEntriesWholeWalkDeadlineAborts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(&http.Client{Transport: newStaticPageTransport()}, "https://example.test", WithPageDelay(maxFetchDuration+time.Minute))
		started := time.Now()
		entries, err := client.FetchEntries(t.Context(), Options{})
		if err == nil {
			t.Fatal("FetchEntries returned nil error, want whole-walk deadline error")
		}
		if entries != nil {
			t.Fatalf("entries = %+v, want nil after the whole-walk deadline", entries)
		}
		if !strings.Contains(err.Error(), "interrupted between pages") {
			t.Errorf("error = %q, want deadline interruption context", err.Error())
		}
		if elapsed := time.Since(started); elapsed != maxFetchDuration {
			t.Errorf("elapsed = %s, want the internal deadline %s", elapsed, maxFetchDuration)
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

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
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

// flakyStatusTransport answers the first failures requests with status, then
// serves a one-record catalogue page. It keeps the retry hermetic inside a
// synctest bubble, where httpx's backoff sleeps advance in virtual time.
type flakyStatusTransport struct {
	status   int
	failures int
	requests int
}

func (tr *flakyStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.requests++
	if tr.requests <= tr.failures {
		return &http.Response{
			StatusCode: tr.status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("upstream busy")),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"totalItems":1,"totalPages":1,"items":[{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)),
		Request:    req,
	}, nil
}

// TestFetchEntriesRetriesTransientStatus pins the per-page retry budget the
// client hands httpx: a Cloudflare-fronted community service answers 503 under
// load, and SeaDex is the one upstream whose failure degrades the cycle while
// PRESERVING stale findings for a whole poll_interval. A regression that drops
// WithMaxAttempts turns a single transient blip into that degradation, and no
// other test looks at the attempt budget at all.
func TestFetchEntriesRetriesTransientStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &flakyStatusTransport{status: http.StatusServiceUnavailable, failures: maxAttempts - 1}
		entries, err := NewClient(&http.Client{Transport: tr}, "https://example.test").FetchEntries(t.Context(), Options{})
		if err != nil {
			t.Fatalf("FetchEntries returned error: %v (a transient 503 must be retried, not degrade the cycle)", err)
		}
		if len(entries) != 1 {
			t.Fatalf("entries = %d, want 1", len(entries))
		}
		if tr.requests != maxAttempts {
			t.Errorf("requests = %d, want %d (the page retried through the configured attempt budget)", tr.requests, maxAttempts)
		}
	})
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

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
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
// contract: a completed catalogue whose collected entry count falls a LITTLE
// short of the API's reported totalItems logs the single alert-stable WARN line
// but still returns the entries, since records deleted from the collection
// during the walk legitimately shift the count. Below half the guard below
// takes over.
func TestFetchEntriesCountMismatchWarnsButSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":3,"totalPages":1,"items":[{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}},{"alID":2,"id":"rec000002","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (a small count mismatch must not fail the fetch)", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if got := recorder.CountExact("seadex catalogue count mismatch"); got != 1 {
		t.Errorf("count-mismatch WARN count = %d, want 1", got)
	}
}

// TestFetchEntriesBelowHalfShortfallErrors pins the last path on which a
// truncated catalogue used to be ACCEPTED (h-f7). A short terminal chunk ends
// the keyset walk, and a shortfall against the API's own reported totalItems was
// waved through with a WARN however large it was - so an upstream that ended the
// walk early while records remained handed the comparison a partial catalogue,
// which resolves every finding whose entry went missing. The keyset cursor makes
// a genuinely SKIPPED record impossible, so an ordinary shortfall is a handful
// of mid-fetch deletions; losing more than HALF the catalogue is not credible
// and now fails the fetch, degrading the cycle and PRESERVING existing findings.
// The trigger is the app-wide shrink policy (degradation.Shrunk),
// shared with the mapping-refresh and library-walk guards.
func TestFetchEntriesBelowHalfShortfallErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":5,"totalPages":1,"items":[{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}},{"alID":2,"id":"rec000002","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want the below-half shortfall to fail the fetch")
	}
	if entries != nil {
		t.Errorf("entries = %+v, want nil: a truncated catalogue must never reach the comparison", entries)
	}
	if !strings.Contains(err.Error(), "refusing to compare against a truncated view") {
		t.Errorf("error = %q, want the truncated-view refusal", err.Error())
	}
	// The refusal replaces the WARN: the fetch failed, so there is no accepted
	// catalogue to warn about a mismatch in.
	if got := recorder.CountExact("seadex catalogue count mismatch"); got != 0 {
		t.Errorf("count-mismatch WARN count = %d, want 0 on a failed fetch", got)
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
		fmt.Fprint(w, `{"totalItems":501,"totalPages":1,"items":[{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}},{"alID":2,"id":"rec000002","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	logger, _ := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want an error (totalItems 501 cannot fit 1 page of %d)", len(entries), perPage)
	}
	if !strings.Contains(err.Error(), "cannot fit the reported") {
		t.Errorf("error = %q, want inconsistent-totals context", err.Error())
	}
}

// pagedRecordingTransport serves a two-chunk catalogue (a full chunk, then a
// short one) and records the virtual time of each chunk request relative to the
// transport's start.
type pagedRecordingTransport struct {
	started time.Time
	times   []time.Duration
}

func (tr *pagedRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.times = append(tr.times, time.Since(tr.started))
	body := fmt.Sprintf(`{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
	if req.URL.Query().Get("filter") != "" {
		body = fmt.Sprintf(`{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, keysetRecords(perPage+1, 1))
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// TestFetchEntriesSleepsOnlyBetweenPages pins WHERE the politeness sleep
// lands, not just that cancellation during it aborts: chunk 1 must be fetched
// immediately (no delay before the first chunk of a cycle), and exactly one
// pageDelay must elapse before chunk 2. A guard drift that also sleeps before
// chunk 1 (or stops sleeping between chunks) shifts every cycle's first fetch
// by a full pageDelay without failing any existing test.
func TestFetchEntriesSleepsOnlyBetweenPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &pagedRecordingTransport{started: time.Now()}
		client := NewClient(&http.Client{Transport: tr}, "https://example.test", WithPageDelay(time.Minute))
		entries, err := client.FetchEntries(t.Context(), Options{})
		if err != nil {
			t.Fatalf("FetchEntries returned error: %v", err)
		}
		if len(entries) != perPage+1 {
			t.Fatalf("entries = %d, want %d", len(entries), perPage+1)
		}
		if len(tr.times) != 2 {
			t.Fatalf("requests = %d, want 2", len(tr.times))
		}
		if tr.times[0] != 0 {
			t.Errorf("chunk 1 fetched after %s of delay, want immediately (no politeness sleep before the first chunk)", tr.times[0])
		}
		if tr.times[1] != time.Minute {
			t.Errorf("chunk 2 fetched after %s, want exactly one pageDelay (1m0s)", tr.times[1])
		}
	})
}

// TestFetchEntriesCleanFetchEmitsNoWarnings pins the OFF state of the client's
// aggregate degradation gates (count mismatch, the window shortfall, the
// cross-fetch shrink signal): a fully healthy fetch with counts agreeing must
// emit none of the alert-stable WARN lines, so the Loki alerts keyed on them can
// never fire on a clean cycle. The
// tracker-link quality lines are internal/scout's (l-f156) and are pinned
// there.
func TestFetchEntriesCleanFetchEmitsNoWarnings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":1,"totalPages":1,"items":[{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","updated":"2026-01-02 03:04:05.000Z","expand":{"trs":[{"tracker":"Nyaa","url":"https://nyaa.si/view/1"}]}}]}`)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	for _, msg := range []string{
		"seadex catalogue count mismatch",
		"seadex change window delivered fewer entries than it reported selecting; this tick's freshness is incomplete and the next reconcile is the backstop",
		"seadex catalogue shrank against this process's previous fetch; upstream may be serving a truncated catalogue",
	} {
		if got := recorder.CountExact(msg); got != 0 {
			t.Errorf("clean fetch logged %q %d times, want 0", msg, got)
		}
	}
}

// TestFetchEntriesWarnsWhenCatalogueShrinksAgainstPreviousFetch pins the one
// completeness signal that is NOT self-attested: every other guard compares the
// collected count against the totalItems the same responses reported, so an
// upstream serving a truncated-but-self-consistent catalogue (a partially
// restored PocketBase, a poisoned CDN response) returns a clean success that
// would resolve every finding whose entry vanished. A client that already
// accepted a larger catalogue in this process must WARN on the shrink - and
// must still return the entries, since the strictness beyond a diagnostic is
// the operator's call.
func TestFetchEntriesWarnsWhenCatalogueShrinksAgainstPreviousFetch(t *testing.T) {
	entryCount := 4
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":1,"items":[%s]}`, entryCount, keysetRecords(1, entryCount))
	}))
	defer server.Close()

	logger, recorder := capture.New()
	client := NewClient(server.Client(), server.URL, WithLogger(logger))
	const msg = "seadex catalogue shrank against this process's previous fetch; upstream may be serving a truncated catalogue"

	if _, err := client.FetchEntries(t.Context(), Options{}); err != nil {
		t.Fatalf("first FetchEntries returned error: %v", err)
	}
	if got := recorder.CountExact(msg); got != 0 {
		t.Fatalf("first fetch logged the shrink WARN %d times, want 0 (no baseline yet)", got)
	}

	entryCount = 1
	entries, err := client.FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("second FetchEntries returned error: %v (a shrunken catalogue is a diagnostic, not a refusal)", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (the shrunken catalogue is still returned)", len(entries))
	}
	if got := recorder.CountExact(msg); got != 1 {
		t.Fatalf("shrink WARN count = %d, want 1", got)
	}
	if got := recorder.CountExact("seadex catalogue count mismatch"); got != 0 {
		t.Errorf("self-consistent shrunken catalogue logged the count mismatch %d times, want 0 "+
			"(the shrink signal exists precisely because the reported total agrees)", got)
	}
	var gotCount, gotPrev int64
	for _, r := range recorder.Records() {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "got":
				gotCount = a.Value.Int64()
			case "previous":
				gotPrev = a.Value.Int64()
			}
			return true
		})
	}
	if gotCount != 1 || gotPrev != 4 {
		t.Errorf("shrink WARN carries got=%d previous=%d, want got=1 previous=4", gotCount, gotPrev)
	}

	// The baseline adopts the shrunken count, so a legitimate upstream shrink
	// warns once instead of latching on every later cycle.
	if _, err := client.FetchEntries(t.Context(), Options{}); err != nil {
		t.Fatalf("third FetchEntries returned error: %v", err)
	}
	if got := recorder.CountExact(msg); got != 1 {
		t.Errorf("shrink WARN count after a stable third fetch = %d, want 1 (the baseline must adopt the new count)", got)
	}
}

// TestFetchEntriesExactlyFullChunkCompletesOnEmptyFollowUp pins the keyset
// walk's boundary at an exactly-full catalogue: a collection holding exactly
// perPage records cannot be recognized as complete from the chunk itself (a
// full chunk always continues), so the walk asks once more and the EMPTY
// follow-up chunk completes it cleanly - the reported totalItems is already
// satisfied, so no truncated-view error and no count-mismatch WARN fire.
func TestFetchEntriesExactlyFullChunkCompletesOnEmptyFollowUp(t *testing.T) {
	var reqs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		if r.URL.Query().Get("filter") == "" {
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":1,"items":[%s]}`, perPage, fullKeysetChunk(1))
			return
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":1,"items":[]}`, perPage)
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (an exactly-full catalogue must complete on the empty follow-up chunk)", err)
	}
	if len(entries) != perPage {
		t.Fatalf("entries = %d, want %d", len(entries), perPage)
	}
	if reqs != 2 {
		t.Errorf("requests = %d, want 2 (the full chunk plus the empty follow-up)", reqs)
	}
	if got := recorder.CountExact("seadex catalogue count mismatch"); got != 0 {
		t.Errorf("count-mismatch WARN count = %d, want 0 (the reported total is satisfied)", got)
	}
}

// TestFetchEntriesRetainsReportedPagesAcrossChunks pins the retained-highest
// rule for the reported PAGE count (fetchTotals.reportedPages is never
// overwritten downward), the page twin of the totalItems retention
// TestFetchEntriesErrorsOnMetadataRegression pins. The terminal chunk here
// reports totalItems but OMITS totalPages (which decodes as zero), so a
// counter that overwrote instead of retaining would make finishFetch's
// metadata self-consistency guard read "totalItems 501 cannot fit the reported
// 0 pages" and hard-fail an otherwise healthy two-chunk walk - every cycle,
// against an upstream that merely omits the field.
func TestFetchEntriesRetainsReportedPagesAcrossChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "" {
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		// The terminal chunk reports the item total but omits totalPages.
		fmt.Fprintf(w, `{"totalItems":%d,"items":[%s]}`, perPage+1, keysetRecords(perPage+1, 1))
	}))
	defer server.Close()

	logger, recorder := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v (a chunk omitting totalPages must not invalidate the retained page count)", err)
	}
	if len(entries) != perPage+1 {
		t.Fatalf("entries = %d, want %d", len(entries), perPage+1)
	}
	if got := recorder.CountExact("seadex catalogue count mismatch"); got != 0 {
		t.Errorf("count-mismatch WARN count = %d, want 0 (the reported total is satisfied)", got)
	}
}

// TestFinishFetchWarnsWhenBudgetMostlySpent pins the cumulative-budget
// advance-notice WARN and its inclusive threshold: consuming at least
// budgetWarnNumerator/budgetWarnDenominator of EITHER cumulative budget logs
// the single operator-facing line (raise the cap before ordinary catalogue
// growth turns every cycle into a hard degradation), while a fetch below both
// thresholds stays silent.
func TestFinishFetchWarnsWhenBudgetMostlySpent(t *testing.T) {
	thresholdBytes := maxTotalBytes * budgetWarnNumerator / budgetWarnDenominator
	thresholdElements := maxTotalElements * budgetWarnNumerator / budgetWarnDenominator
	tests := []struct {
		name     string
		bytes    int
		elements int
		want     int
	}{
		{name: "below both thresholds", bytes: thresholdBytes - 1, elements: thresholdElements - 1, want: 0},
		{name: "byte threshold reached", bytes: thresholdBytes, want: 1},
		{name: "element threshold reached", elements: thresholdElements, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			client := NewClient(nil, "", WithLogger(logger))
			entries, err := client.finishFetch([]seadex.Entry{{AniListID: 1}}, fetchTotals{
				bytes:         tc.bytes,
				elements:      tc.elements,
				reportedTotal: 1,
				reportedPages: 1,
			}, FetchFull)
			if err != nil {
				t.Fatalf("finishFetch returned error: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("finishFetch returned %d entries, want 1", len(entries))
			}
			const msg = "seadex fetch budget mostly spent; raise the caps before the catalogue outgrows them"
			if got := recorder.CountExact(msg); got != tc.want {
				t.Errorf("budget WARN count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFetchEntriesRejectsBrokenEntryIdentities pins the catalogue's
// primary-key invariant (validatePageIdentities): the byte/element/count
// budgets prove a chunk is well-shaped and the pagination arithmetic proves
// the counts add up, but neither notices KEY loss - an entry omitting alID
// decodes it as 0 (which the matcher would read as an ordinary unmapped
// entry) and a repeated alID can stand in for a record that was dropped, both
// while the aggregate counts still agree. Either shape must fail the whole
// fetch with a nil slice so the caller preserves its last known findings and
// feed instead of resolving them against a catalogue that lost an anime.
func TestFetchEntriesRejectsBrokenEntryIdentities(t *testing.T) {
	tests := []struct {
		name  string
		items string
		want  string
	}{
		{
			name:  "omitted alID",
			items: `{"expand":{"trs":[]}},{"alID":2,"id":"rec000002","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}`,
			want:  "page 1 item 1 has invalid AniList ID 0",
		},
		{
			name:  "zero alID",
			items: `{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}},{"alID":0,"expand":{"trs":[]}}`,
			want:  "page 1 item 2 has invalid AniList ID 0",
		},
		{
			name:  "negative alID",
			items: `{"alID":-7,"expand":{"trs":[]}}`,
			want:  "page 1 item 1 has invalid AniList ID -7",
		},
		{
			name:  "duplicate alID on one page",
			items: `{"alID":9,"expand":{"trs":[]}},{"alID":9,"expand":{"trs":[]}}`,
			want:  "page 1 repeats AniList ID 9",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"totalItems":2,"totalPages":1,"items":[%s]}`, tc.items)
			}))
			defer server.Close()

			logger, _ := capture.New()
			entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
			if err == nil {
				t.Fatalf("FetchEntries = %d entries, want an entry-identity error", len(entries))
			}
			if entries != nil {
				t.Fatalf("entries = %d items, want nil on an identity error", len(entries))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestFetchEntriesRejectsDuplicateIdentityAcrossPages pins the CROSS-page half
// of the identity invariant: the seen-id set spans the whole walk, so a second
// chunk repeating an id the first chunk already carried fails the fetch even
// though neither chunk repeats an id on its own.
func TestFetchEntriesRejectsDuplicateIdentityAcrossPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "" {
			// A FULL first chunk (ids 1..perPage) keeps the keyset walk going.
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, keysetRecords(1, 1))
	}))
	defer server.Close()

	logger, _ := capture.New()
	entries, err := NewClient(server.Client(), server.URL, WithLogger(logger)).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want a cross-page duplicate-identity error", len(entries))
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on an identity error", len(entries))
	}
	if !strings.Contains(err.Error(), "page 2 repeats AniList ID 1") {
		t.Errorf("error = %q, want cross-page duplicate-identity context", err.Error())
	}
}

// TestFetchEntriesUnusableCursorAborts pins the FETCH-level consequence of an
// unusable keyset cursor: advanceCursor's own table proves it refuses a
// missing, unsafe, or non-advancing (created, id) pair, but nothing proved
// FetchEntries PROPAGATES that refusal. A full chunk whose last record omits
// id must abort the walk with a nil slice on the first request - never
// re-request the same chunk, and never complete against the prefix it already
// holds.
func TestFetchEntriesUnusableCursorAborts(t *testing.T) {
	var reqs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs++
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s,%s]}`, perPage+1,
			keysetRecords(1, perPage-1),
			`{"alID":999999,"created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want an unusable-cursor error", len(entries))
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil (a truncated view must never reach the comparison)", len(entries))
	}
	if !strings.Contains(err.Error(), "carries no usable keyset cursor") {
		t.Errorf("error = %q, want the unusable-cursor refusal", err.Error())
	}
	if reqs != 1 {
		t.Errorf("requests = %d, want 1 (the walk aborts instead of re-requesting the same chunk)", reqs)
	}
}

// TestFetchEntriesRegressingCursorAborts pins the ordering premise the walk's
// completeness argument rests on: a short terminal chunk is read as exhaustion
// only because the filter asked for everything after the cursor under
// sort=created,id. Here the second chunk is short, carries unique positive
// AniList IDs, and agrees with the reported total - every count, identity and
// shortfall guard passes - but its keyset pair sorts BEFORE the position the
// first chunk established, so the records after that position were never
// delivered. The walk must refuse it rather than return a count-complete but
// key-incomplete catalogue that falsely resolves existing findings.
func TestFetchEntriesRegressingCursorAborts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "" {
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		// A short chunk whose (created, id) pair regresses behind the first
		// chunk's last record (rec000500): earlier created, unique alID.
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1,
			`{"alID":999999,"id":"rec000001","created":"2026-01-01 00:00:00.000Z","expand":{"trs":[]}}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want a non-advancing-cursor error", len(entries))
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil (a truncated view must never reach the comparison)", len(entries))
	}
	if !strings.Contains(err.Error(), "keyset cursor did not advance past") {
		t.Errorf("error = %q, want the non-advancing-cursor refusal", err.Error())
	}
}

// TestFetchEntriesDisorderedShortChunkAborts pins the same validation on a
// SINGLE short chunk - the terminal-chunk case the walk used to skip entirely,
// because the cursor was only checked when another request would be issued. The
// chunk's own records are out of sort order, so the response is not the sorted
// suffix that was requested and its shortness proves nothing about exhaustion.
func TestFetchEntriesDisorderedShortChunkAborts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":2,"totalPages":1,"items":[`+
			`{"alID":2,"id":"rec000002","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}},`+
			`{"alID":1,"id":"rec000001","created":"2026-01-02 03:04:05.000Z","expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL).FetchEntries(t.Context(), Options{})
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want a disordered-chunk error", len(entries))
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil (an unordered chunk proves no exhaustion)", len(entries))
	}
	if !strings.Contains(err.Error(), "keyset cursor did not advance past") {
		t.Errorf("error = %q, want the non-advancing-cursor refusal", err.Error())
	}
}
