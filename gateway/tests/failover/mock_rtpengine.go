package failover

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// MockRTPEngine simulates an RTPEngine for testing
type MockRTPEngine struct {
	ID             string
	Address        string
	Port           int
	FailureMode    bool          // When true, all requests will fail
	DisconnectMode bool          // When true, connection tests will fail
	FailCount      int32         // Number of failures
	Requests       []interface{} // Track incoming requests
	mutex          sync.Mutex
	logger         *zap.Logger
	responseDelay  time.Duration // Artificial delay before responding
}

// NewMockRTPEngine creates a new mock RTPEngine
func NewMockRTPEngine(id string, address string, port int, logger *zap.Logger) *MockRTPEngine {
	return &MockRTPEngine{
		ID:            id,
		Address:       address,
		Port:          port,
		Requests:      make([]interface{}, 0),
		logger:        logger,
		responseDelay: 10 * time.Millisecond, // Small default delay
	}
}

// SimulateFailure sets the engine in failure mode
func (m *MockRTPEngine) SimulateFailure(fail bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.FailureMode = fail
}

// SimulateDisconnection sets the engine in disconnection mode
func (m *MockRTPEngine) SimulateDisconnection(disconnect bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.DisconnectMode = disconnect
}

// SetResponseDelay sets an artificial delay for responses
func (m *MockRTPEngine) SetResponseDelay(delay time.Duration) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.responseDelay = delay
}

// Request simulates sending a request to RTPEngine
func (m *MockRTPEngine) Request(ctx context.Context, command map[string]interface{}) (map[string]interface{}, error) {
	m.mutex.Lock()
	failureMode := m.FailureMode
	disconnectMode := m.DisconnectMode
	responseDelay := m.responseDelay
	m.Requests = append(m.Requests, command)
	m.mutex.Unlock()

	// Simulate processing delay
	select {
	case <-time.After(responseDelay):
		// Continue processing
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if disconnectMode {
		atomic.AddInt32(&m.FailCount, 1)
		return nil, fmt.Errorf("connection refused to %s:%d", m.Address, m.Port)
	}

	if failureMode {
		atomic.AddInt32(&m.FailCount, 1)
		return nil, fmt.Errorf("simulated failure from %s:%d", m.Address, m.Port)
	}

	// Default success response
	cmdType, ok := command["command"].(string)
	if !ok {
		cmdType = "unknown"
	}

	response := map[string]interface{}{
		"result": "ok",
	}

	switch cmdType {
	case "ping":
		response["result"] = "pong"
	case "offer", "answer":
		response["sdp"] = "v=0\r\no=- 1622622139 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 40376 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	case "query":
		response["totals"] = map[string]interface{}{
			"packets": 100,
			"bytes":   16000,
		}
	}

	m.logger.Debug("Mock RTPEngine handled request",
		zap.String("engine", m.ID),
		zap.String("command", cmdType),
		zap.Any("request", command))

	return response, nil
}

// Offer simulates an offer request
func (m *MockRTPEngine) Offer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "offer"
	return m.Request(ctx, params)
}

// Answer simulates an answer request
func (m *MockRTPEngine) Answer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "answer"
	return m.Request(ctx, params)
}

// Delete simulates a delete request
func (m *MockRTPEngine) Delete(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "delete"
	return m.Request(ctx, params)
}

// Query simulates a query request
func (m *MockRTPEngine) Query(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "query"
	return m.Request(ctx, params)
}

// Ping simulates a ping request
func (m *MockRTPEngine) Ping(ctx context.Context) error {
	resp, err := m.Request(ctx, map[string]interface{}{
		"command": "ping",
	})
	if err != nil {
		return err
	}

	if res, ok := resp["result"].(string); !ok || res != "pong" {
		return fmt.Errorf("unexpected ping response: %v", resp)
	}

	return nil
}

// Close simulates closing a connection
func (m *MockRTPEngine) Close() error {
	return nil
}

// GetFailCount returns the current failure count
func (m *MockRTPEngine) GetFailCount() int {
	return int(atomic.LoadInt32(&m.FailCount))
}

// ResetFailCount resets the failure counter
func (m *MockRTPEngine) ResetFailCount() {
	atomic.StoreInt32(&m.FailCount, 0)
}

// GetRequestCount returns the number of requests received
func (m *MockRTPEngine) GetRequestCount() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return len(m.Requests)
}

// ClearRequests clears the request history
func (m *MockRTPEngine) ClearRequests() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.Requests = make([]interface{}, 0)
}
