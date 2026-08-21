package mapping

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestConditionalGetExhaustedTerminalRecordIsDemoted pins the Fribb door's
// terminal log level (l-f101). httpx's retry loop publishes its own generic
// "http retries exhausted" verdict, and the caller republishes the SAME event
// with strictly more context - scout.loadMapping's "mapping degraded" carries
// usable_records, the stale-cache reason and the persisted rejection streak (and
// escalates to ERROR on a sustained streak), and report mode publishes the same
// attribute set as "report: mapping degraded". Leaving both at Warn put two
// warnings in Loki for one Fribb outage, the less informative one first, so
// httpx's verdict is demoted to Debug. It is demoted rather than dropped
// (WithLogger stays) because the per-attempt retry diagnostics are the half
// worth keeping - the same rule internal/seadex and internal/indexer's Prowlarr
// door already apply.
func TestConditionalGetExhaustedTerminalRecordIsDemoted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	logger, rec := capture.New()
	l := NewLoader(srv.Client(), srv.URL, WithRefresh(time.Hour), WithLogger(logger))

	if _, err := l.refreshCache(t.Context(), &Cache{}); err == nil {
		t.Fatal("refreshCache against a permanently-503 upstream = nil error, want the exhausted error")
	}

	// The library's generic terminal verdict must not be a WARN: exactly one
	// layer warns for this outage, and it is the one with the app context.
	if n := rec.CountLevel(slog.LevelWarn, "retries exhausted"); n != 0 {
		t.Errorf("httpx terminal line logged at WARN %d times, want 0 (demoted to Debug): %v", n, rec.Messages())
	}
	// Demoted, not suppressed: the diagnosis stays available at Debug.
	if !rec.Contains("retries exhausted") {
		t.Errorf("httpx terminal line missing entirely, want it kept at Debug: %v", rec.Messages())
	}
	// Regression guard for the reason the logger is kept: the per-attempt retry
	// diagnostics must survive the demotion.
	if n := rec.CountLevel(slog.LevelDebug, "failed, retrying"); n == 0 {
		t.Errorf("no per-attempt retry Debug records, want at least one (WithLogger must not be dropped): %v", rec.Messages())
	}
}
