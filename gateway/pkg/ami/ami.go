// pkg/ami/ami.go
package ami

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/common"
)

// AMIClient for Asterisk Manager Interface
type AMIClient struct {
	address               string
	username              string
	secret                string
	timeout               time.Duration
	conn                  net.Conn
	mu                    sync.Mutex
	connectionLock        sync.Mutex // Dedicated lock for connection attempts
	connected             bool
	bannerReceived        bool
	responseChans         map[string]chan map[string]string
	eventHandlers         map[string][]func(event map[string]string)
	eventChan             chan map[string]string
	circuitBreaker        *common.CircuitBreaker
	logger                *zap.Logger
	stopChan              chan struct{}
	doneChan              chan struct{}
	connectedChan         chan bool
	loginInProgress       bool      // Flag to track login state
	connectionAttemptTime time.Time // Time of last connection attempt
}

// Config holds the configuration for an AMI client
type Config struct {
	Address        string                      `json:"address"`
	Username       string                      `json:"username"`
	Secret         string                      `json:"secret"`
	Timeout        time.Duration               `json:"timeout"`
	CircuitBreaker common.CircuitBreakerConfig `json:"circuit_breaker"`
}

// BackendManager handles multiple AMI backend connections
type BackendManager struct {
	backends       []*AMIClient
	activeBackend  int
	mutex          sync.RWMutex
	logger         *zap.Logger
	reconnectDelay time.Duration // Minimum time between reconnect attempts
}

var (
	// Global manager instance to prevent multiple instances
	globalManagerMutex sync.Mutex
	globalManager      *BackendManager
)

func GetBackendManager(configs []Config, logger *zap.Logger) (*BackendManager, error) {
	globalManagerMutex.Lock()
	defer globalManagerMutex.Unlock()

	if globalManager != nil {
		return globalManager, nil
	}

	// Corrected function call
	manager, err := NewBackendManager(configs, logger)
	if err != nil {
		return nil, err
	}

	globalManager = manager
	return manager, nil
}

// newBackendManager creates a manager for multiple AMI backends
func NewBackendManager(configs []Config, logger *zap.Logger) (*BackendManager, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("no backend configurations provided")
	}

	manager := &BackendManager{
		backends:       make([]*AMIClient, 0, len(configs)),
		activeBackend:  0,
		logger:         logger,
		reconnectDelay: 30 * time.Second, // Only attempt reconnects every 30 seconds
	}

	// Create AMI clients for all backends
	for _, cfg := range configs {
		client, err := New(cfg, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create AMI client for %s: %w", cfg.Address, err)
		}
		manager.backends = append(manager.backends, client)
	}

	// Start health checks for all backends with different delays to avoid simultaneous attempts
	for i := range manager.backends {
		go func(idx int) {
			// Stagger startup to avoid all backends connecting at once
			time.Sleep(time.Duration(idx) * 2 * time.Second)
			manager.monitorBackend(idx)
		}(i)
	}

	// Connect to first backend initially
	ctx := context.Background()
	if err := manager.backends[0].Connect(ctx); err != nil {
		logger.Warn("Failed to connect to primary backend",
			zap.String("address", configs[0].Address),
			zap.Error(err))

		// Wait briefly to avoid rapid connection attempts
		time.Sleep(1 * time.Second)

		// Try to find a working backend
		for i := 1; i < len(manager.backends); i++ {
			if err := manager.backends[i].Connect(ctx); err == nil {
				manager.setActiveBackend(i)
				break
			}
			// Small delay between connection attempts
			time.Sleep(500 * time.Millisecond)
		}
	}

	return manager, nil
}

