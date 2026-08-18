// Package shutdown is the app's one vocabulary for deciding whether a
// terminal error IS this context's shutdown - so every terminal boundary
// (the report subcommand, the compare cycle, the indexer feed goroutine)
// classifies the same condition as a routine-shutdown WARN rather than the
// level=ERROR fault the cycle-error alert fires on.
//
// It is a leaf on purpose.
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
// what an operator greps for.
func Interrupted(ctx context.Context) error {
	// The prefix keeps poll's historical wording: it is what an operator greps for.
	return InterruptedAs(ctx, "poll interrupted")
}

// InterruptedAs is Interrupted with a caller-chosen prefix: the ONE place the
// interruption vocabulary is assembled, so no boundary re-derives which token
// carries the classification. ctx.Err() is the classification token the root's
// single errors.Is(err, context.Canceled) reads; context.Cause(ctx) rides along
// as prose and must never be the token (see Is).
func InterruptedAs(ctx context.Context, prefix string) error {
	return fmt.Errorf("%s: %w (cause: %w)", prefix, ctx.Err(), context.Cause(ctx))
}

// WrapAs re-expresses err as this context's interruption under a caller-chosen
// prefix, adding ctx.Err() as the classification token. An empty prefix yields
// the bare Normalize shape.
func WrapAs(ctx context.Context, prefix string, err error) error {
	if prefix == "" {
		return fmt.Errorf("%w (cause: %w): %w", ctx.Err(), context.Cause(ctx), err)
	}
	return fmt.Errorf("%s: %w (cause: %w): %w", prefix, ctx.Err(), context.Cause(ctx), err)
}

// Is reports whether err is the observable form of THIS context's cancellation,
// so a terminal boundary can classify it as a routine shutdown (WARN) instead of
// a fault (the level=ERROR cycle-error alert).
func Is(ctx context.Context, err error) bool {
	if ctx.Err() == nil || err == nil {
		return false
	}
	return errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))
}

// Normalize adds the stable ctx.Err() as the classification token to an error
// that IS this context's cancellation but does not carry context.Canceled itself
// (a cause-only form), so the root's single errors.Is(err, context.Canceled)
// check classifies it as a routine shutdown.
func Normalize(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, ctx.Err()) || !Is(ctx, err) {
		return err
	}
	return WrapAs(ctx, "", err)
}
