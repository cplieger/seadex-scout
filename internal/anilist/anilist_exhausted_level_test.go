package anilist

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// TestRequestExhaustedTerminalRecordIsDemoted pins the AniList door's terminal
// log level (h-f21) on BOTH paths through the shared request: Fetch (per-id) and
// FetchMany (batch). httpx's retry loop publishes its own generic "retries
// exhausted" verdict, and the matcher republishes the SAME event with strictly
// more context - "anilist batch prefetch failed; skipping per-id fallback for
// pending ids" for a total batch outage, "anilist fallback failed" (with al_id)
// for a per-id miss, and scout's sustained-outage ERROR carrying
// consecutive_anilist_degraded. Leaving both at Warn put two warnings in Loki
// for one AniList outage, the less informative one first, so httpx's verdict is
// demoted to Debug. The option sits on request, which is shared, so the demotion
// is deliberately wider than the single call path the finding named - correct,
// because both paths already publish their own contextual record. It is demoted
// rather than dropped (WithLogger stays) because the per-attempt retry
// diagnostics are the half worth keeping.
func TestRequestExhaustedTerminalRecordIsDemoted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cases := map[string]func(t *testing.T, c *Client){
		"Fetch": func(t *testing.T, c *Client) {
			if _, err := c.Fetch(t.Context(), 154587); err == nil {
				t.Fatal("Fetch against a permanently-503 upstream = nil error, want the exhausted error")
			}
		},
		"FetchMany": func(t *testing.T, c *Client) {
			if _, err := c.FetchMany(t.Context(), []int{154587, 21091}); err == nil {
				t.Fatal("FetchMany against a permanently-503 upstream = nil error, want the exhausted error")
			}
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			logger, rec := capture.New()
			c := NewClient(srv.Client(), srv.URL, 100000, logger)
			call(t, c)

			// The library's generic terminal verdict must not be a WARN:
			// exactly one layer warns, and it is the one with the app context.
			if n := rec.CountLevel(slog.LevelWarn, "retries exhausted"); n != 0 {
				t.Errorf("httpx terminal line logged at WARN %d times, want 0 (demoted to Debug): %v", n, rec.Messages())
			}
			// Demoted, not suppressed: the diagnosis stays available at Debug.
			if !rec.Contains("retries exhausted") {
				t.Errorf("httpx terminal line missing entirely, want it kept at Debug: %v", rec.Messages())
			}
			// Regression guard for the reason the logger is kept: the
			// per-attempt retry diagnostics must survive the demotion.
			if n := rec.CountLevel(slog.LevelDebug, "failed, retrying"); n == 0 {
				t.Errorf("no per-attempt retry Debug records, want at least one (WithLogger must not be dropped): %v", rec.Messages())
			}
		})
	}
}
