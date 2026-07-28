// Package cycle owns running seadex-scout's compare cycle safely: the
// cross-process coalescing that serializes every entry point on one lock, and
// the shutdown-interruption contract that governs what each entry point
// reports.
//
// Two entry points share the lock and this package. RunOnce is the `poll`
// subcommand's single cycle in QUEUE mode (a request arriving while another
// cycle is in flight is queued for the active runner and exits 0); RunLoop is
// the resident daemon's timer in SKIP mode (a tick arriving while a cycle is in
// flight is skipped, since the next tick provides freshness). Both commit every
// verdict they publish from inside the locked body, so no verdict outlives the
// lock that ordered the cycle producing it. RunOnce withholds a verdict when
// cancellation is observed before recording, even if Cycle returned healthy.
//
// The interruption contract is the other half: a shutdown cancellation observed
// at any point makes the invocation report an interruption rather than a result,
// and IsShutdownError / NormalizeShutdownError are how the root's other terminal
// boundaries (the report subcommand, the indexer goroutine) classify the same
// condition, so one vocabulary decides WARN-vs-ERROR everywhere.
//
// It lives here rather than in the composition root because it is a coordination
// concept with its own state machine and its own tests, not construction: main
// keeps dispatch, wiring, and the os.Exit mapping.
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
	"github.com/cplieger/scheduler/v2"
)

// msgCoordErrorAfterRun is the WARN message for a coordination-bookkeeping
// error observed after a cycle actually ran (the run stands; only the
// demand-coalescing accounting degraded). Shared by warnCoordinationError
// (RunOnce's diagnostics) and RunLoop so the two Loki-queried diagnostics
// cannot drift.
const msgCoordErrorAfterRun = "cycle coordination error after run"

// errRecordPollHealth marks a health-marker WRITE failure so the shutdown-wins
// branch in RunOnce can tell it apart from an ordinary cycle error. A cycle
// fault already logs its own ERROR inside Cycle, but this write is the marker's
// only report AND it does not self-heal (a full disk or a bad mode on the marker
// directory keeps failing until the operator acts), so it must not be reduced to
// the displaced-result WARN when shutdown replaces the own-run result.
var errRecordPollHealth = errors.New("record poll health")

// --- Cycle coalescing: the cross-process lock shared by poll and the daemon ---

// dirMode is applied when creating the cycle-lock directory (normally
// /config, which already exists as the mounted volume holding the config and
// state files this lock guards).
const dirMode = 0o700

// NewExclusive builds the cross-process cycle coalescer shared by every
// cycle entry point: the daemon's RunLoop ticks (skip mode) and exec'd `poll`
// subcommands (queue mode) serialize on dir/cycle.lock, closing the
// last-writer-wins race two concurrent cycles run on state.json (AniList memo
// and finding-dedupe loss, duplicate alerts) and feed.json. dir is the single
// /config mount root (config.DefaultCycleLockDir) so the lock lives beside every
// file it guards instead of beside whichever one it was derived from; the kernel
// releases the flock if a process dies, so there is no stale-lock state. The
// gate stops queued reruns (and a not-yet-started initial run) once shutdown is
// signalled; an in-flight run is never interrupted by the gate - context
// cancellation owns that.
func NewExclusive(ctx context.Context, dir string) (*scheduler.Exclusive, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create cycle lock dir %s: %w", dir, err)
	}
	return scheduler.NewExclusive(dir, slog.Default(),
		scheduler.WithGate(func() bool { return ctx.Err() == nil })), nil
}