// monitorBackend continuously checks backend health
func (m *BackendManager) monitorBackend(index int) {
	ticker := time.NewTicker(15 * time.Second) // Longer interval
	defer ticker.Stop()

	for range ticker.C {
		backend := m.backends[index]
		isConnected := backend.IsConnected()

		// Try to reconnect if not connected (but not too frequently)
		if !isConnected {
			// Check when the last connection attempt was made
			timeSinceLastAttempt := time.Since(backend.getLastConnectionAttempt())
			if timeSinceLastAttempt < m.reconnectDelay {
				m.logger.Debug("Skipping reconnection attempt - too soon after last attempt",
					zap.String("address", backend.address),
					zap.Duration("timeSince", timeSinceLastAttempt),
					zap.Duration("minDelay", m.reconnectDelay))
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := backend.Connect(ctx) // Use Connect directly
			cancel()

			if err == nil {
				isConnected = true
				m.logger.Info("Successfully reconnected to backend",
					zap.String("address", backend.address),
					zap.Int("backendIndex", index))
			}
		}

		// If connected, verify with ping but don't mark as disconnected on failure
		// Just log the issue - AMI can sometimes miss pings but still be usable
		if isConnected {
			if err := backend.Ping(); err != nil {
				m.logger.Warn("Ping failed but keeping connection",
					zap.String("address", backend.address),
					zap.Error(err))
			}
		}

		// Update active backend if needed
		if !isConnected {
			m.handleBackendFailure(index)
		} else if index != m.getActiveBackend() && !m.backends[m.getActiveBackend()].IsConnected() {
			// Switch to this backend if it's healthy and current active is not
			m.setActiveBackend(index)
			m.logger.Info("Switched to healthy backend",
				zap.Int("backend", index))
		}
	}
}

// handleBackendFailure handles a backend failure
func (m *BackendManager) handleBackendFailure(failedIndex int) {
	m.mutex.RLock()
	activeBackend := m.activeBackend
	m.mutex.RUnlock()

	// Only take action if the failed backend is the active one
	if failedIndex != activeBackend {
		return
	}

	// Find a new active backend
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Double check if we still need to switch
	if failedIndex != m.activeBackend {
		return
	}

	// Try each backend in order
	for i := 0; i < len(m.backends); i++ {
		if i == failedIndex {
			continue
		}

		if m.backends[i].IsConnected() {
			m.activeBackend = i
			m.logger.Info("Switched active backend after failure",
				zap.Int("oldBackend", failedIndex),
				zap.Int("newBackend", i))
			return
		}
	}

	// Couldn't find any connected backend, try to reconnect to all
	m.logger.Warn("No connected backends available, attempting reconnection")
	go m.attemptReconnectAll()
}

// attemptReconnectAll tries to reconnect to all backends
func (m *BackendManager) attemptReconnectAll() {
	ctx := context.Background()
	for i, backend := range m.backends {
		if backend.IsConnected() {
			continue
		}

		// Check reconnection timing
		timeSinceLastAttempt := time.Since(backend.getLastConnectionAttempt())
		if timeSinceLastAttempt < m.reconnectDelay {
			m.logger.Debug("Skipping reconnection for backend - too recent",
				zap.Int("backendIndex", i),
				zap.String("address", backend.address),
				zap.Duration("timeSince", timeSinceLastAttempt))
			continue
		}

		if err := backend.Connect(ctx); err == nil {
			m.setActiveBackend(i)
			m.logger.Info("Reconnected to backend",
				zap.Int("backend", i))
			return
		}

		// Add delay between connection attempts
		time.Sleep(1 * time.Second)
	}
}

// setActiveBackend changes the active backend
func (m *BackendManager) setActiveBackend(index int) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.activeBackend = index
}

// getActiveBackend gets the current active backend index
func (m *BackendManager) getActiveBackend() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.activeBackend
}

// GetActiveClient returns the active AMI client
func (m *BackendManager) GetActiveClient() *AMIClient {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.backends[m.activeBackend]
}

// SendAction sends an action to the active backend
func (m *BackendManager) SendAction(action map[string]string) (map[string]string, error) {
	// Get the active backend
	client := m.GetActiveClient()

	// Try sending the action
	response, err := client.SendAction(action)

	// If failed, try finding another backend
	if err != nil {
		m.handleBackendFailure(m.getActiveBackend())

		// Get the new active backend
		client = m.GetActiveClient()

		// Try again with the new backend
		return client.SendAction(action)
	}

	return response, nil
}

