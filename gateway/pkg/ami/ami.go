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
	address        string
	username       string
	secret         string
	timeout        time.Duration
	conn           net.Conn
	mu             sync.Mutex
	connected      bool
	bannerReceived bool
	responseChans  map[string]chan map[string]string
	eventHandlers  map[string][]func(event map[string]string)
	eventChan      chan map[string]string
	circuitBreaker *common.CircuitBreaker
	logger         *zap.Logger
	stopChan       chan struct{}
	doneChan       chan struct{}
	connectedChan  chan bool
}

// Config holds the configuration for an AMI client
type Config struct {
	Address        string                      `json:"address"`
	Username       string                      `json:"username"`
	Secret         string                      `json:"secret"`
	Timeout        time.Duration               `json:"timeout"`
	CircuitBreaker common.CircuitBreakerConfig `json:"circuit_breaker"`
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
		config.Timeout = 5 * time.Second
	}

	cbName := fmt.Sprintf("ami-%s", config.Address)
	cb := common.NewCircuitBreaker(cbName, config.CircuitBreaker, logger)

	client := &AMIClient{
		address:        config.Address,
		username:       config.Username,
		secret:         config.Secret,
		timeout:        config.Timeout,
		responseChans:  make(map[string]chan map[string]string),
		eventHandlers:  make(map[string][]func(event map[string]string)),
		eventChan:      make(chan map[string]string, 100),
		circuitBreaker: cb,
		logger:         logger,
		stopChan:       make(chan struct{}),
		doneChan:       make(chan struct{}),
		connectedChan:  make(chan bool, 1),
	}

	return client, nil
}

// Connect establishes a connection to the Asterisk server
func (c *AMIClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	return c.connectLocked(ctx)
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

// connectLocked is the internal connection method (must be called with lock held)
func (c *AMIClient) connectLocked(ctx context.Context) error {
	if !c.circuitBreaker.AllowRequest() {
		return fmt.Errorf("circuit breaker open for AMI %s", c.address)
	}

	// Create a dialer with timeout
	dialer := net.Dialer{
		Timeout: c.timeout,
	}

	// Connect to AMI
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

	// Start reading responses and events
	go c.readLoop()

	// Login to AMI
	// In connectLocked method, update this section:
	// Login to AMI
	loginResponse, err := c.sendActionLocked(map[string]string{
		"Action":   "Login",
		"Username": c.username,
		"Secret":   c.secret,
	})

	if err != nil {
		// Critical change here: close connection BEFORE setting c.conn to nil
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("AMI login failed: %w", err)
	}

	if loginResponse["Response"] != "Success" {
		// Same here: close connection BEFORE setting c.conn to nil
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("AMI login failed: %s", loginResponse["Message"])
	}

	c.connected = true
	c.circuitBreaker.RecordSuccess()

	// Start event dispatcher
	go c.eventDispatcher()

	return nil
}

// closeConn closes the connection
func (c *AMIClient) closeConn() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.bannerReceived = false
}

// Disconnect closes the connection to the Asterisk server
func (c *AMIClient) Disconnect() {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return
	}

	// Signal the event dispatcher to stop
	close(c.stopChan)

	// Send logout action
	_, _ = c.sendActionLocked(map[string]string{
		"Action": "Logoff",
	})

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
	<-c.doneChan
}

