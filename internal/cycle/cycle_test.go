package cycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/slogx/capture"
)

// panicCycler is a Cycler whose cycle always panics, exercising the daemon
// panic shield in runCycle.
type panicCycler struct{}

func (panicCycler) Cycle(context.Context) bool { panic("boom") }

// boolCycler is a Cycler returning a fixed health outcome.
type boolCycler bool

func (b boolCycler) Cycle(context.Context) bool { return bool(b) }

// TestRunCyclePanicShield pins the daemon crash shield: a panicking cycle is
// recovered and reported unhealthy instead of crashing the long-lived daemon,
// and a normal cycle outcome passes through unchanged.
func TestRunCyclePanicShield(t *testing.T) {
	ctx := context.Background()
	if healthy := runCycle(ctx, panicCycler{}); healthy {
		t.Error("runCycle(panicking cycle) = healthy, want unhealthy")
	}
	if healthy := runCycle(ctx, boolCycler(true)); !healthy {
		t.Error("runCycle(healthy cycle) = unhealthy, want healthy")
	}
	if healthy := runCycle(ctx, boolCycler(false)); healthy {
		t.Error("runCycle(unhealthy cycle) = healthy, want unhealthy")
	}
}

// TestRunCyclePanicIsLoggedAtError pins the panic shield's operator signal, the
// only report a swallowed panic gets: recovering the panic keeps the daemon
// alive, so without the ERROR line (the level alerts.yaml's
// SeadexScoutCycleError rule keys on, and which its description names as "a
// panicked run") a cycle panicking on every tick would be invisible beyond the
// health flip. The panic value and the stack ride along as attributes because
// they are the only diagnosis a recovered panic leaves. Serial (capture swaps
// slog.Default).
func TestRunCyclePanicIsLoggedAtError(t *testing.T) {
	rec := capture.Default(t)

	if healthy := runCycle(context.Background(), panicCycler{}); healthy {
		t.Error("runCycle(panicking cycle) = healthy, want unhealthy")
	}

	if got := rec.CountLevel(slog.LevelError, "cycle panicked"); got != 1 {
		t.Fatalf("cycle-panicked ERROR count = %d, want 1: %v", got, rec.Messages())
	}
	if !rec.HasAttr("cycle panicked", "panic", "boom") {
		t.Errorf("panic value attr missing; a recovered panic must name what panicked: %v", rec.Records())
	}
	if !rec.AttrContains("cycle panicked", "stack", "runCycle") {
		t.Errorf("stack attr missing the panicking frame; it is the only diagnosis left: %v", rec.Records())
	}
}

// cancelCycler cancels the poll context during the cycle and returns the
// configured outcome, simulating a shutdown signal landing mid-cycle (cycle
// reports unhealthy) or during the end-of-cycle save (cycle still completed
// healthy).
type cancelCycler struct {
	cancel  context.CancelFunc
	healthy bool
}

func (c cancelCycler) Cycle(context.Context) bool {
	c.cancel()
	return c.healthy
}

// testExclusiveIn builds a cycle coalescer for tests in the given dir,
// wired exactly like production (NewExclusive, including the shutdown
// gate on ctx), so the tests exercise the real gate and lock wiring. Takes
// the dir explicitly so lock-contention tests can share it with holdCycleLock
// or a seeded queue file.
func testExclusiveIn(t *testing.T, ctx context.Context, dir string) *scheduler.Exclusive {
	t.Helper()
	ex, err := NewExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("NewExclusive: %v", err)
	}
	return ex
}

// testExclusive builds a cycle coalescer for tests in a temp dir, wired
// exactly like production (NewExclusive, including the shutdown gate on
// ctx), so the tests exercise the real gate and lock wiring.
func testExclusive(t *testing.T, ctx context.Context) *scheduler.Exclusive {
	t.Helper()
	return testExclusiveIn(t, ctx, t.TempDir())
}

// seedSentinelMarker writes a pre-existing health marker standing in for the
// daemon's last real state and returns its path; assertMarkerUntouched is its
// paired check that the code under test never touched it.
func seedSentinelMarker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".healthy")
	if err := os.WriteFile(path, []byte("sentinel-untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertMarkerUntouched fails the test when the marker content no longer
// matches the seeded sentinel, i.e. the code under test touched the marker.
func assertMarkerUntouched(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("marker file: %v", err)
	}
	if string(got) != "sentinel-untouched" {
		t.Errorf("marker content = %q, want the pre-existing state untouched", got)
	}
}

// assertMarkerPublished fails the test unless the marker reflects a completed
// cycle's verdict rather than the seeded sentinel: healthy replaces the file
// with health's own empty marker, unhealthy removes it (health.applyState).
func assertMarkerPublished(t *testing.T, path string, healthy bool) {
	t.Helper()
	got, err := os.ReadFile(path)
	switch {
	case healthy && err != nil:
		t.Fatalf("marker file after a healthy cycle: %v, want the marker present", err)
	case healthy && string(got) == "sentinel-untouched":
		t.Errorf("marker still holds the sentinel, want the completed cycle's healthy verdict")
	case !healthy && err == nil:
		t.Errorf("marker content = %q, want it removed by the completed cycle's unhealthy verdict", got)
	case !healthy && !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("marker file after an unhealthy cycle: %v, want fs.ErrNotExist", err)
	}
}

// TestRunOnceUniformInterruption pins poll's uniform interruption contract:
// a cancellation observed at ANY phase - before the cycle starts (the shutdown
// gate refuses the run), mid-cycle, or after a cycle that still completed
// healthy (the signal landed during the save) - returns an error wrapping
// context.Canceled (which main classifies as a routine-shutdown WARN, never
// ERROR, and maps to exit 1) and never touches the health marker, leaving it
// at the daemon's last real state.
func TestRunOnceUniformInterruption(t *testing.T) {
	tests := []struct {
		cycler    func(t *testing.T, cancel context.CancelFunc) Cycler
		name      string
		preCancel bool
	}{
		{func(t *testing.T, _ context.CancelFunc) Cycler { return mustNotRunCycler{t: t} }, "pre-cycle cancellation", true},
		{func(_ *testing.T, cancel context.CancelFunc) Cycler { return cancelCycler{cancel: cancel} }, "mid-cycle cancellation", false},
		{func(_ *testing.T, cancel context.CancelFunc) Cycler {
			return cancelCycler{cancel: cancel, healthy: true}
		}, "post-cycle cancellation after a healthy cycle", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ex := testExclusive(t, ctx)
			if tt.preCancel {
				cancel()
			}
			// A pre-existing marker stands in for the daemon's last real state;
			// its content must survive the interrupted poll byte-for-byte.
			path := seedSentinelMarker(t)

			err := RunOnce(ctx, ex, tt.cycler(t, cancel), health.NewMarker(path))

			if err == nil {
				t.Fatal("RunOnce = nil, want the interruption error (exit 1)")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
			}
			assertMarkerUntouched(t, path)
		})
	}
}

