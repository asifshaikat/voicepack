// pkg/rtpengine/rtpengine.go
package rtpengine

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/common"
)

// RTPEngine represents a connection to the rtpengine control interface.
type RTPEngine struct {
	address        string
	port           int
	timeout        time.Duration
	conn           *net.UDPConn
	serverAddr     *net.UDPAddr
	useJSON        bool
	logger         *zap.Logger
	circuitBreaker *common.CircuitBreaker
	mu             sync.Mutex // protects conn
}

// Config holds the configuration for an RTPEngine connection
type Config struct {
	Address        string                      `json:"address"`
	Port           int                         `json:"port"`
	Timeout        time.Duration               `json:"timeout"`
	Weight         int                         `json:"weight"`
	CircuitBreaker common.CircuitBreakerConfig `json:"circuit_breaker"`
}

// New creates a new RTPEngine client
func New(config Config, logger *zap.Logger) (*RTPEngine, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", config.Address, config.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %w", err)
	}

	if config.Timeout <= 0 {
		config.Timeout = 500 * time.Millisecond
	}

	cbName := fmt.Sprintf("rtpengine-%s:%d", config.Address, config.Port)
	cb := common.NewCircuitBreaker(cbName, config.CircuitBreaker, logger)

	r := &RTPEngine{
		address:        config.Address,
		port:           config.Port,
		timeout:        config.Timeout,
		serverAddr:     serverAddr,
		useJSON:        true, // Always use JSON for modern RTPEngine
		logger:         logger,
		circuitBreaker: cb,
	}

	// Don't establish connection in constructor - we'll do it on demand
	// This makes the RTPEngine client more resilient to startup conditions

	return r, nil
}

// ensureConnection makes sure we have a valid connection
func (r *RTPEngine) ensureConnection() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Always create a fresh UDP connection
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}

	conn, err := net.DialUDP("udp", nil, r.serverAddr)
	if err != nil {
		return fmt.Errorf("failed to establish UDP connection: %w", err)
	}

	r.conn = conn
	return nil
}

// Close closes the UDP connection
func (r *RTPEngine) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn != nil {
		err := r.conn.Close()
		r.conn = nil
		return err
	}
	return nil
}

// Request sends a command to rtpengine and returns the response
func (r *RTPEngine) Request(ctx context.Context, command map[string]interface{}) (map[string]interface{}, error) {
	if !r.circuitBreaker.AllowRequest() {
		return nil, fmt.Errorf("circuit breaker open for RTPEngine %s:%d", r.address, r.port)
	}

	// Create a fresh connection for each request to avoid socket issues
	if err := r.ensureConnection(); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, err
	}

	// Ensure connection is properly closed
	defer func() {
		r.mu.Lock()
		if r.conn != nil {
			r.conn.Close()
			r.conn = nil
		}
		r.mu.Unlock()
	}()

	// Generate random cookie
	rand.Seed(time.Now().UnixNano())
	cookie := fmt.Sprintf("%d ", rand.Intn(1000000))

	// Encode the command as JSON
	packet, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("failed to encode command: %w", err)
	}

	// Prepend the cookie
	fullPacket := append([]byte(cookie), packet...)

	// Use context timeout if set
	deadline, ok := ctx.Deadline()
	if !ok {
		// No deadline in context, use our default timeout
		deadline = time.Now().Add(r.timeout)
	}

	// Set write deadline and send the packet
	err = r.conn.SetWriteDeadline(deadline)
	if err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to set write deadline: %w", err)
	}

	_, err = r.conn.Write(fullPacket)
	if err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to send packet: %w", err)
	}

	// Set read deadline
	err = r.conn.SetReadDeadline(deadline)
	if err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to set read deadline: %w", err)
	}

	// Read the response
	responseBuffer := make([]byte, 65535)
	n, _, err := r.conn.ReadFromUDP(responseBuffer)

	if err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}

	response := responseBuffer[:n]

	// Check the cookie
	if !string(response[:len(cookie)]) == cookie {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("cookie mismatch in response")
	}

	// Remove the cookie
	response = response[len(cookie):]

	// Decode the response
	var result map[string]interface{}
	if err := json.Unmarshal(response, &result); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check for errors
	if resultStr, ok := result["result"].(string); ok && resultStr == "error" {
		r.circuitBreaker.RecordFailure()
		errorReason, _ := result["error-reason"].(string)
		return nil, fmt.Errorf("RTPEngine error: %s", errorReason)
	}

	r.circuitBreaker.RecordSuccess()
	return result, nil
}

// Offer sends an 'offer' command to rtpengine
func (r *RTPEngine) Offer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "offer"
	return r.Request(ctx, params)
}

// Answer sends an 'answer' command to rtpengine
func (r *RTPEngine) Answer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "answer"
	return r.Request(ctx, params)
}

// Delete sends a 'delete' command to rtpengine
func (r *RTPEngine) Delete(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "delete"
	return r.Request(ctx, params)
}

// Query sends a 'query' command to rtpengine
func (r *RTPEngine) Query(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "query"
	return r.Request(ctx, params)
}

// List sends a 'list' command to rtpengine
func (r *RTPEngine) List(ctx context.Context) (map[string]interface{}, error) {
	params := map[string]interface{}{
		"command": "list",
	}
	return r.Request(ctx, params)
}

// Ping sends a ping to check rtpengine availability
func (r *RTPEngine) Ping(ctx context.Context) error {
	params := map[string]interface{}{
		"command": "ping",
	}
	_, err := r.Request(ctx, params)
	return err
}

// GetStats returns rtpengine statistics
func (r *RTPEngine) GetStats(ctx context.Context) (map[string]interface{}, error) {
	params := map[string]interface{}{
		"command": "statistics",
	}
	return r.Request(ctx, params)
}
