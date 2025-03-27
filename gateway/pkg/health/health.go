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
	StatusDegraded  Status = "DEGRADED" // New status for partial functionality
	StatusFailover  Status = "FAILOVER" // Indicates failover in progress
)

// Simple component health report
type ComponentHealth struct {
	Component   string          `json:"component"`
	Status      Status          `json:"status"`
	LastChecked time.Time       `json:"lastChecked"`
	Message     string          `json:"message"`
	Backends    []BackendHealth `json:"backends,omitempty"` // Added for multiple backends
	Stats       map[string]int  `json:"stats,omitempty"`    // Stats like success/fail counts
}

// BackendHealth represents health of a single backend
type BackendHealth struct {
	Address     string    `json:"address"`
	Status      Status    `json:"status"`
	LastChecked time.Time `json:"lastChecked"`
	Message     string    `json:"message"`
	FailCount   int       `json:"failCount"`
	IsActive    bool      `json:"isActive"`
}

// HealthMonitor manages component health checks
type HealthMonitor struct {
	// Dependencies
	sipTransport sip.Transport
	amiManager   *ami.Manager
	rtpManager   *rtpengine.Manager
	wsServer     *websocket.Server
	coordinator  *coordinator.Coordinator
	storage      storage.StateStorage
	logger       *zap.Logger

	// Configuration
	opensipsAddress  string
	opensipsBackends []string // Added for multiple backends
	checkInterval    time.Duration
	failoverEnabled  bool
	failureThreshold int // Added to track failures before failover

	// State tracking
	componentStatus map[string]*ComponentHealth
	mutex           sync.RWMutex
	stopChan        chan struct{}

	// Failover tracking
	failoverHistory map[string][]FailoverEvent // Track failover history
}

// FailoverEvent tracks when failovers happen
type FailoverEvent struct {
	Component   string    `json:"component"`
	FromBackend string    `json:"fromBackend"`
	ToBackend   string    `json:"toBackend"`
	Timestamp   time.Time `json:"timestamp"`
	Reason      string    `json:"reason"`
}

// HealthConfig contains configuration options
type HealthConfig struct {
	OpenSIPSAddress  string
	OpenSIPSBackends []string // Added for multiple backends
	CheckInterval    time.Duration
	EnableFailover   bool
	FailureThreshold int // Added threshold for failover
}

// NewHealthMonitor creates a new health monitor
func NewHealthMonitor(
	sipTransport sip.Transport,
	amiManager *ami.Manager,
	rtpManager *rtpengine.Manager,
	wsServer *websocket.Server,
	coordinator *coordinator.Coordinator,
	storage storage.StateStorage,
	logger *zap.Logger,
	config HealthConfig,
) *HealthMonitor {
	// Default failure threshold if not set
	failureThreshold := 3
	if config.FailureThreshold > 0 {
		failureThreshold = config.FailureThreshold
	}

	// Ensure we have at least the primary backend in the list
	opensipsBackends := config.OpenSIPSBackends
	if len(opensipsBackends) == 0 && config.OpenSIPSAddress != "" {
		opensipsBackends = []string{config.OpenSIPSAddress}
	}

	// Use shorter check interval if not specified
	checkInterval := 3 * time.Second // Reduced from default 15s
	if config.CheckInterval > 0 {
		checkInterval = config.CheckInterval
	}

	return &HealthMonitor{
		sipTransport:     sipTransport,
		amiManager:       amiManager,
		rtpManager:       rtpManager,
		wsServer:         wsServer,
		coordinator:      coordinator,
		storage:          storage,
		logger:           logger,
		opensipsAddress:  config.OpenSIPSAddress,
		opensipsBackends: opensipsBackends,
		checkInterval:    checkInterval,
		failoverEnabled:  config.EnableFailover,
		failureThreshold: failureThreshold,
		componentStatus:  make(map[string]*ComponentHealth),
		stopChan:         make(chan struct{}),
		failoverHistory:  make(map[string][]FailoverEvent),
	}
}