// TestRunOnceBusyLockPreCancelled pins the queue-insertion side of poll's
// uniform interruption contract: a poll arriving pre-cancelled while another
// process holds the cycle lock must NOT enqueue demand or report success —
// Exclusive's gate refuses the run, not the queue insertion, so without the
// pre-Run check the cancelled poll would still queue work after shutdown. It
// returns the interruption error (wrapping context.Canceled, so main
// classifies it WARN and exits non-zero), never runs a cycle, leaves the
// health marker untouched, and records no pending rerun for the lock holder.
func TestRunOnceBusyLockPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)
	path := seedSentinelMarker(t)

	err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(path))

	if err == nil {
		t.Fatal("RunOnce = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
	}
	assertMarkerUntouched(t, path)
	if pending, perr := ex.Pending(); perr != nil || pending != 0 {
		t.Errorf("Pending() = (%d, %v), want (0, nil): a cancelled poll must not enqueue demand", pending, perr)
	}
}

// TestRunOnceGatedRun pins the OutcomeGated leg of poll's uniform
// interruption contract: shutdown lands in the race window between
// RunOnce's pre-Run check and the Exclusive's gate evaluation (simulated
// deterministically by a gate that cancels the shared context exactly when
// it is consulted, mirroring NewExclusive's ctx.Err()==nil gate). The
// run is refused (the cycle never executes), RunOnce reports the
// interruption (wrapping context.Canceled so main classifies it WARN and
// exits non-zero), and the health marker is left at the daemon's last real
// state.
func TestRunOnceGatedRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := scheduler.NewExclusive(t.TempDir(), slog.Default(),
		scheduler.WithGate(func() bool {
			cancel() // shutdown arrives exactly as the gate is consulted
			return ctx.Err() == nil
		}))
	path := seedSentinelMarker(t)

	err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(path))

	if err == nil {
		t.Fatal("RunOnce(gated run) = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
	}
	assertMarkerUntouched(t, path)
	if pending, perr := ex.Pending(); perr != nil || pending != 0 {
		t.Errorf("Pending() = (%d, %v), want (0, nil): a gated fresh acquisition must not leave queued demand", pending, perr)
	}
}

// TestRunOnceUninterrupted pins poll's normal contract: a healthy cycle sets
// the marker healthy and exits 0; an unhealthy cycle sets it unhealthy and
// returns the ingest error (exit 1) without reading as an interruption.
func TestRunOnceUninterrupted(t *testing.T) {
	t.Run("healthy cycle sets the marker", func(t *testing.T) {
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		if err := RunOnce(context.Background(), testExclusive(t, context.Background()), boolCycler(true), marker); err != nil {
			t.Fatalf("RunOnce(healthy) = %v, want nil", err)
		}
		if !marker.Healthy() {
			t.Error("marker not healthy after a healthy cycle")
		}
	})
	t.Run("unhealthy cycle sets the marker and errors", func(t *testing.T) {
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		err := RunOnce(context.Background(), testExclusive(t, context.Background()), boolCycler(false), marker)
		if err == nil {
			t.Fatal("RunOnce(unhealthy) = nil, want the ingest error")
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, must not read as an interruption", err)
		}
		if marker.Healthy() {
			t.Error("marker healthy after an unhealthy cycle")
		}
	})
}

// queueThenCancelCycler drives the ran-plus-queued-rerun interruption leg:
// its first (own) run queues demand from a second process on the shared lock
// dir, and the queued rerun then cancels the shared context, simulating
// shutdown arriving while Exclusive services another process's demand after
// this invocation's own run completed. ownUnhealthy makes that own run report
// an ingest failure, the reachable non-cancellation own error whose only
// report is RunOnce's shutdown-wins WARN.
type queueThenCancelCycler struct {
	t            *testing.T
	cancel       context.CancelFunc
	dir          string
	calls        *int
	queuedErr    *error
	ownUnhealthy bool
}

func (c queueThenCancelCycler) Cycle(context.Context) bool {
	*c.calls++
	if *c.calls == 1 {
		// Another process requests a poll while this one holds the lock: it
		// must observe OutcomeQueued (its cycle never runs) and report
		// success — the demand is recorded for the active runner.
		exB := testExclusiveIn(c.t, context.Background(), c.dir)
		marker := health.NewMarker(filepath.Join(c.t.TempDir(), ".healthy"))
		*c.queuedErr = RunOnce(context.Background(), exB, mustNotRunCycler{t: c.t}, marker)
		return !c.ownUnhealthy
	}
	c.cancel() // shutdown lands during the queued rerun
	return true
}

