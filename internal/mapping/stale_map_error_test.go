package mapping

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestStaleMapError_ErrorMessage pins the documented message-text contract of
// both Error() branches: with a wrapped cause the message carries the cause
// text, and the shrunk-refresh guard's cause-less form omits the trailing
// colon-cause segment. The message shape is a pinned log contract (see the
// Error doc comment), so log content depends on this exact shape.
func TestStaleMapError_ErrorMessage(t *testing.T) {
	withCause := &StaleMapError{
		cause:   errors.New("boom"),
		msg:     "refresh failed",
		age:     90 * time.Second,
		records: 3,
	}
	want := "mapping: refresh failed, using stale map (3 records, fetched 1m30s ago): boom"
	if got := withCause.Error(); got != want {
		t.Errorf("Error() with cause = %q, want %q", got, want)
	}

	noCause := &StaleMapError{
		msg:            "refresh shrank below half of previous",
		age:            time.Hour,
		records:        4,
		shrunkReturned: 1,
		shrunkPrevious: 4,
	}
	wantNoCause := "mapping: refresh shrank below half of previous (returned 1, previous 4), using stale map (4 records, fetched 1h0m0s ago)"
	if got := noCause.Error(); got != wantNoCause {
		t.Errorf("Error() without cause = %q, want %q", got, wantNoCause)
	}
	if strings.Contains(noCause.Error(), ": <nil>") {
		t.Errorf("Error() without cause leaked a nil cause: %q", noCause.Error())
	}
}

// TestStaleMapError_UnwrapExposesCause pins the errors.Is/As chain through the
// wrapped refresh failure: a caller can classify the underlying cause (e.g.
// context.DeadlineExceeded during shutdown) through the StaleMapError wrapper,
// and a cause-less shrink-guard error unwraps to nil.
func TestStaleMapError_UnwrapExposesCause(t *testing.T) {
	cause := context.DeadlineExceeded
	err := fmt.Errorf("load: %w", &StaleMapError{cause: cause, msg: "refresh failed"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("errors.Is through StaleMapError = false, want true (Unwrap must expose the cause)")
	}
	if got := (&StaleMapError{msg: "shrunk"}).Unwrap(); got != nil {
		t.Errorf("Unwrap() with no cause = %v, want nil", got)
	}
}

// TestStaleMapError_LogAttrs pins the structured degradation pairs the scout
// cycle appends to its degraded-cycle log line (scout.go consumes LogAttrs via
// errors.AsType): key order and value types must stay queryable in Loki.
//
// stale_consecutive_rejections is deliberately NOT here. The streak has one
// carrier, mapping.Cache.RejectedRefreshes, which is what the scout escalates on
// and appends itself; this type used to hold a second copy that read 0 for a
// transient fetch or parse failure, so a caller preferring these attrs could log
// a zero streak in the very line an escalation fired on a nonzero one.
func TestStaleMapError_LogAttrs(t *testing.T) {
	e := &StaleMapError{msg: "refresh failed", age: 90 * time.Second, records: 7}
	got := e.LogAttrs()
	want := []any{"stale_reason", "refresh failed", "stale_age_seconds", 90.0, "stale_records", 7}
	if len(got) != len(want) {
		t.Fatalf("LogAttrs() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LogAttrs()[%d] = %v (%T), want %v (%T)", i, got[i], got[i], want[i], want[i])
		}
	}
}

// TestStaleMapError_shrunkFormMessageAndLogAttrs pins the shrunk-refresh form
// of the pinned log contract: with shrunkPrevious set, Error() renders the
// counts as a parenthetical on the fixed reason (keeping stale_reason a
// fixed-cardinality Loki class), and LogAttrs appends the
// stale_returned/stale_previous structured pairs after the four base attrs.
func TestStaleMapError_shrunkFormMessageAndLogAttrs(t *testing.T) {
	e := &StaleMapError{
		msg:            "refresh shrank below half of previous",
		age:            90 * time.Second,
		records:        4,
		shrunkReturned: 1,
		shrunkPrevious: 4,
	}
	want := "mapping: refresh shrank below half of previous (returned 1, previous 4), using stale map (4 records, fetched 1m30s ago)"
	if got := e.Error(); got != want {
		t.Errorf("Error() shrunk form = %q, want %q", got, want)
	}
	got := e.LogAttrs()
	wantAttrs := []any{
		"stale_reason", "refresh shrank below half of previous",
		"stale_age_seconds", 90.0,
		"stale_records", 4,
		"stale_returned", 1,
		"stale_previous", 4,
	}
	if len(got) != len(wantAttrs) {
		t.Fatalf("LogAttrs() len = %d, want %d", len(got), len(wantAttrs))
	}
	for i := range wantAttrs {
		if got[i] != wantAttrs[i] {
			t.Errorf("LogAttrs()[%d] = %v (%T), want %v (%T)", i, got[i], got[i], wantAttrs[i], wantAttrs[i])
		}
	}
}

// TestStaleOrFail_recordsReportIndexedCount pins that stale_records is the size
// of the map consumers actually receive (buildIndex's deduplicated, positive-ID
// view), not the raw persisted row count. A cache written by a pre-deduplication
// version can be cacheUsable yet carry duplicate and non-positive rows, so the
// raw length would over-report against Index.Len() and scout's usable_records on
// the same degraded-cycle log line.
func TestStaleOrFail_recordsReportIndexedCount(t *testing.T) {
	prev := &Cache{
		FetchedAt: time.Now().Add(-90 * time.Second),
		Records: []Record{
			{AniListID: 1, Type: "TV", TvdbID: 100},
			{AniListID: 1, Type: "TV", TvdbID: 101},
			{AniListID: 0, Type: "TV", TvdbID: 102},
		},
	}
	next, err := staleOrFail(prev, "refresh failed", errors.New("boom"), errors.New("mapping: no cache"))
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("staleOrFail error = %v, want a *StaleMapError over a usable cache", err)
	}
	if got := buildIndex(next.Records).Len(); got != 1 {
		t.Fatalf("returned stale map indexes %d records, want 1", got)
	}
	if stale.records != 1 {
		t.Errorf("stale_records = %d, want 1 (the indexed size, not the %d persisted rows)", stale.records, len(prev.Records))
	}
	if !strings.Contains(stale.Error(), "(1 records,") {
		t.Errorf("StaleMapError text = %q, want the indexed record count", stale.Error())
	}
}