// Reconnect attempts to reestablish connection after disconnect
func (c *AMIClient) Reconnect(ctx context.Context) error {
	c.mu.Lock()

	// If we're already connected, nothing to do
	if c.connected {
		c.mu.Unlock()
		return nil
	}

	// Make sure any existing connection is properly closed
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	c.bannerReceived = false

	// Reset the connected channel
	select {
	case <-c.connectedChan:
		// Drain the channel
	default:
		// Channel empty, continue
	}

	// Release the lock and try to reconnect
	c.mu.Unlock()

	// Try to reconnect
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
	defer close(c.doneChan)

	for {
		select {
		case <-c.stopChan:
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
		if err := c.connectLocked(context.Background()); err != nil {
			return nil, fmt.Errorf("not connected: %w", err)
		}
	}

	return c.sendActionLocked(action)
}

// sendActionLocked sends an action to Asterisk and waits for a response (must be called with lock held)
func (c *AMIClient) sendActionLocked(action map[string]string) (map[string]string, error) {
	// Add ActionID if not present
	if _, ok := action["ActionID"]; !ok {
		action["ActionID"] = fmt.Sprintf("AMI-%d", time.Now().UnixNano())
	}

	// Format the action
	var buf strings.Builder
	for k, v := range action {
		buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	buf.WriteString("\r\n")

	// Create response channel
	actionID := action["ActionID"]
	responseCh := make(chan map[string]string, 1)
	c.responseChans[actionID] = responseCh

	// Send the action
	_, err := c.conn.Write([]byte(buf.String()))
	if err != nil {
		delete(c.responseChans, actionID)
		c.circuitBreaker.RecordFailure()
		c.connected = false
		c.conn = nil
		return nil, fmt.Errorf("failed to send action: %w", err)
	}

	// Wait for response with timeout
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()

	select {
	case resp, ok := <-responseCh:
		delete(c.responseChans, actionID)
		if !ok {
			c.circuitBreaker.RecordFailure()
			return nil, fmt.Errorf("response channel closed")
		}

		if resp["Response"] == "Error" {
			c.circuitBreaker.RecordFailure()
			return resp, fmt.Errorf("AMI error: %s", resp["Message"])
		}

		c.circuitBreaker.RecordSuccess()
		return resp, nil
	case <-timer.C:
		delete(c.responseChans, actionID)
		c.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

// readLoop reads responses and events from Asterisk
func (c *AMIClient) readLoop() {
	defer func() {
		c.mu.Lock()
		c.connected = false
		// Don't set c.conn to nil here, just close it
		if c.conn != nil {
			c.conn.Close()
		}

		// Close all response channels
		for id, ch := range c.responseChans {
			close(ch)
			delete(c.responseChans, id)
		}
		c.mu.Unlock()
	}()

	// Create a recovered wrapper to prevent panics from killing the application
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("Recovered from panic in AMI readLoop",
				zap.Any("error", r))
		}
	}()

	// Create a local reader to avoid nil pointer issues
	var reader *bufio.Reader

	// Safely get the connection and create a reader
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		c.logger.Error("AMI connection is nil in readLoop")
		return
	}
	reader = bufio.NewReader(c.conn)
	c.mu.Unlock()

	for {
		// Check if connection is still valid before continuing
		c.mu.Lock()
		if c.conn == nil {
			c.mu.Unlock()
			c.logger.Debug("AMI connection closed, exiting readLoop")
			return
		}
		c.mu.Unlock()

		response := make(map[string]string)
		readingMessage := true

		for readingMessage {
			// Safely set read deadline for each line
			c.mu.Lock()
			if c.conn == nil {
				c.mu.Unlock()
				return
			}
			c.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
			c.mu.Unlock()

			// Read line with proper error handling
			line, err := reader.ReadString('\n')
			if err != nil {
				c.logger.Error("AMI read error",
					zap.String("address", c.address),
					zap.Error(err))

				return // Exit on read error
			}

			line = strings.TrimRight(line, "\r\n")

			if line == "" {
				readingMessage = false
				continue
			}

			// Parse key-value pair
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}

			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			response[key] = value
		}

		// Check if it's a response or an event
		if _, hasResponse := response["Response"]; hasResponse {
			// It's a response
			if actionID, ok := response["ActionID"]; ok {
				c.mu.Lock()
				if ch, ok := c.responseChans[actionID]; ok {
					select {
					case ch <- response:
					default:
						// Channel buffer is full (shouldn't happen)
					}
				}
				c.mu.Unlock()
			}
		} else if eventName, ok := response["Event"]; ok {
			// It's an event
			c.logger.Debug("Received AMI event",
				zap.String("event", eventName))

			// Send to event channel
			select {
			case c.eventChan <- response:
			default:
				// Channel buffer is full, log and discard
				c.logger.Warn("Event channel full, discarding event",
					zap.String("event", eventName))
			}
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
