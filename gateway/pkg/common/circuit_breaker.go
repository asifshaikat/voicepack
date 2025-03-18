// pkg/common/circuit_breaker.go
package common

import (
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// CircuitBreakerState represents the state of a circuit breaker
type CircuitBreakerState int32

const (
	StateClosed CircuitBreakerState = iota
	StateOpen
	StateHalfOpen
)

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	name                string
	state               int32 // 0=closed, 1=open, 2=half-open
	failureThreshold    int32
	resetTimeout        time.Duration
	halfOpenMaxReqs     int32
	halfOpenReqs        int32
	consecutiveFailures int32
	lastStateChange     time.Time
	lastFailure         time.Time

	// Metrics
	totalRequests  int64
	totalSuccesses int64
	totalFailures  int64
	opens          int64

	mu     sync.RWMutex // Only for non-atomic fields
	logger *zap.Logger
}

// CircuitBreakerConfig holds configuration for the circuit breaker
type CircuitBreakerConfig struct {
	FailureThreshold int           `json:"failure_threshold"`
	ResetTimeout     time.Duration `json:"reset_timeout"`
	HalfOpenMaxReqs  int           `json:"half_open_max_requests"`
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(name string, config CircuitBreakerConfig, logger *zap.Logger) *CircuitBreaker {
	if logger == nil {
		// Create default logger
		logger, _ = zap.NewProduction()
	}

	// Set defaults
	failureThreshold := int32(config.FailureThreshold)
	if failureThreshold <= 0 {
		failureThreshold = 5
	}

	resetTimeout := config.ResetTimeout
	if resetTimeout <= 0 {
		resetTimeout = 30 * time.Second
	}

	halfOpenMaxReqs := int32(config.HalfOpenMaxReqs)
	if halfOpenMaxReqs <= 0 {
		halfOpenMaxReqs = 3
	}

	return &CircuitBreaker{
		name:             name,
		state:            int32(StateClosed), // Closed initially
		failureThreshold: failureThreshold,
		resetTimeout:     resetTimeout,
		halfOpenMaxReqs:  halfOpenMaxReqs,
		lastStateChange:  time.Now(),
		logger:           logger,
	}
}

// GetState returns the current state of the circuit breaker
func (cb *CircuitBreaker) GetState() CircuitBreakerState {
	return CircuitBreakerState(atomic.LoadInt32(&cb.state))
}

// AllowRequest determines if a request should be allowed
func (cb *CircuitBreaker) AllowRequest() bool {
	state := CircuitBreakerState(atomic.LoadInt32(&cb.state))

	atomic.AddInt64(&cb.totalRequests, 1)

	switch state {
	case StateClosed: // Closed - allow all requests
		return true

	case StateOpen: // Open - check timeout
		cb.mu.RLock()
		timeout := time.Since(cb.lastStateChange) > cb.resetTimeout
		cb.mu.RUnlock()

		if timeout {
			// Try half-open state
			if atomic.CompareAndSwapInt32(&cb.state, int32(StateOpen), int32(StateHalfOpen)) {
				cb.mu.Lock()
				cb.lastStateChange = time.Now()
				cb.halfOpenReqs = 0
				cb.mu.Unlock()

				cb.logger.Info("Circuit half-open - testing recovery",
					zap.String("circuit", cb.name))
			}
			return cb.AllowRequest() // Recurse to check half-open logic
		}
		return false

	case StateHalfOpen: // Half-open - allow limited requests
		reqs := atomic.AddInt32(&cb.halfOpenReqs, 1)
		return reqs <= cb.halfOpenMaxReqs
	}

	return false // Shouldn't get here
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	atomic.AddInt64(&cb.totalSuccesses, 1)
	atomic.StoreInt32(&cb.consecutiveFailures, 0)

	state := CircuitBreakerState(atomic.LoadInt32(&cb.state))
	if state == StateHalfOpen {
		// Reset on success in half-open state
		if atomic.CompareAndSwapInt32(&cb.state, int32(StateHalfOpen), int32(StateClosed)) {
			cb.mu.Lock()
			cb.lastStateChange = time.Now()
			cb.mu.Unlock()

			cb.logger.Info("Circuit closed - service recovered",
				zap.String("circuit", cb.name))
		}
	}
}

// RecordFailure records a failed request
func (cb *CircuitBreaker) RecordFailure() {
	atomic.AddInt64(&cb.totalFailures, 1)
	failures := atomic.AddInt32(&cb.consecutiveFailures, 1)

	cb.mu.Lock()
	cb.lastFailure = time.Now()
	cb.mu.Unlock()

	state := CircuitBreakerState(atomic.LoadInt32(&cb.state))
	threshold := atomic.LoadInt32(&cb.failureThreshold)

	// Open circuit on too many failures
	if (state == StateClosed && failures >= threshold) || state == StateHalfOpen {
		if atomic.CompareAndSwapInt32(&cb.state, int32(state), int32(StateOpen)) {
			atomic.AddInt64(&cb.opens, 1)

			cb.mu.Lock()
			cb.lastStateChange = time.Now()
			cb.mu.Unlock()

			cb.logger.Warn("Circuit opened due to failures",
				zap.String("circuit", cb.name),
				zap.Int32("failures", failures),
				zap.Int32("threshold", threshold))
		}
	}
}

// GetMetrics returns metrics for the circuit breaker
func (cb *CircuitBreaker) GetMetrics() map[string]int64 {
	return map[string]int64{
		"requests":  atomic.LoadInt64(&cb.totalRequests),
		"successes": atomic.LoadInt64(&cb.totalSuccesses),
		"failures":  atomic.LoadInt64(&cb.totalFailures),
		"opens":     atomic.LoadInt64(&cb.opens),
	}
}

// Reset resets the circuit breaker to closed state
func (cb *CircuitBreaker) Reset() {
	atomic.StoreInt32(&cb.state, int32(StateClosed))
	atomic.StoreInt32(&cb.consecutiveFailures, 0)

	cb.mu.Lock()
	cb.lastStateChange = time.Now()
	cb.mu.Unlock()

	cb.logger.Info("Circuit manually reset", zap.String("circuit", cb.name))
}