// queuedRerunMarkerCycler queues demand from another process on its first run
// (so Exclusive services a rerun) and records, at the START of that rerun, what
// the shared health marker held. The rerun begins under a fresh acquisition of
// the same cycle lock, so an in-lock commit is already visible to it while a
// commit deferred until Run returns is not.
type queuedRerunMarkerCycler struct {
	t           *testing.T
	dir         string
	markerPath  string
	calls       *int
	seenAtRerun *string
}

func (c queuedRerunMarkerCycler) Cycle(context.Context) bool {
	*c.calls++
	if *c.calls == 1 {
		exB := testExclusiveIn(c.t, context.Background(), c.dir)
		marker := health.NewMarker(filepath.Join(c.t.TempDir(), ".healthy"))
		if err := RunOnce(context.Background(), exB, mustNotRunCycler{t: c.t}, marker); err != nil {
			c.t.Errorf("queued requester RunOnce = %v, want nil", err)
		}
		return true
	}
	got, err := os.ReadFile(c.markerPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		*c.seenAtRerun = "<absent>"
	case err != nil:
		c.t.Errorf("read marker during the queued rerun: %v", err)
	default:
		*c.seenAtRerun = string(got)
	}
	return true
}

// TestHealthPublishedInsideCycleLock pins WHERE poll commits a cycle's
// health verdict, which is the whole point of d-gpt-u1c4-1's fix. The marker is
// cross-process shared state like state.json and feed.json, and `cycle.lock` is
// what orders every writer of those — but Exclusive releases the lock before
// Run returns, so a verdict committed after Run is unordered: a newer cycle from
// a daemon tick or another poll process can publish in between and then be
// overwritten by this older, superseded verdict.
//
// The observable proof is timing against a queued rerun. Run 1 completes and
// queues demand from another process; the rerun then starts under a fresh
// acquisition of the same lock. A verdict committed INSIDE the locked body is
// therefore already on disk when the rerun begins; one deferred until Run
// returns is not, and the rerun still sees the seeded sentinel. The daemon tick
// always wrote in-lock (RunLoop's closure), so this also pins parity
// between the two entry points that share the marker.
func TestHealthPublishedInsideCycleLock(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	path := seedSentinelMarker(t)
	var (
		calls       int
		seenAtRerun string
	)
	cy := queuedRerunMarkerCycler{t: t, dir: dir, markerPath: path, calls: &calls, seenAtRerun: &seenAtRerun}

	if err := RunOnce(ctx, ex, cy, health.NewMarker(path)); err != nil {
		t.Fatalf("RunOnce = %v, want nil (a healthy own run)", err)
	}
	if calls != 2 {
		t.Fatalf("cycle calls = %d, want 2 (the own run plus the queued rerun)", calls)
	}
	if seenAtRerun == "sentinel-untouched" {
		t.Error("the queued rerun still saw the seeded sentinel: run 1's verdict was committed " +
			"after Exclusive.Run released the lock, so it is not ordered against other cycles")
	}
	assertMarkerPublished(t, path, true)
}

// TestRunOnceRanQueuedThenCancelled pins the ran-plus-queued-rerun leg of
// poll's uniform interruption contract: this process's own run completes
// healthy, Exclusive then services another process's queued rerun, and
// shutdown lands during that rerun. Run returns OutcomeRanQueued with a nil
// own result, but the cancellation observed by then must win — RunOnce
// returns the interruption error (wrapping context.Canceled, so main
// classifies it WARN and exits non-zero) instead of the own run's success —
// while the queued requester itself still returned nil.
//
// The interruption governs this INVOCATION's result only. The own run
// completed, so it published its healthy verdict inside the locked body, where
// the cycle lock orders the write against every other writer of the shared
// marker (d-gpt-u1c4-1); a later shutdown does not withdraw a completed
// cycle's health. The interrupted RERUN publishes nothing.
func TestRunOnceRanQueuedThenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	var calls int
	var queuedErr error
	cy := queueThenCancelCycler{t: t, cancel: cancel, dir: dir, calls: &calls, queuedErr: &queuedErr}
	path := seedSentinelMarker(t)

	err := RunOnce(ctx, ex, cy, health.NewMarker(path))

	if calls != 2 {
		t.Fatalf("cycle calls = %d, want 2 (the own run plus the queued rerun)", calls)
	}
	if queuedErr != nil {
		t.Errorf("queued requester RunOnce = %v, want nil (recorded demand is success)", queuedErr)
	}
	if err == nil {
		t.Fatal("RunOnce = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
	}
	assertMarkerPublished(t, path, true)
}

// TestRunOnceLogsOwnErrorBeforeShutdown pins the shutdown-wins branch's
// preservation of a non-cancellation OWN-run failure: the interruption replaces
// this invocation's result, so an unhealthy own cycle followed by shutdown
// during another process's queued rerun would otherwise disappear from both
// the exit result and the logs. The WARN is its only report. The completed
// unhealthy cycle still publishes its verdict under the cycle lock — losing a
// real ingest failure to a later, unrelated shutdown would leave the marker
// claiming health the cycle disproved. Serial (capture swaps slog.Default).
func TestRunOnceLogsOwnErrorBeforeShutdown(t *testing.T) {
	rec := capture.Default(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	path := seedSentinelMarker(t)
	var calls int
	var queuedErr error
	cy := queueThenCancelCycler{t: t, cancel: cancel, dir: dir, calls: &calls, queuedErr: &queuedErr, ownUnhealthy: true}

	err := RunOnce(ctx, ex, cy, health.NewMarker(path))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("RunOnce = %v, want the shutdown interruption", err)
	}
	if queuedErr != nil {
		t.Errorf("queued requester RunOnce = %v, want nil", queuedErr)
	}
	if got := rec.CountLevel(slog.LevelWarn, "own cycle reported an error before shutdown"); got != 1 {
		t.Errorf("own-error-before-shutdown WARN count = %d, want 1: %v", got, rec.Messages())
	}
	assertMarkerPublished(t, path, false)
}

