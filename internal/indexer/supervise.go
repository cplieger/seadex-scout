package indexer

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/cplieger/seadex-scout/internal/cycle"
)

// stopWait bounds how long Stop blocks on the feed's graceful drain. The feed's
// own budget (shutdownGrace) is the WHOLE Docker default stop grace the public
// compose example relies on, and a search in flight against a slow Prowlarr
// holds its connection for minutes (writeTimeout is derived from the bounded
// Prowlarr retry budget), so waiting the drain out leaves the daemon no margin
// for the work that follows its stop - client cleanup, health-marker removal,
// the shutdown record - which would race SIGKILL and lose. Cap the wait
// instead: the abandoned goroutine dies with the process, which is exactly what
// SIGKILL would have done, minus the diagnostics.
//
// It lives here, beside the budgets it is reasoned against, rather than in the
// composition root that used to hold it: the root could neither see nor verify
// shutdownGrace and writeTimeout (both unexported), so the number's whole
// justification sat one package away from the facts it depends on.
const stopWait = 3 * time.Second

// Supervise starts the feed in its own supervised goroutine and returns the func
// that stops it: it cancels the feed's context and waits up to stopWait for the
// graceful drain, warning when the drain outruns that budget. cleanup releases
// the caller's feed-scoped resources (the Prowlarr HTTP client the composition
// root built) and runs on every exit path - a Run return, or a recovered panic -
// so the transport is freed immediately even if the daemon keeps running.
//
// The feed runs on its own cancellable child of ctx so the returned stop func
// can force it down even while ctx is still live (a daemon unwinding its defers
// after a startup error); otherwise the wait would park the exiting daemon
// against a still-serving feed.
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
// take down the long-lived daemon, releases the feed's resources on every exit
// path, and closes done last so Supervise's stop func can wait for a
// fully-drained goroutine. Split from Supervise so the shield is exercisable
// with a fake runner, without binding a port.
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

// logStop classifies the feed's Run error for the shared slog stream, emitting
// through the feed's own logger so the terminal record carries the same
// component=indexer context as its normal logs. Both shutdown-path cases are
// routine on a redeploy (WARN, kept off the level=ERROR cycle-error alert,
// matching the compare cycle's walk/matching/save classification), but they
// carry distinct messages: webhttp.Run returns DeadlineExceeded specifically
// when its graceful-shutdown budget expired, meaning in-flight Torznab requests
// were cut off - information worth its own log line rather than vanishing into
// the clean-shutdown message. Any error outside a shutdown is a fault and stays
// ERROR. The WARN-vs-ERROR vocabulary is internal/cycle's, so this terminal
// boundary reads the same rule the report subcommand and the compare cycle do.
func logStop(ctx context.Context, log *slog.Logger, err error) {
	switch {
	case ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded):
		log.Warn("indexer shutdown budget expired; in-flight requests aborted", "error", err, "cause", context.Cause(ctx))
	case cycle.IsShutdownError(ctx, err):
		// Bind cancelled mid-startup, or a clean graceful drain: routine.
		// cycle.IsShutdownError also accepts the cause-only form
		// (net.ListenConfig surfacing context.Cause(ctx) verbatim), which a
		// plain errors.Is(err, context.Canceled) would misread as a fault.
		log.Warn("indexer feed stopped during shutdown", "error", err, "cause", context.Cause(ctx))
	default:
		log.Error("indexer feed stopped", "error", err)
	}
}
