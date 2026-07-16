package seadex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchEntriesEntryCapErrors pins the memory bound the page cap cannot
// cover: a misbehaving upstream that stuffs far more than perPage items into
// each page must trip the total-entry cap (maxEntries) with an error and a nil
// slice, instead of accumulating unbounded memory across pages.
func TestFetchEntriesEntryCapErrors(t *testing.T) {
	item := `{"alID":1,"expand":{"trs":[]}}`
	var b strings.Builder
	b.WriteString(`{"totalPages":2,"items":[`)
	for i := 0; i <= maxEntries; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(item)
	}
	b.WriteString(`]}`)
	page := b.String()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background())
	if err == nil {
		t.Fatal("FetchEntries returned nil error, want entry-cap error")
	}
	if entries != nil {
		t.Fatalf("entries = %d items, want nil on cap error", len(entries))
	}
	if !strings.Contains(err.Error(), "exceeded cap") {
		t.Errorf("error = %q, want entry-cap context", err.Error())
	}
}

// TestFetchEntriesByteCapErrors pins the cumulative-byte bound the entry cap
// cannot cover: pages holding FEW but HUGE items (far under the entry-count
// cap, each page under the per-page byte cap) must trip the total-byte cap
// (maxTotalBytes) with an error and a nil slice, instead of accumulating up to
// maxPages*maxPageBytes of memory. The bulk rides an unknown JSON field so the
// test itself stays cheap on retained memory while len(body) is what counts.
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
