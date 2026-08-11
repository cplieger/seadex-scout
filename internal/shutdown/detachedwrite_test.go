package shutdown

import (
	"context"
	"errors"
	"testing"
	"time"
)

// detachedCtxTestKey is the private key TestDetachedWriteContext threads through
// the detached write context to prove values survive context.WithoutCancel.
type detachedCtxTestKey struct{}

// TestDetachedWriteContext pins the report write's shutdown contract: the write
// context is detached from the caller's UNCONDITIONALLY (a signal landing during
// the write - not only before it - must not discard the ~25m artifact), the
// caller's values survive, and the shutdown gate is deferred rather than lost
// (the detached context is cancelled detachedWriteGrace after the shutdown, with
// DeadlineExceeded as the CAUSE so DetachedWriteError still classifies a
// grace-exhausted write as a routine shutdown).
func TestDetachedWriteContext(t *testing.T) {
	t.Run("a live context is detached and survives the shutdown that follows", func(t *testing.T) {
		parent := context.WithValue(context.Background(), detachedCtxTestKey{}, "report")
		ctx, cancelParent := context.WithCancel(parent)

		got, cancel := DetachedWriteContext(ctx)
		defer cancel()
		if got.Err() != nil {
			t.Fatalf("Err() = %v, want nil", got.Err())
		}

		cancelParent()
		if got.Err() != nil {
			t.Errorf("Err() = %v right after the signal, want nil (the write gets its grace)", got.Err())
		}
		if v, _ := got.Value(detachedCtxTestKey{}).(string); v != "report" {
			t.Errorf("Value = %q, want %q (WithoutCancel keeps values)", v, "report")
		}

		cancel()
		if got.Err() == nil {
			t.Error("cancel() left the detached context live; the caller's defer must release it")
		}
	})
	t.Run("an already-cancelled caller still gets a live write context", func(t *testing.T) {
		parent := context.WithValue(context.Background(), detachedCtxTestKey{}, "report")
		ctx, cancelParent := context.WithCancel(parent)
		cancelParent()

		got, cancel := DetachedWriteContext(ctx)
		defer cancel()
		if got.Err() != nil {
			t.Fatalf("Err() = %v, want nil (a shutdown must not cost the ~25m report artifact)", got.Err())
		}
		if v, _ := got.Value(detachedCtxTestKey{}).(string); v != "report" {
			t.Errorf("Value = %q, want %q (WithoutCancel keeps values)", v, "report")
		}
	})
	t.Run("the shutdown grace bounds the write and reports the deadline as the cause", func(t *testing.T) {
		ctx, cancelParent := context.WithCancel(context.Background())
		got, cancel := detachedWriteContextGrace(ctx, time.Millisecond)
		defer cancel()

		cancelParent()
		select {
		case <-got.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the detached write context outlived its shutdown grace")
		}
		if !errors.Is(context.Cause(got), context.DeadlineExceeded) {
			t.Errorf("cause = %v, want context.DeadlineExceeded (DetachedWriteError keys on it)", context.Cause(got))
		}
	})
	t.Run("an untouched grace never cancels the write", func(t *testing.T) {
		got, cancel := detachedWriteContextGrace(context.Background(), time.Millisecond)
		defer cancel()
		time.Sleep(20 * time.Millisecond)
		if got.Err() != nil {
			t.Errorf("Err() = %v, want nil (the grace arms only on a shutdown)", got.Err())
		}
	})
}