// TestRunLoopShutdownMidCycle pins the daemon twin of RunOnce's
// interruption contract: a shutdown-interrupted unhealthy cycle must not
// overwrite the health marker (the guard `if !healthy && ctx.Err() != nil`),
// while a cycle that still completed healthy during shutdown records its
// outcome. A regression here would flip the container unhealthy on every
// redeploy. Both paths run through the cycle lock's acquired path, so they
// also prove a tick executes normally under RunOrSkip.
func TestRunLoopShutdownMidCycle(t *testing.T) {
	t.Run("interrupted unhealthy cycle leaves the marker", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		marker.Set(true)
		RunLoop(ctx, time.Hour, testExclusive(t, ctx), cancelCycler{cancel: cancel}, marker)
		if !marker.Healthy() {
			t.Error("marker unhealthy after a shutdown-interrupted cycle")
		}
	})
	t.Run("healthy cycle finished during shutdown still records", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		marker.Set(false)
		RunLoop(ctx, time.Hour, testExclusive(t, ctx), cancelCycler{cancel: cancel, healthy: true}, marker)
		if !marker.Healthy() {
			t.Error("marker not healthy after a healthy cycle")
		}
	})
}

// mustNotRunCycler fails the test if a cycle executes; it pins the paths where
// the cycle lock must prevent any run (a busy skip, a queued request).
type mustNotRunCycler struct{ t *testing.T }

func (c mustNotRunCycler) Cycle(context.Context) bool {
	c.t.Error("cycle ran, want it not to run")
	return true
}

// holdCycleLock seeds a bare flock holder on dir's cycle.lock, simulating a
// cycle in flight in another process (flock contends per open file
// description, so an in-process holder exercises the same kernel path).
func holdCycleLock(t *testing.T, dir string) {
	t.Helper()
	holder, ok, err := scheduler.TryLock(filepath.Join(dir, scheduler.ExclusiveLockName))
	if err != nil || !ok {
		t.Fatalf("seed TryLock = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	t.Cleanup(holder.Unlock)
}

// TestRunLoopSkipsBusyTick pins the daemon's skip mode: a tick arriving
// while another process holds the cycle lock is skipped - the cycle never
// runs, the health marker is untouched, and the library's pinned busy WARN is
// emitted. Serial (capture swaps slog.Default).
func TestRunLoopSkipsBusyTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle lock busy; skipping tick")
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)

	markerPath := seedSentinelMarker(t)

	// FireOnStart executes the first tick immediately; it skips (the lock is
	// busy), then the loop waits out the interval until cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLoop(ctx, time.Hour, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath))
	}()
	<-done

	if !rec.Contains("cycle lock busy; skipping tick") {
		t.Errorf("missing the library's busy-skip line: %v", rec.Messages())
	}
	assertMarkerUntouched(t, markerPath)
}

// cancelHandler records logs and cancels the scheduler context after the
// expected record, providing deterministic event-driven synchronization.
type cancelHandler struct {
	next    slog.Handler
	cancel  context.CancelFunc
	message string
}

func (h *cancelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *cancelHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.next.Handle(ctx, record)
	if record.Message == h.message {
		h.cancel()
	}
	return err
}

func (h *cancelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &cancelHandler{next: h.next.WithAttrs(attrs), cancel: h.cancel, message: h.message}
}

func (h *cancelHandler) WithGroup(name string) slog.Handler {
	return &cancelHandler{next: h.next.WithGroup(name), cancel: h.cancel, message: h.message}
}

// captureAndCancelOn installs a recording default logger whose handler cancels
// the given context after the expected message is recorded, replacing
// wall-clock polling with event-driven synchronization. Serial (swaps
// slog.Default; restored via t.Cleanup).
func captureAndCancelOn(t *testing.T, cancel context.CancelFunc, message string) *capture.Recorder {
	t.Helper()
	_, rec := capture.New()
	prev := slog.Default()
	slog.SetDefault(slog.New(&cancelHandler{next: rec, cancel: cancel, message: message}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// TestRunOnceQueuedWhenBusy pins poll's queue mode against a busy cycle
// lock: the request is queued for the active runner, the cycle does NOT run in
// this process, the marker stays untouched, and RunOnce exits 0 (nil) with
// the coalescing log lines. Serial (capture swaps slog.Default).
func TestRunOnceQueuedWhenBusy(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)

	markerPath := seedSentinelMarker(t)

	if err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath)); err != nil {
		t.Fatalf("RunOnce(busy) = %v, want nil (queued is success, exit 0)", err)
	}

	if pending, perr := ex.Pending(); perr != nil || pending != 1 {
		t.Errorf("Pending = (%d, %v), want (1, nil)", pending, perr)
	}
	if !rec.Contains("cycle lock busy; queued rerun request") {
		t.Errorf("missing the library's queued line: %v", rec.Messages())
	}
	if !rec.Contains("compare cycle already in flight; demand queued for the active runner") {
		t.Errorf("missing poll's own coalescing line: %v", rec.Messages())
	}
	assertMarkerUntouched(t, markerPath)
}

