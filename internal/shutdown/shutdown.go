// Package shutdown is the app's one vocabulary for deciding whether a
// terminal error IS this context's shutdown - so every terminal boundary
// (the report subcommand, the compare cycle, the indexer feed goroutine)
// classifies the same condition as a routine-shutdown WARN rather than the
// level=ERROR fault the cycle-error alert fires on.
//
// It is a leaf on purpose. The rule is stdlib-only, while the cycle
// coalescer that used to host it needs health, scheduler/v2 and a flock on
// /config, so hosting it there made every consumer of a three-function
// vocabulary inherit the coalescer's whole dependency set.
package shutdown

import (
	"context"
	"errors"
	"fmt"
)

// Interrupted wraps the stable ctx.Err() (always context.Canceled here) with
// the uniform interruption message every entry point shares, so the root
// classifies it as a routine-shutdown WARN and the contract reads identically
// from every phase. The message keeps poll's historical wording because it is
// what an operator greps for. The message speaks only of THIS INVOCATION's result, not of the
// marker: an interruption leaves the marker untouched whenever no verdict was
// published before the cancellation was observed - including a cycle that
// completed healthy and was then interrupted at the recording boundary, whose
// verdict recordRunHealth deliberately withholds because an interrupted run's
// outcome is not a trustworthy health verdict. What the
// interruption never does is reach BACK: a verdict already published inside the
// cycle lock - an earlier run of this invocation, or a daemon tick's - stands,
// because a completed cycle's health is not this process's to withdraw.
// The daemon tick deliberately differs in the boundary case: cycle.RunLoop
// publishes a healthy verdict even when the cancellation is already visible,
// withholding only an UNHEALTHY interrupted cycle. The cancellation cause rides
// along as a second %w so the message still names the signal ("terminated signal
// received"), never as the classification token: a cause is whatever the
// cancelling site passed to context.WithCancelCause, so only ctx.Err() is
// guaranteed to be context.Canceled. (signal.NotifyContext's own signalError
// does satisfy errors.Is(_, context.Canceled) - golang/go#77639, backported to
// Go 1.26 in #79499 - which is what keeps the net/http errors this app
// classifies classifiable at all, since net/http reports a cancelled request as
// context.Cause(ctx).)
func Interrupted(ctx context.Context) error {
	// The prefix keeps poll's historical wording: it is what an operator greps for.
	return InterruptedAs(ctx, "poll interrupted")
}

// InterruptedAs is Interrupted with a caller-chosen prefix: the ONE place the
// interruption vocabulary is assembled, so no boundary re-derives which token
// carries the classification. ctx.Err() is the classification token the root's
// single errors.Is(err, context.Canceled) reads; context.Cause(ctx) rides along
// as prose and must never be the token (see IsShutdownError).
func InterruptedAs(ctx context.Context, prefix string) error {
	return fmt.Errorf("%s: %w (cause: %w)", prefix, ctx.Err(), context.Cause(ctx))
}

// WrapAs re-expresses err as this context's interruption under a caller-chosen
// prefix, adding ctx.Err() as the classification token. An empty prefix yields
// the bare NormalizeShutdownError shape.
func WrapAs(ctx context.Context, prefix string, err error) error {
	if prefix == "" {
		return fmt.Errorf("%w (cause: %w): %w", ctx.Err(), context.Cause(ctx), err)
	}
	return fmt.Errorf("%s: %w (cause: %w): %w", prefix, ctx.Err(), context.Cause(ctx), err)
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
	return WrapAs(ctx, "", err)
}
