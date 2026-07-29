package mapping

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// absentArrMessage is the fixed ERROR message the accept-without-baseline
// diagnostic emits. It is a Loki-queryable log contract (the class is the
// message, the facts ride as structured fields), so the tests pin it verbatim.
const absentArrMessage = "mapping: accepted a refresh with no records for an arr; " +
	"that arr will match nothing this cycle, pin the affected entries in overrides.json"

const (
	seriesOnlyBody = `[{"anilist_id":1,"type":"tv","tvdb_id":100}]`
	movieOnlyBody  = `[{"anilist_id":2,"type":"MOVIE","themoviedb_id":{"movie":[7]}}]`
	bothArrsBody   = `[{"anilist_id":1,"type":"tv","tvdb_id":100},` +
		`{"anilist_id":2,"type":"MOVIE","themoviedb_id":{"movie":[7]}}]`
)

// routingBodyServer serves body as a plain 200 with no validators, so every
// refresh in this file takes the acceptRefresh path rather than a 304.
func routingBodyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestAbsentRoutingClasses pins the pure helper: it reports which routing
// classes a body carries no RESOLVABLE record for, in a stable order, and
// nothing else - no threshold, and no other population.
func TestAbsentRoutingClasses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []Record
		want    []string
	}{
		{"series only", []Record{{AniListID: 1, Type: "TV", TvdbID: 100}}, []string{"movie-routed"}},
		{"movie only", []Record{{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{7}}}, []string{"series-routed"}},
		{"both present", []Record{
			{AniListID: 1, Type: "TV", TvdbID: 100},
			{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{7}},
		}, nil},
		{"empty body", nil, []string{"movie-routed", "series-routed"}},
		{"type labels without resolvable ids", []Record{
			{AniListID: 1, Type: "TV"},
			{AniListID: 2, Type: "MOVIE"},
		}, []string{"movie-routed", "series-routed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := absentRoutingClasses(tc.records); !slices.Equal(got, tc.want) {
				t.Errorf("absentRoutingClasses = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoader_refreshCache_firstBootSeriesOnlyBodyAcceptedWithDiagnostic pins the
// accept-without-baseline contract: with no previous cache there is no proof of
// LOSS, so a body that routes nothing to Radarr is ACCEPTED - and reported at
// ERROR, naming the absent class, the accepted record count, and the routed
// identifier count, so the first-boot hole is no longer silent.
func TestLoader_refreshCache_firstBootSeriesOnlyBodyAcceptedWithDiagnostic(t *testing.T) {
	ts := routingBodyServer(t, seriesOnlyBody)
	logger, logs := capture.New()
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, logger)

	next, err := l.refreshCache(context.Background(), nil)
	if err != nil {
		t.Fatalf("first-boot series-only refresh error = %v, want nil (no baseline cannot prove loss)", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("accepted records = %+v, want the single series record", next.Records)
	}
	if got := logs.CountLevel(slog.LevelError, absentArrMessage); got != 1 {
		t.Fatalf("absent-arr ERROR count = %d, want 1 (messages %v)", got, logs.Messages())
	}
	if !logs.AttrContains(absentArrMessage, "absent_routing_classes", "movie-routed") {
		t.Errorf("absent_routing_classes missing movie-routed (records %v)", logs.Messages())
	}
	if logs.AttrContains(absentArrMessage, "absent_routing_classes", "series-routed") {
		t.Errorf("absent_routing_classes claims series-routed absent, but the body carries one")
	}
	if !logs.HasAttr(absentArrMessage, "records", "1") {
		t.Errorf("absent-arr ERROR missing records=1")
	}
	if !logs.HasAttr(absentArrMessage, "routed_identifiers", "1") {
		t.Errorf("absent-arr ERROR missing routed_identifiers=1")
	}
}

// TestLoader_refreshCache_unusablePreviousSeriesOnlyResetsStreak covers the other
// reachable no-baseline state - a cache that exists but is not usable - and pins
// that the diagnostic does not turn an accepted refresh into a rejection: the
// persisted rejection streak still resets to zero.
func TestLoader_refreshCache_unusablePreviousSeriesOnlyResetsStreak(t *testing.T) {
	ts := routingBodyServer(t, movieOnlyBody)
	logger, logs := capture.New()
	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{}},
		RejectedRefreshes: 2,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, logger)

	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refresh onto an unusable cache error = %v, want nil", err)
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("RejectedRefreshes = %d, want 0: this is an accepted refresh", next.RejectedRefreshes)
	}
	if got := logs.CountLevel(slog.LevelError, absentArrMessage); got != 1 {
		t.Fatalf("absent-arr ERROR count = %d, want 1 (messages %v)", got, logs.Messages())
	}
	if !logs.AttrContains(absentArrMessage, "absent_routing_classes", "series-routed") {
		t.Errorf("absent_routing_classes missing series-routed for a movie-only body")
	}
}

// TestLoader_refreshCache_firstBootHealthyBodyLogsNoDiagnostic pins the negative
// case: a first boot whose body carries both routing classes is accepted with no
// absent-arr record, so the diagnostic cannot become boot noise.
func TestLoader_refreshCache_firstBootHealthyBodyLogsNoDiagnostic(t *testing.T) {
	ts := routingBodyServer(t, bothArrsBody)
	logger, logs := capture.New()
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, logger)

	if _, err := l.refreshCache(context.Background(), nil); err != nil {
		t.Fatalf("healthy first-boot refresh error = %v, want nil", err)
	}
	if got := logs.CountExact(absentArrMessage); got != 0 {
		t.Errorf("absent-arr ERROR count = %d, want 0 (messages %v)", got, logs.Messages())
	}
}

// TestLoader_refreshCache_usablePreviousZeroToZeroStaysSilent pins the scoping
// that keeps the diagnostic off the per-cycle path: with a usable baseline the
// relative guards own the decision, and an unchanged absent class (0 -> 0) is
// accepted silently - nothing was lost, so there is nothing to report.
func TestLoader_refreshCache_usablePreviousZeroToZeroStaysSilent(t *testing.T) {
	ts := routingBodyServer(t, seriesOnlyBody)
	logger, logs := capture.New()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, logger)

	if _, err := l.refreshCache(context.Background(), prev); err != nil {
		t.Fatalf("unchanged series-only refresh error = %v, want nil", err)
	}
	if got := logs.CountExact(absentArrMessage); got != 0 {
		t.Errorf("absent-arr ERROR count = %d, want 0 with a usable baseline (messages %v)", got, logs.Messages())
	}
}

// TestLoader_refreshCache_usablePreviousExtinctionStillRejected pins that the
// diagnostic did not weaken the guard it sits beside: with a usable baseline
// carrying movie-routed records, a series-only refresh is still REFUSED by the
// extinction guard with its existing message, and no acceptance diagnostic is
// logged for a refresh that was never accepted.
func TestLoader_refreshCache_usablePreviousExtinctionStillRejected(t *testing.T) {
	ts := routingBodyServer(t, seriesOnlyBody)
	logger, logs := capture.New()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records: []Record{
			{AniListID: 1, Type: "TV", TvdbID: 100},
			{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{7}},
		},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, logger)

	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("refresh that lost every movie-routed record returned nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "movie-routed records went extinct") {
		t.Errorf("rejection error = %v, want the existing extinction message", err)
	}
	if len(next.Records) != 2 {
		t.Errorf("records = %d, want the 2 stale records retained", len(next.Records))
	}
	if got := logs.CountExact(absentArrMessage); got != 0 {
		t.Errorf("absent-arr ERROR count = %d, want 0 for a REFUSED refresh (messages %v)", got, logs.Messages())
	}
}
