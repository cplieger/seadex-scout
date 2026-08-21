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
// It is a measured allowance, not a guess.
const coldReconcileAllowance = 3 * time.Hour

// WatchdogLease is the freshness lease the DAEMON arms against its own interval:
// 0 (no watchdog) in external mode, else healthLeaseFactor intervals, floored at
// coldReconcileAllowance.
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
func StartWedgeWatchdog(ctx context.Context, marker *health.Marker, path string, lease time.Duration) func() {
	if lease <= 0 {
		return func() {}
	}
	// The watchdog runs on its OWN cancellable child of ctx, so the returned func can
	// STOP it rather than only wait for it: ctx also drives the cycle loop, so stopping
	// the watchdog earlier would otherwise block forever.
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
