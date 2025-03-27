// pkg/rtpengine/manager.go
package rtpengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/common"
	"gateway/pkg/coordinator"
	"gateway/pkg/storage"
)

// Define EngineStatus type
type EngineStatus struct {
	IsHealthy  bool
	FailCount  int
	LastFailed time.Time
}

// Manager represents an RTPEngine manager that handles multiple RTPEngine instances.
type Manager struct {
	engines          []*RTPEngine
	weights          []int
	activeIdx        int32
	mutex            sync.RWMutex
	logger           *zap.Logger
	registry         *common.GoroutineRegistry
	storage          storage.StateStorage
	engineStatus     []EngineStatus
	failureThreshold int
}

// ManagerConfig defines the configuration for the RTPEngine manager
type ManagerConfig struct {
	Engines       []Config // RTPEngine configurations
	DialTimeout   time.Duration
	RequestExpiry time.Duration

	// Testing mode fields
	Disabled bool // Whether this component is disabled in testing mode
}

// NewManager creates a new RTPEngine manager
func NewManager(config ManagerConfig, logger *zap.Logger, registry *common.GoroutineRegistry, storage storage.StateStorage, coordinator *coordinator.Coordinator) (*Manager, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	if len(config.Engines) == 0 {
		return nil, errors.New("at least one RTPEngine instance required")
	}

	var engines []*RTPEngine
	var weights []int

	for _, engineConfig := range config.Engines {
		engine, err := New(engineConfig, logger)
		if err != nil {
			logger.Warn("Failed to initialize RTPEngine",
				zap.String("address", engineConfig.Address),
				zap.Int("port", engineConfig.Port),
				zap.Error(err))
			continue
		}

		// Use weight from config or default to 1
		weight := engineConfig.Weight
		if weight <= 0 {
			weight = 1
		}

		engines = append(engines, engine)
		weights = append(weights, weight)
	}

	if len(engines) == 0 {
		return nil, errors.New("all RTPEngine instances failed to initialize")
	}

	return &Manager{
		engines:  engines,
		weights:  weights,
		logger:   logger,
		registry: registry,
		storage:  storage,
	}, nil
}

// Start begins monitoring RTPEngine instances and performing health checks
func (m *Manager) Start(ctx context.Context) {
	// Start a health check for each engine
	for i, engine := range m.engines {
		i, engine := i, engine // Capture loop variables
		m.registry.Go(fmt.Sprintf("rtpengine-health-%d", i), func(ctx context.Context) {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := engine.Ping(ctx); err != nil {
						m.logger.Warn("RTPEngine health check failed",
							zap.String("address", engine.address),
							zap.Int("port", engine.port),
							zap.Error(err))
					} else {
						m.logger.Debug("RTPEngine health check succeeded",
							zap.String("address", engine.address),
							zap.Int("port", engine.port))
					}
				}
			}
		})
	}
}

// Stop stops the RTPEngine manager and closes all connections
func (m *Manager) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, engine := range m.engines {
		if err := engine.Close(); err != nil {
			m.logger.Warn("Error closing RTPEngine connection",
				zap.String("address", engine.address),
				zap.Int("port", engine.port),
				zap.Error(err))
		}
	}
}

// GetEngine returns an RTPEngine instance based on the provided session ID
func (m *Manager) GetEngine(ctx context.Context, callID string) (*RTPEngine, error) {
	// If we have a call ID, try to find an existing session
	if callID != "" && m.storage != nil {
		session, err := m.storage.GetRTPSession(ctx, callID)
		if err == nil && session != nil {
			if session.EngineIdx >= 0 && session.EngineIdx < len(m.engines) {
				return m.engines[session.EngineIdx], nil
			}
		}
	}

	// No existing session or storage error, use the active engine
	activeIdx := atomic.LoadInt32(&m.activeIdx)
	return m.engines[activeIdx], nil
}

// ProcessOffer processes an SDP offer with immediate failover on error
func (m *Manager) ProcessOffer(ctx context.Context, callID string, fromTag string, sdp string, flags map[string]string) (string, error) {
	m.mutex.RLock()
	engine := m.engines[atomic.LoadInt32(&m.activeIdx)]
	m.mutex.RUnlock()

	if engine == nil {
		return "", errors.New("no RTPEngine available")
	}

	// Initialize engine status if needed
	if m.engineStatus == nil {
		m.initEngineStatus()
	}

	// Build the message parameters
	params := map[string]interface{}{
		"call-id":  callID,
		"from-tag": fromTag,
		"sdp":      sdp,
	}

	// Add any additional flags
	if flags != nil {
		for k, v := range flags {
		params[k] = v
	}
	}

	// Send to current engine
	result, err := engine.Offer(ctx, params)
	if err != nil {
		m.logger.Error("ProcessOffer failed, triggering immediate failover",
			zap.Error(err),
			zap.String("callID", callID),
			zap.String("engine", engine.address))

		// Record failure and try immediate failover
		m.recordEngineFailure(engine.address)

		// Try failover now instead of waiting for health check
		if newEngine := m.tryFailover(ctx); newEngine != nil {
			m.logger.Info("Immediate failover successful, retrying offer with new engine",
				zap.String("oldEngine", engine.address),
				zap.String("newEngine", newEngine.address),
				zap.String("callID", callID))

			// Retry with new engine
			return m.failoverOffer(ctx, callID, fromTag, sdp, map[string]interface{}{})
		}

		return "", err
	}

	// Extract the SDP from the response
	sdpResult, ok := result["sdp"].(string)
	if !ok {
		return "", fmt.Errorf("no SDP in RTPEngine response")
	}

	return sdpResult, nil
}