// Start begins health monitoring
func (h *HealthMonitor) Start(ctx context.Context) error {
	h.logger.Info("Starting health monitoring",
		zap.Int("num_opensips_backends", len(h.opensipsBackends)),
		zap.Bool("failover_enabled", h.failoverEnabled),
		zap.Int("failure_threshold", h.failureThreshold),
		zap.Duration("check_interval", h.checkInterval))

	// Initialize component status with backend details
	h.componentStatus["opensips"] = &ComponentHealth{
		Component: "opensips",
		Status:    StatusUnhealthy,
		Backends:  make([]BackendHealth, len(h.opensipsBackends)),
		Stats:     map[string]int{"success": 0, "failure": 0, "failovers": 0},
	}

	// Initialize backends for OpenSIPS
	for i, backend := range h.opensipsBackends {
		h.componentStatus["opensips"].Backends[i] = BackendHealth{
			Address:  backend,
			Status:   StatusUnhealthy,
			IsActive: i == 0, // First one is initially active
		}
	}

	// Setup Asterisk component with multiple backends if available
	h.componentStatus["asterisk"] = &ComponentHealth{
		Component: "asterisk",
		Status:    StatusUnhealthy,
		Stats:     map[string]int{"success": 0, "failure": 0, "failovers": 0},
	}

	// Populate AMI backends if available
	if h.amiManager != nil {
		clients := h.amiManager.GetAllClients()
		if len(clients) > 0 {
			backends := make([]BackendHealth, len(clients))
			for i, client := range clients {
				backends[i] = BackendHealth{
					Address:  client.GetAddress(),
					Status:   StatusUnhealthy,
					IsActive: false,
				}
			}
			h.componentStatus["asterisk"].Backends = backends

			// Mark active client
			activeClient := h.amiManager.GetActiveClient()
			if activeClient != nil {
				activeAddr := activeClient.GetAddress()
				for i, backend := range backends {
					if backend.Address == activeAddr {
						h.componentStatus["asterisk"].Backends[i].IsActive = true
						break
					}
				}
			}
		}
	}

	// Setup RTPEngine component
	h.componentStatus["rtpengine"] = &ComponentHealth{
		Component: "rtpengine",
		Status:    StatusUnhealthy,
		Stats:     map[string]int{"success": 0, "failure": 0, "failovers": 0},
	}

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

// checkAllComponents performs health checks for all components
func (h *HealthMonitor) checkAllComponents(ctx context.Context) {
	h.logger.Debug("Running health checks for all components")

	// Check OpenSIPS backends
	h.checkOpenSIPSBackends(ctx)

	// Check Asterisk
	h.checkAsterisk(ctx)

	// Check RTPEngine
	h.checkRTPEngine(ctx)

	// Check WebSocket backends if available
	if h.wsServer != nil && h.wsServer.HasBackends() {
		h.checkWebSocketBackends(ctx)
	}

	// Store health status for external visibility
	h.storeHealthStatus(ctx)

	// Log a summary of component health
	h.logHealthSummary()
}

// logHealthSummary logs a summary of all component health
func (h *HealthMonitor) logHealthSummary() {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	for name, component := range h.componentStatus {
		backendCounts := map[Status]int{
			StatusHealthy:   0,
			StatusUnhealthy: 0,
			StatusDegraded:  0,
		}

		// Count backend statuses
		for _, backend := range component.Backends {
			backendCounts[backend.Status]++
		}

		// Log component status with backend counts
		h.logger.Info("Component health status",
			zap.String("component", name),
			zap.String("status", string(component.Status)),
			zap.Int("healthy_backends", backendCounts[StatusHealthy]),
			zap.Int("unhealthy_backends", backendCounts[StatusUnhealthy]),
			zap.Int("degraded_backends", backendCounts[StatusDegraded]),
			zap.String("message", component.Message),
			zap.Time("last_checked", component.LastChecked))
	}
}

// checkOpenSIPSBackends checks health of all OpenSIPS backends
func (h *HealthMonitor) checkOpenSIPSBackends(ctx context.Context) {
	h.logger.Debug("Checking OpenSIPS backends",
		zap.Int("backend_count", len(h.opensipsBackends)))

	if len(h.opensipsBackends) == 0 {
		h.logger.Warn("No OpenSIPS backends configured")
		return
	}

	h.mutex.Lock()
	status := h.componentStatus["opensips"]
	h.mutex.Unlock()

	// Track overall status
	overallHealthy := false
	activeHealthy := false
	activeBackendIndex := -1

	// Find active backend
	for i, backend := range status.Backends {
		if backend.IsActive {
			activeBackendIndex = i
			break
		}
	}

	// Default to first backend if none marked active
	if activeBackendIndex == -1 && len(status.Backends) > 0 {
		activeBackendIndex = 0
		status.Backends[0].IsActive = true
	}

	// Check each backend
	wg := sync.WaitGroup{}
	results := make([]bool, len(h.opensipsBackends))

	for i, backend := range h.opensipsBackends {
		wg.Add(1)
		go func(idx int, addr string) {
			defer wg.Done()
			healthy := h.checkSingleOpenSIPSBackend(ctx, addr, idx)
			results[idx] = healthy

			// Update backend status
			h.mutex.Lock()
			defer h.mutex.Unlock()

			backendStatus := &status.Backends[idx]
			backendStatus.LastChecked = time.Now()

			if healthy {
				backendStatus.Status = StatusHealthy
				backendStatus.Message = "OpenSIPS responding to OPTIONS"
				backendStatus.FailCount = 0
			} else {
				backendStatus.FailCount++
				if backendStatus.FailCount >= h.failureThreshold {
					backendStatus.Status = StatusUnhealthy
					backendStatus.Message = fmt.Sprintf("Failed health check %d times", backendStatus.FailCount)
				}
			}
		}(i, backend)
	}

	wg.Wait()

	// Process results and update component status
	h.mutex.Lock()
	defer h.mutex.Unlock()

	// Check if any backend is healthy
	healthyCount := 0
	for i, healthy := range results {
		if healthy {
			healthyCount++
			overallHealthy = true

			// Check if active backend is healthy
			if i == activeBackendIndex {
				activeHealthy = true
			}
		}
	}

	// Update stats
	status.Stats["total_checks"]++
	if healthyCount > 0 {
		status.Stats["success"]++
	} else {
		status.Stats["failure"]++
	}

	// If active backend is unhealthy but others are healthy, trigger failover
	if !activeHealthy && overallHealthy && h.failoverEnabled {
		// Find first healthy backend to failover to
		newActiveIdx := -1
		for i, healthy := range results {
			if healthy && i != activeBackendIndex {
				newActiveIdx = i
				break
			}
		}

		if newActiveIdx != -1 {
			// Perform failover
			oldBackend := h.opensipsBackends[activeBackendIndex]
			newBackend := h.opensipsBackends[newActiveIdx]

			h.logger.Warn("Initiating OpenSIPS failover",
				zap.String("from", oldBackend),
				zap.String("to", newBackend),
				zap.Int("fail_count", status.Backends[activeBackendIndex].FailCount))

			// Update active status
			for i := range status.Backends {
				status.Backends[i].IsActive = (i == newActiveIdx)
			}

			// Record failover event
			event := FailoverEvent{
				Component:   "opensips",
				FromBackend: oldBackend,
				ToBackend:   newBackend,
				Timestamp:   time.Now(),
				Reason:      fmt.Sprintf("Health check failure count: %d", status.Backends[activeBackendIndex].FailCount),
			}

			h.failoverHistory["opensips"] = append(h.failoverHistory["opensips"], event)
			status.Stats["failovers"]++

			// Trigger actual failover
			h.triggerFailover("opensips")
		}
	}

	// Update overall component status
	status.LastChecked = time.Now()

	if healthyCount == len(h.opensipsBackends) {
		status.Status = StatusHealthy
		status.Message = "All OpenSIPS backends healthy"
	} else if healthyCount > 0 {
		status.Status = StatusDegraded
		status.Message = fmt.Sprintf("%d/%d OpenSIPS backends healthy", healthyCount, len(h.opensipsBackends))
	} else {
		status.Status = StatusUnhealthy
		status.Message = "All OpenSIPS backends unhealthy"
	}
}

// checkSingleOpenSIPSBackend checks a single OpenSIPS backend
func (h *HealthMonitor) checkSingleOpenSIPSBackend(ctx context.Context, address string, index int) bool {
	// Extract host from address
	host := address
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}

	// Construct address with port 6060 for OpenSIPS health check
	targetAddress := host + ":6060"

	h.logger.Debug("Checking OpenSIPS backend",
		zap.String("address", targetAddress),
		zap.Int("index", index))

	// Create a SIP OPTIONS message
	optionsMsg := buildSipOptionsMessage(host)

	// Get the UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", targetAddress)
	if err != nil {
		h.logger.Warn("Failed to resolve OpenSIPS backend address",
			zap.String("address", targetAddress),
			zap.Error(err))
		return false
	}

	// Connect to the backend
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		h.logger.Warn("Failed to connect to OpenSIPS backend",
			zap.String("address", targetAddress),
			zap.Error(err))
		return false
	}
	defer conn.Close()

	// Set timeout
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send OPTIONS message
	_, err = conn.Write([]byte(optionsMsg))
	if err != nil {
		h.logger.Warn("Failed to send OPTIONS to OpenSIPS backend",
			zap.String("address", targetAddress),
			zap.Error(err))
		return false
	}

	// Read response
	buffer := make([]byte, 4096)
	n, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		h.logger.Warn("Failed to read response from OpenSIPS backend",
			zap.String("address", targetAddress),
			zap.Error(err))
		return false
	}

	response := string(buffer[:n])

	// Check response
	if strings.Contains(response, "SIP/2.0 200") ||
		strings.Contains(response, "SIP/2.0 405") ||
		strings.Contains(response, "SIP/2.0 403") {

		h.logger.Debug("OpenSIPS backend healthy",
			zap.String("address", targetAddress),
			zap.String("response_code", response[:15]))
		return true
	}

	h.logger.Warn("OpenSIPS backend returned unexpected response",
		zap.String("address", targetAddress),
		zap.String("response", response[:100]))
	return false
}

