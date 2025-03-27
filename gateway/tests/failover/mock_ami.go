package failover

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// MockAMIClient simulates an AMI client for testing
type MockAMIClient struct {
	ID             string
	Address        string
	Username       string
	Secret         string
	Connected      bool
	FailureMode    bool
	DisconnectMode bool
	FailCount      int32
	Actions        []map[string]string
	Events         []map[string]string
	EventHandlers  map[string][]func(event map[string]string)
	mutex          sync.Mutex
	logger         *zap.Logger
	responseDelay  time.Duration
}

// NewMockAMIClient creates a new mock AMI client
func NewMockAMIClient(id string, address string, username string, secret string, logger *zap.Logger) *MockAMIClient {
	return &MockAMIClient{
		ID:            id,
		Address:       address,
		Username:      username,
		Secret:        secret,
		Connected:     false,
		Actions:       make([]map[string]string, 0),
		Events:        make([]map[string]string, 0),
		EventHandlers: make(map[string][]func(event map[string]string)),
		logger:        logger,
		responseDelay: 10 * time.Millisecond,
	}
}

// SimulateFailure sets the client in failure mode
func (m *MockAMIClient) SimulateFailure(fail bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.FailureMode = fail
}

// SimulateDisconnection sets the client in disconnection mode
func (m *MockAMIClient) SimulateDisconnection(disconnect bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.DisconnectMode = disconnect
	if disconnect {
		m.Connected = false
	}
}

// SetResponseDelay sets an artificial delay for responses
func (m *MockAMIClient) SetResponseDelay(delay time.Duration) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.responseDelay = delay
}

// Connect simulates connecting to AMI
func (m *MockAMIClient) Connect(ctx context.Context) error {
	m.mutex.Lock()
	disconnectMode := m.DisconnectMode
	responseDelay := m.responseDelay
	m.mutex.Unlock()

	select {
	case <-time.After(responseDelay):
		// Continue processing
	case <-ctx.Done():
		return ctx.Err()
	}

	if disconnectMode {
		atomic.AddInt32(&m.FailCount, 1)
		return fmt.Errorf("connection refused to %s", m.Address)
	}

	m.mutex.Lock()
	m.Connected = true
	m.mutex.Unlock()

	m.logger.Debug("Mock AMI client connected",
		zap.String("client", m.ID),
		zap.String("address", m.Address))

	return nil
}

// IsConnected checks if the client is connected
func (m *MockAMIClient) IsConnected() bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.Connected
}

// Disconnect simulates disconnecting from AMI
func (m *MockAMIClient) Disconnect() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.Connected = false
	m.logger.Debug("Mock AMI client disconnected",
		zap.String("client", m.ID),
		zap.String("address", m.Address))
}

// SendAction simulates sending an action to AMI
func (m *MockAMIClient) SendAction(action map[string]string) (map[string]string, error) {
	m.mutex.Lock()
	connected := m.Connected
	failureMode := m.FailureMode
	disconnectMode := m.DisconnectMode
	responseDelay := m.responseDelay
	m.Actions = append(m.Actions, action)
	m.mutex.Unlock()

	// Simulate processing delay
	time.Sleep(responseDelay)

	if !connected || disconnectMode {
		atomic.AddInt32(&m.FailCount, 1)
		return nil, fmt.Errorf("not connected to %s", m.Address)
	}

	if failureMode {
		atomic.AddInt32(&m.FailCount, 1)
		return nil, fmt.Errorf("simulated failure from %s", m.Address)
	}

	// Default success response
	actionName := action["Action"]
	response := map[string]string{
		"Response": "Success",
		"Message":  "Action processed successfully",
		"ActionID": action["ActionID"],
	}

	switch actionName {
	case "Ping":
		response["Ping"] = "Pong"
	case "Originate":
		response["OriginateID"] = fmt.Sprintf("call-%d", time.Now().UnixNano())
	case "SIPpeers":
		response["PeerCount"] = "2"
	}

	m.logger.Debug("Mock AMI client handled action",
		zap.String("client", m.ID),
		zap.String("action", actionName))

	return response, nil
}

// Ping simulates a ping action
func (m *MockAMIClient) Ping() error {
	_, err := m.SendAction(map[string]string{
		"Action":   "Ping",
		"ActionID": fmt.Sprintf("ping-%d", time.Now().UnixNano()),
	})
	return err
}

// RegisterEventHandler registers an event handler
func (m *MockAMIClient) RegisterEventHandler(eventType string, handler func(event map[string]string)) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, exists := m.EventHandlers[eventType]; !exists {
		m.EventHandlers[eventType] = make([]func(event map[string]string), 0)
	}
	m.EventHandlers[eventType] = append(m.EventHandlers[eventType], handler)
}

// TriggerEvent simulates an event from Asterisk
func (m *MockAMIClient) TriggerEvent(event map[string]string) {
	m.mutex.Lock()
	m.Events = append(m.Events, event)
	eventType := event["Event"]
	handlers := m.EventHandlers[eventType]
	wildcardHandlers := m.EventHandlers["*"]
	m.mutex.Unlock()

	// Call specific event handlers
	for _, handler := range handlers {
		go handler(event)
	}

	// Call wildcard handlers
	for _, handler := range wildcardHandlers {
		go handler(event)
	}

	m.logger.Debug("Mock AMI client triggered event",
		zap.String("client", m.ID),
		zap.String("event", eventType))
}

// Reconnect simulates reconnecting to AMI
func (m *MockAMIClient) Reconnect(ctx context.Context) error {
	m.Disconnect()
	return m.Connect(ctx)
}

// GetAddress returns the client's address
func (m *MockAMIClient) GetAddress() string {
	return m.Address
}

// ConnectNonBlocking simulates non-blocking connection
func (m *MockAMIClient) ConnectNonBlocking(ctx context.Context) {
	go func() {
		err := m.Connect(ctx)
		if err != nil {
			m.logger.Debug("Mock AMI non-blocking connection failed",
				zap.String("client", m.ID),
				zap.String("address", m.Address),
				zap.Error(err))
		}
	}()
}

// WaitForBanner simulates waiting for a banner message
func (m *MockAMIClient) WaitForBanner(timeout time.Duration) bool {
	m.mutex.Lock()
	connected := m.Connected
	m.mutex.Unlock()
	return connected
}

// GetFailCount returns the current failure count
func (m *MockAMIClient) GetFailCount() int {
	return int(atomic.LoadInt32(&m.FailCount))
}

// ResetFailCount resets the failure counter
func (m *MockAMIClient) ResetFailCount() {
	atomic.StoreInt32(&m.FailCount, 0)
}

// GetActionCount returns the number of actions received
func (m *MockAMIClient) GetActionCount() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return len(m.Actions)
}

// ClearActions clears the action history
func (m *MockAMIClient) ClearActions() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.Actions = make([]map[string]string, 0)
}