// Interrupted wraps the stable ctx.Err() (always context.Canceled here) with
// the uniform interruption message every entry point shares, so the root
// classifies it as a routine-shutdown WARN and the contract reads identically
// from every phase. The message keeps poll's historical wording because it is
// what an operator greps for. The message speaks only of THIS INVOCATION's result, not of the
// marker: an interruption leaves the marker untouched whenever no verdict was
// published before the cancellation was observed - including a cycle that
// completed healthy and was then interrupted at the recording boundary, whose
// verdict recordRunHealth deliberately withholds because an interrupted run's
// outcome is not a trustworthy health verdict. TestRunOnceUniformInterruption's
// post-cycle case pins the mid-cycle form of this (its Cycler cancels before
// returning, so runOnce reports the interruption itself); the narrower window
// between runOnce's check and recordRunHealth's has no test seam, so
// recordRunHealth's own ctx.Err() check is the only guard on it. What the
// interruption never does is reach BACK: a verdict already published inside the
// cycle lock - an earlier run of this invocation, or a daemon tick's - stands,
// because a completed cycle's health is not this process's to withdraw.
// The daemon tick deliberately differs in the boundary case: RunLoop publishes
// a healthy verdict even when the cancellation is already visible, withholding
// only an UNHEALTHY interrupted cycle. The cancellation cause rides along as a
// second %w so the message still names the signal ("terminated signal
// received"), never as the classification token: a cause is whatever the
// cancelling site passed to context.WithCancelCause, so only ctx.Err() is
// guaranteed to be context.Canceled. (signal.NotifyContext's own signalError
// does satisfy errors.Is(_, context.Canceled) - golang/go#77639, backported to
// Go 1.26 in #79499 - which is what keeps the net/http errors this app
// classifies classifiable at all, since net/http reports a cancelled request as
// context.Cause(ctx).)
func Interrupted(ctx context.Context) error {
	return fmt.Errorf("poll interrupted: %w (cause: %w)", ctx.Err(), context.Cause(ctx))
}

// IsShutdownError reports whether err is the observable form of THIS context's
// cancellation, so a terminal boundary can classify it as a routine shutdown
// (WARN) instead of a fault (the level=ERROR cycle-error alert). It proves the
// match rather than assuming it: a cancelled context alone is not enough
// (an unrelated coincident fault must still read as a fault), so err must
// carry either the stable ctx.Err() or the cancellation cause. Matching the
// cause is what keeps a dependency that surfaces context.Cause(ctx) verbatim
// (net/http does, and a WithCancelCause cause need not wrap context.Canceled)
// classifiable at all.
func IsShutdownError(ctx context.Context, err error) bool {
	if ctx.Err() == nil || err == nil {
		return false
	}
	return errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))
}

// NormalizeShutdownError adds the stable ctx.Err() as the classification token
// to an error that IS this context's cancellation but does not carry
// context.Canceled itself (a cause-only form), so the root's single
// errors.Is(err, context.Canceled) check classifies it as a routine shutdown.
// Anything else - a nil error, one that already carries ctx.Err(), or a
// genuine fault that merely happened to land while the context was cancelled -
// is returned untouched.
func NormalizeShutdownError(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, ctx.Err()) || !IsShutdownError(ctx, err) {
		return err
	}
	return fmt.Errorf("%w (cause: %w): %w", ctx.Err(), context.Cause(ctx), err)
}

// runOnce is one execution of RunOnce's cycle body under the cycle lock: run the
// cycle and apply the interruption contract (a cancellation observed at any
// point - even when the cycle still managed to complete healthy, e.g. the
// signal landed during the end-of-cycle save - is reported as an interruption,
// since an interrupted run's outcome is not a trustworthy health verdict). It
// only REPORTS the cycle's health verdict; committing it to the shared marker
// is recordRunHealth's job, called immediately after this returns and still
// INSIDE the locked body (see recordRunHealth for why the lock must cover it).
// The active runner may execute this body again for demand queued by other
// processes; each execution records its own verdict, so the marker always
// reflects the last cycle that actually completed.
func runOnce(ctx context.Context, sc Cycler) (healthy bool, err error) {
	healthy = runCycle(ctx, sc)
	if ctx.Err() != nil {
		return healthy, Interrupted(ctx)
	}
	if !healthy {
		return healthy, errors.New("compare cycle failed (library ingest)")
	}
	return healthy, nil
}

