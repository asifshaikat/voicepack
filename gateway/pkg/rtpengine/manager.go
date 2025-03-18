// pkg/rtpengine/manager.go
package rtpengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/common"
	"gateway/pkg/storage"
)

// Manager represents an RTPEngine manager that handles multiple RTPEngine instances.
type Manager struct {
	engines   []*RTPEngine
	weights   []int
	activeIdx int32
	mutex     sync.RWMutex
	logger    *zap.Logger
	registry  *common.GoroutineRegistry
	storage   storage.StateStorage
}

// ManagerConfig holds the configuration for the RTPEngine manager
type ManagerConfig struct {
	Engines []Config `json:"engines"`
}

// NewManager creates a new RTPEngine manager
func NewManager(config ManagerConfig, logger *zap.Logger, registry *common.GoroutineRegistry, storage storage.StateStorage) (*Manager, error) {
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

// ProcessOffer processes an SDP offer through RTPEngine
func (m *Manager) ProcessOffer(ctx context.Context, callID, fromTag string, sdp string, options map[string]interface{}) (string, error) {
	// Create base parameters
	params := map[string]interface{}{
		"call-id":  callID,
		"from-tag": fromTag,
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

	// Send the offer
	result, err := engine.Offer(ctx, params)
	if err != nil {
		m.logger.Warn("RTPEngine offer failed, trying failover",
			zap.String("address", engine.address),
			zap.Int("port", engine.port),
			zap.Error(err))

		return m.failoverOffer(ctx, callID, fromTag, sdp, options)
	}

	// Extract the SDP
	sdpResult, ok := result["sdp"].(string)
	if !ok {
		return "", fmt.Errorf("no SDP in RTPEngine response")
	}

	// Store the session
	idx := -1
	for i, e := range m.engines {
		if e == engine {
			idx = i
			break
		}
	}

	if idx >= 0 && m.storage != nil {
		m.storage.StoreRTPSession(ctx, &storage.RTPSession{
			CallID:     callID,
			EngineIdx:  idx,
			EngineAddr: fmt.Sprintf("%s:%d", engine.address, engine.port),
			FromTag:    fromTag,
			Created:    time.Now(),
		})
	}

	return sdpResult, nil
}

// ProcessAnswer processes an SDP answer through RTPEngine
func (m *Manager) ProcessAnswer(ctx context.Context, callID, fromTag, toTag string, sdp string, options map[string]interface{}) (string, error) {
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
func (m *Manager) failoverOffer(ctx context.Context, callID, fromTag string, sdp string, options map[string]interface{}) (string, error) {
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
			"sdp":      sdp,
		}

		// Add any additional options
		for k, v := range options {
			params[k] = v
		}

		// Try this engine
		result, err := engine.Offer(ctx, params)
		if err != nil {
			m.logger.Warn("RTPEngine failover attempt failed",
				zap.String("address", engine.address),
				zap.Int("port", engine.port),
				zap.Error(err))
			continue
		}

		// Success - update active engine
		atomic.StoreInt32(&m.activeIdx, int32(idx))
		m.logger.Info("Switched active RTPEngine",
			zap.String("from", fmt.Sprintf("%s:%d", m.engines[currentIdx].address, m.engines[currentIdx].port)),
			zap.String("to", fmt.Sprintf("%s:%d", engine.address, engine.port)))

		// Store the session
		if m.storage != nil {
			m.storage.StoreRTPSession(ctx, &storage.RTPSession{
				CallID:     callID,
				EngineIdx:  idx,
				EngineAddr: fmt.Sprintf("%s:%d", engine.address, engine.port),
				FromTag:    fromTag,
				Created:    time.Now(),
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

// failoverAnswer attempts to send an answer to an alternative RTPEngine
func (m *Manager) failoverAnswer(ctx context.Context, callID, fromTag, toTag string, sdp string, options map[string]interface{}) (string, error) {
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
