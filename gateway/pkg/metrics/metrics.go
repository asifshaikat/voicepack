// pkg/metrics/metrics.go
package metrics

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

var (
	// Singleton metrics registry
	registry    *prometheus.Registry
	metricsOnce sync.Once

	// Leadership metrics
	leaderStatus *prometheus.GaugeVec

	// Backend health metrics
	backendHealth *prometheus.GaugeVec
	backendActive *prometheus.GaugeVec

	// Failover metrics
	failoverEvents *prometheus.CounterVec

	// Circuit breaker metrics
	circuitBreakerState   *prometheus.GaugeVec
	circuitBreakerChanges *prometheus.CounterVec

	// Connection metrics
	connectionCount *prometheus.GaugeVec

	// Storage metrics
	storageOperations    *prometheus.CounterVec
	storageErrors        *prometheus.CounterVec
	storageOperationTime *prometheus.HistogramVec

	// Global nodeID and version
	globalNodeID  string
	globalVersion string
)

// InitMetrics initializes the Prometheus metrics
func InitMetrics(version, nodeID string) {
	globalVersion = version
	globalNodeID = nodeID

	metricsOnce.Do(func() {
		registry = prometheus.NewRegistry()

		// Leadership metrics
		leaderStatus = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_coordinator_leader_status",
				Help: "Leadership status (1 if leader, 0 if follower)",
			},
			[]string{"component", "node_id"},
		)
		registry.MustRegister(leaderStatus)

		// Backend health metrics
		backendHealth = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_backend_health",
				Help: "Health status of backend (1 if healthy, 0 if unhealthy)",
			},
			[]string{"type", "address", "node_id"},
		)
		registry.MustRegister(backendHealth)

		backendActive = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_backend_active",
				Help: "Whether the backend is active (1) or standby (0)",
			},
			[]string{"type", "address", "node_id"},
		)
		registry.MustRegister(backendActive)

		// Failover metrics
		failoverEvents = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_failover_total",
				Help: "Number of failover events",
			},
			[]string{"component", "from", "to", "reason", "node_id"},
		)
		registry.MustRegister(failoverEvents)

		// Circuit breaker metrics
		circuitBreakerState = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_circuitbreaker_state",
				Help: "Circuit breaker state (0=Closed, 1=Open, 2=HalfOpen)",
			},
			[]string{"name", "node_id"},
		)
		registry.MustRegister(circuitBreakerState)

		circuitBreakerChanges = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_circuitbreaker_changes_total",
				Help: "Number of circuit breaker state changes",
			},
			[]string{"name", "from_state", "to_state", "node_id"},
		)
		registry.MustRegister(circuitBreakerChanges)

		// Connection metrics
		connectionCount = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_connections",
				Help: "Number of active connections",
			},
			[]string{"type", "node_id"},
		)
		registry.MustRegister(connectionCount)

		// Storage metrics
		storageOperations = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_storage_operations_total",
				Help: "Number of storage operations",
			},
			[]string{"operation", "type", "node_id"},
		)
		registry.MustRegister(storageOperations)

		storageErrors = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_storage_errors_total",
				Help: "Number of storage errors",
			},
			[]string{"operation", "type", "node_id"},
		)
		registry.MustRegister(storageErrors)

		storageOperationTime = prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "gateway_storage_operation_seconds",
				Help:    "Duration of storage operations",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms to ~1s
			},
			[]string{"operation", "type", "node_id"},
		)
		registry.MustRegister(storageOperationTime)
	})
}

// StartMetricsServer starts an HTTP server to expose Prometheus metrics
func StartMetricsServer(addr string, logger *zap.Logger) *http.Server {
	// InitMetrics should have been called before this

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Health endpoint that just returns 200 OK
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Status endpoint that provides a JSON summary of all components
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// This will be populated with actual status data by the main application
		// For now, we just provide a placeholder
		w.Write([]byte(`{"status":"active", "node_id":"` + globalNodeID + `", "timestamp":"` + time.Now().Format(time.RFC3339) + `"}`))
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		logger.Info("Starting metrics server", zap.String("addr", addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Metrics server error", zap.Error(err))
		}
	}()

	return server
}

// Shutdown stops the metrics server
func Shutdown(ctx context.Context, server *http.Server, logger *zap.Logger) {
	logger.Info("Shutting down metrics server")
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Error shutting down metrics server", zap.Error(err))
	}
}

// SetLeaderStatus updates the leader status metric
func SetLeaderStatus(isLeader bool) {
	component := "gateway"
	value := 0.0
	if isLeader {
		value = 1.0
	}
	leaderStatus.WithLabelValues(component, globalNodeID).Set(value)
}

// SetBackendHealth updates the backend health metric
func SetBackendHealth(backendType, address, nodeID string, isHealthy bool) {
	value := 0.0
	if isHealthy {
		value = 1.0
	}
	backendHealth.WithLabelValues(backendType, address, nodeID).Set(value)
}

// SetBackendActive updates the backend active metric
func SetBackendActive(backendType, address, nodeID string, isActive bool) {
	value := 0.0
	if isActive {
		value = 1.0
	}
	backendActive.WithLabelValues(backendType, address, nodeID).Set(value)
}

// RecordFailoverEvent increments the failover counter
func RecordFailoverEvent(component, from, to, reason, nodeID string) {
	failoverEvents.WithLabelValues(component, from, to, reason, nodeID).Inc()
}

// SetCircuitBreakerState updates the circuit breaker state metric
func SetCircuitBreakerState(name, nodeID string, state int) {
	circuitBreakerState.WithLabelValues(name, nodeID).Set(float64(state))
}

// RecordCircuitBreakerChange increments the circuit breaker change counter
func RecordCircuitBreakerChange(name, fromState, toState, nodeID string) {
	circuitBreakerChanges.WithLabelValues(name, fromState, toState, nodeID).Inc()
}

// SetConnectionCount updates the connection count metric
func SetConnectionCount(connectionType, nodeID string, count int) {
	connectionCount.WithLabelValues(connectionType, nodeID).Set(float64(count))
}

// RecordStorageOperation increments the storage operation counter
func RecordStorageOperation(operation, storageType, nodeID string) {
	storageOperations.WithLabelValues(operation, storageType, nodeID).Inc()
}

// RecordStorageError increments the storage error counter
func RecordStorageError(operation, storageType, nodeID string) {
	storageErrors.WithLabelValues(operation, storageType, nodeID).Inc()
}

// ObserveStorageOperationDuration records the duration of a storage operation
func ObserveStorageOperationDuration(operation, storageType, nodeID string, duration time.Duration) {
	storageOperationTime.WithLabelValues(operation, storageType, nodeID).Observe(duration.Seconds())
}
