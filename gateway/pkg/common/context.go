// pkg/common/context.go
package common

import (
	"context"
	"time"
)

// DefaultTimeout is the default timeout for operations
const DefaultTimeout = 30 * time.Second

// EnsureTimeout ensures a context has a timeout
func EnsureTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	// Check if context already has a deadline
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			// Context already has a valid deadline
			return ctx, func() {}
		}
	}

	// No deadline or expired deadline, create a new one
	return context.WithTimeout(ctx, timeout)
}

// ContextWithTimeout creates a context with timeout if none exists
func ContextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return EnsureTimeout(parent, timeout)
}

// QuickTimeout creates a context with a short timeout
func QuickTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return EnsureTimeout(parent, 5*time.Second)
}

// LongTimeout creates a context with a longer timeout
func LongTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return EnsureTimeout(parent, 2*time.Minute)
}