// executeRuns runs the cycle body under the cycle lock and captures the
// first execution's outcome - this invocation's own run - plus the run count.
// Each execution commits its OWN health verdict inside the locked body
// (recordRunHealth), so no verdict outlives the lock that ordered the cycle
// producing it. The closure returns nil to Exclusive so exErr stays purely a
// coordination-infrastructure signal (job outcomes must not stop queued demand
// or muddy RunOnce's infra-error accounting). The closure can run again for
// demand queued by OTHER processes; a rerun has no exit code to report through,
// so its business error is logged here - without that line a shutdown observed
// mid-rerun would vanish (the cycle's own faults already log inside Cycle).
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
// The lock must cover the write. The marker is cross-process shared state like
// state.json and feed.json, and `cycle.lock` is what orders every writer of
// those - but Exclusive releases the lock before Run returns (runHolding
// unlocks, then re-checks for queued demand), so a verdict committed after Run
// is committed unordered: a newer cycle run by a daemon tick or another poll
// process can publish in between and then be overwritten by this older,
// already-superseded verdict, leaving the marker reporting the opposite of the
// latest completed cycle with a freshness timestamp out of cycle order. The
// daemon tick never had this problem - it always wrote inside RunOrSkip's
// closure - so committing here restores parity between the two entry points
// that share the marker.
//
// An INTERRUPTED cycle records nothing - including a Cycler that returned
// healthy before the cancellation was observed here: a result the shutdown
// reached first is not one this process publishes, so the marker keeps whatever
// the last published verdict was. The cancellation check is on the CONTEXT, not
// only on the cycle error: IsShutdownError deliberately answers false for a nil
// error, so a cancellation that lands after runOnce returned a healthy nil
// result but before this recording boundary would otherwise go unobserved here
// and publish a healthy marker that RunOnce then reports as an interruption. A
// cycle error that already carries the shutdown is returned unchanged;
// otherwise the package's uniform interruption result is synthesized. Once a
// verdict HAS been published, a later shutdown does not withdraw it - RunOnce's
// final check governs this invocation's exit code, not the marker.
//
// Write-failure reporting preserves runOnce's former semantics: on this
// invocation's OWN run the failure becomes the process result (it outranks an
// unhealthy cycle's ingest error and is the write's only report); on a queued
// rerun serviced for ANOTHER process it has no exit code to surface through, so
// it is logged and the run's own error is returned unchanged.
func recordRunHealth(ctx context.Context, marker *health.Marker, healthy bool, runs int, cycleErr error) error {
	if ctx.Err() != nil {
		if IsShutdownError(ctx, cycleErr) {
			return cycleErr
		}
		return Interrupted(ctx)
	}
	err := marker.SetChecked(healthy)
	if err == nil {
		return cycleErr
	}
	err = fmt.Errorf("%w: %w", errRecordPollHealth, err)
	if runs > 1 {
		// A health-marker write failure is NOT the same class as a queued
		// rerun's cycle error: a cycle fault already logs its own ERROR inside
		// Cycle, whereas this write is the marker's only report, it has no exit
		// code to surface through (the verdict came from another process's
		// queued run), and it does not self-heal - a full disk or a bad mode on
		// /tmp keeps failing until the operator acts. In external mode the
		// probe runs WithMaxAge(0), so a marker stuck stale by failing writes
		// never turns the container unhealthy either. ERROR is the only signal
		// this fault gets.
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
		slog.Warn("cycle coordination error after queueing; demand stands", "error", err)
	case scheduler.OutcomeRan, scheduler.OutcomeRanQueued, scheduler.OutcomeSkipped:
		slog.Warn(msgCoordErrorAfterRun, "error", err)
	default:
		slog.Warn("cycle coordination failed during shutdown", "error", err)
	}
}

