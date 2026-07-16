package seadex

import (
	"testing"
	"time"
)

// TestParsePBTime pins the tolerant PocketBase timestamp parsing: both
// space-separated layouts (with and without fractional seconds) and RFC3339
// parse, while empty, whitespace, and garbage values fall to the zero time
// (which sorts oldest, so an unparseable record lands at the feed's tail
// instead of erroring the fetch).
func TestParsePBTime(t *testing.T) {
	tests := []struct {
		want time.Time
		name string
		in   string
	}{
		{name: "fractional space layout", in: "2026-01-02 03:04:05.000Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "whole-second space layout", in: "2026-01-02 03:04:05Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "rfc3339", in: "2026-01-02T03:04:05Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "surrounding whitespace trimmed", in: "  2026-01-02 03:04:05Z  ", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "empty is zero", in: "", want: time.Time{}},
		{name: "whitespace only is zero", in: "   ", want: time.Time{}},
		{name: "garbage is zero", in: "not a timestamp", want: time.Time{}},
		{name: "unsupported layout is zero", in: "02/01/2026 03:04", want: time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePBTime(tc.in); !got.Equal(tc.want) {
				t.Errorf("parsePBTime(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestPageComplete pins the pagination-completeness decision table, including
// the arm the HTTP-level tests never reach in-package: an empty FINAL page (or
// an empty FIRST page when the API reports zero total pages) completes cleanly,
// while an empty page before the reported total is a truncated-view error, and
// ANY page with invalid metadata (totalPages < 1 — the empty first page being
// the one exception — or a page, empty or not, past the reported total) errors
// rather than being accepted as a complete catalogue.
func TestPageComplete(t *testing.T) {
	tests := []struct {
		name       string
		page       int
		itemCount  int
		totalPages int
		wantDone   bool
		wantErr    bool
	}{
		{name: "mid page continues", page: 1, itemCount: 500, totalPages: 3, wantDone: false},
		{name: "final page with items completes", page: 3, itemCount: 12, totalPages: 3, wantDone: true},
		{name: "single page completes", page: 1, itemCount: 7, totalPages: 1, wantDone: true},
		{name: "empty final page completes", page: 1, itemCount: 0, totalPages: 1, wantDone: true},
		{name: "empty page with zero total completes", page: 1, itemCount: 0, totalPages: 0, wantDone: true},
		{name: "later empty page with zero total errors", page: 2, itemCount: 0, totalPages: 0, wantErr: true},
		{name: "later empty page with negative total errors", page: 2, itemCount: 0, totalPages: -1, wantErr: true},
		{name: "empty page before total errors", page: 2, itemCount: 0, totalPages: 3, wantErr: true},
		{name: "empty page past reported total errors", page: 3, itemCount: 0, totalPages: 2, wantErr: true},
		{name: "non-empty page with zero total errors", page: 1, itemCount: 500, totalPages: 0, wantErr: true},
		{name: "non-empty page with negative total errors", page: 1, itemCount: 500, totalPages: -1, wantErr: true},
		{name: "non-empty page past reported total errors", page: 4, itemCount: 5, totalPages: 3, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, err := pageComplete(tc.page, tc.itemCount, tc.totalPages)
			if (err != nil) != tc.wantErr {
				t.Fatalf("pageComplete(%d, %d, %d) error = %v, wantErr %v", tc.page, tc.itemCount, tc.totalPages, err, tc.wantErr)
			}
			if err == nil && done != tc.wantDone {
				t.Errorf("pageComplete(%d, %d, %d) done = %v, want %v", tc.page, tc.itemCount, tc.totalPages, done, tc.wantDone)
			}
		})
	}
}

// TestEntryHasTheoreticalBest pins the theoretical-best predicate both
// consumers branch on (compare's theoretical_best info finding and audit's
// theoretical qualifier): a named theoretical best reports true, empty false.
func TestEntryHasTheoreticalBest(t *testing.T) {
	if (&Entry{}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = true for empty TheoreticalBest, want false")
	}
	if !(&Entry{TheoreticalBest: "a stated remux"}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = false with TheoreticalBest set, want true")
	}
}
