package ami

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/common"
	"gateway/pkg/coordinator"
	"gateway/pkg/storage"
)

// Manager handles multiple AMI clients with failover
type Manager struct {
	clients      []*AMIClient
	activeIdx    int32
	mu           sync.RWMutex
	logger       *zap.Logger
	registry     *common.GoroutineRegistry
	storage      storage.StateStorage
	coordinator  *coordinator.Coordinator
	maxRetries   int
	retryDelay   time.Duration
	initializing int32 // atomic flag to track initialization state
}

// ManagerConfig holds the configuration for the AMI manager
type ManagerConfig struct {
	Clients    []Config `json:"clients"`
	MaxRetries int      `json:"max_retries"`
	RetryDelay string   `json:"retry_delay"`
}

// NewManager creates a new AMI manager
func NewManager(config ManagerConfig, logger *zap.Logger, registry *common.GoroutineRegistry,
	storage storage.StateStorage, coord *coordinator.Coordinator) (*Manager, error) {
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

	// Set default values for retry parameters
	maxRetries := 5
	if config.MaxRetries > 0 {
		maxRetries = config.MaxRetries
	}

	retryDelay := 5 * time.Second
	if config.RetryDelay != "" {
		if parsedDelay, err := time.ParseDuration(config.RetryDelay); err == nil && parsedDelay > 0 {
			retryDelay = parsedDelay
		}
	}

	return &Manager{
		clients:     clients,
		logger:      logger,
		registry:    registry,
		storage:     storage,
		coordinator: coord,
		maxRetries:  maxRetries,
		retryDelay:  retryDelay,
	}, nil
}