// recordEngineFailure marks an engine as unhealthy and increments metrics
func (m *Manager) recordEngineFailure(engineAddr string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Initialize engine status if needed
	if m.engineStatus == nil {
		m.initEngineStatus()
	}

	// Mark the engine unhealthy
	for i, engine := range m.engines {
		if engine.address == engineAddr {
			// Increment fail count
			m.engineStatus[i].FailCount++
			m.engineStatus[i].LastFailed = time.Now()

			// Mark as unhealthy if it exceeds threshold
			if m.engineStatus[i].FailCount >= m.failureThreshold {
				m.engineStatus[i].IsHealthy = false
				m.logger.Warn("RTPEngine marked unhealthy due to failures",
					zap.String("engine", engineAddr),
					zap.Int("failCount", m.engineStatus[i].FailCount),
					zap.Int("threshold", m.failureThreshold))
			}
			break
		}
	}
}

// tryFailover attempts to find and switch to a healthy engine
// Returns the new engine if successful, nil otherwise
func (m *Manager) tryFailover(ctx context.Context) *RTPEngine {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Initialize engine status if needed
	if m.engineStatus == nil {
		m.initEngineStatus()
	}

	currentIdx := atomic.LoadInt32(&m.activeIdx)
	if int(currentIdx) >= len(m.engines) || m.engines[currentIdx] == nil {
		return nil
	}

	currentAddr := m.engines[currentIdx].address
	m.logger.Info("Attempting RTPEngine failover", zap.String("from", currentAddr))

	// Find next healthy engine
	for i, engine := range m.engines {
		if int32(i) != currentIdx && m.engineStatus[i].IsHealthy {
			m.logger.Info("Switching to new RTPEngine",
				zap.String("from", currentAddr),
				zap.String("to", engine.address))

			// Use simplified migration instead of complex MigrateActiveCalls
			// We're only updating the active engine index and not migrating existing calls
			oldIdx := int(currentIdx)
			newIdx := i

			// Release mutex before calling SimplifyMigration to avoid deadlock
			m.mutex.Unlock()
			m.SimplifyMigration(ctx, oldIdx, newIdx)
			m.mutex.Lock() // Re-acquire mutex

			// Record failover event
			m.recordFailoverEvent(currentAddr, engine.address, "Reactive failover")

			return engine
		}
	}

	m.logger.Error("Failover failed, no healthy engine available")
	return nil
}

// recordFailoverEvent logs a failover for monitoring
func (m *Manager) recordFailoverEvent(from, to, reason string) {
	// Store failover event for monitoring
	event := map[string]interface{}{
		"component": "rtpengine",
		"from":      from,
		"to":        to,
		"timestamp": time.Now().Unix(),
		"reason":    reason,
	}

	jsonBytes, _ := json.Marshal(event)

	// Store in shared storage if available
	if m.storage != nil {
		storageKey := fmt.Sprintf("failover:rtpengine:%d", time.Now().UnixNano())
		storeCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		if err := m.storage.Set(storeCtx, storageKey, jsonBytes, 24*time.Hour); err != nil {
			m.logger.Error("Failed to store failover event", zap.Error(err))
		}
	}
}

// ProcessAnswer processes an SDP answer through RTPEngine
func (m *Manager) ProcessAnswer(ctx context.Context, callID, fromTag, toTag string, sdp string, options map[string]interface{}) (string, error) {
	ctx, cancel := common.ContextWithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Create base parameters
	params := map[string]interface{}{
		"call-id":  callID,
		"from-tag": fromTag,
		"to-tag":   toTag,
		"sdp":      sdp,
	}

	// Add any additional options
	for k, v := range options {
		params[k] = v
	}

	// Get the appropriate engine
	engine, err := m.GetEngine(ctx, callID)
	if err != nil {
		return "", fmt.Errorf("failed to get RTPEngine: %w", err)
	}

	// Send the answer
	result, err := engine.Answer(ctx, params)
	if err != nil {
		m.logger.Warn("RTPEngine answer failed, trying failover",
			zap.String("address", engine.address),
			zap.Int("port", engine.port),
			zap.Error(err))

		return m.failoverAnswer(ctx, callID, fromTag, toTag, sdp, options)
	}

	// Extract the SDP
	sdpResult, ok := result["sdp"].(string)
	if !ok {
		return "", fmt.Errorf("no SDP in RTPEngine response")
	}

	// Update the session
	if m.storage != nil {
		session, err := m.storage.GetRTPSession(ctx, callID)
		if err == nil && session != nil {
			session.ToTag = toTag
			session.Updated = time.Now()
			m.storage.StoreRTPSession(ctx, session)
		}
	}

	return sdpResult, nil
}

