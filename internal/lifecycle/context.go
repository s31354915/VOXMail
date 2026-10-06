// Package lifecycle contains small helpers for application shutdown and
// background-work ownership.
package lifecycle

import (
	"context"
	"time"
)

// CleanupTimeout bounds best-effort durable bookkeeping after its primary
// operation has been canceled. It is intentionally short so cleanup cannot
// hold shutdown or a worker indefinitely.
const CleanupTimeout = 5 * time.Second

// CleanupContext returns a context for final durable status updates. It keeps
// values from parent, deliberately ignores parent cancellation, and applies a
// fresh bounded deadline. Callers must call the returned cancel function.
//
// This is not a replacement for the operation context: use the operation
// context for primary work, and use this helper only after the outcome is
// already known and a small bookkeeping write should still be attempted.
func CleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), CleanupTimeout)
}