// Start connects to the primary AMI server and starts health checks
// This method is non-blocking and continues application startup even if AMI connection fails
func (m *Manager) Start(ctx context.Context) error {
	// Set initializing flag
	atomic.StoreInt32(&m.initializing, 1)
	defer atomic.StoreInt32(&m.initializing, 0)

	// Start connection in background for primary client
	m.logger.Info("Starting non-blocking AMI connection",
		zap.String("address", m.clients[0].address))

	// Use non-blocking connection
	m.clients[0].ConnectNonBlocking(ctx)

	// Wait briefly for banner
	bannerReceived := m.clients[0].WaitForBanner(3 * time.Second)

	if bannerReceived {
		m.logger.Info("AMI banner received from primary server, continuing startup",
			zap.String("address", m.clients[0].address))
	} else {
		m.logger.Warn("AMI initial connection not completed quickly, continuing startup",
			zap.String("address", m.clients[0].address))

		// Try backup clients for quick connection
		for i := 1; i < len(m.clients); i++ {
			m.clients[i].ConnectNonBlocking(ctx)
			if m.clients[i].WaitForBanner(1 * time.Second) {
				// Got a banner from backup, switch to it
				atomic.StoreInt32(&m.activeIdx, int32(i))
				m.logger.Info("Connected to backup AMI (banner received)",
					zap.String("address", m.clients[i].address))
				break
			}
		}

		// Start retry loop in background for all clients that aren't connected
		go m.retryAllConnections(ctx)
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
					if !client.IsConnected() {
						// Skip health check if not connected
						continue
					}

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

	// Register event handlers
	m.registerEventHandlers()

	// Start connection monitor to handle reconnections
	m.registry.Go("ami-connection-monitor", func(ctx context.Context) {
		m.monitorConnections(ctx)
	})

	return nil
}

// retryAllConnections attempts to connect to all AMI servers that aren't connected
func (m *Manager) retryAllConnections(ctx context.Context) {
	baseDelay := m.retryDelay

	for i, client := range m.clients {
		i, client := i, client // Capture loop variables

		// Skip clients that are already connected
		if client.IsConnected() {
			continue
		}

		go func() {
			retryCount := 0

			for retryCount < m.maxRetries {
				// Skip if already connected
				if client.IsConnected() {
					return
				}

				// Calculate backoff delay
				delay := time.Duration(float64(baseDelay) * math.Pow(1.5, float64(retryCount)))
				if delay > 2*time.Minute {
					delay = 2 * time.Minute // Cap at 2 minutes
				}

				m.logger.Info("Retrying AMI connection",
					zap.String("address", client.address),
					zap.Int("attempt", retryCount+1),
					zap.Duration("delay", delay))

				// Wait before retry
				select {
				case <-ctx.Done():
					return // Context cancelled
				case <-time.After(delay):
					// Continue with retry
				}

				// Try to connect
				err := client.Reconnect(ctx)
				if err != nil {
					retryCount++
					m.logger.Error("AMI reconnection failed",
						zap.String("address", client.address),
						zap.Error(err),
						zap.Int("retryCount", retryCount))
				} else {
					m.logger.Info("AMI reconnection successful",
						zap.String("address", client.address),
						zap.Int("attempts", retryCount+1))

					// If this is the first client and no other active client,
					// make this the active client
					if i == 0 && atomic.LoadInt32(&m.activeIdx) == 0 {
						// This is the primary and we reconnected it
						return
					}

					// Check if we should make this the active client
					activeIdx := atomic.LoadInt32(&m.activeIdx)
					if !m.clients[activeIdx].IsConnected() {
						// Current active client is not connected, switch to this one
						atomic.StoreInt32(&m.activeIdx, int32(i))
						m.logger.Info("Switched to newly connected AMI client",
							zap.String("address", client.address))
					}
					return
				}
			}

			m.logger.Warn("Maximum AMI reconnection attempts reached",
				zap.String("address", client.address),
				zap.Int("maxRetries", m.maxRetries))
		}()
	}
}

// monitorConnections monitors the connection status of all clients
func (m *Manager) monitorConnections(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			activeIdx := atomic.LoadInt32(&m.activeIdx)

			// Check if active client is connected
			if !m.clients[activeIdx].IsConnected() {
				m.logger.Warn("Active AMI client disconnected, looking for alternative",
					zap.String("address", m.clients[activeIdx].address))

				// Find a connected client to switch to
				foundConnected := false
				for i, client := range m.clients {
					if i != int(activeIdx) && client.IsConnected() {
						atomic.StoreInt32(&m.activeIdx, int32(i))
						m.logger.Info("Switched to connected AMI client",
							zap.String("from", m.clients[activeIdx].address),
							zap.String("to", client.address))
						foundConnected = true
						break
					}
				}

				if !foundConnected {
					// No connected clients, try to reconnect them all
					for i, client := range m.clients {
						i, client := i, client // Capture loop variables
						go func() {
							connCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
							defer cancel()
							if err := client.Reconnect(connCtx); err != nil {
								m.logger.Debug("Reconnection attempt failed",
									zap.String("address", client.address),
									zap.Error(err))
							} else {
								m.logger.Info("Reconnected AMI client",
									zap.String("address", client.address))
								if !foundConnected {
									// First successful reconnect, make it active
									atomic.StoreInt32(&m.activeIdx, int32(i))
									foundConnected = true
								}
							}
						}()
					}
				}
			}
		}
	}
}

