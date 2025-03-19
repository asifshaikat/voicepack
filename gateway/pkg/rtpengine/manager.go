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
	"gateway/pkg/coordinator"
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

// ProcessOffer processes an SDP offer through RTPEngine
func (m *Manager) ProcessOffer(ctx context.Context, callID, fromTag string, sdp string, options map[string]interface{}) (string, error) {
	ctx, cancel := common.ContextWithTimeout(ctx, 5*time.Second)
	defer cancel()
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
func (m *Manager) failoverOffer(ctx context.Context, callID, fromTag string, sdp string, options map[string]interface{}) (string, error) {
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
		// Start a background task to migrate active calls from the old engine to the new one

		atomic.StoreInt32(&m.activeIdx, int32(idx))

		go func(oldIdx, newIdx int) {
			// Create a background context for migration
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			// Migrate active calls
			if err := m.MigrateActiveCalls(bgCtx, oldIdx, newIdx); err != nil {
				m.logger.Error("Failed to migrate active calls",
					zap.Error(err),
					zap.Int("fromIdx", oldIdx),
					zap.Int("toIdx", newIdx))
			}
		}(int(currentIdx), idx)
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

// MigrateActiveCalls moves active media sessions from one RTPEngine to another
func (m *Manager) MigrateActiveCalls(ctx context.Context, fromIdx, toIdx int) error {
	if fromIdx < 0 || fromIdx >= len(m.engines) || toIdx < 0 || toIdx >= len(m.engines) {
		return fmt.Errorf("invalid engine indices: from=%d, to=%d", fromIdx, toIdx)
	}

	if fromIdx == toIdx {
		return nil // Nothing to do
	}

	fromEngine := m.engines[fromIdx]
	toEngine := m.engines[toIdx]

	m.logger.Info("Starting migration of active calls",
		zap.String("fromEngine", fmt.Sprintf("%s:%d", fromEngine.address, fromEngine.port)),
		zap.String("toEngine", fmt.Sprintf("%s:%d", toEngine.address, toEngine.port)))

	// Get all active sessions
	sessions, err := m.storage.ListRTPSessions(ctx)
	if err != nil {
		m.logger.Error("Failed to list RTP sessions", zap.Error(err))
		return err
	}

	var migrationCount int
	var errorCount int

	// Create a worker pool for parallel migration
	const workerCount = 10
	var wg sync.WaitGroup
	sessionCh := make(chan *storage.RTPSession, len(sessions))
	resultCh := make(chan struct {
		callID string
		err    error
	}, len(sessions))

	// Start worker goroutines
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for session := range sessionCh {
				// Skip sessions that aren't on the source engine
				if session.EngineIdx != fromIdx {
					continue
				}

				err := m.migrateSession(ctx, session, toEngine, toIdx)
				resultCh <- struct {
					callID string
					err    error
				}{session.CallID, err}
			}
		}()
	}

	// Feed sessions to workers
	for _, session := range sessions {
		sessionCh <- session
	}
	close(sessionCh)

	// Wait for workers and collect results
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Process results
	for result := range resultCh {
		if result.err != nil {
			m.logger.Error("Failed to migrate RTP session",
				zap.String("callID", result.callID),
				zap.Error(result.err))
			errorCount++
		} else {
			migrationCount++
		}
	}

	m.logger.Info("RTP session migration completed",
		zap.Int("total", len(sessions)),
		zap.Int("migrated", migrationCount),
		zap.Int("errors", errorCount))

	if errorCount > 0 {
		return fmt.Errorf("failed to migrate %d/%d sessions", errorCount, len(sessions))
	}

	return nil
}

