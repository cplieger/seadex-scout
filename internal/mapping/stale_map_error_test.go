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
		msg:     "refresh returned 1 records, less than half of previous 4",
		age:     time.Hour,
		records: 4,
	}
	wantNoCause := "mapping: refresh returned 1 records, less than half of previous 4, using stale map (4 records, fetched 1h0m0s ago)"
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
func TestStaleMapError_LogAttrs(t *testing.T) {
	e := &StaleMapError{msg: "refresh failed", age: 90 * time.Second, records: 7, rejections: 2}
	got := e.LogAttrs()
	want := []any{"stale_reason", "refresh failed", "stale_age_seconds", 90.0, "stale_records", 7, "stale_consecutive_rejections", 2}
	if len(got) != len(want) {
		t.Fatalf("LogAttrs() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LogAttrs()[%d] = %v (%T), want %v (%T)", i, got[i], got[i], want[i], want[i])
		}
	}
}
