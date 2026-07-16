package match

import (
	"context"
	"log/slog"
	"testing"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// cancelledAniList fails every lookup with context.Canceled, modelling a
// shutdown/redeploy landing mid-cycle (the calls observe the cancellation).
type cancelledAniList struct{}

func (cancelledAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return anilist.Media{}, context.Canceled
}

func (cancelledAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	return nil, context.Canceled
}

// TestMatchCancelledLookupsLogDebugNotWarn pins the log-level contract for a
// shutdown mid-cycle: a context.Canceled from the batch prefetch AND from the
// per-entry Fetch must log at Debug, never Warn (a routine redeploy must not be
// attributed to an AniList outage in Loki), while the cycle is still flagged
// Degraded so findings stay preserved and the id stays un-memoized for retry.
func TestMatchCancelledLookupsLogDebugNotWarn(t *testing.T) {
	logger, recorder := capture.New()
	snap := &library.Snapshot{}
	idx := mapping.NewIndex(nil) // no record: the entry needs the AniList lookup

	res := NewMatcher(cancelledAniList{}, logger).Match(context.Background(), []seadex.Entry{{AniListID: 42}}, snap, idx, Memo{})

	if !res.Degraded {
		t.Error("Degraded = false, want true: a cancelled needed lookup must preserve findings")
	}
	if _, cached := res.Memo.Entries[42]; cached {
		t.Error("cancelled lookup was memoized; it must be retried next cycle")
	}
	for _, rec := range recorder.Records() {
		if rec.Level >= slog.LevelWarn {
			t.Errorf("cancellation logged at %s (%q); a shutdown must log at Debug, not Warn", rec.Level, rec.Message)
		}
	}
}
