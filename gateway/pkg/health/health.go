package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"gateway/pkg/ami"
	"gateway/pkg/coordinator"
	"gateway/pkg/rtpengine"
	"gateway/pkg/sip"
	"gateway/pkg/storage"
	"gateway/pkg/websocket"

	"go.uber.org/zap"
)

// Simple health status types
type Status string

const (
	StatusHealthy   Status = "HEALTHY"
	StatusUnhealthy Status = "UNHEALTHY"
)

// Simple component health report
type ComponentHealth struct {
	Component   string    `json:"component"`
	Status      Status    `json:"status"`
	LastChecked time.Time `json:"lastChecked"`
	Message     string    `json:"message"`
}

// HealthMonitor manages component health checks
type HealthMonitor struct {
	// Dependencies
	sipTransport sip.Transport // Changed to interface instead of concrete type
	amiManager   *ami.Manager
	rtpManager   *rtpengine.Manager
	wsServer     *websocket.Server
	coordinator  *coordinator.Coordinator
	storage      storage.StateStorage
	logger       *zap.Logger

	// Configuration
	opensipsAddress string
	checkInterval   time.Duration
	failoverEnabled bool

	// State tracking
	componentStatus map[string]*ComponentHealth
	mutex           sync.RWMutex
	stopChan        chan struct{}
}

// HealthConfig contains configuration options
type HealthConfig struct {
	OpenSIPSAddress string
	CheckInterval   time.Duration
	EnableFailover  bool
}

// NewHealthMonitor creates a new health monitor
func NewHealthMonitor(
	sipTransport sip.Transport, // Changed to interface instead of concrete type
	amiManager *ami.Manager,
	rtpManager *rtpengine.Manager,
	wsServer *websocket.Server,
	coordinator *coordinator.Coordinator,
	storage storage.StateStorage,
	logger *zap.Logger,
	config HealthConfig,
) *HealthMonitor {
	return &HealthMonitor{
		sipTransport:    sipTransport,
		amiManager:      amiManager,
		rtpManager:      rtpManager,
		wsServer:        wsServer,
		coordinator:     coordinator,
		storage:         storage,
		logger:          logger,
		opensipsAddress: config.OpenSIPSAddress,
		checkInterval:   config.CheckInterval,
		failoverEnabled: config.EnableFailover,
		componentStatus: make(map[string]*ComponentHealth),
		stopChan:        make(chan struct{}),
	}
}

// Start begins health monitoring
func (h *HealthMonitor) Start(ctx context.Context) error {
	h.logger.Info("Starting health monitoring")

	// Initialize component status
	h.componentStatus["opensips"] = &ComponentHealth{Component: "opensips", Status: StatusUnhealthy}
	h.componentStatus["asterisk"] = &ComponentHealth{Component: "asterisk", Status: StatusUnhealthy}
	h.componentStatus["rtpengine"] = &ComponentHealth{Component: "rtpengine", Status: StatusUnhealthy}

	// Run initial checks immediately
	h.checkAllComponents(ctx)

	// Start periodic checks
	go h.monitorLoop(ctx)

	return nil
}

// Stop halts health monitoring
func (h *HealthMonitor) Stop() {
	close(h.stopChan)
}

// GetComponentHealth returns current health status for a component
func (h *HealthMonitor) GetComponentHealth(component string) *ComponentHealth {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	if status, exists := h.componentStatus[component]; exists {
		// Return a copy to prevent race conditions
		copy := *status
		return &copy
	}
	return nil
}

// GetAllComponentHealth returns health status for all components
func (h *HealthMonitor) GetAllComponentHealth() map[string]*ComponentHealth {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	// Create a copy of the current status
	result := make(map[string]*ComponentHealth)
	for k, v := range h.componentStatus {
		copy := *v
		result[k] = &copy
	}
	return result
}

// monitorLoop runs periodic health checks
func (h *HealthMonitor) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(h.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.checkAllComponents(ctx)
		case <-h.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}
// In your health monitoring system
func (h *HealthMonitor) checkWebSocketBackends(ctx context.Context) {
    if h.wsServer == nil {
        return
    }
    
    // Trigger WebSocket backend health checks
    h.wsServer.CheckBackendHealth(ctx)
    
    // Get status from recent checks
    h.mutex.Lock()
    defer h.mutex.Unlock()
    
    if _, exists := h.componentStatus["websocket_backends"]; !exists {
        h.componentStatus["websocket_backends"] = &ComponentHealth{
            Component: "websocket_backends",
            Status:    StatusUnhealthy,
        }
    }
    
    // Update component status based on WebSocket backend health
    status := h.componentStatus["websocket_backends"]
    status.LastChecked = time.Now()
    
    // This assumes you'll add a method to the WebSocket server to report overall health
    if h.wsServer.HasHealthyBackends() {
        status.Status = StatusHealthy
        status.Message = "At least one WebSocket backend is healthy"
    } else {
        status.Status = StatusUnhealthy
        status.Message = "No healthy WebSocket backends available"
    }
}
// checkAllComponents performs health checks for all components
func (h *HealthMonitor) checkAllComponents(ctx context.Context) {
	h.checkOpenSIPS(ctx)
	h.checkAsterisk(ctx)
	h.checkRTPEngine(ctx)

	// Store health status for external visibility
	h.storeHealthStatus(ctx)
}

