// pkg/ami/manager.go
package ami

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

// Manager handles multiple AMI clients with failover
type Manager struct {
	clients   []*AMIClient
	activeIdx int32
	mu        sync.RWMutex
	logger    *zap.Logger
	registry  *common.GoroutineRegistry
	storage   storage.StateStorage
}

// ManagerConfig holds the configuration for the AMI manager
type ManagerConfig struct {
	Clients []Config `json:"clients"`
}

// NewManager creates a new AMI manager
func NewManager(config ManagerConfig, logger *zap.Logger, registry *common.GoroutineRegistry, storage storage.StateStorage) (*Manager, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	if len(config.Clients) == 0 {
		return nil, errors.New("at least one AMI client required")
	}

	var clients []*AMIClient

	for _, clientConfig := range config.Clients {
		client, err := New(clientConfig, logger)
		if err != nil {
			logger.Warn("Failed to create AMI client",
				zap.String("address", clientConfig.Address),
				zap.Error(err))
			continue
		}

		clients = append(clients, client)
	}

	if len(clients) == 0 {
		return nil, errors.New("all AMI clients failed to initialize")
	}

	return &Manager{
		clients:  clients,
		logger:   logger,
		registry: registry,
		storage:  storage,
	}, nil
}

// Start connects to the primary AMI server and starts health checks
func (m *Manager) Start(ctx context.Context) error {
	// Connect to the primary client
	if err := m.clients[0].Connect(ctx); err != nil {
		m.logger.Error("Failed to connect to primary AMI",
			zap.String("address", m.clients[0].address),
			zap.Error(err))

		// Try to connect to backup clients
		for i := 1; i < len(m.clients); i++ {
			if err := m.clients[i].Connect(ctx); err != nil {
				m.logger.Error("Failed to connect to backup AMI",
					zap.String("address", m.clients[i].address),
					zap.Error(err))
				continue
			}

			// Connected successfully, set as active
			atomic.StoreInt32(&m.activeIdx, int32(i))
			m.logger.Info("Connected to backup AMI",
				zap.String("address", m.clients[i].address))
			break
		}
	}

	// Start health checks for each client
	for i, client := range m.clients {
		i, client := i, client // Capture loop variables
		m.registry.Go(fmt.Sprintf("ami-health-%d", i), func(ctx context.Context) {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := client.Ping(); err != nil {
						m.logger.Warn("AMI health check failed",
							zap.String("address", client.address),
							zap.Error(err))

						// If this is the active client, try to reconnect or failover
						if atomic.LoadInt32(&m.activeIdx) == int32(i) {
							m.tryReconnectOrFailover(ctx, i)
						}
					} else {
						m.logger.Debug("AMI health check succeeded",
							zap.String("address", client.address))
					}
				}
			}
		})
	}

	// Register for events on all clients
	m.registerEventHandlers()

	return nil
}

// tryReconnectOrFailover attempts to reconnect to the current AMI or failover to a backup
func (m *Manager) tryReconnectOrFailover(ctx context.Context, currentIdx int) {
	// Try to reconnect first
	if err := m.clients[currentIdx].Connect(ctx); err != nil {
		m.logger.Error("Failed to reconnect to AMI",
			zap.String("address", m.clients[currentIdx].address),
			zap.Error(err))

		// Try to failover to a backup
		for i := 0; i < len(m.clients); i++ {
			if i == currentIdx {
				continue // Skip the failed client
			}

			if err := m.clients[i].Connect(ctx); err != nil {
				m.logger.Error("Failed to connect to backup AMI",
					zap.String("address", m.clients[i].address),
					zap.Error(err))
				continue
			}

			// Connected successfully, set as active
			atomic.StoreInt32(&m.activeIdx, int32(i))
			m.logger.Info("Switched to backup AMI",
				zap.String("from", m.clients[currentIdx].address),
				zap.String("to", m.clients[i].address))

			// Replay any pending actions
			m.replayPendingActions(ctx)

			break
		}
	}
}

// replayPendingActions replays any pending AMI actions from storage
func (m *Manager) replayPendingActions(ctx context.Context) {
	if m.storage == nil {
		return
	}

	// This is a placeholder - in a real implementation, you would:
	// 1. Load pending actions from storage
	// 2. Retry each action on the new active client
	// 3. Update the action in storage or remove it if successful

	m.logger.Info("Replaying pending AMI actions")
}

// registerEventHandlers registers event handlers on all clients
func (m *Manager) registerEventHandlers() {
	// Register for all events using wildcard
	for _, client := range m.clients {
		client.RegisterEventHandler("*", func(event map[string]string) {
			// Process events as needed
			if eventName, ok := event["Event"]; ok {
				m.logger.Debug("Received AMI event",
					zap.String("event", eventName),
					zap.String("address", client.address))

				// You would handle specific events here, e.g.:
				switch eventName {
				case "Hangup":
					m.handleHangupEvent(event)
				case "NewChannel":
					m.handleNewChannelEvent(event)
				}
			}
		})
	}
}

