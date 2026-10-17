package mapping

import (
	"strings"
	"testing"
	"time"
)

// TestLoader_refreshCache_usablePreviousExtinctionStillRejected pins the
// extinction guard end to end: with a usable baseline carrying movie-routed
// records, a series-only release is REFUSED with the extinction message and
// the stale records are retained.
func TestLoader_refreshCache_usablePreviousExtinctionStillRejected(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 1, Type: "tv", TVDB: 100}))
	prev := staleCache("")
	prev.Records = append(prev.Records, Record{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{7}})
	next, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), prev)
	if err == nil {
		t.Fatal("refresh that lost every movie-routed record returned nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "movie-routed records went extinct") {
		t.Errorf("rejection error = %v, want the extinction message", err)
	}
	if len(next.Records) != 2 {
		t.Errorf("records = %d, want the 2 stale records retained", len(next.Records))
	}
}