// checkOpenSIPS verifies OpenSIPS is operational by sending OPTIONS message
func (h *HealthMonitor) checkOpenSIPS(ctx context.Context) {
	h.logger.Debug("Checking OpenSIPS health")

	// Extract domain from opensipsAddress
	host := h.opensipsAddress
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}

	// Construct address with port 6060 for OpenSIPS health check
	targetAddress := host + ":6060"

	h.logger.Debug("OpenSIPS check target", zap.String("address", targetAddress))

	// Create a SIP OPTIONS message just like in the keepalive code
	optionsMsg := buildSipOptionsMessage(host)

	// Get the UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", targetAddress)
	if err != nil {
		h.mutex.Lock()
		defer h.mutex.Unlock()

		status := h.componentStatus["opensips"]
		status.LastChecked = time.Now()
		status.Status = StatusUnhealthy
		status.Message = fmt.Sprintf("Failed to resolve OpenSIPS address: %v", err)
		h.logger.Warn("OpenSIPS health check failed", zap.Error(err))
		return
	}

	// Use the UDP transport to send the message as bytes
	// Access the UDP connection from the transport (this is implementation specific)
	// This is a bit hacky, but we need to bypass the normal Send method
	// and directly send raw bytes over UDP

	// For this example, we'll use a simple UDP dialer since we don't know
	// the exact implementation details of your UDPTransport
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		h.mutex.Lock()
		defer h.mutex.Unlock()

		status := h.componentStatus["opensips"]
		status.LastChecked = time.Now()
		status.Status = StatusUnhealthy
		status.Message = fmt.Sprintf("Failed to connect to OpenSIPS: %v", err)
		h.logger.Warn("OpenSIPS health check failed", zap.Error(err))
		return
	}
	defer conn.Close()

	// Set a timeout for the UDP connection
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send the OPTIONS message
	_, err = conn.Write([]byte(optionsMsg))
	if err != nil {
		h.mutex.Lock()
		defer h.mutex.Unlock()

		status := h.componentStatus["opensips"]
		status.LastChecked = time.Now()
		status.Status = StatusUnhealthy
		status.Message = fmt.Sprintf("Failed to send OPTIONS to OpenSIPS: %v", err)
		h.logger.Warn("OpenSIPS health check failed", zap.Error(err))
		return
	}

	// Read the response
	buffer := make([]byte, 4096)
	n, _, err := conn.ReadFromUDP(buffer)

	// Update status based on the response
	h.mutex.Lock()
	defer h.mutex.Unlock()

	status := h.componentStatus["opensips"]
	status.LastChecked = time.Now()

	if err != nil {
		status.Status = StatusUnhealthy
		status.Message = fmt.Sprintf("Failed to read response from OpenSIPS: %v", err)
		h.logger.Warn("OpenSIPS health check failed", zap.Error(err))
		return
	}

	response := string(buffer[:n])

	// Check for valid responses (200 OK or 405 Method Not Allowed)
	if strings.Contains(response, "SIP/2.0 200") ||
		strings.Contains(response, "SIP/2.0 405") ||
		strings.Contains(response, "SIP/2.0 403") {
		status.Status = StatusHealthy
		message := "OpenSIPS responding to OPTIONS"
		if strings.Contains(response, "SIP/2.0 403") {
			message = "OpenSIPS responding (with 403 - authentication needed)"
		}
		status.Message = message
		h.logger.Debug("OpenSIPS health check successful")
	} else {
		status.Status = StatusUnhealthy
		status.Message = fmt.Sprintf("Unexpected response: %s", response)
		h.logger.Warn("OpenSIPS returned unexpected response",
			zap.String("response", response))

		if h.failoverEnabled && h.coordinator != nil && h.coordinator.IsLeader() {
			h.logger.Warn("Initiating OpenSIPS failover")
			h.triggerFailover("opensips")
		}
	}
}

// buildSipOptionsMessage creates a SIP OPTIONS message
func buildSipOptionsMessage(serverAddress string) string {
	callID := fmt.Sprintf("health-%d", time.Now().UnixNano())
	branch := fmt.Sprintf("z9hG4bK-%d", time.Now().UnixNano())
	fromTag := fmt.Sprintf("fromTag-%d", time.Now().UnixNano())

	return fmt.Sprintf(
		"OPTIONS sip:%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP health.monitor;branch=%s;rport\r\n"+
			"Max-Forwards: 69\r\n"+
			"From: <sip:proxy@localhost>;tag=%s\r\n"+ // Changed to match OpenSIPS pattern
			"To: <sip:%s>\r\n"+ // Direct IP address
			"Call-ID: %s\r\n"+
			"CSeq: 1 OPTIONS\r\n"+
			"User-Agent: Qalqul-Health-Monitor\r\n"+
			"Allow: INVITE, ACK, CANCEL, OPTIONS, BYE\r\n"+
			"Content-Length: 0\r\n\r\n",
		serverAddress,
		branch,
		fromTag,
		serverAddress,
		callID,
	)
}

