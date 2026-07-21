package seadex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unsafe"
)

// repeatJSON joins n copies of one JSON element with commas, for building
// hostile-cardinality array bodies.
func repeatJSON(elem string, n int) string {
	elems := make([]string, n)
	for i := range elems {
		elems[i] = elem
	}
	return strings.Join(elems, ",")
}

// fetchHostilePage serves one fixed page body and asserts FetchEntries rejects
// it with a nil slice and an error carrying wantErr.
func fetchHostilePage(t *testing.T, page, wantErr string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatalf("FetchEntries returned nil error, want %q error", wantErr)
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on cap error", len(entries))
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("error = %q, want substring %q", err.Error(), wantErr)
	}
}

// TestFetchEntriesDecodeCardinalityCapsError pins the decode-layer allocation
// bounds json.Unmarshal could not provide: hostile array cardinality (a page
// stuffing more than the requested perPage items, or one item amplifying
// through its nested torrents/files/tags arrays) must be rejected DURING the
// bounded decode - before allocation scales with the hostile input - with an
// error and a nil slice. The "many tiny items" and "one item with an oversized
// nested files array" cases are the two amplification shapes the wire-size cap
// alone cannot stop.
func TestFetchEntriesDecodeCardinalityCapsError(t *testing.T) {
	tests := []struct {
		name    string
		page    string
		wantErr string
	}{
		{
			name: "many tiny items exceed perPage",
			page: `{"totalPages":1,"items":[` +
				repeatJSON(`{"alID":1,"expand":{"trs":[]}}`, perPage+1) + `]}`,
			wantErr: fmt.Sprintf("page items: jsonx/bounded: array cardinality cap exceeded: %d", perPage),
		},
		{
			name: "oversized torrents array in one item",
			page: `{"totalPages":1,"items":[{"alID":1,"expand":{"trs":[` +
				repeatJSON(`{}`, maxTorrentsPerEntry+1) + `]}}]}`,
			wantErr: fmt.Sprintf("torrents per entry: jsonx/bounded: array cardinality cap exceeded: %d", maxTorrentsPerEntry),
		},
		{
			name: "oversized nested files array in one torrent",
			page: `{"totalPages":1,"items":[{"alID":1,"expand":{"trs":[{"files":[` +
				repeatJSON(`{}`, maxFilesPerTorrent+1) + `]}]}}]}`,
			wantErr: fmt.Sprintf("files per torrent: jsonx/bounded: array cardinality cap exceeded: %d", maxFilesPerTorrent),
		},
		{
			name: "oversized tags array in one torrent",
			page: `{"totalPages":1,"items":[{"alID":1,"expand":{"trs":[{"tags":[` +
				repeatJSON(`""`, maxTagsPerTorrent+1) + `]}]}}]}`,
			wantErr: fmt.Sprintf("tags per torrent: jsonx/bounded: array cardinality cap exceeded: %d", maxTagsPerTorrent),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fetchHostilePage(t, tc.page, tc.wantErr)
		})
	}
}

// TestDecodePageElementBudgetErrors pins the aggregate element budget: the
// per-parent cardinality caps compose multiplicatively (perPage x torrents x
// files/tags), so a page of minimal elements staying under every per-parent
// cap could still decode into hundreds of MB. The decoder must abort once the
// TOTAL decoded array elements cross maxPageElements, before the remainder is
// materialized.
func TestDecodePageElementBudgetErrors(t *testing.T) {
	// 40 items x 512 torrents x 60 tags = 1+512+30720 elements per item,
	// 1,249,320 total: over the 250K budget while every per-parent cap holds.
	torrent := `{"tags":[` + repeatJSON(`""`, 60) + `]}`
	item := `{"alID":1,"expand":{"trs":[` + repeatJSON(torrent, 512) + `]}}`
	page := `{"totalPages":1,"items":[` + repeatJSON(item, 40) + `]}`

	_, _, err := decodePage([]byte(page), maxPageElements)
	if err == nil {
		t.Fatal("decodePage returned nil error, want element-budget error")
	}
	want := fmt.Sprintf("jsonx/bounded: element budget exceeded: %d", maxPageElements)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
}

