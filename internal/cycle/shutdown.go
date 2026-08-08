package cycle

import (
	"context"
	"errors"
	"fmt"
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
//
// This is the same escape hatch Scout.save already uses for the AniList memo,
// for the same reason: the write is cheap and its input took tens of minutes to
// produce, so a shutdown that arrives after generation must not cost the
// artifact. The detach is unconditional because the shutdown does not have to
// arrive BEFORE the call to cost the pair: handing the live signal context to
// WriteFiles left the whole write - including the CPU-bound render of a
// several-hundred-row report - racing the signal, and a signal landing one
// instruction after the call aborted the write at audit's next per-stage gate
// and discarded the artifact, which is exactly the loss this escape hatch
// exists to prevent.
//
// The shutdown gate is not lost, only deferred: the detached context is
// cancelled detachedWriteGrace after the caller's context is done (immediately
// arming that timer when it is already done, so a shutdown that arrived before
// the call still bounds the write at the same grace it always did). A write
// that outlives the grace is cut off exactly as before, so a shutdown never
// spends more than detachedWriteGrace on the write, and WriteFiles' own
// per-stage context gates stay exactly as documented - this decision is made
// HERE, beside the shutdown-interruption vocabulary the whole app classifies
// against, rather than by weakening that contract inside audit.
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
// before the call or during the write). Exhausting that shutdown grace is
// the shutdown truncating the run - a transient, designed outcome - not the
// genuine operation timeout the root's dispatchOutcome default arm reports at
// level=ERROR, and alerts.yaml documents a shutdown-interrupted run as excluded
// from the level=ERROR cycle-error rule. Adding the caller's ctx.Err() makes
// the root's single errors.Is(err, context.Canceled) check classify it WARN
// (still exit 1: the pair did not land). Any other write failure - ENOSPC,
// EACCES, an encode error - is a genuine fault and passes through untouched.
func DetachedWriteError(ctx context.Context, err error) error {
	if ctx.Err() == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("report write cut short by shutdown: %w (cause: %w): %w",
		ctx.Err(), context.Cause(ctx), err)
}