// checkAsterisk verifies Asterisk AMI connectivity by checking banner received
func (h *HealthMonitor) checkAsterisk(ctx context.Context) {
	h.logger.Debug("Checking Asterisk health")

	// Use the AMI manager's GetActiveClient method
	client := h.amiManager.GetActiveClient()

	h.mutex.Lock()
	defer h.mutex.Unlock()

	status := h.componentStatus["asterisk"]
	status.LastChecked = time.Now()

	if client == nil {
		status.Status = StatusUnhealthy
		status.Message = "No active AMI client"
		h.logger.Warn("Asterisk AMI check failed: no active client")

		if h.failoverEnabled {
			h.logger.Warn("Initiating Asterisk failover/reconnect")
			h.triggerFailover("asterisk")
		}
		return
	}

	// Log the specific connection and banner states
	isConnected := client.IsConnected()
	hasBanner := client.IsBannerReceived()

	h.logger.Debug("AMI client status",
		zap.Bool("isConnected", isConnected),
		zap.Bool("hasBanner", hasBanner))

	// If banner is received, that's the most important health indicator
	// The banner is only sent when AMI is ready to accept commands
	if hasBanner {
		status.Status = StatusHealthy
		status.Message = "AMI banner received - Asterisk ready"
		return
	}

	// Only check connection if banner check failed
	if isConnected {
		// Connected but no banner yet - mark as healthy but note the banner issue
		status.Status = StatusHealthy
		status.Message = "AMI connected but waiting for banner"
	} else {
		status.Status = StatusUnhealthy
		status.Message = "AMI connection not established"
		h.logger.Warn("Asterisk AMI check failed: not connected")

		if h.failoverEnabled {
			h.logger.Warn("Initiating Asterisk failover/reconnect")
			h.triggerFailover("asterisk")
		}
	}
}

// checkRTPEngine verifies RTPEngine is operational
func (h *HealthMonitor) checkRTPEngine(ctx context.Context) {
	h.logger.Debug("Checking RTPEngine health")

	h.mutex.Lock()
	defer h.mutex.Unlock()

	status := h.componentStatus["rtpengine"]
	status.LastChecked = time.Now()

	// Basic check - if manager exists, consider it available
	if h.rtpManager != nil {
		status.Status = StatusHealthy
		status.Message = "RTPEngine manager available"
	} else {
		status.Status = StatusUnhealthy
		status.Message = "RTPEngine manager not available"

		if h.failoverEnabled {
			h.logger.Warn("RTPEngine health check failed")
			h.triggerFailover("rtpengine")
		}
	}
}

// triggerFailover handles component failover
func (h *HealthMonitor) triggerFailover(component string) {
	switch component {
	case "opensips":
		// Notify clients of potential service disruption
		if h.wsServer != nil {
			h.wsServer.PrepareForFailover()
		}

		// For coordinator, just log the issue since we don't have a specific method for leadership transfer
		if h.coordinator != nil && h.coordinator.IsLeader() {
			h.logger.Warn("OpenSIPS failover might require manual intervention")
		}

	case "asterisk":
		// Use the AMI manager's Reconnect method from the provided code
		if h.amiManager != nil {
			h.logger.Info("Attempting AMI reconnection")
			if err := h.amiManager.Reconnect(); err != nil {
				h.logger.Error("AMI reconnection failed", zap.Error(err))
			}
		}

	case "rtpengine":
		// For RTPEngine, we don't have direct access to failover methods
		h.logger.Info("RTPEngine failover would need to be handled externally")
	}
}

// storeHealthStatus saves current health state to storage
func (h *HealthMonitor) storeHealthStatus(ctx context.Context) {
	if h.storage == nil {
		return
	}

	// Copy current status
	h.mutex.RLock()
	statusCopy := make(map[string]*ComponentHealth)
	for k, v := range h.componentStatus {
		copy := *v
		statusCopy[k] = &copy
	}
	h.mutex.RUnlock()

	// Convert to JSON since storage.Set expects []byte
	jsonData, err := json.Marshal(statusCopy)
	if err != nil {
		h.logger.Error("Failed to marshal health status", zap.Error(err))
		return
	}

	// Store in shared state storage
	err = h.storage.Set(ctx, "health:status", jsonData, 0)
	if err != nil {
		h.logger.Error("Failed to store health status", zap.Error(err))
	}
}