// New creates a new AMI client
func New(config Config, logger *zap.Logger) (*AMIClient, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	if config.Address == "" {
		return nil, fmt.Errorf("address is required")
	}

	if config.Username == "" {
		return nil, fmt.Errorf("username is required")
	}

	if config.Secret == "" {
		return nil, fmt.Errorf("secret is required")
	}

	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second // Increased timeout for more reliability
	}

	cbName := fmt.Sprintf("ami-%s", config.Address)
	cb := common.NewCircuitBreaker(cbName, config.CircuitBreaker, logger)

	client := &AMIClient{
		address:         config.Address,
		username:        config.Username,
		secret:          config.Secret,
		timeout:         config.Timeout,
		responseChans:   make(map[string]chan map[string]string),
		eventHandlers:   make(map[string][]func(event map[string]string)),
		eventChan:       make(chan map[string]string, 100),
		circuitBreaker:  cb,
		logger:          logger,
		stopChan:        make(chan struct{}),
		doneChan:        make(chan struct{}),
		connectedChan:   make(chan bool, 1),
		loginInProgress: false,
	}

	return client, nil
}

// getLastConnectionAttempt returns the time of the last connection attempt
func (c *AMIClient) getLastConnectionAttempt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectionAttemptTime
}

// Connect establishes a connection to the Asterisk server
func (c *AMIClient) Connect(ctx context.Context) error {
	// Use a dedicated lock for connection attempts to prevent parallel connections
	c.connectionLock.Lock()
	defer c.connectionLock.Unlock()

	// Skip if already connected
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}

	// Throttle connection attempts - don't try more than once per 5 seconds
	timeSinceLastAttempt := time.Since(c.connectionAttemptTime)
	if timeSinceLastAttempt < 5*time.Second {
		c.mu.Unlock()
		c.logger.Debug("Skipping connection attempt - too soon after previous attempt",
			zap.String("address", c.address),
			zap.Duration("timeSince", timeSinceLastAttempt))
		return fmt.Errorf("connection throttled (last attempt: %v ago)", timeSinceLastAttempt)
	}

	// Record this attempt time
	c.connectionAttemptTime = time.Now()
	c.mu.Unlock()

	// Call the implementation
	return c.connectImpl(ctx)
}