// handleHangupEvent processes Hangup events
func (m *Manager) handleHangupEvent(event map[string]string) {
	channel := event["Channel"]
	cause := event["Cause"]
	uniqueID := event["Uniqueid"]

	m.logger.Info("Channel hangup",
		zap.String("channel", channel),
		zap.String("cause", cause),
		zap.String("uniqueID", uniqueID))

	// You would update your application state here
}

// handleNewChannelEvent processes NewChannel events
func (m *Manager) handleNewChannelEvent(event map[string]string) {
	channel := event["Channel"]
	state := event["ChannelState"]
	uniqueID := event["Uniqueid"]

	m.logger.Info("New channel created",
		zap.String("channel", channel),
		zap.String("state", state),
		zap.String("uniqueID", uniqueID))

	// You would update your application state here
}

// GetActiveClient returns the currently active AMI client
func (m *Manager) GetActiveClient() *AMIClient {
	idx := atomic.LoadInt32(&m.activeIdx)
	return m.clients[idx]
}

// SendAction sends an action to the active AMI client with failover
func (m *Manager) SendAction(action map[string]string) (map[string]string, error) {
	// Add ActionID if not present
	if _, ok := action["ActionID"]; !ok {
		action["ActionID"] = fmt.Sprintf("AMI-%d", time.Now().UnixNano())
	}

	actionID := action["ActionID"]

	// Store the action for potential replay
	if m.storage != nil {
		ctx := context.Background()
		m.storage.StoreAMIAction(ctx, &storage.AMIAction{
			ActionID:   actionID,
			Command:    "action",
			Params:     action,
			Timestamp:  time.Now(),
			ExpireTime: time.Now().Add(1 * time.Hour),
		})
	}

	// Get the active client
	client := m.GetActiveClient()

	// Send the action
	response, err := client.SendAction(action)
	if err != nil {
		m.logger.Warn("AMI action failed, trying failover",
			zap.String("address", client.address),
			zap.Error(err))

		return m.failoverAction(context.Background(), action)
	}

	// Clean up the stored action
	if m.storage != nil {
		ctx := context.Background()
		m.storage.DeleteAMIAction(ctx, actionID)
	}

	return response, nil
}

// failoverAction attempts to send an action to a backup AMI client
func (m *Manager) failoverAction(ctx context.Context, action map[string]string) (map[string]string, error) {
	currentIdx := atomic.LoadInt32(&m.activeIdx)

	// Try each client in turn
	for i := 0; i < len(m.clients); i++ {
		idx := (int(currentIdx) + i + 1) % len(m.clients)
		if idx == int(currentIdx) {
			continue // Skip the one that just failed
		}

		client := m.clients[idx]

		// Try to connect if needed
		if err := client.Connect(ctx); err != nil {
			m.logger.Warn("AMI failover connection failed",
				zap.String("address", client.address),
				zap.Error(err))
			continue
		}

		// Try this client
		response, err := client.SendAction(action)
		if err != nil {
			m.logger.Warn("AMI failover action failed",
				zap.String("address", client.address),
				zap.Error(err))
			continue
		}

		// Success - update active client
		atomic.StoreInt32(&m.activeIdx, int32(idx))
		m.logger.Info("Switched active AMI client",
			zap.String("from", m.clients[currentIdx].address),
			zap.String("to", client.address))

		// Clean up the stored action
		if m.storage != nil {
			actionID := action["ActionID"]
			m.storage.DeleteAMIAction(ctx, actionID)
		}

		return response, nil
	}

	// All clients failed
	actionID := action["ActionID"]

	// Update retry count if we have storage
	if m.storage != nil {
		amiAction, err := m.storage.GetAMIAction(ctx, actionID)
		if err == nil && amiAction != nil {
			amiAction.Retries++
			m.storage.StoreAMIAction(ctx, amiAction)
		}
	}

	return nil, fmt.Errorf("all AMI clients failed")
}

// Originate initiates a call with failover support
func (m *Manager) Originate(channel, exten, context, priority, application, data, callerId string, timeout int) (map[string]string, error) {
	action := map[string]string{
		"Action": "Originate",
	}

	if channel != "" {
		action["Channel"] = channel
	}

	if exten != "" {
		action["Exten"] = exten
	}

	if context != "" {
		action["Context"] = context
	}

	if priority != "" {
		action["Priority"] = priority
	}

	if application != "" {
		action["Application"] = application
	}

	if data != "" {
		action["Data"] = data
	}

	if callerId != "" {
		action["CallerID"] = callerId
	}

	if timeout > 0 {
		action["Timeout"] = fmt.Sprintf("%d", timeout)
	}

	return m.SendAction(action)
}

// Hangup hangs up a channel with failover support
func (m *Manager) Hangup(channel, cause string) (map[string]string, error) {
	action := map[string]string{
		"Action":  "Hangup",
		"Channel": channel,
	}

	if cause != "" {
		action["Cause"] = cause
	}

	return m.SendAction(action)
}

// Shutdown closes all AMI connections
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, client := range m.clients {
		client.Disconnect()
	}
}
