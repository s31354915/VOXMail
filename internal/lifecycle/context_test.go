package lifecycle

import (
	"context"
	"testing"
	"time"
)

func TestCleanupContextDetachesCancellationButPreservesValues(t *testing.T) {
	type key struct{}
	parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), key{}, "request-value"))
	cancelParent()

	ctx, cancel := CleanupContext(parent)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("cleanup context is canceled: %v", err)
	}
	if got := ctx.Value(key{}); got != "request-value" {
		t.Fatalf("context value = %v, want request-value", got)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > CleanupTimeout {
		t.Fatalf("cleanup deadline remaining = %s, want 0 < remaining <= %s", remaining, CleanupTimeout)
	}
}

func TestCleanupContextUsesBoundedContextForNilParent(t *testing.T) {
	ctx, cancel := CleanupContext(nil)
	defer cancel()
	if ctx == nil {
		t.Fatal("CleanupContext(nil) returned nil context")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("nil-parent cleanup context has no deadline")
	}
}