// TestRunOnceQueuedThenCancelled pins the queued-then-cancelled branch of
// poll's uniform interruption contract: the library's pinned "cycle lock
// busy; queued rerun request" line fires after the demand is recorded and
// before Run returns, so cancelling on that record deterministically lands
// the shutdown between queueing and RunOnce's post-queue check. The
// invocation must report the interruption (exit non-zero, wrapping
// context.Canceled) with the marker untouched, while the recorded demand
// still stands for the active runner. Serial (capture swaps slog.Default).
func TestRunOnceQueuedThenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle lock busy; queued rerun request")
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)
	path := seedSentinelMarker(t)

	err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(path))

	if err == nil {
		t.Fatal("RunOnce(queued, then cancelled) = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN)", err)
	}
	assertMarkerUntouched(t, path)
	if pending, perr := ex.Pending(); perr != nil || pending != 1 {
		t.Errorf("Pending = (%d, %v), want (1, nil): the recorded demand must still stand", pending, perr)
	}
	if !rec.Contains("cycle lock busy; queued rerun request") {
		t.Errorf("missing the library's queued line: %v", rec.Messages())
	}
}

// TestRunOnceDiscardedWhenQueueFull pins the queue-full path: with a rerun
// already queued (depth 1), a second busy poll is discarded - still exit 0,
// no run, marker untouched - because the queued rerun already guarantees a
// run starts after this request arrived. Serial (capture swaps slog.Default).
func TestRunOnceDiscardedWhenQueueFull(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	if err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("first busy RunOnce = %v, want nil (queued)", err)
	}
	if err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("second busy RunOnce = %v, want nil (discarded)", err)
	}

	if pending, perr := ex.Pending(); perr != nil || pending != 1 {
		t.Errorf("Pending = (%d, %v), want (1, nil) (discard must not grow the queue)", pending, perr)
	}
	if !rec.Contains("cycle lock busy; rerun already queued; discarding request") {
		t.Errorf("missing the library's discard line: %v", rec.Messages())
	}
}

// signalCycler counts cycle executions; the first execution signals started
// and blocks until release is closed (later executions pass straight
// through), so a test can deterministically hold a cycle in flight.
type signalCycler struct {
	started chan struct{}
	release chan struct{}
	runs    *atomic.Int32
}

func (c *signalCycler) Cycle(context.Context) bool {
	if c.runs.Add(1) == 1 {
		close(c.started)
	}
	<-c.release
	return true
}

// TestRunOnceExecutesQueuedRerun pins the queue-of-1 coalescing end to end
// within one process: a second poll arriving mid-cycle queues and exits 0
// immediately (never blocking for the job's duration), and the active runner
// executes exactly one rerun for it at cycle end - so the queued demand gets
// a run that started after it arrived. Serial (capture swaps slog.Default).
func TestRunOnceExecutesQueuedRerun(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	sc := &signalCycler{
		started: make(chan struct{}),
		release: make(chan struct{}),
		runs:    &atomic.Int32{},
	}
	holderErr := make(chan error, 1)
	go func() { holderErr <- RunOnce(ctx, ex, sc, marker) }()
	<-sc.started // the holder is mid-cycle

	// The overlapping poll queues its demand and returns without running.
	if err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("overlapping RunOnce = %v, want nil (queued)", err)
	}

	close(sc.release) // let the holder finish; its consume loop reruns once
	if err := <-holderErr; err != nil {
		t.Fatalf("holder RunOnce = %v, want nil", err)
	}

	if runs := sc.runs.Load(); runs != 2 {
		t.Errorf("cycle ran %d times, want 2 (own run + one queued rerun)", runs)
	}
	if pending, perr := ex.Pending(); perr != nil || pending != 0 {
		t.Errorf("Pending after rerun = (%d, %v), want (0, nil)", pending, perr)
	}
	if !rec.Contains("running queued cycle request") {
		t.Errorf("missing the library's rerun line: %v", rec.Messages())
	}
	if !marker.Healthy() {
		t.Error("marker not healthy after the rerun recorded its outcome")
	}
}

// queuedFailureCycler holds its first cycle in flight (signalling started,
// blocking until release) and reports healthy; any later execution - the
// queued rerun - fails, so a test can pin the rerun-failure warning.
type queuedFailureCycler struct {
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
}

func (c *queuedFailureCycler) Cycle(context.Context) bool {
	if c.runs.Add(1) == 1 {
		close(c.started)
		<-c.release
		return true
	}
	return false
}

// TestRunOnceLogsQueuedRerunFailure pins the failure signal of a queued
// rerun: the rerun has no requesting process left to receive an exit code, so
// the WARN line in executeRuns is its only observable failure signal.
// Serial (capture swaps slog.Default).
func TestRunOnceLogsQueuedRerunFailure(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	ex := testExclusive(t, ctx)
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
	sc := &queuedFailureCycler{started: make(chan struct{}), release: make(chan struct{})}
	holderErr := make(chan error, 1)
	go func() { holderErr <- RunOnce(ctx, ex, sc, marker) }()
	<-sc.started

	if err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("overlapping RunOnce = %v, want nil (queued)", err)
	}
	close(sc.release)
	if err := <-holderErr; err != nil {
		t.Fatalf("holder RunOnce = %v, want nil from its own healthy run", err)
	}

	if !rec.Contains("queued rerun cycle reported an error") {
		t.Errorf("missing queued-rerun error warning: %v", rec.Messages())
	}
	// The marker records the LAST run's verdict, not this invocation's own: each
	// execution commits its own verdict inside the locked body, so the queued
	// rerun's unhealthy cycle is the last one published.
	if marker.Healthy() {
		t.Error("marker healthy after the queued rerun's unhealthy cycle: RunOnce must record the last run's verdict")
	}
}

// TestRunOnceCoordinationFailure pins the infrastructure-failure path:
// an unusable cycle lock (the lock path is a directory) means nothing ran and
// no demand was recorded, so RunOnce returns the error (exit 1) and never
// reads as an interruption.
func TestRunOnceCoordinationFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	err := RunOnce(ctx, ex, mustNotRunCycler{t: t}, marker)

	if err == nil {
		t.Fatal("RunOnce(unusable lock) = nil, want the coordination error (exit 1)")
	}
	if !strings.Contains(err.Error(), "cycle coordination failed") {
		t.Errorf("err = %q, want it wrapped as cycle coordination failed", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, must not read as an interruption", err)
	}
}

