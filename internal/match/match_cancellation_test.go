package match

import (
	"context"
	"errors"
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

// cancelOnFetchAniList models a shutdown landing mid-cycle: the batch
// prefetch partial-fails (a non-nil empty map plus an error, leaving the ids
// for the per-id retry), and the single Fetch cancels the run's context while
// still answering successfully, so the cancellation is first observed by the
// NEXT entry's loop check.
type cancelOnFetchAniList struct {
	cancel     context.CancelFunc
	fetchCalls int
}

func (c *cancelOnFetchAniList) Fetch(_ context.Context, _ int) (anilist.Media, error) {
	c.fetchCalls++
	c.cancel()
	return anilist.Media{Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020}, nil
}

func (c *cancelOnFetchAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	return map[int]anilist.Media{}, errors.New("anilist 500 on a later chunk")
}

// TestMatchMidRunCancellationRetainsCompletedMatches pins the mid-loop
// cancellation arm's retention contract (the twin of
// TestMatchCancelledContextStopsBeforeEntries, which pins the zero-matched
// pre-cancelled boundary): when the context is cancelled AFTER some entries
// were matched, the already-completed matches are returned rather than
// discarded, the remaining entries are skipped with no further AniList
// traffic, the cycle is flagged Degraded, and a never-attempted id stays OUT
// of IncompleteIDs (a shutdown is a whole-cycle event per the Result
// contract).
func TestMatchMidRunCancellationRetainsCompletedMatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", TmdbID: 100, Year: 2020},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: the per-id Fetch cancels the ctx and still answers
		{AniListID: 22, Type: "MOVIE"}, // never reached: the loop breaks on the cancelled ctx
	})
	fake := &cancelOnFetchAniList{cancel: cancel}

	res := NewMatcher(fake, nil).Match(ctx, []seadex.Entry{{AniListID: 11}, {AniListID: 22}}, snap, idx, Memo{})

	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
		t.Fatalf("matches = %+v, want exactly the one title match completed before the cancellation", res.Matches)
	}
	if !res.Degraded {
		t.Error("Degraded = false, want true when the loop is cut short mid-run")
	}
	if fake.fetchCalls != 1 {
		t.Errorf("single Fetch calls = %d, want 1 (no further AniList traffic after the cancellation)", fake.fetchCalls)
	}
	if _, ok := res.IncompleteIDs[22]; ok {
		t.Errorf("IncompleteIDs = %v, want id 22 absent: a never-attempted id is a whole-cycle shutdown event, not a per-id incomplete lookup", res.IncompleteIDs)
	}
}
