// pkg/rtpengine/rtpengine.go
package rtpengine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/zeebo/bencode"
	"go.uber.org/zap"

	"gateway/pkg/common"
)

// RTPEngine represents a connection to the rtpengine control interface using bencode.
type RTPEngine struct {
	address        string
	port           int
	timeout        time.Duration
	conn           *net.UDPConn
	serverAddr     *net.UDPAddr
	logger         *zap.Logger
	circuitBreaker *common.CircuitBreaker
	mu             sync.Mutex // protects conn
}

// Config holds the configuration for an RTPEngine connection.
type Config struct {
	Address        string                      `json:"address"`
	Port           int                         `json:"port"`
	Timeout        time.Duration               `json:"timeout"`
	Weight         int                         `json:"weight"`
	CircuitBreaker common.CircuitBreakerConfig `json:"circuit_breaker"`
}

// New creates a new RTPEngine client.
// This client always uses bencode for communication.
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
		logger:         logger,
		circuitBreaker: cb,
	}

	// Connection is established on demand.
	return r, nil
}

// ensureConnection makes sure we have a valid UDP connection.
func (r *RTPEngine) ensureConnection() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Always create a fresh UDP connection.
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

// Close closes the UDP connection.
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

// Request sends a command to rtpengine and returns the response, always using bencode.
func (r *RTPEngine) Request(ctx context.Context, command map[string]interface{}) (map[string]interface{}, error) {
	if !r.circuitBreaker.AllowRequest() {
		return nil, fmt.Errorf("circuit breaker open for RTPEngine %s:%d", r.address, r.port)
	}

	// Create a fresh connection for each request.
	if err := r.ensureConnection(); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, err
	}

	// Ensure the connection is closed after the request.
	defer func() {
		r.mu.Lock()
		if r.conn != nil {
			r.conn.Close()
			r.conn = nil
		}
		r.mu.Unlock()
	}()

	// Generate a random cookie.
	rand.Seed(time.Now().UnixNano())
	cookie := fmt.Sprintf("%d ", rand.Intn(1000000))

	// Encode the command using bencode.
	var buf bytes.Buffer
	if err := bencode.NewEncoder(&buf).Encode(command); err != nil {
		return nil, fmt.Errorf("failed to encode command: %w", err)
	}
	packet := buf.Bytes()

	// Prepend the cookie.
	fullPacket := append([]byte(cookie), packet...)

	// Use context deadline if set; otherwise, use the default timeout.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(r.timeout)
	}

	// Set the write deadline and send the packet.
	if err := r.conn.SetWriteDeadline(deadline); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to set write deadline: %w", err)
	}
	if _, err := r.conn.Write(fullPacket); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to send packet: %w", err)
	}

	// Set the read deadline.
	if err := r.conn.SetReadDeadline(deadline); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to set read deadline: %w", err)
	}

	// Read the response.
	responseBuffer := make([]byte, 65535)
	n, _, err := r.conn.ReadFromUDP(responseBuffer)
	if err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}
	response := responseBuffer[:n]

	// Check that the cookie in the response matches.
	if len(response) < len(cookie) || string(response[:len(cookie)]) != cookie {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("cookie mismatch in response")
	}

	// Remove the cookie and trim any extra whitespace.
	response = response[len(cookie):]
	response = bytes.TrimSpace(response)

	// Decode the response using bencode.
	var result map[string]interface{}
	if err := bencode.NewDecoder(bytes.NewReader(response)).Decode(&result); err != nil {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check for errors in the response.
	if res, ok := result["result"]; !ok {
		r.circuitBreaker.RecordFailure()
		return nil, fmt.Errorf("no result field in response: %v", result)
	} else if resStr, ok := res.(string); ok && resStr == "error" {
		r.circuitBreaker.RecordFailure()
		errorReason, _ := result["error-reason"].(string)
		return nil, fmt.Errorf("RTPEngine error: %s", errorReason)
	}

	r.circuitBreaker.RecordSuccess()
	return result, nil
}

// Offer sends an 'offer' command to rtpengine.
func (r *RTPEngine) Offer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "offer"
	return r.Request(ctx, params)
}

// Answer sends an 'answer' command to rtpengine.
func (r *RTPEngine) Answer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "answer"
	return r.Request(ctx, params)
}

// Delete sends a 'delete' command to rtpengine.
func (r *RTPEngine) Delete(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "delete"
	return r.Request(ctx, params)
}

// Query sends a 'query' command to rtpengine.
func (r *RTPEngine) Query(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
	params["command"] = "query"
	return r.Request(ctx, params)
}

// List sends a 'list' command to rtpengine.
func (r *RTPEngine) List(ctx context.Context) (map[string]interface{}, error) {
	params := map[string]interface{}{
		"command": "list",
	}
	return r.Request(ctx, params)
}

// Ping sends a ping command to check rtpengine availability.
// It expects the response "pong" to declare the RTP engine healthy.
func (r *RTPEngine) Ping(ctx context.Context) error {
	params := map[string]interface{}{
		"command": "ping",
	}
	result, err := r.Request(ctx, params)
	if err != nil {
		return err
	}
	if res, ok := result["result"].(string); !ok || res != "pong" {
		return fmt.Errorf("unexpected ping response: %v", result)
	}
	r.logger.Info("RTPengine healthy: ping returned pong")
	return nil
}

// GetStats returns rtpengine statistics.
func (r *RTPEngine) GetStats(ctx context.Context) (map[string]interface{}, error) {
	params := map[string]interface{}{
		"command": "statistics",
	}
	return r.Request(ctx, params)
}