// checkAsterisk has been enhanced to provide more detailed logging about AMI clients
func (h *HealthMonitor) checkAsterisk(ctx context.Context) {
	h.logger.Debug("Checking Asterisk health")

	// Get active client and all clients
	activeClient := h.amiManager.GetActiveClient()
	allClients := h.amiManager.GetAllClients()

	h.mutex.Lock()
	defer h.mutex.Unlock()

	status := h.componentStatus["asterisk"]
	status.LastChecked = time.Now()

	// Update backends list if needed
	if len(status.Backends) != len(allClients) {
		status.Backends = make([]BackendHealth, len(allClients))
		for i, client := range allClients {
			status.Backends[i] = BackendHealth{
				Address:  client.GetAddress(),
				Status:   StatusUnhealthy,
				IsActive: client == activeClient,
			}
		}
	}

	// Check if we have any clients
	if len(allClients) == 0 {
		status.Status = StatusUnhealthy
		status.Message = "No AMI clients configured"
		h.logger.Warn("Asterisk check: no AMI clients configured")
		return
	}

	// Check active client
	if activeClient == nil {
		status.Status = StatusUnhealthy
		status.Message = "No active AMI client"
		h.logger.Warn("Asterisk check: no active AMI client")

		// Try to find any connected client
		for _, client := range allClients {
			if client.IsConnected() {
				h.logger.Info("Found connected client that is not active, suggesting failover",
					zap.String("address", client.GetAddress()))

				if h.failoverEnabled {
					h.logger.Warn("Initiating Asterisk failover/reconnect")
					h.triggerFailover("asterisk")
					status.Stats["failovers"]++
				}
				break
			}
		}

		return
	}

	// Update active client status
	isConnected := activeClient.IsConnected()
	hasBanner := activeClient.IsBannerReceived()

	h.logger.Debug("Active AMI client status",
		zap.String("address", activeClient.GetAddress()),
		zap.Bool("connected", isConnected),
		zap.Bool("banner_received", hasBanner))

	// Update backend statuses
	healthyCount := 0
	for i, client := range allClients {
		connected := client.IsConnected()
		banner := client.IsBannerReceived()
		isActive := (client == activeClient)

		// Update backend status
		backend := &status.Backends[i]
		backend.Address = client.GetAddress()
		backend.LastChecked = time.Now()
		backend.IsActive = isActive

		if connected && banner {
			backend.Status = StatusHealthy
			backend.Message = "AMI connected and banner received"
			backend.FailCount = 0
			healthyCount++
		} else if connected {
			backend.Status = StatusDegraded
			backend.Message = "AMI connected but no banner"
			backend.FailCount = 0
			healthyCount++
		} else {
			backend.FailCount++
			backend.Status = StatusUnhealthy
			backend.Message = fmt.Sprintf("AMI not connected (failures: %d)", backend.FailCount)
		}

		// Log the backend status
		h.logger.Debug("AMI backend status",
			zap.String("address", backend.Address),
			zap.String("status", string(backend.Status)),
			zap.Bool("is_active", backend.IsActive),
			zap.Int("fail_count", backend.FailCount))
	}

	// Update overall status
	if hasBanner {
		status.Status = StatusHealthy
		status.Message = "AMI banner received - Asterisk ready"
		status.Stats["success"]++
	} else if isConnected {
		status.Status = StatusDegraded
		status.Message = "AMI connected but waiting for banner"
		status.Stats["success"]++
	} else {
		status.Status = StatusUnhealthy
		status.Message = "Active AMI client not connected"
		status.Stats["failure"]++

		// If active client is unhealthy but other healthy clients exist, suggest failover
		if healthyCount > 0 && h.failoverEnabled {
			h.logger.Warn("Active AMI client unhealthy but other healthy clients available, triggering failover")
			h.triggerFailover("asterisk")
			status.Stats["failovers"]++
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

// GetFailoverHistory returns the failover history for a component
func (h *HealthMonitor) GetFailoverHistory(component string) []FailoverEvent {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	if history, exists := h.failoverHistory[component]; exists {
		// Return a copy to prevent race conditions
		result := make([]FailoverEvent, len(history))
		copy(result, history)
		return result
	}

	return nil
}

// GetAllFailoverHistory returns failover history for all components
func (h *HealthMonitor) GetAllFailoverHistory() map[string][]FailoverEvent {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	result := make(map[string][]FailoverEvent)
	for k, v := range h.failoverHistory {
		copy := make([]FailoverEvent, len(v))
		for i, event := range v {
			copy[i] = event
		}
		result[k] = copy
	}

	return result
}

// checkWebSocketBackends checks WebSocket backend health
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
