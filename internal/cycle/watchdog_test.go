package cycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/slogx/capture"
)

// TestWatchdogLease pins the daemon's freshness lease: external mode arms no
// watchdog, an interval well above the floor keeps the documented
// three-interval lease (a 3h interval still restarts a wedged loop at 9h), and
// a shorter interval is floored at coldReconcileAllowance - the marker is
// refreshed only when a pass COMPLETES, so a tighter deadline would call a
// slow-but-healthy cold reconcile wedged and restart it before it can persist
// its memo.
//
// The lease is stated in terms of this package's own constants, so the test
// carries no app-package dependency; the coupling to the CONFIG's default
// cadence is a wiring fact and is pinned in the composition root's test, beside
// the call that arms this lease from cfg.PollInterval.
func TestWatchdogLease(t *testing.T) {
	for name, tc := range map[string]struct {
		interval time.Duration
		want     time.Duration
	}{
		"external mode disables the watchdog": {0, 0},
		"a negative interval disables it too": {-time.Hour, 0},
		// The deployed 3h interval must keep exactly the 9h lease it had before
		// the tick/reconcile split: the 3x arm wins there, unchanged.
		"the deployed interval keeps 3x": {3 * time.Hour, 9 * time.Hour},
		"a long interval scales":         {12 * time.Hour, 36 * time.Hour},
		// The cold-reconcile floor wins for every interval at or below one third
		// of it, which now includes the tick cadence the default ships at: 3x15m
		// is 45 minutes, and a cold reconcile has been measured well past that.
		// Flooring here is what stops the watchdog demanding the restart that
		// makes the next boot cold again.
		"a tick-cadence interval takes the floor": {15 * time.Minute, coldReconcileAllowance},
		"the config floor takes the floor":        {5 * time.Minute, coldReconcileAllowance},
		"the crossover point":                     {coldReconcileAllowance / healthLeaseFactor, coldReconcileAllowance},
		"just above the crossover scales":         {coldReconcileAllowance/healthLeaseFactor + time.Minute, coldReconcileAllowance + healthLeaseFactor*time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			if got := WatchdogLease(tc.interval); got != tc.want {
				t.Errorf("WatchdogLease(%v) = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}

// TestColdReconcileAllowanceCoversAMeasuredColdReconcile pins the NUMBER, not
// just the arithmetic around it.
//
// TestWatchdogLease states its expectations in terms of coldReconcileAllowance
// itself, so it passes for any value large enough to beat the 3x arm - including
// the 1h the design rejected by name. The whole argument for this constant is
// that it carries a MEASURED cold pass with margin, so the measurement is what
// the test has to assert against.
func TestColdReconcileAllowanceCoversAMeasuredColdReconcile(t *testing.T) {
	// measuredColdReconcile is the observed worst case on a large library (the
	// typical cold pass is ~25 minutes). The lease has to survive it plus the
	// loop's own delay, or the probe restarts the walk before it can persist the
	// AniList memo - which makes the next boot cold again.
	const measuredColdReconcile = 2 * time.Hour
	if coldReconcileAllowance <= measuredColdReconcile {
		t.Errorf("coldReconcileAllowance = %v, want more than the measured %v cold reconcile it exists to cover",
			coldReconcileAllowance, measuredColdReconcile)
	}
}

// TestStartWedgeWatchdog covers the wedge detection that moved out of the health
// subcommand: the daemon marks itself unhealthy when no pass has completed within
// the lease, so the probe needs no config and cannot be broken by one.
//
// It uses a real marker on a temp path with a tiny lease rather than a clock seam:
// the behaviour under test is "did it notice a stale mtime", and mtime is the
// signal, so faking time would test the fake.
func TestStartWedgeWatchdog(t *testing.T) {
	// staleMarker returns a healthy marker whose mtime is an hour old - exactly
	// what a wedged loop looks like from outside, since only a COMPLETED pass
	// refreshes it.
	staleMarker := func(t *testing.T) (*health.Marker, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".healthy")
		marker := health.NewMarker(path)
		marker.Set(true)
		if !marker.Healthy() {
			t.Fatal("marker not healthy after Set(true)")
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("backdating the marker: %v", err)
		}
		return marker, path
	}
	const wedgeMsg = "no cycle has completed within the health lease; marking unhealthy"

	t.Run("a zero lease disables it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".healthy")
		stop := StartWedgeWatchdog(t.Context(), health.NewMarker(path), path, 0)
		stop() // must not block: external mode arms no goroutine
	})

	t.Run("a stale marker is marked unhealthy and named once", func(t *testing.T) {
		rec := capture.Default(t)
		marker, path := staleMarker(t)

		if !checkMarkerAge(marker, path, time.Minute, false) {
			t.Error("checkMarkerAge(stale, warned=false) = false, want true (the wedge must latch)")
		}
		if marker.Healthy() {
			t.Error("marker still healthy after a stale check; docker would never restart a wedged loop")
		}
		if n := rec.Count(wedgeMsg); n != 1 {
			t.Errorf("wedge ERROR count = %d, want 1 (the operator needs the cause named): %v", n, rec.Records())
		}
	})

	t.Run("an already-warned wedge is not re-logged", func(t *testing.T) {
		rec := capture.Default(t)
		marker, path := staleMarker(t)

		if !checkMarkerAge(marker, path, time.Minute, true) {
			t.Error("checkMarkerAge(stale, warned=true) = false, want true (the latch must stay set)")
		}
		if marker.Healthy() {
			t.Error("marker still healthy on a re-observed wedge; the unhealthy verdict must be re-applied every tick")
		}
		if n := rec.Count(wedgeMsg); n != 0 {
			t.Errorf("wedge ERROR count = %d, want 0; a wedged loop stays wedged and this line names the cause once, "+
				"not once per lease/%d tick", n, watchdogPollDivisor)
		}
	})

	t.Run("a fresh marker clears the latch and stays healthy", func(t *testing.T) {
		rec := capture.Default(t)
		path := filepath.Join(t.TempDir(), ".healthy")
		marker := health.NewMarker(path)
		marker.Set(true)

		if checkMarkerAge(marker, path, time.Hour, true) {
			t.Error("checkMarkerAge(fresh, warned=true) = true, want false (a completed pass clears the latch, so the next wedge is named again)")
		}
		if !marker.Healthy() {
			t.Error("a fresh marker was marked unhealthy; the watchdog must not restart a healthy loop")
		}
		if n := rec.Count(wedgeMsg); n != 0 {
			t.Errorf("wedge ERROR count = %d, want 0 for a fresh marker", n)
		}
	})

	t.Run("a marker the watchdog cannot age is left to the probe", func(t *testing.T) {
		rec := capture.Default(t)
		// Two distinct states the new reading tells apart and the old age-or-error
		// helper could not: ABSENT and UNREADABLE. Neither is a wedge - an absent
		// marker is what Set(false) looks like on some failure paths, and the probe
		// already reads both as unhealthy - so the watchdog must pass its latch
		// through untouched and stay silent for each.
		dir := t.TempDir()
		loop := filepath.Join(dir, "loop")
		if err := os.Symlink(loop, loop); err != nil {
			t.Logf("symlink loop unsupported here (%v); covering the absent case only", err)
			loop = ""
		}
		paths := map[string]string{"absent": filepath.Join(dir, "nope")}
		if loop != "" {
			paths["unreadable"] = loop
		}
		for state, path := range paths {
			t.Run(state, func(t *testing.T) {
				marker := health.NewMarker(path)
				for _, warned := range []bool{false, true} {
					if got := checkMarkerAge(marker, path, time.Minute, warned); got != warned {
						t.Errorf("checkMarkerAge(%s, warned=%t) = %t, want %t (not this goroutine's to interpret)", state, warned, got, warned)
					}
				}
			})
		}
		if n := rec.Count(wedgeMsg); n != 0 {
			t.Errorf("wedge ERROR count = %d, want 0; a marker that cannot be aged already probes unhealthy and must not be reported as a wedge", n)
		}
	})

	t.Run("the goroutine marks a wedged loop unhealthy and stops", func(t *testing.T) {
		capture.Default(t)
		marker, path := staleMarker(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// lease/watchdogPollDivisor is the re-check period, so a 60ms lease
		// checks every 10ms against an hour-old marker.
		stop := StartWedgeWatchdog(ctx, marker, path, 60*time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for marker.Healthy() {
			if time.Now().After(deadline) {
				t.Fatal("the watchdog never marked a wedged loop unhealthy")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		stop() // must return: the loop exits on ctx.Done
	})
}
