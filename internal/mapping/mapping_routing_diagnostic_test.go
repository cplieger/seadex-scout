package mapping

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

const seriesOnlyBody = `[{"anilist_id":1,"type":"tv","tvdb_id":100}]`

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

// TestLoader_refreshCache_usablePreviousExtinctionStillRejected pins the
// extinction guard end to end: with a usable baseline carrying movie-routed
// records, a series-only refresh is REFUSED with the extinction message and
// the stale records are retained.
func TestLoader_refreshCache_usablePreviousExtinctionStillRejected(t *testing.T) {
	ts := routingBodyServer(t, seriesOnlyBody)
	logger, _ := capture.New()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records: []Record{
			{AniListID: 1, Type: "TV", TvdbID: 100},
			{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{7}},
		},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, logger)

	next, err := l.refreshCache(t.Context(), prev)
	if err == nil {
		t.Fatal("refresh that lost every movie-routed record returned nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "movie-routed records went extinct") {
		t.Errorf("rejection error = %v, want the existing extinction message", err)
	}
	if len(next.Records) != 2 {
		t.Errorf("records = %d, want the 2 stale records retained", len(next.Records))
	}
}
