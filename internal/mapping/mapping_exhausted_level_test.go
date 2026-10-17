package mapping

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestRefreshExhaustedTerminalRecordIsDemoted pins the mapping door's
// terminal log level. httpx's retry loop publishes its own generic "retries
// exhausted" verdict, and the caller republishes the SAME event with strictly
// more context - scout.loadMapping's "mapping degraded" carries
// usable_records, the stale-cache reason and the persisted rejection streak -
// so httpx's verdict sits at Debug rather than putting two warnings in Loki
// for one mapping outage. Demoted rather than dropped (WithLogger stays)
// because the per-attempt retry diagnostics are the half worth keeping.
func TestRefreshExhaustedTerminalRecordIsDemoted(t *testing.T) {
	rel := newRelease(t, testTag, nil)
	rel.edit(func(r *release) {
		r.latest = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	})
	logger, rec := capture.New()
	l := NewLoader(rel.srv.Client(), rel.source(), WithRefresh(time.Hour), WithLogger(logger))
	if _, err := l.refreshCache(t.Context(), &Cache{}); err == nil {
		t.Fatal("refreshCache against a permanently-503 upstream = nil error, want the exhausted error")
	}
	if n := rec.CountLevel(slog.LevelWarn, "retries exhausted"); n != 0 {
		t.Errorf("httpx terminal line logged at WARN %d times, want 0 (demoted to Debug): %v", n, rec.Messages())
	}
	if !rec.Contains("retries exhausted") {
		t.Errorf("httpx terminal line missing entirely, want it kept at Debug: %v", rec.Messages())
	}
	if n := rec.CountLevel(slog.LevelDebug, "failed, retrying"); n == 0 {
		t.Errorf("no per-attempt retry Debug records, want at least one (WithLogger must not be dropped): %v", rec.Messages())
	}
	if got := len(rel.paths()); got != maxAttempts {
		t.Errorf("latest-release requests = %d, want %d: a 503 is retried", got, maxAttempts)
	}
}