// DeleteSession deletes a session from RTPEngine
func (m *Manager) DeleteSession(ctx context.Context, callID string) error {
	ctx, cancel := common.ContextWithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Get the appropriate engine
	engine, err := m.GetEngine(ctx, callID)
	if err != nil {
		return fmt.Errorf("failed to get RTPEngine: %w", err)
	}

	// Create parameters
	params := map[string]interface{}{
		"call-id": callID,
	}

	// Send the delete
	_, err = engine.Delete(ctx, params)
	if err != nil {
		m.logger.Warn("RTPEngine delete failed, trying failover",
			zap.String("address", engine.address),
			zap.Int("port", engine.port),
			zap.Error(err))

		return m.failoverDelete(ctx, callID)
	}

	// Clean up the session
	if m.storage != nil {
		m.storage.DeleteRTPSession(ctx, callID)
	}

	return nil
}

// failoverOffer attempts to send an offer to an alternative RTPEngine
func (m *Manager) failoverOffer(ctx context.Context, callID, fromTag, sdp string, options map[string]interface{}) (string, error) {
	m.mutex.Lock()
	activeIdx := atomic.LoadInt32(&m.activeIdx)
	m.mutex.Unlock()

	// Find a healthy engine
	var healthyEngine *RTPEngine
	var healthyIdx int

	for i, engine := range m.engines {
		if int32(i) != activeIdx && engine != nil {
			// Initialize engine status if needed
			m.mutex.Lock()
			if m.engineStatus == nil {
				m.initEngineStatus()
			}
			isHealthy := m.engineStatus[i].IsHealthy
			m.mutex.Unlock()

			if isHealthy {
				healthyEngine = engine
				healthyIdx = i
				break
			}
		}
	}

	if healthyEngine == nil {
		return "", fmt.Errorf("no healthy RTPEngine available for failover")
	}

	// Switch to the healthy engine for future requests
	oldIdx := int(activeIdx)
	m.SimplifyMigration(ctx, oldIdx, healthyIdx)

	// Send the offer to the new engine
	result, err := healthyEngine.Offer(ctx, map[string]interface{}{
			"call-id":  callID,
			"from-tag": fromTag,
			"sdp":      sdp,
	})

		if err != nil {
		return "", fmt.Errorf("failover offer failed: %w", err)
	}

	// Extract SDP from result
		sdpResult, ok := result["sdp"].(string)
		if !ok {
		return "", fmt.Errorf("no SDP in failover result")
		}

		return sdpResult, nil
}

// failoverAnswer attempts to send an answer to an alternative RTPEngine
func (m *Manager) failoverAnswer(ctx context.Context, callID, fromTag, toTag string, sdp string, options map[string]interface{}) (string, error) {
	ctx, cancel := common.ContextWithTimeout(ctx, 5*time.Second)
	defer cancel()
	currentIdx := atomic.LoadInt32(&m.activeIdx)

	// Try each engine in turn
	for i := 0; i < len(m.engines); i++ {
		idx := (int(currentIdx) + i + 1) % len(m.engines)
		if idx == int(currentIdx) {
			continue // Skip the one that just failed
		}

		engine := m.engines[idx]

		// Create parameters
		params := map[string]interface{}{
			"call-id":  callID,
			"from-tag": fromTag,
			"to-tag":   toTag,
			"sdp":      sdp,
		}

		// Add any additional options
		for k, v := range options {
			params[k] = v
		}

		// Try this engine
		result, err := engine.Answer(ctx, params)
		if err != nil {
			m.logger.Warn("RTPEngine failover answer attempt failed",
				zap.String("address", engine.address),
				zap.Int("port", engine.port),
				zap.Error(err))
			continue
		}

		// Success - update active engine
		atomic.StoreInt32(&m.activeIdx, int32(idx))
		m.logger.Info("Switched active RTPEngine for answer",
			zap.String("from", fmt.Sprintf("%s:%d", m.engines[currentIdx].address, m.engines[currentIdx].port)),
			zap.String("to", fmt.Sprintf("%s:%d", engine.address, engine.port)))

		// Update the session
		if m.storage != nil {
			m.storage.StoreRTPSession(ctx, &storage.RTPSession{
				CallID:     callID,
				EngineIdx:  idx,
				EngineAddr: fmt.Sprintf("%s:%d", engine.address, engine.port),
				FromTag:    fromTag,
				ToTag:      toTag,
				Updated:    time.Now(),
			})
		}

		// Extract the SDP
		sdpResult, ok := result["sdp"].(string)
		if !ok {
			return "", fmt.Errorf("no SDP in RTPEngine response")
		}

		return sdpResult, nil
	}

	return "", fmt.Errorf("all RTPEngine instances failed")
}