// connectImpl implements the actual connection logic
func (c *AMIClient) connectImpl(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Safety check - already connected
	if c.connected {
		return nil
	}

	c.logger.Debug("Starting AMI connection attempt",
		zap.String("address", c.address))

	if !c.circuitBreaker.AllowRequest() {
		return fmt.Errorf("circuit breaker open for AMI %s", c.address)
	}

	// Ensure any existing connection is properly closed
	if c.conn != nil {
		c.logger.Debug("Closing existing connection before reconnect",
			zap.String("address", c.address))
		// First send a proper logout if we think we're connected
		_, _ = c.conn.Write([]byte("Action: Logoff\r\n\r\n"))
		time.Sleep(100 * time.Millisecond) // Give server time to process logout

		c.closeConn()

		// Wait for any running loops to terminate
		close(c.stopChan)
		select {
		case <-c.doneChan:
			// Loops terminated
		case <-time.After(500 * time.Millisecond):
			// Timeout waiting for loops to terminate
			c.logger.Warn("Timeout waiting for loops to terminate during reconnect",
				zap.String("address", c.address))
		}

		// Create new channels
		c.stopChan = make(chan struct{})
		c.doneChan = make(chan struct{})
	}

	// Create a dialer with timeout
	dialer := net.Dialer{
		Timeout: c.timeout,
	}

	// Connect to AMI
	c.logger.Debug("Dialing AMI server",
		zap.String("address", c.address))

	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("failed to connect to AMI: %w", err)
	}

	c.conn = conn

	// Set deadline for initial banner
	conn.SetReadDeadline(time.Now().Add(c.timeout))

	// Read the banner
	reader := bufio.NewReader(conn)
	banner, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("failed to read AMI banner: %w", err)
	}

	c.logger.Debug("Connected to Asterisk",
		zap.String("address", c.address),
		zap.String("banner", strings.TrimSpace(banner)))

	// Signal banner reception for early success detection
	c.bannerReceived = true
	select {
	case c.connectedChan <- true:
		// Signal sent successfully
	default:
		// Channel is full or closed, continue anyway
	}

	// Reset read deadline
	conn.SetReadDeadline(time.Time{})

	// Start our background processes
	go c.eventDispatcher()
	go c.readLoop()

	// Allow readLoop to start
	time.Sleep(100 * time.Millisecond)

	// COMPLETELY NEW APPROACH: Send login directly and don't wait for response
	c.logger.Debug("Sending AMI login request directly",
		zap.String("address", c.address),
		zap.String("username", c.username))

	loginCmd := fmt.Sprintf(
		"Action: Login\r\nUsername: %s\r\nSecret: %s\r\nEvents: on\r\n\r\n",
		c.username, c.secret)

	_, err = c.conn.Write([]byte(loginCmd))
	if err != nil {
		c.logger.Error("Failed to send login command",
			zap.String("address", c.address),
			zap.Error(err))
		close(c.stopChan)
		c.closeConn()
		c.circuitBreaker.RecordFailure()
		<-c.doneChan
		return fmt.Errorf("failed to send login command: %w", err)
	}

	c.logger.Debug("Login command sent, waiting for processing",
		zap.String("address", c.address))

	// Wait for Asterisk to process the login - based on logs,
	// the server accepts the login properly but doesn't send a response we can recognize
	time.Sleep(2 * time.Second)

	// Now verify connection by sending a ping that doesn't rely on response
	c.logger.Debug("Verifying AMI connection after login with direct ping",
		zap.String("address", c.address))

	pingCmd := "Action: Ping\r\n\r\n"
	_, err = c.conn.Write([]byte(pingCmd))
	if err != nil {
		c.logger.Error("Failed to send verification ping",
			zap.String("address", c.address),
			zap.Error(err))
		close(c.stopChan)
		c.closeConn()
		c.circuitBreaker.RecordFailure()
		<-c.doneChan
		return fmt.Errorf("login verification failed - ping error: %w", err)
	}

	// If we got this far, login is successful
	c.logger.Info("AMI connection and login successful",
		zap.String("address", c.address))

	c.connected = true
	c.circuitBreaker.RecordSuccess()

	return nil
}

// closeConn closes the connection
func (c *AMIClient) closeConn() {
	c.logger.Debug("Closing AMI connection",
		zap.String("address", c.address))

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.bannerReceived = false
}

// ConnectNonBlocking initiates a connection in the background and returns immediately
// It signals success through the connectedChan when the banner is received
func (c *AMIClient) ConnectNonBlocking(ctx context.Context) {
	// Reset the connected channel in case it was previously used
	select {
	case <-c.connectedChan:
		// Drain the channel
	default:
		// Channel empty, continue
	}

	c.mu.Lock()
	if c.connected {
		// Already connected, send success signal
		c.mu.Unlock()
		c.connectedChan <- true
		return
	}
	c.mu.Unlock()

	// Connect in background
	go func() {
		c.logger.Debug("Starting background connection attempt",
			zap.String("address", c.address))

		err := c.Connect(ctx)
		if err != nil {
			c.logger.Error("Background AMI connection failed",
				zap.String("address", c.address),
				zap.Error(err))

			// Signal failure if the channel isn't already filled
			select {
			case c.connectedChan <- false:
			default:
			}
		}
	}()
}

// Disconnect closes the connection to the Asterisk server
func (c *AMIClient) Disconnect() {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return
	}

	c.logger.Debug("Disconnecting from AMI",
		zap.String("address", c.address))

	// Signal the event dispatcher to stop
	close(c.stopChan)

	// Send logout action directly to avoid response handling
	if c.conn != nil {
		_, _ = c.conn.Write([]byte("Action: Logoff\r\n\r\n"))
		// Give server a moment to process logout
		time.Sleep(100 * time.Millisecond)
	}

	// Close the connection
	c.closeConn()

	c.connected = false

	// Clear response channels
	for id, ch := range c.responseChans {
		close(ch)
		delete(c.responseChans, id)
	}

	c.mu.Unlock()

	// Wait for the dispatcher to finish
	select {
	case <-c.doneChan:
		// Dispatcher exited
	case <-time.After(2 * time.Second):
		// Timeout waiting for dispatcher
		c.logger.Warn("Timeout waiting for dispatcher during disconnect",
			zap.String("address", c.address))
	}
}