// TestFetchEntriesCumulativeElementCapErrors pins the fetch-wide element
// budget the per-page cap cannot cover: pages each individually below
// maxPageElements (and far under the cumulative byte cap) still accumulate
// retained decoded entries across the whole fetch, so their combined element
// count must trip maxTotalElements with errCumulativeElements and a nil slice
// - before the excess page's elements are materialized - instead of amplifying
// dozens of compact pages into decoded slice backing arrays that OOM-kill the
// deployment container.
func TestFetchEntriesCumulativeElementCapErrors(t *testing.T) {
	// 10 items x (1 + 512 torrents + 512x40 tags) = 209,930 elements per
	// page: under the 250K per-page budget, over the 250K fetch-wide budget
	// on page 2. Each page is ~0.7 MB, so the byte caps never fire first.
	torrent := `{"tags":[` + repeatJSON(`""`, 40) + `]}`
	item := `{"alID":1,"expand":{"trs":[` + repeatJSON(torrent, 512) + `]}}`
	page := `{"totalPages":3,"items":[` + repeatJSON(item, 10) + `]}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if !errors.Is(err, errCumulativeElements) {
		t.Fatalf("FetchEntries error = %v, want errCumulativeElements", err)
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on cap error", len(entries))
	}
}

// TestFetchAndAppendEntryCapBeforeAppend pins the relocated total-entry guard:
// a page whose items would push the accumulated catalogue past maxEntries is
// rejected BEFORE any of its items are converted or appended, so the decoded
// page never amplifies into public Entry structs once the budget is spent.
func TestFetchAndAppendEntryCapBeforeAppend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalPages":1,"items":[{"alID":1,"expand":{"trs":[]}}]}`)
	}))
	defer server.Close()

	c := NewClient(server.Client(), server.URL, 0, nil)
	all := make([]Entry, maxEntries)
	var tot fetchTotals
	out, done, err := c.fetchAndAppend(context.Background(), 1, all, &tot)
	if err == nil {
		t.Fatal("fetchAndAppend returned nil error, want entry-cap error")
	}
	if done {
		t.Error("fetchAndAppend done = true, want false on cap error")
	}
	if len(out) != maxEntries {
		t.Errorf("out = %d entries, want the untouched %d (nothing appended past the cap)", len(out), maxEntries)
	}
	want := fmt.Sprintf("entry count exceeded cap %d", maxEntries)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
}

