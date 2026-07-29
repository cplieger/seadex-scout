package match

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// TestAniListExhaustionWarnsOnceWithAppContext pins the OTHER half of the
// AniList door's terminal-level demotion (h-f21) across the real client: the
// generic httpx verdict is gone from WARN, and the contextual matcher record
// that replaces it is still published. This is the cross-layer assertion the
// package-local tests cannot make - the sibling tests here
// (TestMatchTotalOutageLogsSingleWarn, TestMatchTransientFailuresLogWarn) pin
// the matcher's records against fakes with no httpx in the stack, and
// internal/anilist's TestRequestExhaustedTerminalRecordIsDemoted pins the level
// on both request paths with no matcher above it. A total batch outage is the
// shape reachable through one permanently-failing upstream (the per-id fallback
// is deliberately skipped in that case, which is what makes this ONE warning).
func TestAniListExhaustionWarnsOnceWithAppContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// One logger for both layers: the library's verdict and the app's land in
	// the same recorder, which is what an operator's Loki stream sees.
	logger, recorder := capture.New()
	client := anilist.NewClient(srv.Client(), srv.URL, 100000, logger)

	res := NewMatcher(client, logger).Match(t.Context(),
		[]seadex.Entry{{AniListID: 41}, {AniListID: 42}}, &library.Snapshot{}, mapping.NewIndex(nil), Memo{})

	if !res.Degraded {
		t.Error("Degraded = false, want true on a total AniList outage")
	}
	if n := recorder.CountLevel(slog.LevelWarn, "retries exhausted"); n != 0 {
		t.Errorf("httpx generic terminal line logged at WARN %d times, want 0 (the app's record is the one that warns): %v", n, recorder.Messages())
	}
	if n := recorder.CountExact("anilist batch prefetch failed; skipping per-id fallback for pending ids"); n != 1 {
		t.Errorf("contextual total-outage WARN count = %d, want 1: %v", n, recorder.Messages())
	}
	// Demoted, not suppressed: the diagnosis stays available at Debug.
	if !recorder.Contains("retries exhausted") {
		t.Errorf("httpx terminal line missing entirely, want it kept at Debug: %v", recorder.Messages())
	}
}