// TestNewExclusiveMkdirError pins the fail-fast contract: an uncreatable
// lock directory surfaces as a wrapped error (the daemon and poll refuse to
// start uncoordinated) instead of degrading to per-tick failures.
func TestNewExclusiveMkdirError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewExclusive(context.Background(), filepath.Join(blocker, "sub"))

	if err == nil {
		t.Fatal("NewExclusive(uncreatable dir) = nil, want error")
	}
	if !strings.Contains(err.Error(), "create cycle lock dir") {
		t.Errorf("err = %q, want it wrapped as create cycle lock dir", err)
	}
}

// TestRunLoopCoordinationFailure pins the daemon's infrastructure-failure
// contract: an unusable cycle lock (the lock path is a directory) means the
// tick could not run at all, which must be logged at ERROR ("cycle
// coordination failed; tick did not run") so the level=ERROR Loki alert fires
// - cycles have stopped - while the cycle never runs and the health marker is
// untouched. Serial (capture swaps slog.Default).
func TestRunLoopCoordinationFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle coordination failed; tick did not run")
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := seedSentinelMarker(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLoop(ctx, time.Hour, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath))
	}()
	<-done

	if !rec.Contains("cycle coordination failed; tick did not run") {
		t.Errorf("missing the coordination-failure ERROR: %v", rec.Messages())
	}
	assertMarkerUntouched(t, markerPath)
}

// TestRunOnceQueueErrorAfterRun pins poll's exit-code contract when the run
// succeeded but the queue bookkeeping is broken (the queue file is a
// directory): the cycle this invocation paid for completed healthy, so
// RunOnce exits 0 and the marker records the outcome, with the coordination
// error demoted to the after-run WARN instead of failing the poll. Serial
// (capture swaps slog.Default).
func TestRunOnceQueueErrorAfterRun(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveQueueName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	if err := RunOnce(ctx, ex, boolCycler(true), marker); err != nil {
		t.Fatalf("RunOnce(healthy run, broken queue file) = %v, want nil (the paid-for cycle succeeded)", err)
	}
	if !marker.Healthy() {
		t.Error("marker not healthy after the healthy cycle")
	}
	if !rec.Contains("cycle coordination error after run") {
		t.Errorf("missing the after-run coordination WARN: %v", rec.Messages())
	}
}

// TestRunLoopQueueErrorAfterRun pins the daemon's alert-hygiene twin of
// poll's after-run demotion: when the tick's cycle ran (the lock was free) but
// the queue bookkeeping is broken (the queue file is a directory), the
// coordination error is the after-run WARN - never the ERROR that fires the
// cycle-error Loki alert on every tick - and the marker records the cycle's
// health. Serial (capture swaps slog.Default).
func TestRunLoopQueueErrorAfterRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle coordination error after run")
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveQueueName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLoop(ctx, time.Hour, ex, boolCycler(true), marker)
	}()
	<-done

	if !rec.Contains("cycle coordination error after run") {
		t.Errorf("missing the after-run coordination WARN: %v", rec.Messages())
	}
	if !marker.Healthy() {
		t.Error("marker not healthy after the healthy cycle")
	}
	for _, r := range rec.Records() {
		if r.Level == slog.LevelError {
			t.Errorf("unexpected ERROR record %q; a queue error after a ran tick must stay WARN", r.Message)
		}
	}
}

// TestInterruptedClassifiesNonCanceledCause pins poll's interruption
// classification against a cancellation cause that does NOT itself wrap
// context.Canceled. A WithCancelCause cause is whatever the cancelling site
// passed, so only ctx.Err() is guaranteed to be context.Canceled (Go 1.26's
// signal.NotifyContext cause happens to satisfy errors.Is via signalError.Is,
// but a cause in general - and net/http, which surfaces context.Cause
// verbatim - does not). Interrupted must therefore wrap the stable ctx.Err()
// for main's routine-shutdown WARN classification while the cause stays
// errors.Is-able for diagnostics.
func TestInterruptedClassifiesNonCanceledCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("terminated signal received")
	cancel(cause)

	err := Interrupted(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled regardless of the cancellation cause", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want the cancellation cause to stay errors.Is-able", err)
	}
}

// TestRunOnceMarkerWriteFailure pins recordRunHealth's marker-write failure
// branch - the write RunOnce commits from inside the locked job body: the marker
// directory is present at construction (so the marker
// does not enter its degraded no-op mode) and is then replaced by a regular
// file, so SetChecked reaches its transient-error return for every UID
// (root-safe, unlike a read-only-dir chmod) and a healthy cycle still exits
// non-zero with the record-poll-health error - the external scheduler must
// see the fail rather than trusting an unrecorded outcome.
func TestRunOnceMarkerWriteFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "marker-dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(dir, ".healthy"))
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := RunOnce(ctx, testExclusive(t, ctx), boolCycler(true), marker)

	if err == nil {
		t.Fatal("RunOnce(blocked marker path) = nil, want the record-poll-health error (exit 1)")
	}
	if !strings.Contains(err.Error(), "record poll health") {
		t.Errorf("err = %q, want it wrapped as record poll health", err)
	}
}

