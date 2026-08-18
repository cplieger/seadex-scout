// Package cycle owns running seadex-scout's compare cycle safely: the
// cross-process coalescing that serializes every entry point on one lock, and
// the shutdown-interruption contract that governs what each entry point
// reports.
//
// Two entry points share the lock and this package.
package cycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/seadex-scout/internal/shutdown"
)

// errRecordPollHealth marks a health-marker WRITE failure so the shutdown-wins
// branch in RunOnce can tell it apart from an ordinary cycle error.
var errRecordPollHealth = errors.New("record poll health")

// --- Cycle coalescing: the cross-process lock shared by poll and the daemon ---

// dirMode is applied when creating the cycle-lock's parent directory (normally
// /config, which already exists as the mounted volume holding the config and
// state files this lock guards). The report dir's mode is reportfs.DirMode's,
// pinned by reportfs.MakeDir for both of that directory's creators.
const dirMode = 0o700

// NewExclusive builds the cross-process cycle coalescer shared by every
// cycle entry point: the daemon's RunLoop ticks (skip mode) and exec'd `poll`
// subcommands (queue mode) serialize on dir/cycle.lock, closing the
// last-writer-wins race two concurrent cycles run on state.json (AniList memo
// and finding-dedupe loss, duplicate alerts) and feed.json.
func NewExclusive(ctx context.Context, dir string) (*scheduler.Exclusive, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create cycle lock dir %s: %w", dir, err)
	}
	return scheduler.NewExclusive(dir, slog.Default(),
		scheduler.WithGate(func() bool { return ctx.Err() == nil })), nil
}

// runOnce is one execution of RunOnce's cycle body under the cycle lock: run the
// cycle and apply the interruption contract (a cancellation observed at any
// point - even when the cycle still managed to complete healthy, e.g. the
// signal landed during the end-of-cycle save - is reported as an interruption,
// since an interrupted run's outcome is not a trustworthy health verdict).
func runOnce(ctx context.Context, sc Cycler) (healthy bool, err error) {
	healthy, panicked := runCycle(ctx, sc)
	if ctx.Err() != nil {
		return healthy, shutdown.Interrupted(ctx)
	}
	if !healthy {
		if panicked {
			// A recovered panic is a code fault, not an arr/ingest fault: the
			// remediation the operator reads must not point at the arr config.
			return healthy, errors.New("compare cycle panicked")
		}
		return healthy, errors.New("compare cycle failed (library ingest)")
	}
	return healthy, nil
}

// executeRuns runs the cycle body under the cycle lock and captures the
// first execution's outcome - this invocation's own run - plus the run count.
// Each execution commits its OWN health verdict inside the locked body
// (recordRunHealth), so no verdict outlives the lock that ordered the cycle
// producing it.
func executeRuns(ctx context.Context, ex *scheduler.Exclusive, sc Cycler, marker *health.Marker) (outcome scheduler.Outcome, runs int, own, exErr error) {
	outcome, exErr = ex.Run(func() error {
		healthy, err := runOnce(ctx, sc)
		runs++
		err = recordRunHealth(ctx, marker, healthy, runs, err)
		if runs == 1 {
			own = err
		} else if err != nil {
			slog.Warn("queued rerun cycle reported an error", "error", err)
		}
		return nil
	})
	return outcome, runs, own, exErr
}

// recordRunHealth commits ONE cycle's health verdict to the shared marker, and
// only when the result is trustworthy: it is called from inside Exclusive's
// locked job body.
//
// The lock must cover the write.
func recordRunHealth(ctx context.Context, marker *health.Marker, healthy bool, runs int, cycleErr error) error {
	if ctx.Err() != nil {
		if shutdown.IsShutdownError(ctx, cycleErr) {
			return cycleErr
		}
		return shutdown.Interrupted(ctx)
	}
	err := marker.SetChecked(healthy)
	if err == nil {
		return cycleErr
	}
	err = fmt.Errorf("%w: %w", errRecordPollHealth, err)
	if runs > 1 {
		// A health-marker write failure is not a cycle fault's class: a cycle error
		// already logs its own ERROR, whereas this write is the marker's only report, it
		// has no exit code to surface through, and it does not self-heal.
		slog.Error("queued rerun could not record poll health", "error", err)
		return cycleErr
	}
	return err
}

// warnCoordinationError logs the coordination-infrastructure diagnostic for an
// invocation whose demand or run still stands despite the error: demand was
// recorded (Queued/Discarded), a run completed (Ran/RanQueued/Skipped), or
// Exclusive failed before recording demand (anything else - reachable only
// from RunOnce's shutdown branch).
func warnCoordinationError(outcome scheduler.Outcome, err error) {
	switch outcome {
	case scheduler.OutcomeQueued, scheduler.OutcomeDiscarded:
		slog.Warn("cycle coordination error after queueing; demand stands", "outcome", outcome.String(), "error", err)
	case scheduler.OutcomeRan, scheduler.OutcomeRanQueued, scheduler.OutcomeSkipped:
		slog.Warn("cycle coordination error after run", "outcome", outcome.String(), "error", err)
	default:
		slog.Warn("cycle coordination failed during shutdown", "outcome", outcome.String(), "error", err)
	}
}

