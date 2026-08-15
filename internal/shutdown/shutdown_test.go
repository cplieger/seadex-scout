package shutdown

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

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
	ctx, cancel := context.WithCancelCause(t.Context())
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
	causeCtx, cancelCause := context.WithCancelCause(t.Context())
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
		{"fault with a live context stays a fault", t.Context(), fault, false, true},
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