// TestRunOnceQueueErrorThenCancelled pins the cancelled-with-coordination-
// error diagnostics leg of poll's uniform interruption contract: the tick's
// own run completes (the lock was free) but the queue bookkeeping is broken
// (the queue file is a directory) AND shutdown lands during the run. The
// coordination error must surface as the after-run WARN inside the cancelled
// path, and the interruption must still win over the own-run result: RunOnce
// returns the error wrapping context.Canceled (main classifies it WARN, exit
// non-zero) with the marker untouched. Serial (capture swaps slog.Default).
func TestRunOnceQueueErrorThenCancelled(t *testing.T) {
	rec := capture.Default(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveQueueName), 0o755); err != nil {
		t.Fatal(err)
	}
	path := seedSentinelMarker(t)

	err := RunOnce(ctx, ex, cancelCycler{cancel: cancel, healthy: true}, health.NewMarker(path))

	if err == nil {
		t.Fatal("RunOnce(cancelled run, broken queue bookkeeping) = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (the interruption wins over the own-run result)", err)
	}
	const msg = "cycle coordination error after run"
	if got := rec.CountLevel(slog.LevelWarn, msg); got != 1 {
		t.Errorf("after-run coordination WARN count = %d, want 1: %v", got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, msg); got != 0 {
		t.Errorf("after-run coordination ERROR count = %d, want 0: %v", got, rec.Messages())
	}
	assertMarkerUntouched(t, path)
}

// TestWarnCoordinationError pins the outcome-to-diagnostic mapping of the
// coordination-error WARN lines (operator-facing Loki diagnostics): recorded
// demand (Queued/Discarded) logs the demand-stands line, a completed run
// (Ran/RanQueued/Skipped) logs the after-run line, and every other outcome -
// reachable only from RunOnce's shutdown branch - logs the during-shutdown
// line. All three are WARN, never the ERROR that fires the cycle-error Loki
// alert. Serial (capture swaps slog.Default).
func TestWarnCoordinationError(t *testing.T) {
	tests := []struct {
		name    string
		outcome scheduler.Outcome
		wantMsg string
	}{
		{"queued demand stands", scheduler.OutcomeQueued, "cycle coordination error after queueing; demand stands"},
		{"discarded demand stands", scheduler.OutcomeDiscarded, "cycle coordination error after queueing; demand stands"},
		{"ran is after-run", scheduler.OutcomeRan, "cycle coordination error after run"},
		{"ran-plus-queued is after-run", scheduler.OutcomeRanQueued, "cycle coordination error after run"},
		{"skipped is after-run", scheduler.OutcomeSkipped, "cycle coordination error after run"},
		{"none is the shutdown diagnostic", scheduler.OutcomeNone, "cycle coordination failed during shutdown"},
		{"gated is the shutdown diagnostic", scheduler.OutcomeGated, "cycle coordination failed during shutdown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)

			warnCoordinationError(tt.outcome, errors.New("queue file unusable"))

			records := rec.Records()
			if len(records) != 1 {
				t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
			}
			if records[0].Message != tt.wantMsg {
				t.Errorf("msg = %q, want %q", records[0].Message, tt.wantMsg)
			}
			if records[0].Level != slog.LevelWarn {
				t.Errorf("level = %v, want WARN (a stands-anyway diagnostic must not fire the cycle-error alert)", records[0].Level)
			}
		})
	}
}

// TestNonRunResult pins the non-run outcome mapping of poll's exit
// contract at the helper seam: Queued/Discarded are success (exit 0) even
// when the coordination bookkeeping errored (the error is demoted to the
// demand-stands WARN, and each outcome logs its own coalescing Info line);
// Gated applies the uniform interruption contract (an error wrapping
// context.Canceled, so main classifies it WARN and exits non-zero); and every
// run-shaped outcome falls through unhandled to RunOnce's ran/own
// accounting. Serial (capture swaps slog.Default).
func TestNonRunResult(t *testing.T) {
	t.Run("queued with a coordination error is still success and logs demand-stands", func(t *testing.T) {
		rec := capture.Default(t)

		handled, err := nonRunResult(context.Background(), scheduler.OutcomeQueued, errors.New("queue bookkeeping broken"))

		if !handled {
			t.Fatal("handled = false, want true (queued ends the poll)")
		}
		if err != nil {
			t.Fatalf("err = %v, want nil (recorded demand is success, exit 0)", err)
		}
		if got := rec.CountLevel(slog.LevelWarn, "cycle coordination error after queueing; demand stands"); got != 1 {
			t.Errorf("demand-stands WARN count = %d, want 1: %v", got, rec.Messages())
		}
		if !rec.Contains("compare cycle already in flight; demand queued for the active runner") {
			t.Errorf("missing the queued coalescing line: %v", rec.Messages())
		}
	})
	t.Run("discarded logs the already-covered message", func(t *testing.T) {
		rec := capture.Default(t)

		handled, err := nonRunResult(context.Background(), scheduler.OutcomeDiscarded, nil)

		if !handled || err != nil {
			t.Fatalf("nonRunResult(discarded) = (handled=%v, err=%v), want (true, nil)", handled, err)
		}
		if !rec.Contains("compare cycle already in flight; demand already covered by the queued rerun") {
			t.Errorf("missing the discarded coalescing line: %v", rec.Messages())
		}
	})
	t.Run("gated applies the uniform interruption contract", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		handled, err := nonRunResult(ctx, scheduler.OutcomeGated, nil)

		if !handled {
			t.Fatal("handled = false, want true (a gated run ends the poll)")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, exit non-zero)", err)
		}
	})
	t.Run("run-shaped outcomes fall through unhandled", func(t *testing.T) {
		for _, outcome := range []scheduler.Outcome{scheduler.OutcomeNone, scheduler.OutcomeRan, scheduler.OutcomeRanQueued, scheduler.OutcomeSkipped} {
			if handled, err := nonRunResult(context.Background(), outcome, nil); handled || err != nil {
				t.Errorf("nonRunResult(%v) = (handled=%v, err=%v), want (false, nil): must fall through to the ran/own accounting", outcome, handled, err)
			}
		}
	})
}

