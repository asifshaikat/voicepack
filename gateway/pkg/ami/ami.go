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
	responseChans  map[string]chan map[string]string
	eventHandlers  map[string][]func(event map[string]string)
	eventChan      chan map[string]string
	circuitBreaker *common.CircuitBreaker
	logger         *zap.Logger
	stopChan       chan struct{}
	doneChan       chan struct{}
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

	// Start reading responses and events
	go c.readLoop()

	// Login to AMI
	loginResponse, err := c.SendAction(map[string]string{
		"Action":   "Login",
		"Username": c.username,
		"Secret":   c.secret,
	})

	if err != nil {
		c.closeConn()
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("AMI login failed: %w", err)
	}

	if loginResponse["Response"] != "Success" {
		c.closeConn()
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
		if err := c.connectLocked(); err != nil {
			return nil, fmt.Errorf("not connected: %w", err)
		}
	}

	return c.sendActionLocked(action)
}

// connectLocked connects to Asterisk (must be called with lock held)
func (c *AMIClient) connectLocked() error {
	if c.connected {
		return nil
	}

	if !c.circuitBreaker.AllowRequest() {
		return fmt.Errorf("circuit breaker open for AMI %s", c.address)
	}

	// Connect to AMI
	conn, err := net.DialTimeout("tcp", c.address, c.timeout)
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
		c.conn = nil
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("failed to read AMI banner: %w", err)
	}

	c.logger.Debug("Connected to Asterisk",
		zap.String("address", c.address),
		zap.String("banner", strings.TrimSpace(banner)))

	// Start reading responses and events
	go c.readLoop()

	// Login to AMI
	loginResponse, err := c.sendActionLocked(map[string]string{
		"Action":   "Login",
		"Username": c.username,
		"Secret":   c.secret,
	})

	if err != nil {
		conn.Close()
		c.conn = nil
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("AMI login failed: %w", err)
	}

	if loginResponse["Response"] != "Success" {
		conn.Close()
		c.conn = nil
		c.circuitBreaker.RecordFailure()
		return fmt.Errorf("AMI login failed: %s", loginResponse["Message"])
	}

	c.connected = true
	c.circuitBreaker.RecordSuccess()

	// Start event dispatcher if it's not already running
	select {
	case <-c.doneChan:
		// Dispatcher is not running, start it
		c.stopChan = make(chan struct{})
		c.doneChan = make(chan struct{})
		go c.eventDispatcher()
	default:
		// Dispatcher is already running
	}

	return nil
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
		c.conn = nil
		// Close all response channels
		for id, ch := range c.responseChans {
			close(ch)
			delete(c.responseChans, id)
		}
		c.mu.Unlock()
	}()

	reader := bufio.NewReader(c.conn)

	for {
		response := make(map[string]string)
		readingMessage := true

		for readingMessage {
			// Reset read deadline for each line
			c.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

			line, err := reader.ReadString('\n')
			if err != nil {
				c.logger.Error("AMI read error",
					zap.String("address", c.address),
					zap.Error(err))
				return
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