// nonRunResult maps the coalescing outcomes that end a RunOnce WITHOUT an
// own run to its exit contract: Queued/Discarded log success (any
// coordination error is a stands-anyway diagnostic) and Gated applies the
// uniform interruption contract. It reports handled=false for every outcome
// that falls through to RunOnce's ran/own accounting (None, Ran, RanQueued,
// and the queue-mode-unreachable Skipped).
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
		return true, Interrupted(ctx)
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
//     interrupted one non-zero (see runOnce). The closure can run again for
//     demand queued by OTHER processes; those cycles report through their own
//     log lines and publish their own marker verdict as they complete - never
//     through this process's exit code.
//   - Queued / Discarded: a cycle is already in flight (an overlapping poll or
//     a daemon tick); the request was recorded for (or is already covered by)
//     the active runner, which is owed to start a run after it arrived. That
//     is success for this process: log and exit 0, marker untouched (the
//     active runner's cycle records its own outcome).
//   - Gated: shutdown was signalled before the run started - the uniform
//     interruption contract applies (exit non-zero, WARN classification,
//     marker untouched).
//   - Nothing ran and no demand recorded (a cycle-lock infrastructure
//     failure): exit non-zero with the error.
//
// Whatever the outcome, a cancellation observed by the time Run returns wins
// for THIS PROCESS'S RESULT: the uniform interruption contract applies (exit
// non-zero, WARN classification) even when this process's own run completed,
// because Exclusive can spend post-run time servicing another process's queued
// rerun and shutdown can land there. It does not reach back into the marker:
// any verdict already published inside the locked body stands (recordRunHealth),
// where the cycle lock orders it against every other writer, and a published
// verdict is not this invocation's to withdraw. A path where no verdict was
// published leaves the marker untouched.
func RunOnce(ctx context.Context, ex *scheduler.Exclusive, sc Cycler, marker *health.Marker) error {
	// A pre-cancelled invocation must not enqueue demand: Exclusive's gate
	// refuses the RUN, not the queue insertion, so with the lock held by
	// another process a cancelled poll would still queue a rerun and report
	// success, adding work after shutdown was signalled. The uniform
	// interruption contract applies instead (exit non-zero, marker untouched).
	if ctx.Err() != nil {
		return Interrupted(ctx)
	}
	outcome, runs, own, exErr := executeRuns(ctx, ex, sc, marker)
	if ctx.Err() != nil {
		// Cancellation observed by the time Run returns wins over EVERY
		// outcome: while Run coordinated with a busy owner (Queued/Discarded)
		// or while Exclusive spent post-run time servicing another process's
		// queued rerun after this invocation's own run completed
		// (RanQueued). In all of them this invocation reports the uniform
		// interruption contract (exit non-zero, WARN classification, marker
		// untouched by this return) rather than a success or own-run result
		// observed after shutdown was signalled; any recorded demand still
		// stands for the active runner.
		if exErr != nil {
			// Outcome-specific diagnostics: "demand stands" is only true
			// when demand was actually recorded (Queued/Discarded); after a
			// run the error is post-run bookkeeping; and OutcomeNone means
			// Exclusive failed BEFORE recording demand, so no demand stands.
			warnCoordinationError(outcome, exErr)
		}
		if own != nil && !errors.Is(own, context.Canceled) {
			// The interruption replaces this invocation's own result, so the
			// own run's error has no exit code left to report through. Same
			// reasoning executeRuns applies to a queued rerun's error. An
			// already-interrupted own result is skipped: Interrupted below
			// reports it.
			if errors.Is(own, errRecordPollHealth) {
				// A marker-WRITE failure needs an operator; every other own
				// error either self-heals or already logged its own ERROR
				// inside Cycle. Without this line the displaced result would
				// reduce a permanent health-freshness fault to a shutdown WARN
				// and the ERROR-keyed alert would never fire.
				slog.Error("own cycle could not record poll health before shutdown", "error", own)
			}
			slog.Warn("own cycle reported an error before shutdown", "error", own)
		}
		return Interrupted(ctx)
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
// Each tick body runs under the cross-process cycle lock in skip mode: a tick
// arriving while a cycle is already in flight (an exec'd `poll` racing the
// loop) is skipped with a WARN and the marker untouched - the next tick
// provides freshness, and the in-flight cycle records its own outcome. An
// acquired tick also executes demand queued by `poll` requests that arrived
// during it (one rerun per queued request), each recording its own health.
// An unusable cycle LOCK means the tick could not run at all and is logged at
// ERROR - cycles have stopped, which the operator must see (the level=ERROR
// Loki alert fires). A queue-file failure is different: it is only observable
// after the tick's cycle already ran, so it degrades the demand-coalescing
// bookkeeping only and stays a WARN, keeping a broken queue file from firing
// the cycle-error alert on every tick (TestRunLoopQueueErrorAfterRun).
func RunLoop(ctx context.Context, interval time.Duration, ex *scheduler.Exclusive, sc Cycler, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		outcome, err := ex.RunOrSkip(func() error {
			healthy := runCycle(ctx, sc)
			if !healthy && ctx.Err() != nil {
				return nil // shutdown mid-cycle: cancellation is not an ingest fault
			}
			if err := marker.SetChecked(healthy); err != nil {
				// The tick has no exit code to report the write through, and a
				// marker-write failure does not self-heal (a full disk, a bad
				// mode on /tmp): ERROR is the only signal it gets, exactly as
				// recordRunHealth argues for a queued rerun's write. Without it
				// a wedged marker restarts the container at
				// WithMaxAge(3*poll_interval) with no logged cause.
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
			slog.Warn(msgCoordErrorAfterRun, "outcome", outcome.String(), "error", err)
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
// crash the long-lived daemon. A panic is reported as an unhealthy cycle.
func runCycle(ctx context.Context, sc Cycler) (healthy bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("cycle panicked", "panic", r, "stack", string(debug.Stack()))
			healthy = false
		}
	}()
	return sc.Cycle(ctx)
}
