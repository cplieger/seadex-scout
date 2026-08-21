package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// TestLogStopClassifiesShutdownAndFault pins the feed's stop log contract:
// during a shutdown, an expired graceful-shutdown budget (DeadlineExceeded from
// webhttp.Run, meaning in-flight Torznab requests were cut off) gets its own
// WARN message distinct from the routine clean-shutdown WARN, and any error
// outside a shutdown stays the ERROR fault line.
func TestLogStopClassifiesShutdownAndFault(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		err       error
		wantMsg   string
		wantLevel slog.Level
	}{
		{"budget expired during shutdown", canceled, context.DeadlineExceeded, "indexer shutdown budget expired; in-flight requests aborted", slog.LevelWarn},
		{"clean stop during shutdown", canceled, context.Canceled, "indexer feed stopped during shutdown", slog.LevelWarn},
		{"fault outside shutdown", t.Context(), errors.New("bind failed"), "indexer feed stopped", slog.LevelError},
		{"deadline exceeded outside shutdown stays a fault", t.Context(), context.DeadlineExceeded, "indexer feed stopped", slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, rec := capture.New()

			logStop(tt.ctx, log.With("component", "indexer"), tt.err)

			records := rec.Records()
			if len(records) != 1 {
				t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
			}
			if records[0].Message != tt.wantMsg {
				t.Errorf("msg = %q, want %q", records[0].Message, tt.wantMsg)
			}
			if records[0].Level != tt.wantLevel {
				t.Errorf("level = %v, want %v", records[0].Level, tt.wantLevel)
			}
		})
	}
}

// TestLogStopClassifiesCauseOnlyCancellation pins the sibling terminal
// boundary: net.ListenConfig.Listen can report a cancelled bind as the
// cancellation CAUSE rather than context.Canceled, and that stop is still a
// routine shutdown - it must log the WARN, never the ERROR fault line that
// fires the cycle-error alert on a redeploy.
func TestLogStopClassifiesCauseOnlyCancellation(t *testing.T) {
	cause := errors.New("terminated signal received") // deliberately NOT wrapping context.Canceled
	ctx, cancelCause := context.WithCancelCause(t.Context())
	cancelCause(cause)
	log, rec := capture.New()

	logStop(ctx, log.With("component", "indexer"), fmt.Errorf("listen: %w", cause))

	records := rec.Records()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
	}
	if records[0].Message != "indexer feed stopped during shutdown" {
		t.Errorf("msg = %q, want the routine shutdown WARN", records[0].Message)
	}
	if records[0].Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN (a cause-only cancellation is not a fault)", records[0].Level)
	}
}

// TestSupervisePanicShield pins the feed's crash shield (the twin of
// internal/cycle's compare-cycle panic shield): a panicking feed goroutine is
// recovered - it must not crash the long-lived daemon that runs the feed -
// logged as the component=indexer panic ERROR, its resources are still released
// (cleanup runs on the panic path), and done is closed so the stop func cannot
// deadlock.
func TestSupervisePanicShield(t *testing.T) {
	log, rec := capture.New()
	done := make(chan struct{})
	cleaned := make(chan struct{})

	supervise(t.Context(), done,
		func(context.Context) error { panic("boom") },
		func() { close(cleaned) },
		log.With("component", "indexer"))
	<-done

	const msg = "indexer feed panicked"
	if got := rec.CountLevel(slog.LevelError, msg); got != 1 {
		t.Errorf("panic-shield ERROR count = %d, want 1: %v", got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelWarn, msg); got != 0 {
		t.Errorf("panic-shield WARN count = %d, want 0: %v", got, rec.Messages())
	}
	if !rec.HasAttr(msg, "component", "indexer") {
		t.Errorf("panic-shield record missing component=indexer: %v", rec.Records())
	}
	select {
	case <-cleaned:
	default:
		t.Error("cleanup not released on the panic path (the Prowlarr transport would leak)")
	}
}

// TestSuperviseStopWaitsForDrain pins the stop contract Supervise returns: it
// cancels the feed's context (even one derived from a still-live parent) and
// returns only after the goroutine has fully drained - the daemon's own
// shutdown work (client cleanup, marker removal) runs after the feed is gone,
// not beside it.
func TestSuperviseStopWaitsForDrain(t *testing.T) {
	log, _ := capture.New()
	done := make(chan struct{})
	stopped := make(chan struct{})
	cleaned := make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	supervise(ctx, done, func(rctx context.Context) error {
		<-rctx.Done()
		close(stopped)
		return rctx.Err()
	}, func() { close(cleaned) }, log)

	cancel()
	<-done

	select {
	case <-stopped:
	default:
		t.Error("the feed goroutine did not observe its cancelled context")
	}
	select {
	case <-cleaned:
	default:
		t.Error("cleanup did not run before done was closed")
	}
}

// TestSuperviseStopForcesTheFeedDownUnderALiveParent pins the exported stop
// contract the unexported-supervise cases cannot reach: Supervise runs the feed
// on its OWN cancellable child of ctx, so the returned stop func brings a
// SERVING feed down while the parent context is still live (a daemon unwinding
// its defers after a startup error), and it returns only once the goroutine has
// drained and released the Prowlarr transport - not after waiting out stopWait.
func TestSuperviseStopForcesTheFeedDownUnderALiveParent(t *testing.T) {
	orig := listenAddr
	listenAddr = "127.0.0.1:0"
	t.Cleanup(func() { listenAddr = orig })
	log, rec := capture.New()
	cleaned := make(chan struct{})

	// The parent stays live for the whole test body, so only the child context
	// Supervise derives can stop the feed.
	stop := New(&Config{APIKey: "k"}, log, nil).Supervise(t.Context(), func() { close(cleaned) })
	stop()

	select {
	case <-cleaned:
	default:
		t.Error("stop returned before the feed goroutine released its resources (the Prowlarr transport would leak)")
	}
	const drainWarn = "indexer feed did not drain within the shutdown budget; continuing shutdown"
	if got := rec.CountLevel(slog.LevelWarn, drainWarn); got != 0 {
		t.Errorf("stop waited out the %v drain budget instead of stopping the feed: %v", stopWait, rec.Messages())
	}
}

// TestSuperviseCleanReturnLogsNothing pins the silence of a feed that returns
// without an error: there is no stop to classify, so nothing is logged at all.
// Every classification arm logStop can pick is a WARN or an ERROR, and
// SeadexScoutCycleError keys on level=ERROR, so routing a clean return into it
// would page the operator on an ordinary exit - and once that fires routinely,
// the same alert stops meaning anything when the feed really does fail.
func TestSuperviseCleanReturnLogsNothing(t *testing.T) {
	log, rec := capture.New()
	done := make(chan struct{})
	cleaned := make(chan struct{})

	supervise(t.Context(), done, func(context.Context) error { return nil }, func() { close(cleaned) }, log)
	<-done

	select {
	case <-cleaned:
	default:
		t.Error("cleanup did not run before done was closed")
	}
	if rec.Len() != 0 {
		t.Errorf("a feed that returned cleanly logged %d records, want none: %v", rec.Len(), rec.Messages())
	}
}