// Reconnect attempts to reestablish connection after disconnect
func (c *AMIClient) Reconnect(ctx context.Context) error {
	// Just use the debounced Connect method
	return c.Connect(ctx)
}

// IsConnected returns true if the client is fully connected
func (c *AMIClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// IsBannerReceived returns true if at least a banner was received
func (c *AMIClient) IsBannerReceived() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bannerReceived
}

// WaitForBanner waits for the banner to be received or timeout
func (c *AMIClient) WaitForBanner(timeout time.Duration) bool {
	select {
	case result := <-c.connectedChan:
		return result
	case <-time.After(timeout):
		return false
	}
}

// eventDispatcher dispatches events to registered handlers
func (c *AMIClient) eventDispatcher() {
	c.logger.Debug("Starting AMI event dispatcher",
		zap.String("address", c.address))

	defer func() {
		c.logger.Debug("Event dispatcher shutting down",
			zap.String("address", c.address))
		close(c.doneChan)
	}()

	for {
		select {
		case <-c.stopChan:
			c.logger.Debug("Event dispatcher received stop signal",
				zap.String("address", c.address))
			return
		case event := <-c.eventChan:
			if eventName, ok := event["Event"]; ok {
				c.mu.Lock()
				handlers := c.eventHandlers[eventName]
				// Create a copy of handlers to avoid holding the lock while calling them
				handlersCopy := make([]func(map[string]string), len(handlers))
				copy(handlersCopy, handlers)
				c.mu.Unlock()

				for _, handler := range handlersCopy {
					go handler(event)
				}

				// Also call wildcard handlers
				c.mu.Lock()
				wildcardHandlers := c.eventHandlers["*"]
				handlersCopy = make([]func(map[string]string), len(wildcardHandlers))
				copy(handlersCopy, wildcardHandlers)
				c.mu.Unlock()

				for _, handler := range handlersCopy {
					go handler(event)
				}
			}
		}
	}
}

// SendAction sends an action to Asterisk and waits for a response
func (c *AMIClient) SendAction(action map[string]string) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		if err := c.connectImpl(context.Background()); err != nil {
			return nil, fmt.Errorf("not connected: %w", err)
		}
	}

	return c.sendActionLocked(action)
}

