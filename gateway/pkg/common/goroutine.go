// pkg/common/goroutine.go
package common

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// GoroutineRegistry manages goroutines for proper tracking and shutdown
type GoroutineRegistry struct {
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	total      int64
	active     int64
	panicCount int64
	logger     *zap.Logger
}

// NewGoroutineRegistry creates a new goroutine registry
func NewGoroutineRegistry(logger *zap.Logger) *GoroutineRegistry {
	if logger == nil {
		// Create default logger
		logger, _ = zap.NewProduction()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &GoroutineRegistry{
		ctx:    ctx,
		cancel: cancel,
		logger: logger,
	}
}

// Go starts a new goroutine with proper tracking and panic recovery
func (gr *GoroutineRegistry) Go(name string, f func(ctx context.Context)) {
	gr.wg.Add(1)
	id := atomic.AddInt64(&gr.total, 1)
	atomic.AddInt64(&gr.active, 1)

	go func() {
		defer gr.wg.Done()
		defer atomic.AddInt64(&gr.active, -1)

		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt64(&gr.panicCount, 1)

				stack := make([]byte, 4096)
				stack = stack[:runtime.Stack(stack, false)]
				gr.logger.Error("Goroutine panic",
					zap.String("name", name),
					zap.Int64("id", id),
					zap.Any("panic", r),
					zap.String("stack", string(stack)))
			}
		}()

		gr.logger.Debug("Starting goroutine",
			zap.String("name", name),
			zap.Int64("id", id))

		f(gr.ctx)

		gr.logger.Debug("Goroutine completed",
			zap.String("name", name),
			zap.Int64("id", id))
	}()
}

// Shutdown cancels the context and waits for all goroutines to finish
func (gr *GoroutineRegistry) Shutdown(timeout time.Duration) error {
	gr.logger.Info("Shutting down goroutine registry",
		zap.Int64("activeCount", atomic.LoadInt64(&gr.active)))

	// Cancel context to signal goroutines to stop
	gr.cancel()

	// Create a channel to signal completion
	done := make(chan struct{})

	// Create a new context with timeout for the wait
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Start a goroutine to wait for all workers to finish
	go func() {
		gr.wg.Wait()
		close(done)
	}()

	// Wait for completion or timeout
	select {
	case <-done:
		gr.logger.Info("All goroutines have exited")
		return nil
	case <-ctx.Done():
		gr.logger.Warn("Shutdown timed out, some goroutines didn't exit",
			zap.Int64("remaining", atomic.LoadInt64(&gr.active)),
			zap.Duration("timeout", timeout))
		return fmt.Errorf("shutdown timed out, %d goroutines didn't exit",
			atomic.LoadInt64(&gr.active))
	}
}

// ActiveCount returns the number of active goroutines
func (gr *GoroutineRegistry) ActiveCount() int64 {
	return atomic.LoadInt64(&gr.active)
}

// Context returns the registry's context
func (gr *GoroutineRegistry) Context() context.Context {
	return gr.ctx
}

// TotalCount returns the total number of goroutines created
func (gr *GoroutineRegistry) TotalCount() int64 {
	return atomic.LoadInt64(&gr.total)
}

// PanicCount returns the number of goroutine panics
func (gr *GoroutineRegistry) PanicCount() int64 {
	return atomic.LoadInt64(&gr.panicCount)
}
