package mapping

import (
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

const recoveredMessage = "mapping: rejection streak ended, the cached release is the latest"

// TestLoader_refreshCache_unchangedTagLogsEndedRejectionStreak pins the
// recovery observability contract: a check finding the cached release still
// the latest, which ends a persisted rejection streak, logs the recovery
// message carrying the ended_rejection_streak attribute.
func TestLoader_refreshCache_unchangedTagLogsEndedRejectionStreak(t *testing.T) {
	rel := newRelease(t, testTag, nil)
	previous := staleCache(testTag)
	previous.RejectedRefreshes = 3
	logger, logs := capture.New()
	if _, err := NewLoader(rel.srv.Client(), rel.source(), WithRefresh(time.Hour), WithLogger(logger)).refreshCache(t.Context(), previous); err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if logs.CountLevel(slog.LevelInfo, recoveredMessage) != 1 {
		t.Fatalf("refreshCache logs = %v, want one rejection-streak recovery message at INFO", logs.Messages())
	}
	if !logs.HasAttr("", "ended_rejection_streak", "3") || !logs.HasAttr("", "records", "1") {
		t.Errorf("refreshCache logs = %v, want ended_rejection_streak=3 and records=1", logs.Messages())
	}
}

// TestLoader_refreshCache_acceptedRefreshLogsEndedRejectionStreak pins the
// accepted-release recovery signal: an accepted refresh that ends a
// persisted rejection streak carries ended_rejection_streak and its tag on
// the refreshed message.
func TestLoader_refreshCache_acceptedRefreshLogsEndedRejectionStreak(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 2, Type: "tv", TVDB: 200}))
	previous := staleCache("v2026.10.16")
	previous.RejectedRefreshes = 3
	logger, logs := capture.New()
	if _, err := NewLoader(rel.srv.Client(), rel.source(), WithRefresh(time.Hour), WithLogger(logger)).refreshCache(t.Context(), previous); err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if logs.CountExact("mapping: refreshed") != 1 {
		t.Fatalf("refreshCache logs = %v, want one refreshed message", logs.Messages())
	}
	for key, want := range map[string]string{"ended_rejection_streak": "3", "records": "1", "tag": testTag} {
		if !logs.HasAttr("", key, want) {
			t.Errorf("refreshCache logs = %v, want %s=%s", logs.Messages(), key, want)
		}
	}
}

// TestLoader_refreshCache_noRejectionStreakLogsNoRecoverySignal pins the
// absence side of both recovery signals: with no persisted streak, neither
// path announces a healed one.
func TestLoader_refreshCache_noRejectionStreakLogsNoRecoverySignal(t *testing.T) {
	t.Run("release unchanged", func(t *testing.T) {
		rel := newRelease(t, testTag, nil)
		logger, logs := capture.New()
		if _, err := NewLoader(rel.srv.Client(), rel.source(), WithRefresh(time.Hour), WithLogger(logger)).refreshCache(t.Context(), staleCache(testTag)); err != nil {
			t.Fatalf("refreshCache error: %v", err)
		}
		if n := logs.CountExact(recoveredMessage); n != 0 {
			t.Errorf("an unchanged release with no persisted streak logged the recovery message %d times, want 0", n)
		}
	})
	t.Run("accepted refresh", func(t *testing.T) {
		rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 2, Type: "tv", TVDB: 200}))
		logger, logs := capture.New()
		if _, err := NewLoader(rel.srv.Client(), rel.source(), WithRefresh(time.Hour), WithLogger(logger)).refreshCache(t.Context(), staleCache("")); err != nil {
			t.Fatalf("refreshCache error: %v", err)
		}
		if logs.CountExact("mapping: refreshed") != 1 {
			t.Fatalf("refreshCache logs = %v, want one refreshed message", logs.Messages())
		}
		if got, ok := logs.AttrValue("mapping: refreshed", "ended_rejection_streak"); ok {
			t.Errorf("refreshed log carries ended_rejection_streak=%s, want the attribute absent with no persisted streak", got)
		}
	})
}