// tryReconnectOrFailover attempts to reconnect to the current AMI or failover to a backup
func (m *Manager) tryReconnectOrFailover(ctx context.Context, currentIdx int) {
	// Try to reconnect first
	ctx, cancel := common.ContextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := m.clients[currentIdx].Reconnect(ctx); err != nil {
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

	m.logger.Info("Replaying pending AMI actions")

	// Create a context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Get all pending AMI actions
	actionIDs, err := m.storage.ListAMIActionIDs(timeoutCtx)
	if err != nil {
		m.logger.Error("Failed to retrieve pending AMI actions", zap.Error(err))
		return
	}

	if len(actionIDs) == 0 {
		m.logger.Info("No pending AMI actions to replay")
		return
	}

	m.logger.Info("Found pending AMI actions to replay", zap.Int("count", len(actionIDs)))

	// Get active client
	client := m.GetActiveClient()

	// Replay each action
	for _, actionID := range actionIDs {
		action, err := m.storage.GetAMIAction(timeoutCtx, actionID)
		if err != nil {
			m.logger.Error("Failed to retrieve AMI action",
				zap.String("actionID", actionID),
				zap.Error(err))
			continue
		}

		// Skip expired actions
		if !action.ExpireTime.IsZero() && action.ExpireTime.Before(time.Now()) {
			m.logger.Debug("Skipping expired AMI action",
				zap.String("actionID", actionID),
				zap.Time("expireTime", action.ExpireTime))
			m.storage.DeleteAMIAction(timeoutCtx, actionID)
			continue
		}

		// Skip actions with too many retries
		if action.Retries > 3 {
			m.logger.Warn("AMI action exceeded retry limit",
				zap.String("actionID", actionID),
				zap.Int("retries", action.Retries))
			m.storage.DeleteAMIAction(timeoutCtx, actionID)
			continue
		}

		// Increment retry counter
		action.Retries++
		m.storage.StoreAMIAction(timeoutCtx, action)

		// Convert parameters from string map to message format
		actionParams := make(map[string]string)
		for k, v := range action.Params {
			actionParams[k] = v
		}

		m.logger.Info("Replaying AMI action",
			zap.String("actionID", actionID),
			zap.String("command", action.Command),
			zap.Int("retry", action.Retries))

		// Send the action
		response, err := client.SendAction(actionParams)
		if err != nil {
			m.logger.Error("Failed to replay AMI action",
				zap.String("actionID", actionID),
				zap.Error(err))
			continue
		}

		// Check if successful
		if response["Response"] == "Success" {
			m.logger.Info("Successfully replayed AMI action",
				zap.String("actionID", actionID))
			m.storage.DeleteAMIAction(timeoutCtx, actionID)
		} else {
			m.logger.Error("AMI action replay returned error",
				zap.String("actionID", actionID),
				zap.String("message", response["Message"]))
		}
	}
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

// IsConnected returns whether the active AMI client is connected
func (m *Manager) IsConnected() bool {
	// During initialization phase, return true to allow startup to continue
	if atomic.LoadInt32(&m.initializing) == 1 {
		return true
	}

	client := m.GetActiveClient()
	return client != nil && client.IsConnected()
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

	// Check if client is connected
	if !client.IsConnected() {
		// Try to connect first
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := client.Connect(ctx); err != nil {
			m.logger.Warn("Active AMI client not connected and reconnection failed",
				zap.String("address", client.address),
				zap.Error(err))

			// Try failover
			return m.failoverAction(context.Background(), action)
		}
	}

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
	// Ensure this operation has a timeout
	ctx, cancel := common.ContextWithTimeout(ctx, 10*time.Second)
	defer cancel()
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

// Reconnect attempts to reconnect all clients
func (m *Manager) Reconnect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First try to reconnect active client
	activeIdx := atomic.LoadInt32(&m.activeIdx)
	if err := m.clients[activeIdx].Reconnect(ctx); err == nil {
		m.logger.Info("Reconnected active AMI client",
			zap.String("address", m.clients[activeIdx].address))
		return nil
	}

	// Try all other clients
	for i, client := range m.clients {
		if i == int(activeIdx) {
			continue // Skip active client, we already tried it
		}

		if err := client.Reconnect(ctx); err == nil {
			// Successfully reconnected this client, make it active
			atomic.StoreInt32(&m.activeIdx, int32(i))
			m.logger.Info("Reconnected and switched to backup AMI client",
				zap.String("address", client.address))
			return nil
		}
	}

	return errors.New("failed to reconnect any AMI client")
}

// Shutdown closes all AMI connections
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, client := range m.clients {
		client.Disconnect()
	}
}
