package match

import (
	"context"
	"testing"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// partialOutageAniList models a PARTIAL AniList incident: FetchMany returns
// the first requested id's media together with an error (a failed later
// chunk), and every single Fetch fails transiently.
type partialOutageAniList struct{}

func (partialOutageAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return anilist.Media{}, context.DeadlineExceeded
}

func (partialOutageAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	return map[int]anilist.Media{ids[0]: {Titles: []string{"Returned"}, Format: "TV"}}, context.DeadlineExceeded
}

// TestMatchTransientFailuresLogWarn pins the non-cancellation side of the
// degraded-lookup log contract (the twin of
// TestMatchCancelledLookupsLogDebugNotWarn): a genuine transient AniList
// failure (not context.Canceled) must surface as the two WARN lines the
// operator's Loki view keys on - the batch-prefetch "incomplete" warn (a
// PARTIAL batch failure: id 41 returned, the error hitting id 42's chunk) and
// the per-id "anilist fallback failed" warn from 42's retry - not be
// misclassified down to the Debug cancellation arm.
func TestMatchTransientFailuresLogWarn(t *testing.T) {
	logger, recorder := capture.New()
	snap := &library.Snapshot{}
	idx := mapping.NewIndex(nil) // no records: both entries need the AniList lookup

	res := NewMatcher(partialOutageAniList{}, logger).Match(context.Background(),
		[]seadex.Entry{{AniListID: 41}, {AniListID: 42}}, snap, idx, Memo{})

	if !res.Degraded {
		t.Error("Degraded = false, want true when a needed AniList lookup fails transiently")
	}
	if got := recorder.CountExact("anilist batch prefetch incomplete; remaining ids fall back to per-id fetch"); got != 1 {
		t.Errorf("batch-prefetch WARN count = %d, want 1: a non-cancelled partial batch failure must warn, not hide in the Debug cancellation arm", got)
	}
	if got := recorder.CountExact("anilist fallback failed"); got != 1 {
		t.Errorf("per-id fallback WARN count = %d, want 1: a non-cancelled Fetch failure must warn, not hide in the Debug cancellation arm", got)
	}
}

// TestMatchTotalOutageLogsSingleWarn pins the total-outage log contract: a
// batch prefetch that fails entirely emits exactly ONE alert-stable WARN
// ("anilist batch prefetch failed; skipping per-id fallback for pending ids")
// and NO per-id "anilist fallback failed" lines, since the per-id fallback is
// skipped rather than emitting one doomed WARN per pending id.
// degradedAniList (match_test.go) fails the batch entirely.
func TestMatchTotalOutageLogsSingleWarn(t *testing.T) {
	logger, recorder := capture.New()
	snap := &library.Snapshot{}
	idx := mapping.NewIndex(nil) // no records: both entries need the AniList lookup

	res := NewMatcher(degradedAniList{}, logger).Match(context.Background(),
		[]seadex.Entry{{AniListID: 41}, {AniListID: 42}}, snap, idx, Memo{})

	if !res.Degraded {
		t.Error("Degraded = false, want true on a total AniList outage")
	}
	if got := recorder.CountExact("anilist batch prefetch failed; skipping per-id fallback for pending ids"); got != 1 {
		t.Errorf("total-outage WARN count = %d, want exactly 1", got)
	}
	if got := recorder.CountExact("anilist fallback failed"); got != 0 {
		t.Errorf("per-id fallback WARN count = %d, want 0 (the doomed per-id tail is skipped)", got)
	}
}
