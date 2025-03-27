// pkg/api/status.go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/ami"
	"gateway/pkg/coordinator"
	"gateway/pkg/health"
	"gateway/pkg/rtpengine"
	"gateway/pkg/storage"
	"gateway/pkg/websocket"
)

// StatusHandler handles requests to the /status endpoint
type StatusHandler struct {
	amiManager    *ami.Manager
	rtpManager    *rtpengine.Manager
	wsServer      *websocket.Server
	coordinator   *coordinator.Coordinator
	healthMonitor *health.HealthMonitor
	storage       storage.StateStorage
	logger        *zap.Logger
	nodeID        string
	version       string
	startTime     time.Time
}

// NewStatusHandler creates a new status handler
func NewStatusHandler(
	amiManager *ami.Manager,
	rtpManager *rtpengine.Manager,
	wsServer *websocket.Server,
	coordinator *coordinator.Coordinator,
	healthMonitor *health.HealthMonitor,
	storage storage.StateStorage,
	logger *zap.Logger,
	nodeID string,
	version string,
) *StatusHandler {
	return &StatusHandler{
		amiManager:    amiManager,
		rtpManager:    rtpManager,
		wsServer:      wsServer,
		coordinator:   coordinator,
		healthMonitor: healthMonitor,
		storage:       storage,
		logger:        logger,
		nodeID:        nodeID,
		version:       version,
		startTime:     time.Now(),
	}
}

// ServeHTTP handles HTTP requests for the status endpoint
func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// If a specific component is requested, show only that component
	component := r.URL.Query().Get("component")
	format := r.URL.Query().Get("format")

	status := h.getStatus(component)

	// Set Content-Type header
	w.Header().Set("Content-Type", "application/json")

	// Set HTTP status code based on overall health
	if status.Status == "healthy" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	// Render response
	if format == "pretty" {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		encoder.Encode(status)
	} else {
		json.NewEncoder(w).Encode(status)
	}
}

// SystemStatus represents the full status of the system
type SystemStatus struct {
	Status            string                 `json:"status"`
	NodeID            string                 `json:"node_id"`
	Hostname          string                 `json:"hostname"`
	Version           string                 `json:"version"`
	UpTime            string                 `json:"uptime"`
	UpTimeSeconds     int64                  `json:"uptime_seconds"`
	Timestamp         string                 `json:"timestamp"`
	IsLeader          bool                   `json:"is_leader"`
	Components        map[string]interface{} `json:"components,omitempty"`
	AMI               interface{}            `json:"ami,omitempty"`
	RTPEngine         interface{}            `json:"rtpengine,omitempty"`
	WebSocket         interface{}            `json:"websocket,omitempty"`
	Storage           interface{}            `json:"storage,omitempty"`
	SystemInfo        map[string]interface{} `json:"system_info"`
	FailoverEvents    []interface{}          `json:"failover_events,omitempty"`
	ActiveConnections int                    `json:"active_connections"`
}

