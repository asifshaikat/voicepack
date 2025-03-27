package failover

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MockCoordinator provides a mock implementation of the coordinator for testing
type MockCoordinator struct {
	leader       map[string]bool // Component name -> is leader
	mutex        sync.RWMutex
	logger       *zap.Logger
	leaseTimeout time.Duration
	callbacks    map[string][]func(bool)
}

// NewMockCoordinator creates a new mock coordinator
func NewMockCoordinator(logger *zap.Logger) *MockCoordinator {
	return &MockCoordinator{
		leader:       make(map[string]bool),
		logger:       logger,
		leaseTimeout: 5 * time.Second,
		callbacks:    make(map[string][]func(bool)),
	}
}

// IsLeader returns whether this node is the leader for a component
func (m *MockCoordinator) IsLeader(components ...string) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// If no components specified, check the global leader
	if len(components) == 0 {
		if isLeader, ok := m.leader["global"]; ok {
			return isLeader
		}
		return false
	}

	// Check each component
	for _, component := range components {
		if isLeader, ok := m.leader[component]; ok && isLeader {
			return true
		}
	}

	return false
}

// SetLeader sets leadership status for a component
func (m *MockCoordinator) SetLeader(component string, isLeader bool) {
	m.mutex.Lock()

	// Store previous state
	oldLeaderStatus := m.leader[component]

	// Update leader status
	m.leader[component] = isLeader

	// Get callbacks that need to be fired
	var callbacksToFire []func(bool)
	if callbacks, ok := m.callbacks[component]; ok && oldLeaderStatus != isLeader {
		callbacksToFire = make([]func(bool), len(callbacks))
		copy(callbacksToFire, callbacks)
	}

	m.mutex.Unlock()

	// Fire callbacks outside the lock
	for _, callback := range callbacksToFire {
		callback(isLeader)
	}

	m.logger.Info("Mock coordinator leadership changed",
		zap.String("component", component),
		zap.Bool("isLeader", isLeader))
}

// RegisterLeadershipCallback registers a callback to be called when leadership changes
func (m *MockCoordinator) RegisterLeadershipCallback(component string, callback func(bool)) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, ok := m.callbacks[component]; !ok {
		m.callbacks[component] = make([]func(bool), 0)
	}

	m.callbacks[component] = append(m.callbacks[component], callback)
}

// Start starts the coordinator (no-op in mock)
func (m *MockCoordinator) Start(ctx context.Context) error {
	return nil
}

// Stop stops the coordinator (no-op in mock)
func (m *MockCoordinator) Stop() error {
	return nil
}

// GetLeaseTimeout returns the configured lease timeout
func (m *MockCoordinator) GetLeaseTimeout() time.Duration {
	return m.leaseTimeout
}

// SetLeaseTimeout sets the lease timeout
func (m *MockCoordinator) SetLeaseTimeout(timeout time.Duration) {
	m.leaseTimeout = timeout
}

// TriggerLeadershipChange simulates a leadership change event
func (m *MockCoordinator) TriggerLeadershipChange(component string, isLeader bool) {
	m.SetLeader(component, isLeader)
}
