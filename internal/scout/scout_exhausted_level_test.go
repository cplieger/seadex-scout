package scout

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// exhaustingMapLoader returns a Fribb loader whose upstream answers 503 to every
// attempt, so the refresh runs the retry budget to exhaustion (as opposed to
// unreachableMapLoader's transport refusal, which is terminal on the first
// attempt and never reaches httpx's exhausted verdict).
func exhaustingMapLoader(t *testing.T, logger *slog.Logger) *mapping.Loader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return mapping.NewLoader(srv.Client(), srv.URL, filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger)
}

// staleMappingState is a persisted cache holding usable records fetched beyond
// the loader's refresh window, so a failing refresh degrades to the stale map
// (a *mapping.StaleMapError) instead of returning an unusable one - the shape
// both mapping-degraded log sites are defined against.
func staleMappingState() state.State {
	return state.State{
		Mapping: mapping.Cache{
			FetchedAt: time.Now().Add(-2 * time.Hour),
			Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
		},
		Baselined: true,
	}
}

// TestMappingExhaustionWarnsOnceWithAppContext pins the OTHER half of the Fribb
// door's terminal-level demotion (l-f101): the generic httpx verdict is gone
// from WARN, and the contextual caller record that replaces it is still there.
// Both log sites are covered because the demotion is on the shared request
// option and therefore affects the daemon and the one-shot report alike: the
// cycle publishes "mapping degraded" (escalating on a persisted rejection
// streak, which a stale-but-usable refresh does not trip) and report mode
// publishes "report: mapping degraded". Asserting the absence at WARN
// specifically, not the absence of the record - it survives at Debug, which
// internal/mapping's own test pins.
func TestMappingExhaustionWarnsOnceWithAppContext(t *testing.T) {
	cases := map[string]struct {
		load    func(t *testing.T, s *Scout, st *state.State)
		wantMsg string
	}{
		"cycle": {
			load: func(t *testing.T, s *Scout, st *state.State) {
				t.Helper()
				if _, _, err := s.loadMapping(t.Context(), st); err == nil {
					t.Fatal("loadMapping against a permanently-503 Fribb upstream = nil error, want a degraded error")
				}
			},
			wantMsg: "mapping degraded",
		},
		"report": {
			load: func(t *testing.T, s *Scout, st *state.State) {
				t.Helper()
				if _, err := s.reportMapping(t.Context(), st); err != nil {
					t.Fatalf("reportMapping on a stale-but-usable map = %v, want nil (degraded, not failed)", err)
				}
			},
			wantMsg: "report: mapping degraded",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			// One logger for both layers: the library's verdict and the app's
			// land in the same recorder, which is what an operator's Loki
			// stream sees.
			s := New(&Deps{Logger: logger, Mapping: exhaustingMapLoader(t, logger)})
			st := staleMappingState()
			tc.load(t, s, &st)

			if n := recorder.CountLevel(slog.LevelWarn, "retries exhausted"); n != 0 {
				t.Errorf("httpx generic terminal line logged at WARN %d times, want 0 (the app's record is the one that warns): %v", n, recorder.Messages())
			}
			if n := recorder.CountExact(tc.wantMsg); n != 1 {
				t.Errorf("%q count = %d, want 1 (the contextual record must still be published): %v", tc.wantMsg, n, recorder.Messages())
			}
			if _, ok := recordAttr(recorder, tc.wantMsg, "stale_reason"); !ok {
				t.Errorf("%q carries no stale_reason attribute; the context that justifies replacing the generic line is missing", tc.wantMsg)
			}
		})
	}
}