// TestFetchEntriesByteCapErrors pins the cumulative-byte bound the entry cap
// cannot cover: pages holding FEW but HUGE items (far under the entry-count
// cap, each page under the per-page byte cap) must trip the total-byte cap
// (maxTotalBytes) with an error and a nil slice, instead of accumulating up to
// maxPages*maxPageBytes of memory. The bulk rides an unknown JSON field so the
// test itself stays cheap on retained memory while len(body) is what counts.
// The budget now caps the wire read itself (fetchPage downloads at most the
// remaining allowance), so the over-budget page is rejected before decode -
// same observable contract, earlier enforcement.
func TestFetchEntriesByteCapErrors(t *testing.T) {
	// One page just under the per-page cap; the cumulative cap trips after
	// ceil(maxTotalBytes/pageSize) pages, well before maxPages.
	const padSize = 47 << 20
	page := `{"totalPages":200,"items":[{"alID":1,"expand":{"trs":[]}}],"pad":"` +
		strings.Repeat("x", padSize) + `"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want byte-cap error")
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on cap error", len(entries))
	}
	if !strings.Contains(err.Error(), "cumulative page bytes exceeded cap") {
		t.Errorf("error = %q, want cumulative-byte-cap context", err.Error())
	}
}

// TestFetchAndAppendExhaustedByteBudgetErrors pins the pre-fetch budget gate:
// once the cumulative byte budget is spent (tot.bytes == maxTotalBytes), the
// next fetchAndAppend must return errCumulativeBytes WITHOUT dialing the
// upstream (the base URL here is unroutable, so any request attempt fails the
// test with a different error) and without touching the accumulated entries.
func TestFetchAndAppendExhaustedByteBudgetErrors(t *testing.T) {
	c := NewClient(&http.Client{}, "http://unreachable.invalid", 0, nil)
	tot := fetchTotals{bytes: maxTotalBytes}
	all := []Entry{{AniListID: 1}}

	out, done, err := c.fetchAndAppend(context.Background(), 3, all, &tot)
	if !errors.Is(err, errCumulativeBytes) {
		t.Fatalf("fetchAndAppend error = %v, want errCumulativeBytes without any upstream request", err)
	}
	if done {
		t.Error("fetchAndAppend done = true, want false on exhausted budget")
	}
	if len(out) != 1 || out[0].AniListID != 1 {
		t.Errorf("out = %+v, want the accumulated entries untouched", out)
	}
}

// TestFetchAndAppendExhaustedElementBudgetErrors pins the pre-fetch element
// budget gate, the element twin of the byte-budget test: once the cumulative
// decoded-element budget is spent (tot.elements == maxTotalElements), the next
// fetchAndAppend must return errCumulativeElements WITHOUT dialing the
// upstream (the base URL here is unroutable, so any request attempt fails the
// test with a different error) and without touching the accumulated entries.
func TestFetchAndAppendExhaustedElementBudgetErrors(t *testing.T) {
	c := NewClient(&http.Client{}, "http://unreachable.invalid", 0, nil)
	tot := fetchTotals{elements: maxTotalElements}
	all := []Entry{{AniListID: 1}}

	out, done, err := c.fetchAndAppend(context.Background(), 3, all, &tot)
	if !errors.Is(err, errCumulativeElements) {
		t.Fatalf("fetchAndAppend error = %v, want errCumulativeElements without any upstream request", err)
	}
	if done {
		t.Error("fetchAndAppend done = true, want false on exhausted budget")
	}
	if len(out) != 1 || out[0].AniListID != 1 {
		t.Errorf("out = %+v, want the accumulated entries untouched", out)
	}
}

// TestFetchEntriesPerPageByteCapErrors pins the per-page arm of the wire-size
// guard's classification: a SINGLE page whose body exceeds the full per-page
// bound (maxPageBytes) is a per-page violation and must surface as the
// page-level fetch error naming page 1 - never as errCumulativeBytes, whose
// alert-stable "cumulative page bytes exceeded cap" message would
// misattribute a one-page anomaly to fetch-wide budget exhaustion.
func TestFetchEntriesPerPageByteCapErrors(t *testing.T) {
	// The pad pushes the body past maxPageBytes; it rides an unknown JSON
	// field so only len(body) matters (it is never decoded).
	page := `{"totalPages":1,"items":[{"alID":1,"expand":{"trs":[]}}],"pad":"` +
		strings.Repeat("x", maxPageBytes) + `"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want per-page byte-cap error")
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on cap error", len(entries))
	}
	if errors.Is(err, errCumulativeBytes) {
		t.Errorf("error = %v, want the per-page violation NOT classified as the cumulative-byte cap", err)
	}
	if !strings.Contains(err.Error(), "fetch page 1") {
		t.Errorf("error = %q, want it to name the failed page 1", err.Error())
	}
}

// TestFetchEntriesPerPageElementCapErrors pins the per-page arm of the decode
// element budget's classification: a SINGLE page tripping the full per-page
// element bound (maxPageElements) is a per-page violation and must surface as
// the page-level decode error naming page 1 - never as errCumulativeElements,
// whose alert-stable message would misattribute a one-page anomaly to
// fetch-wide budget exhaustion.
func TestFetchEntriesPerPageElementCapErrors(t *testing.T) {
	// Same shape as TestDecodePageElementBudgetErrors: ~1.25M aggregate
	// elements while every per-parent cap holds, served over HTTP so the
	// classification in fetchPage runs with the FULL per-page element limit.
	torrent := `{"tags":[` + repeatJSON(`""`, 60) + `]}`
	item := `{"alID":1,"expand":{"trs":[` + repeatJSON(torrent, 512) + `]}}`
	page := `{"totalPages":1,"items":[` + repeatJSON(item, 40) + `]}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want per-page element-budget error")
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on cap error", len(entries))
	}
	if errors.Is(err, errCumulativeElements) {
		t.Errorf("error = %v, want the per-page violation NOT classified as the cumulative-element cap", err)
	}
	if !strings.Contains(err.Error(), "fetch page 1") {
		t.Errorf("error = %q, want it to name the failed page 1", err.Error())
	}
	want := fmt.Sprintf("jsonx/bounded: element budget exceeded: %d", maxPageElements)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
}

// TestSeadexWorkingSetBudget pins the JOINT sizing of the fetch-wide budgets
// against the deployment container: the caps are independently admissible, so
// their maxima can occur in the same fetch, and an admitted catalogue may
// simultaneously retain maxTotalBytes of decoded string content, the raw page
// fetchPage still holds (maxPageBytes), and maxTotalElements of element
// structs. That conservative working set must stay under a 192 MiB ceiling so
// the guards fire (clean degradation) with at least 64 MiB of the 256 MiB
// container left for slice spare capacity, decoder buffers, the loaded
// state/mapping/library snapshots, and the Go runtime — instead of the kernel
// OOM-killing the process.
func TestSeadexWorkingSetBudget(t *testing.T) {
	const ceiling = 192 << 20 // 256 MiB container minus 64 MiB headroom
	workingSet := maxTotalBytes + maxPageBytes + maxTotalElements*int(unsafe.Sizeof(Torrent{}))
	if workingSet >= ceiling {
		t.Errorf("conservative SeaDex working set = %d bytes (%d MiB), want under the %d MiB ceiling; "+
			"resize maxTotalBytes/maxPageBytes/maxTotalElements jointly",
			workingSet, workingSet>>20, ceiling>>20)
	}
}
