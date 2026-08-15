package indexer

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/cplieger/seadex-scout/internal/shutdown"
)

// stopWait bounds how long Stop blocks on the feed's graceful drain. The feed's
// own budget (shutdownGrace) is the WHOLE Docker default stop grace, and a search
// in flight against a slow Prowlarr holds its connection for minutes, so waiting
// the drain out would leave the daemon no margin for client cleanup, the health
// marker and the shutdown record. The abandoned goroutine dies with the process,
// which is what SIGKILL would have done minus the diagnostics. It lives here,
// beside the unexported budgets it is reasoned against.
const stopWait = 3 * time.Second

// Supervise starts the feed in its own supervised goroutine and returns the func
// that stops it: it cancels the feed's context and waits up to stopWait for the
// graceful drain, warning when the drain outruns that budget. cleanup releases
// the caller's feed-scoped resources and runs on every exit path. The feed runs
// on its own cancellable child of ctx so the stop func can force it down while
// ctx is still live, rather than parking an exiting daemon against it.
func (ix *Indexer) Supervise(ctx context.Context, cleanup func()) (stop func()) {
	ictx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	supervise(ictx, done, ix.Run, cleanup, ix.log)
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(stopWait):
			ix.log.Warn("indexer feed did not drain within the shutdown budget; continuing shutdown",
				"wait", stopWait)
		}
	}
}

// supervise launches the feed goroutine: it runs the feed until it returns
// (classifying the stop via logStop), recovers a panic so a crashing feed cannot
// take down the daemon, releases the feed's resources on every exit path, and
// closes done last so the stop func can wait for a fully-drained goroutine.
func supervise(ctx context.Context, done chan struct{}, run func(context.Context) error, cleanup func(), log *slog.Logger) {
	go func() {
		defer close(done)
		defer cleanup()
		defer func() {
			if r := recover(); r != nil {
				log.Error("indexer feed panicked", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		if err := run(ctx); err != nil {
			logStop(ctx, log, err)
		}
	}()
}

// logStop classifies the feed's Run error for the shared slog stream, through the
// feed's own logger so the terminal record carries component=indexer. Both
// shutdown-path cases are routine on a redeploy (WARN, kept off the cycle-error
// alert) but carry distinct messages: webhttp.Run returns DeadlineExceeded when
// its graceful-shutdown budget expired, meaning in-flight requests were cut off.
func logStop(ctx context.Context, log *slog.Logger, err error) {
	switch {
	case ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded):
		log.Warn("indexer shutdown budget expired; in-flight requests aborted", "error", err, "cause", context.Cause(ctx))
	case shutdown.IsShutdownError(ctx, err):
		// Bind cancelled mid-startup, or a clean graceful drain: routine.
		// IsShutdownError also accepts the cause-only form, which a plain
		// errors.Is(err, context.Canceled) would misread as a fault.
		log.Warn("indexer feed stopped during shutdown", "error", err, "cause", context.Cause(ctx))
	default:
		log.Error("indexer feed stopped", "error", err)
	}
}