// failoverDelete attempts to delete a session on an alternative RTPEngine
func (m *Manager) failoverDelete(ctx context.Context, callID string) error {
	ctx, cancel := common.ContextWithTimeout(ctx, 5*time.Second)
	defer cancel()
	currentIdx := atomic.LoadInt32(&m.activeIdx)

	// Try each engine in turn
	for i := 0; i < len(m.engines); i++ {
		idx := (int(currentIdx) + i + 1) % len(m.engines)
		if idx == int(currentIdx) {
			continue // Skip the one that just failed
		}

		engine := m.engines[idx]

		// Create parameters
		params := map[string]interface{}{
			"call-id": callID,
		}

		// Try this engine
		_, err := engine.Delete(ctx, params)
		if err != nil {
			m.logger.Warn("RTPEngine failover delete attempt failed",
				zap.String("address", engine.address),
				zap.Int("port", engine.port),
				zap.Error(err))
			continue
		}

		// Success - update active engine
		atomic.StoreInt32(&m.activeIdx, int32(idx))
		m.logger.Info("Switched active RTPEngine for delete",
			zap.String("from", fmt.Sprintf("%s:%d", m.engines[currentIdx].address, m.engines[currentIdx].port)),
			zap.String("to", fmt.Sprintf("%s:%d", engine.address, engine.port)))

		// Clean up the session
		if m.storage != nil {
			m.storage.DeleteRTPSession(ctx, callID)
		}

		return nil
	}

	return fmt.Errorf("all RTPEngine instances failed")
}

// SimplifyMigration replaces the complex MigrateActiveCalls logic with a simpler approach
// that only routes new calls to the new active engine without trying to migrate existing sessions
func (m *Manager) SimplifyMigration(ctx context.Context, oldEngineIdx, newEngineIdx int) {
	if oldEngineIdx == newEngineIdx {
		return
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	if oldEngineIdx < 0 || oldEngineIdx >= len(m.engines) || newEngineIdx < 0 || newEngineIdx >= len(m.engines) {
		m.logger.Error("Invalid engine indices for migration",
			zap.Int("oldEngineIdx", oldEngineIdx),
			zap.Int("newEngineIdx", newEngineIdx),
			zap.Int("engineCount", len(m.engines)))
		return
	}

	oldEngine := m.engines[oldEngineIdx]
	newEngine := m.engines[newEngineIdx]

	// Just switch the active engine - no migration of existing sessions
	atomic.StoreInt32(&m.activeIdx, int32(newEngineIdx))

	m.logger.Info("Switched active RTPEngine for new calls only",
		zap.String("from", oldEngine.address),
		zap.Int("fromPort", oldEngine.port),
		zap.String("to", newEngine.address),
		zap.Int("toPort", newEngine.port))

	// Record a migration event if storage is available
	if m.storage != nil {
		event := map[string]interface{}{
			"component":    "rtpengine",
			"event":        "engine_switch",
			"old_engine":   fmt.Sprintf("%s:%d", oldEngine.address, oldEngine.port),
			"new_engine":   fmt.Sprintf("%s:%d", newEngine.address, newEngine.port),
			"timestamp":    time.Now().Unix(),
			"no_migration": true,
		}

		eventBytes, _ := json.Marshal(event)
		storeCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		key := fmt.Sprintf("events:rtpengine:migration:%d", time.Now().UnixNano())
		if err := m.storage.Set(storeCtx, key, eventBytes, 24*time.Hour); err != nil {
			m.logger.Error("Failed to store migration event", zap.Error(err))
		}
	}
}

// Initialize engine status
func (m *Manager) initEngineStatus() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Default failure threshold
	if m.failureThreshold <= 0 {
		m.failureThreshold = 3
	}

	// Initialize status for all engines
	m.engineStatus = make([]EngineStatus, len(m.engines))
	for i := range m.engineStatus {
		m.engineStatus[i].IsHealthy = true
	}
}

// getFlagsOrDefault helper function
func getFlagsOrDefault(flags []map[string]string) map[string]string {
	if len(flags) > 0 && flags[0] != nil {
		return flags[0]
	}
	return map[string]string{}
}
