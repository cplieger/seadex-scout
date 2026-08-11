package cycle

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/health"
)

// healthLeaseFactor is how many poll intervals of silence the health marker
// tolerates before the probe calls the compare loop wedged.
const healthLeaseFactor = 3

// coldReconcileAllowance is the floor on the marker freshness lease: the time a
// FULL pass is allowed to take before the probe may call the loop wedged.
//
// It is a measured allowance, not a guess. A cold reconcile - no state.json, so
// the AniList memo is built from scratch over the whole catalogue - has been
// measured at ~25 minutes and historically ran to ~2h on a large library. 3h
// carries that with margin, and
// TestColdReconcileAllowanceCoversAMeasuredColdReconcile is what pins it: the
// arithmetic tests state their expectations in terms of this constant, so only a
// test against the MEASUREMENT can defend the number itself.
//
// It exists as its own constant because the thing it has to cover stopped being
// the same thing as the poll interval. The lease used to floor on
// config.DefaultPollInterval, which was correct while that default WAS the full
// cycle's cadence: flooring at 3 x 3h gave a cold cycle 9h. The default is now
// the TICK interval (15m), so that floor would give a cold reconcile 45 minutes
// and then ask for the restart that kills the walk before the memo is saved -
// making the next boot cold again, which is the exact self-defeating loop the
// old floor was written to prevent. Same failure, same remedy, different
// number to peg it to.
const coldReconcileAllowance = 3 * time.Hour

// WatchdogLease is the freshness lease the DAEMON arms against its own interval:
// 0 (no watchdog) in external mode, else healthLeaseFactor intervals, floored at
// coldReconcileAllowance. It used to be armed by the health subcommand from a
// config read; the daemon owns it now because the daemon is what knows the
// cadence, and a healthcheck must not depend on a file the operator can make
// unreadable (see the root's runHealthProbe).
//
// The lease exists because the marker is refreshed only when a pass COMPLETES,
// and the loop measures its delay AFTER the job returns, so the gap between two
// refreshes is one pass plus up to 1.1 intervals (scheduler's 0.10 jitter).
//
// In steady state that gap is a TICK - one small request, or one tiny one when
// nothing changed - so healthLeaseFactor*interval alone would be a tight wedge
// deadline. It is not the binding constraint at the default cadence, and the
// floor is why: 3x15m is 45 minutes, which a cold reconcile plus one interval of
// loop delay does not fit.
//
// The floor covers the one pass that is not cheap. Every reconcileEvery-th
// iteration is a full pass, and the FIRST iteration after any boot is one, so a
// cold boot has no tick-refreshed marker to lean on. Taking the larger of the
// two keeps the deployed 3h interval on exactly its old 9h lease while giving a
// 15m interval 3h rather than 45m. The consequence to be honest about: at the
// 15m default the wedge deadline IS the floor, so a genuinely wedged loop is
// caught by alerts.yaml's stall rule (2h, from the completion lines) well before
// this marker expires. The marker's job here is to stop a restart loop, not to
// be the fastest wedge detector.
//
// What moving the lease into the daemon gives up, stated plainly: a probe-side
// deadline also caught a FULLY HUNG process, because a stale mtime is visible
// from outside even when nothing inside the container runs, whereas the watchdog
// goroutine is as wedged as everything else in that case. That case is already
// owned by the layer that watches it - alerts.yaml's deadman rules fire on log
// SILENCE, which a hung process produces - while the case this removes (an
// unreadable config silently disabling wedge detection, reported only into
// Docker's health log) was live and invisible.
//
// The rejected alternative is still rejected: refreshing the marker mid-pass
// would make the lease measure liveness rather than completion, which changes
// what the marker MEANS (this package publishes a verdict per completed pass)
// for a case a floor covers without touching the contract.
func WatchdogLease(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return max(healthLeaseFactor*interval, coldReconcileAllowance)
}

// watchdogPollDivisor sets how often the watchdog re-checks the marker's age,
// as a fraction of the lease. Checking at a fraction rather than at the lease
// itself bounds how long a wedge stays unreported to lease + lease/divisor.
const watchdogPollDivisor = 6

// StartWedgeWatchdog marks the container unhealthy when no pass has completed
// within lease. It returns a func that stops the watchdog and waits for its
// goroutine to exit; calling it more than once is safe.
//
// This is the wedge detection that used to live in the health subcommand as
// health.WithMaxAge, sized from a config read. It belongs beside the cycle
// coordination: the daemon already knows its own cadence, so nothing needs to be
// re-derived from a file the operator can make unreadable, and the healthcheck
// collapses to a pure marker read. Every completed pass calls marker.Set(true)
// (this package's per-pass verdict), which refreshes the marker's mtime, so "age
// since the last completed pass" is exactly what this measures - the same signal
// the probe-side lease measured, read from the inside.
//
// A zero lease disables it, which is external mode (poll_interval: off):
// idle-until-poll is healthy, and there is no cadence to be late against.
//
// It only ever sets the marker FALSE. Recovery stays the cycle's job, because
// the marker's meaning is a completed pass's verdict and a watchdog has not
// completed a pass - clearing it here would claim progress that did not happen.
func StartWedgeWatchdog(ctx context.Context, marker *health.Marker, path string, lease time.Duration) func() {
	if lease <= 0 {
		return func() {}
	}
	// The watchdog runs on its OWN cancellable child of ctx, so the returned
	// func can STOP it rather than only wait for it: ctx also drives the cycle
	// loop, so a caller that stops the watchdog before that shared context is
	// cancelled would otherwise block forever on a goroutine nothing had asked
	// to exit.
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchMarker(watchCtx, marker, path, lease)
	}()
	return func() {
		stop()
		<-done
	}
}

// watchMarker is StartWedgeWatchdog's loop body: it re-checks the marker's age
// every lease/watchdogPollDivisor and marks unhealthy once the lease is past.
func watchMarker(ctx context.Context, marker *health.Marker, path string, lease time.Duration) {
	tick := time.NewTicker(lease / watchdogPollDivisor)
	defer tick.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			warned = checkMarkerAge(marker, path, lease, warned)
		}
	}
}

// checkMarkerAge marks the container unhealthy when the marker is older than
// lease, returning the new warned state so the diagnostic is emitted once per
// wedge rather than once per tick (a wedged loop stays wedged, and this line
// exists to name the cause, not to count it).
//
// The reading is health.Inspect's, not a local stat: the library computes the
// age and classifies the marker for its own probe, and re-deriving that here was
// a second implementation of one decision (health v1.5.0 exposes it for exactly
// this caller). What this function keeps is the POLICY - which state is a wedge,
// what to log, and that only the cycle may clear the marker.
//
// Only MarkerStale is a wedge. Absent, unreadable and degraded are deliberately
// not this goroutine's to interpret: an absent marker is what Set(false) looks
// like on some failure paths, the probe already reads all three, and treating
// absence as a wedge would call a cold start one. That distinction is the reason
// this reads Inspect rather than ProbeCheck, whose 0-or-1 cannot express it.
func checkMarkerAge(marker *health.Marker, path string, lease time.Duration, warned bool) bool {
	f := health.Inspect(path, health.WithMaxAge(lease))
	switch f.State {
	case health.MarkerFresh:
		return false
	case health.MarkerStale:
		if !warned {
			slog.Error("no cycle has completed within the health lease; marking unhealthy",
				"age", f.Age.Round(time.Second), "lease", f.MaxAge)
		}
		marker.Set(false)
		return true
	case health.MarkerAbsent, health.MarkerUnreadable, health.MarkerDirUnavailable:
		return warned
	}
	return warned
}