// migrateSession migrates a single RTP session from one engine to another
func (m *Manager) migrateSession(ctx context.Context, session *storage.RTPSession, toEngine *RTPEngine, toIdx int) error {

	callID := session.CallID
	fromTag := session.FromTag
	toTag := session.ToTag

	m.logger.Debug("Migrating session",
		zap.String("callID", callID),
		zap.String("fromTag", fromTag),
		zap.String("toTag", toTag))

	// Step 1: Query call details from source engine
	params := map[string]interface{}{
		"call-id": callID,
	}

	if fromTag != "" {
		params["from-tag"] = fromTag
	}

	if toTag != "" {
		params["to-tag"] = toTag
	}

	// Get the source engine
	sourceEngine := m.engines[session.EngineIdx]

	// Query the call details
	queryResult, err := sourceEngine.Query(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to query source engine: %w", err)
	}

	// Check if the call still exists
	if result, ok := queryResult["result"].(string); !ok || result != "ok" {
		// Call no longer exists, update storage and return
		session.EngineIdx = toIdx
		session.EngineAddr = fmt.Sprintf("%s:%d", toEngine.address, toEngine.port)
		session.Updated = time.Now()
		m.storage.StoreRTPSession(ctx, session)
		return nil
	}

	// Step 2: Create call on destination engine with similar parameters
	// We need to create a new offer with the same parameters
	// First, let's create options based on detected media
	options := make(map[string]interface{})

	// Determine if WebRTC is being used
	totals, ok := queryResult["totals"].(map[string]interface{})
	if ok {
		if _, hasICE := totals["DTLS"]; hasICE {
			options["ICE"] = "force"
			options["DTLS"] = "passive"
		}
	}

	// Add SDP from the query if available
	sdpOffer := ""
	if sdp, ok := queryResult["sdp-a"].(string); ok && sdp != "" {
		sdpOffer = sdp
	} else if sdp, ok := queryResult["sdp-b"].(string); ok && sdp != "" {
		sdpOffer = sdp
	}

	if sdpOffer == "" {
		return fmt.Errorf("no SDP available for migration")
	}

	// Create the call on the destination engine
	newParams := map[string]interface{}{
		"call-id":  callID,
		"from-tag": fromTag,
		"sdp":      sdpOffer,
		"replace":  "origin,session-connection",
		"label":    "migrated",
	}

	// Add any additional options
	for k, v := range options {
		newParams[k] = v
	}

	// Send the offer to new engine
	offerResult, err := toEngine.Offer(ctx, newParams)
	if err != nil {
		return fmt.Errorf("failed to create offer on destination engine: %w", err)
	}

	// Check for success
	if result, ok := offerResult["result"].(string); !ok || result != "ok" {
		return fmt.Errorf("offer creation failed on destination engine")
	}

	// If we have a to-tag, send an answer as well
	if toTag != "" {
		answerSDP := ""
		if sdp, ok := queryResult["sdp-b"].(string); ok && sdp != "" {
			answerSDP = sdp
		} else if sdp, ok := queryResult["sdp-a"].(string); ok && sdp != "" {
			answerSDP = sdp
		}

		if answerSDP != "" {
			answerParams := map[string]interface{}{
				"call-id":  callID,
				"from-tag": fromTag,
				"to-tag":   toTag,
				"sdp":      answerSDP,
				"replace":  "origin,session-connection",
				"label":    "migrated",
			}

			// Add any additional options
			for k, v := range options {
				answerParams[k] = v
			}

			// Send the answer to new engine
			_, err := toEngine.Answer(ctx, answerParams)
			if err != nil {
				// Try to clean up the call on destination
				toEngine.Delete(ctx, map[string]interface{}{"call-id": callID})
				return fmt.Errorf("failed to create answer on destination engine: %w", err)
			}
		}
	}

	// Step 3: Update session in storage
	session.EngineIdx = toIdx
	session.EngineAddr = fmt.Sprintf("%s:%d", toEngine.address, toEngine.port)
	session.Updated = time.Now()
	m.storage.StoreRTPSession(ctx, session)

	// Step 4: Delete the call on the source engine after a delay
	// This gives time for media to switch over
	time.AfterFunc(500*time.Millisecond, func() {
		deleteCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		sourceEngine.Delete(deleteCtx, map[string]interface{}{"call-id": callID})
	})

	return fmt.Errorf("all RTPEngine instances failed")
}