// nonRunResult maps the coalescing outcomes that end a RunOnce WITHOUT an
// own run to its exit contract: Queued/Discarded log success (any
// coordination error is a stands-anyway diagnostic) and Gated applies the
// uniform interruption contract.
func nonRunResult(ctx context.Context, outcome scheduler.Outcome, exErr error) (handled bool, err error) {
	switch outcome {
	case scheduler.OutcomeQueued, scheduler.OutcomeDiscarded:
		if exErr != nil {
			warnCoordinationError(outcome, exErr)
		}
		msg := "compare cycle already in flight; demand queued for the active runner"
		if outcome == scheduler.OutcomeDiscarded {
			msg = "compare cycle already in flight; demand already covered by the queued rerun"
		}
		slog.Info(msg, "outcome", outcome.String())
		return true, nil
	case scheduler.OutcomeGated:
		return true, shutdown.Interrupted(ctx)
	default:
		return false, nil
	}
}

// RunOnce runs ONE compare cycle under the cross-process cycle lock in QUEUE
// mode - the `poll` subcommand's entry point - and maps the coalescing outcome
// to the caller's exit contract:
//
//   - Ran (or ran plus queued reruns): the exit code reflects this
//     invocation's OWN (first) run - a healthy cycle exits 0, an unhealthy or
//     interrupted one non-zero (see runOnce).
func RunOnce(ctx context.Context, ex *scheduler.Exclusive, sc Cycler, marker *health.Marker) error {
	// A pre-cancelled invocation must not enqueue demand: Exclusive's gate refuses the
	// RUN, not the queue insertion, so a cancelled poll would add work after shutdown.
	if ctx.Err() != nil {
		return shutdown.Interrupted(ctx)
	}
	outcome, runs, own, exErr := executeRuns(ctx, ex, sc, marker)
	if ctx.Err() != nil {
		// Cancellation observed by the time Run returns wins over EVERY outcome, whether
		// it coordinated with a busy owner or serviced another process's queued rerun.
		if exErr != nil {
			// Outcome-specific diagnostics: "demand stands" is only true when demand was
			// actually recorded, and OutcomeNone means Exclusive failed before recording it.
			warnCoordinationError(outcome, exErr)
		}
		if own != nil && !errors.Is(own, context.Canceled) {
			// The interruption replaces this invocation's own result, so the
			// own run's error has no exit code left to report through.
			if errors.Is(own, errRecordPollHealth) {
				// A marker-WRITE failure needs an operator; every other own
				// error either self-heals or already logged its own ERROR
				// inside Cycle.
				slog.Error("own cycle could not record poll health before shutdown", "error", own)
			}
			slog.Warn("own cycle reported an error before shutdown", "error", own)
		}
		return shutdown.Interrupted(ctx)
	}
	if handled, err := nonRunResult(ctx, outcome, exErr); handled {
		return err
	}
	if runs == 0 {
		return fmt.Errorf("cycle coordination failed: %w", exErr)
	}
	if exErr != nil {
		// The run itself completed; a queue-file error only degrades the
		// demand-coalescing bookkeeping, so it is logged rather than failing
		// the cycle this invocation paid for.
		warnCoordinationError(outcome, exErr)
	}
	return own
}

// RunLoop runs a cycle on each tick of a POLL_INTERVAL timer with ±10%
// jitter until ctx is cancelled. The first iteration fires immediately so a
// cycle runs promptly on boot; the marker is set to each cycle's health.
func RunLoop(ctx context.Context, interval time.Duration, ex *scheduler.Exclusive, sc Cycler, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		outcome, err := ex.RunOrSkip(func() error {
			healthy, _ := runCycle(ctx, sc)
			if !healthy && ctx.Err() != nil {
				return nil // shutdown mid-cycle: cancellation is not an ingest fault
			}
			if err := marker.SetChecked(healthy); err != nil {
				// The tick has no exit code to report the write through and a
				// marker-write failure does not self-heal, so ERROR is its only signal.
				slog.Error("tick could not record cycle health", "error", err)
			}
			return nil
		})
		switch {
		case err == nil:
		case outcome == scheduler.OutcomeNone:
			slog.Error("cycle coordination failed; tick did not run", "error", err)
		default:
			// The tick's cycle ran; a queue-file error only degrades the
			// demand-coalescing bookkeeping.
			warnCoordinationError(outcome, err)
		}
	}, scheduler.LoopOptions{Interval: interval, FireOnStart: true, Jitter: 0.10})
}

// Cycler runs one compare cycle, reporting whether the library ingest was
// healthy. Satisfied by *scout.Scout; a consumer-side seam so the daemon's
// panic shield is testable without a real Scout.
type Cycler interface {
	Cycle(ctx context.Context) bool
}

// runCycle runs one cycle, recovering from a panic so a single bad cycle cannot
// crash the long-lived daemon. A panic is reported as an unhealthy cycle, and
// panicked distinguishes it from the other producer of healthy=false (a failed
// arr walk) so a caller naming the cause to an operator names the right one.
func runCycle(ctx context.Context, sc Cycler) (healthy, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("cycle panicked", "panic", r, "stack", string(debug.Stack()))
			healthy, panicked = false, true
		}
	}()
	return sc.Cycle(ctx), false
}