// TestNormalizeShutdownErrorClassifiesCauseOnlyForm pins the terminal-boundary
// shutdown classifier: an error that IS this context's cancellation but only
// carries the CAUSE (a WithCancelCause cause need not wrap context.Canceled,
// and net/http surfaces the cause verbatim) is stamped with the stable
// ctx.Err() so main's single errors.Is(err, context.Canceled) check reads it as
// a routine-shutdown WARN. A nil error, an error already carrying
// context.Canceled, and a genuine fault that merely landed while the context
// was cancelled all pass through untouched, so a real fault is never hidden.
func TestNormalizeShutdownErrorClassifiesCauseOnlyForm(t *testing.T) {
	cause := errors.New("terminated signal received") // deliberately NOT wrapping context.Canceled
	causeCtx, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(cause)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	fault := errors.New("walk failed")

	tests := []struct {
		name          string
		ctx           context.Context
		err           error
		wantCancel    bool
		wantUnchanged bool
	}{
		{"cause-only cancellation is stamped", causeCtx, fmt.Errorf("audit: %w", cause), true, false},
		{"already-canceled error passes through", canceled, fmt.Errorf("audit: %w", context.Canceled), true, true},
		{"unrelated fault during shutdown stays a fault", causeCtx, fault, false, true},
		{"fault with a live context stays a fault", context.Background(), fault, false, true},
		{"nil stays nil", causeCtx, nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeShutdownError(tt.ctx, tt.err)

			if gotCancel := errors.Is(got, context.Canceled); gotCancel != tt.wantCancel {
				t.Errorf("errors.Is(%v, context.Canceled) = %v, want %v", got, gotCancel, tt.wantCancel)
			}
			// Identity, not chain membership, is the assertion here: a stamped
			// error still wraps the original (checked below).
			if unchanged := got == tt.err; unchanged != tt.wantUnchanged {
				t.Errorf("NormalizeShutdownError(%v) = %v, want the error returned unchanged = %v", tt.err, got, tt.wantUnchanged)
			}
			if tt.err != nil && !errors.Is(got, tt.err) {
				t.Errorf("NormalizeShutdownError(%v) = %v, want the original error still in the chain", tt.err, got)
			}
		})
	}
}

// markerBreakingCycler queues demand from another process on its first run (so
// Exclusive services a rerun) and then breaks the shared marker's parent
// directory just before the RERUN's health write, exercising the queued-rerun
// leg of recordRunHealth's write-failure branch.
type markerBreakingCycler struct {
	t         *testing.T
	dir       string
	markerDir string
	calls     *int
}

func (c markerBreakingCycler) Cycle(context.Context) bool {
	*c.calls++
	if *c.calls == 1 {
		exB := testExclusiveIn(c.t, context.Background(), c.dir)
		marker := health.NewMarker(filepath.Join(c.t.TempDir(), ".healthy"))
		if err := RunOnce(context.Background(), exB, mustNotRunCycler{t: c.t}, marker); err != nil {
			c.t.Errorf("queued requester RunOnce = %v, want nil", err)
		}
		return true
	}
	// Replace the marker's directory with a regular file so SetChecked fails
	// for every UID (root-safe, unlike a read-only-dir chmod).
	if err := os.RemoveAll(c.markerDir); err != nil {
		c.t.Errorf("clear marker dir: %v", err)
	}
	if err := os.WriteFile(c.markerDir, []byte("blocker"), 0o600); err != nil {
		c.t.Errorf("block marker dir: %v", err)
	}
	return true
}

// TestRunOnceQueuedRerunMarkerWriteFailure pins the queued-rerun leg of
// recordRunHealth's marker-write failure, the one fault whose ONLY report is
// its log line: the verdict came from another process's queued demand, so there
// is no exit code to surface through, and the write does not self-heal (a full
// disk or a bad mode on /tmp keeps failing until the operator acts). It must
// therefore log at ERROR - the level alerts.yaml's SeadexScoutCycleError rule
// keys on, and which that rule's description names by this exact message -
// while this invocation's own healthy run still exits 0 and the failure is not
// re-reported as a cycle fault. Serial (capture swaps slog.Default).
func TestRunOnceQueuedRerunMarkerWriteFailure(t *testing.T) {
	rec := capture.Default(t)
	ctx := t.Context()
	dir := t.TempDir()
	ex := testExclusiveIn(t, ctx, dir)
	markerDir := filepath.Join(t.TempDir(), "marker-dir")
	if err := os.Mkdir(markerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(markerDir, ".healthy"))
	var calls int

	err := RunOnce(ctx, ex, markerBreakingCycler{t: t, dir: dir, markerDir: markerDir, calls: &calls}, marker)
	if err != nil {
		t.Fatalf("RunOnce = %v, want nil: a queued rerun's marker-write failure has no exit code to report through and must not fail this invocation's own healthy run", err)
	}
	if calls != 2 {
		t.Fatalf("cycle calls = %d, want 2 (the own run plus the queued rerun)", calls)
	}
	const msg = "queued rerun could not record poll health"
	if got := rec.CountLevel(slog.LevelError, msg); got != 1 {
		t.Errorf("queued-rerun marker-failure ERROR count = %d, want 1 (alerts.yaml names this message as an operator-actionable fault): %v", got, rec.Messages())
	}
	if !rec.AttrContains(msg, "error", "record poll health") {
		t.Errorf("ERROR line lost the wrapped record-poll-health cause: %v", rec.Records())
	}
	// The write failure is reported once, by its own ERROR: returning it as the
	// rerun's cycle error instead would re-report it as a cycle fault, which is
	// a different class (a cycle fault logs its own ERROR inside Cycle).
	if got := rec.CountExact("queued rerun cycle reported an error"); got != 0 {
		t.Errorf("queued-rerun cycle-error WARN count = %d, want 0: a marker-write failure is not a cycle fault: %v", got, rec.Messages())
	}
}