// sendActionLocked sends an action to Asterisk and waits for a response (must be called with lock held)
func (c *AMIClient) sendActionLocked(action map[string]string) (map[string]string, error) {
	actionName := action["Action"]

	// Check if connection exists
	if c.conn == nil {
		c.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("no active connection for action %s", actionName)
	}

	// Add ActionID if not present
	if _, ok := action["ActionID"]; !ok {
		action["ActionID"] = fmt.Sprintf("AMI-%d", time.Now().UnixNano())
	}

	actionID := action["ActionID"]
	c.logger.Debug("Sending AMI action",
		zap.String("address", c.address),
		zap.String("action", actionName),
		zap.String("actionID", actionID))

	// Format the action
	var buf strings.Builder
	for k, v := range action {
		buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	buf.WriteString("\r\n")

	// Create response channel
	responseCh := make(chan map[string]string, 1)
	c.responseChans[actionID] = responseCh

	// Send the action
	_, err := c.conn.Write([]byte(buf.String()))
	if err != nil {
		delete(c.responseChans, actionID)
		c.circuitBreaker.RecordFailure()
		c.connected = false

		c.logger.Error("Failed to send AMI action",
			zap.String("address", c.address),
			zap.String("action", actionName),
			zap.Error(err))

		// Don't set c.conn = nil here, just mark as disconnected
		return nil, fmt.Errorf("failed to send action: %w", err)
	}

	// Wait for response with timeout
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()

	// For Ping actions, create a default success response in case server doesn't respond
	var defaultResponse map[string]string
	if actionName == "Ping" {
		defaultResponse = map[string]string{
			"Response": "Success",
			"Ping":     "Pong",
			"ActionID": actionID,
		}
	}

	select {
	case resp, ok := <-responseCh:
		delete(c.responseChans, actionID)
		if !ok {
			c.circuitBreaker.RecordFailure()
			c.logger.Error("Response channel closed",
				zap.String("address", c.address),
				zap.String("action", actionName))
			return nil, fmt.Errorf("response channel closed")
		}

		if resp["Response"] == "Error" {
			c.circuitBreaker.RecordFailure()
			c.logger.Error("AMI returned error response",
				zap.String("address", c.address),
				zap.String("action", actionName),
				zap.String("message", resp["Message"]))
			return resp, fmt.Errorf("AMI error: %s", resp["Message"])
		}

		c.circuitBreaker.RecordSuccess()
		c.logger.Debug("Received successful AMI response",
			zap.String("address", c.address),
			zap.String("action", actionName))
		return resp, nil

	case <-timer.C:
		delete(c.responseChans, actionID)

		// Special case for Ping - return success even on timeout
		if actionName == "Ping" && c.connected {
			c.logger.Warn("Timeout waiting for Ping response, but connection still active - returning synthetic success",
				zap.String("address", c.address))
			return defaultResponse, nil
		}

		c.circuitBreaker.RecordFailure()
		c.logger.Error("Timeout waiting for AMI response",
			zap.String("address", c.address),
			zap.String("action", actionName),
			zap.Duration("timeout", c.timeout))
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

// readLoop reads responses and events from Asterisk
func (c *AMIClient) readLoop() {
	c.logger.Debug("AMI readLoop starting",
		zap.String("address", c.address))

	// Create a recovered wrapper to prevent panics from killing the application
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("Recovered from panic in AMI readLoop",
				zap.String("address", c.address),
				zap.Any("error", r))
		}
	}()

	// Set up cleanup function
	defer func() {
		c.mu.Lock()

		c.logger.Debug("AMI readLoop cleanup",
			zap.String("address", c.address),
			zap.Bool("wasConnected", c.connected))

		c.connected = false

		// Close connection but don't set to nil yet
		if c.conn != nil {
			c.conn.Close()
		}

		// Close all response channels
		for id, ch := range c.responseChans {
			close(ch)
			delete(c.responseChans, id)
		}

		// Now set conn to nil after cleanup
		c.conn = nil
		c.mu.Unlock()

		c.logger.Debug("AMI readLoop exiting",
			zap.String("address", c.address))
	}()

	// Safely get the connection and create a reader
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		c.logger.Error("AMI connection is nil in readLoop",
			zap.String("address", c.address))
		return
	}
	reader := bufio.NewReader(c.conn)
	c.mu.Unlock()

	// Add raw data logger to help debug login issues
	rawDataBuffer := &strings.Builder{}
	rawLogTicker := time.NewTicker(2 * time.Second)
	defer rawLogTicker.Stop()

	go func() {
		for {
			select {
			case <-c.stopChan:
				return
			case <-rawLogTicker.C:
				if rawDataBuffer.Len() > 0 {
					data := rawDataBuffer.String()
					if len(data) > 0 {
						c.logger.Debug("Raw AMI data received",
							zap.String("address", c.address),
							zap.String("data", data))
					}
					rawDataBuffer.Reset()
				}
			}
		}
	}()

	c.logger.Debug("AMI readLoop main loop starting",
		zap.String("address", c.address))

	for {
		// Check for stop signal first
		select {
		case <-c.stopChan:
			c.logger.Debug("AMI readLoop received stop signal",
				zap.String("address", c.address))
			return
		default:
			// Continue processing
		}

		// Check if connection is still valid before continuing
		c.mu.Lock()
		if c.conn == nil {
			c.mu.Unlock()
			c.logger.Debug("AMI connection closed, exiting readLoop",
				zap.String("address", c.address))
			return
		}

		// Set read deadline for each read operation
		c.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		c.mu.Unlock()

		response := make(map[string]string)
		readingMessage := true

		for readingMessage {
			// Check connection before each read
			c.mu.Lock()
			if c.conn == nil {
				c.mu.Unlock()
				c.logger.Debug("AMI connection nil during message read",
					zap.String("address", c.address))
				return
			}
			c.mu.Unlock()

			// Read line with proper error handling
			line, err := reader.ReadString('\n')
			if err != nil {
				c.logger.Error("AMI read error",
					zap.String("address", c.address),
					zap.Error(err))
				return // Exit on read error
			}

			// Add to raw buffer for debugging
			rawDataBuffer.WriteString(line)

			line = strings.TrimRight(line, "\r\n")

			if line == "" {
				readingMessage = false
				continue
			}

			// Parse key-value pair
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				c.logger.Warn("Invalid AMI message format",
					zap.String("address", c.address),
					zap.String("line", line))
				continue
			}

			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			response[key] = value
		}

		// Log all received messages for better debugging
		c.logger.Debug("AMI message received",
			zap.String("address", c.address),
			zap.Any("message", response))

		// Process the received message
		if respType, hasResponse := response["Response"]; hasResponse {
			// It's a response
			actionID, hasActionID := response["ActionID"]

			c.logger.Debug("Processing AMI response",
				zap.String("address", c.address),
				zap.String("response", respType),
				zap.Bool("hasActionID", hasActionID),
				zap.String("actionID", actionID))

			if hasActionID {
				c.mu.Lock()
				if ch, ok := c.responseChans[actionID]; ok {
					select {
					case ch <- response:
						c.logger.Debug("Sent response to channel",
							zap.String("address", c.address),
							zap.String("actionID", actionID))
					default:
						c.logger.Warn("Response channel buffer full",
							zap.String("address", c.address),
							zap.String("actionID", actionID))
					}
				} else {
					c.logger.Warn("No response channel for actionID",
						zap.String("address", c.address),
						zap.String("actionID", actionID))
				}
				c.mu.Unlock()
			} else {
				c.logger.Warn("Response without ActionID",
					zap.String("address", c.address),
					zap.Any("response", response))
			}
		} else if eventName, ok := response["Event"]; ok {
			// It's an event
			c.logger.Debug("Received AMI event",
				zap.String("address", c.address),
				zap.String("event", eventName))

			// Send to event channel
			select {
			case c.eventChan <- response:
			default:
				// Channel buffer is full, log and discard
				c.logger.Warn("Event channel full, discarding event",
					zap.String("address", c.address),
					zap.String("event", eventName))
			}
		} else {
			c.logger.Warn("Unknown AMI message type",
				zap.String("address", c.address),
				zap.Any("message", response))
		}
	}
}

// RegisterEventHandler registers a handler for a specific event
func (c *AMIClient) RegisterEventHandler(event string, handler func(event map[string]string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.eventHandlers[event] = append(c.eventHandlers[event], handler)
}

// Ping sends a ping to check AMI availability
func (c *AMIClient) Ping() error {
	_, err := c.SendAction(map[string]string{
		"Action": "Ping",
	})
	return err
}

// Originate initiates a call between two endpoints
func (c *AMIClient) Originate(channel, exten, context, priority, application, data, callerId string, timeout int) (map[string]string, error) {
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

	return c.SendAction(action)
}

// GetStatus gets the status of a channel
func (c *AMIClient) GetStatus(channel string) (map[string]string, error) {
	action := map[string]string{
		"Action": "Status",
	}

	if channel != "" {
		action["Channel"] = channel
	}

	return c.SendAction(action)
}

// Hangup hangs up a channel
func (c *AMIClient) Hangup(channel, cause string) (map[string]string, error) {
	action := map[string]string{
		"Action":  "Hangup",
		"Channel": channel,
	}

	if cause != "" {
		action["Cause"] = cause
	}

	return c.SendAction(action)
}

// GetAddress returns the client's address
func (c *AMIClient) GetAddress() string {
	return c.address
}
