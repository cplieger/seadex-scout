package shutdown

import (
	"context"
	"errors"
	"sync"
	"time"
)

// detachedWriteGrace bounds the detached report write, mirroring scout's
// saveGrace: inside Docker's default 10s stop grace, so the pair lands before
// SIGKILL.
const detachedWriteGrace = 5 * time.Second

// DetachedWriteContext returns the context the report's file write runs under -
// always a detached copy of the caller's (context.WithoutCancel keeps the
// values, drops the cancellation) - plus its cancel func, which the caller must
// defer.
func DetachedWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return detachedWriteContextGrace(ctx, detachedWriteGrace)
}

// detachedWriteContextGrace is DetachedWriteContext with an explicit grace, so
// the arming behaviour is testable without waiting out the production budget.
func detachedWriteContextGrace(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	// WithCancelCause, not WithTimeout: the deadline may not exist yet (it is
	// armed only once a shutdown lands), and carrying DeadlineExceeded as the
	// CAUSE keeps DetachedWriteError's grace-exhausted classification working -
	// audit's stage gates wrap both ctx.Err() and context.Cause(ctx).
	writeCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	released := make(chan struct{})
	go func() {
		select {
		case <-released:
			return
		case <-ctx.Done():
		}
		select {
		case <-released:
		case <-time.After(grace):
			cancel(context.DeadlineExceeded)
		}
	}()
	var once sync.Once
	return writeCtx, func() {
		// The caller's defer may fire after an explicit cancel; closing the
		// release channel once keeps that idempotent (cancel already is).
		once.Do(func() { close(released) })
		cancel(context.Canceled)
	}
}

// DetachedWriteError re-classifies a report-write failure that happened because
// the DETACHED write context's shutdown grace ran out (DetachedWriteContext arms
// that grace when the caller's context is done, whether the shutdown arrived
// before the call or during the write).
func DetachedWriteError(ctx context.Context, err error) error {
	if ctx.Err() == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return WrapAs(ctx, "report write cut short by shutdown", err)
}