// getStatus returns the current system status
func (h *StatusHandler) getStatus(requestedComponent string) SystemStatus {
	hostname, _ := os.Hostname()

	// Gather component health
	componentHealth := make(map[string]interface{})
	if h.healthMonitor != nil {
		health := h.healthMonitor.GetAllComponentHealth()
		for name, componentStatus := range health {
			componentHealth[name] = componentStatus
		}
	}

	// Get overall system status
	status := "healthy"
	for _, comp := range componentHealth {
		if comp.(map[string]interface{})["status"] == "unhealthy" {
			status = "unhealthy"
			break
		}
	}

	// Get AIM status if available
	var amiStatus interface{}
	if requestedComponent == "" || requestedComponent == "ami" {
		if h.amiManager != nil {
			// Note: assuming the AMI manager has a GetStatus method
			// This would need to be implemented in your AMI manager
			// amiStatus = h.amiManager.GetStatus()
			amiStatus = map[string]interface{}{
				"status":            "OK", // Placeholder
				"clients_connected": true,
			}
		}
	}

	// Get RTPEngine status if available
	var rtpStatus interface{}
	if requestedComponent == "" || requestedComponent == "rtpengine" {
		if h.rtpManager != nil {
			// Note: assuming the RTPEngine manager has a GetStatus method
			// This would need to be implemented in your RTP manager
			// rtpStatus = h.rtpManager.GetStatus()
			rtpStatus = map[string]interface{}{
				"status":       "OK", // Placeholder
				"active_calls": 0,
			}
		}
	}

	// Get WebSocket status if available
	var wsStatus interface{}
	if requestedComponent == "" || requestedComponent == "websocket" {
		if h.wsServer != nil {
			// Note: assuming the WebSocket server has a GetStatus method
			// This would need to be implemented in your WebSocket server
			// wsStatus = h.wsServer.GetStatus()
			wsStatus = map[string]interface{}{
				"status":      "OK", // Placeholder
				"connections": 0,
			}
		}
	}

	// Get Storage status if available
	var storageStatus interface{}
	if requestedComponent == "" || requestedComponent == "storage" {
		if h.storage != nil {
			// Create a background context for storage operations
			ctx := context.Background()
			stats, err := h.storage.Stats(ctx)
			if err == nil {
				storageStatus = stats
			} else {
				storageStatus = map[string]interface{}{
					"status": "error",
					"error":  err.Error(),
				}
			}
		}
	}

	// Get failover events
	var failoverEvents []interface{}
	if h.storage != nil {
		// This is a placeholder - in a real implementation, you would retrieve
		// failover events from storage and add them here
		failoverEvents = []interface{}{}
	}

	// System info
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	systemInfo := map[string]interface{}{
		"num_goroutines": runtime.NumGoroutine(),
		"num_cpu":        runtime.NumCPU(),
		"memory_alloc":   mem.Alloc,
		"memory_sys":     mem.Sys,
		"gc_cycles":      mem.NumGC,
	}

	// Active connections
	activeConnections := 0
	if h.wsServer != nil {
		// Assuming there's a method to get the current connection count
		// activeConnections = h.wsServer.GetConnectionCount()
	}

	// Build the full status
	return SystemStatus{
		Status:            status,
		NodeID:            h.nodeID,
		Hostname:          hostname,
		Version:           h.version,
		UpTime:            time.Since(h.startTime).String(),
		UpTimeSeconds:     int64(time.Since(h.startTime).Seconds()),
		Timestamp:         time.Now().Format(time.RFC3339),
		IsLeader:          h.coordinator != nil && h.coordinator.IsLeader(),
		Components:        componentHealth,
		AMI:               amiStatus,
		RTPEngine:         rtpStatus,
		WebSocket:         wsStatus,
		Storage:           storageStatus,
		SystemInfo:        systemInfo,
		FailoverEvents:    failoverEvents,
		ActiveConnections: activeConnections,
	}
}

// RegisterStatusEndpoints registers the status endpoints with the provided HTTP server
func RegisterStatusEndpoints(
	mux *http.ServeMux,
	amiManager *ami.Manager,
	rtpManager *rtpengine.Manager,
	wsServer *websocket.Server,
	coordinator *coordinator.Coordinator,
	healthMonitor *health.HealthMonitor,
	storage storage.StateStorage,
	logger *zap.Logger,
	nodeID string,
	version string,
) {
	statusHandler := NewStatusHandler(
		amiManager,
		rtpManager,
		wsServer,
		coordinator,
		healthMonitor,
		storage,
		logger,
		nodeID,
		version,
	)

	// Register the main status endpoint
	mux.Handle("/status", statusHandler)

	// Register a simple health endpoint that returns 200/503 based on health
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := statusHandler.getStatus("")
		if status.Status == "healthy" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Service Unavailable"))
		}
	})
}
