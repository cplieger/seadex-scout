package seadex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
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
			t.Fatalf("unexpected request for page %d after empty intermediate page", page)
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
