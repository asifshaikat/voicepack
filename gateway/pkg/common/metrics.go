// pkg/common/metrics.go
package common

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// MetricsServer provides Prometheus metrics for the gateway
type MetricsServer struct {
	registry        *prometheus.Registry
	server          *http.Server
	collectors      []prometheus.Collector
	logger          *zap.Logger
	callsActive     prometheus.Gauge
	callsTotal      prometheus.Counter
	callDuration    prometheus.Histogram
	errors          *prometheus.CounterVec
	rtpSessions     prometheus.Gauge
	sipTransactions prometheus.Gauge
	mu              sync.Mutex
}

// NewMetricsServer creates a new metrics server
func NewMetricsServer(addr string, logger *zap.Logger) (*MetricsServer, error) {
	if logger == nil {
		// Create default logger
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	registry := prometheus.NewRegistry()

	// Create metrics
	callsActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_calls_active",
		Help: "The number of currently active calls",
	})

	callsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gateway_calls_total",
		Help: "The total number of calls processed",
	})

	callDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_call_duration_seconds",
		Help:    "The duration of calls in seconds",
		Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10s, 20s, 40s, ...
	})

	errors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_errors_total",
		Help: "The total number of errors by type",
	}, []string{"type"})

	rtpSessions := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_rtp_sessions",
		Help: "The number of active RTP sessions",
	})

	sipTransactions := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_sip_transactions",
		Help: "The number of active SIP transactions",
	})

	// Register metrics
	registry.MustRegister(callsActive)
	registry.MustRegister(callsTotal)
	registry.MustRegister(callDuration)
	registry.MustRegister(errors)
	registry.MustRegister(rtpSessions)
	registry.MustRegister(sipTransactions)

	// Register Go metrics
	registry.MustRegister(prometheus.NewGoCollector())
	registry.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	// Create HTTP server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return &MetricsServer{
		registry:        registry,
		server:          server,
		logger:          logger,
		callsActive:     callsActive,
		callsTotal:      callsTotal,
		callDuration:    callDuration,
		errors:          errors,
		rtpSessions:     rtpSessions,
		sipTransactions: sipTransactions,
	}, nil
}

// Start starts the metrics server
func (m *MetricsServer) Start(ctx context.Context) error {
	// Start HTTP server in a goroutine
	go func() {
		m.logger.Info("Starting metrics server", zap.String("addr", m.server.Addr))

		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			m.logger.Error("Metrics server failed", zap.Error(err))
		}
	}()

	// Wait for context to be done
	<-ctx.Done()

	// Shutdown HTTP server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.logger.Info("Shutting down metrics server")
	return m.server.Shutdown(shutdownCtx)
}

// RegisterCollector adds a custom Prometheus collector
func (m *MetricsServer) RegisterCollector(collector prometheus.Collector) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.registry.MustRegister(collector)
	m.collectors = append(m.collectors, collector)
}

// IncrementCallsActive increments the active calls counter
func (m *MetricsServer) IncrementCallsActive() {
	m.callsActive.Inc()
}

// DecrementCallsActive decrements the active calls counter
func (m *MetricsServer) DecrementCallsActive() {
	m.callsActive.Dec()
}

// IncrementCallsTotal increments the total calls counter
func (m *MetricsServer) IncrementCallsTotal() {
	m.callsTotal.Inc()
}

// ObserveCallDuration records a call duration
func (m *MetricsServer) ObserveCallDuration(duration time.Duration) {
	m.callDuration.Observe(duration.Seconds())
}

// IncrementError increments an error counter
func (m *MetricsServer) IncrementError(errorType string) {
	m.errors.WithLabelValues(errorType).Inc()
}

// SetRTPSessions sets the number of active RTP sessions
func (m *MetricsServer) SetRTPSessions(count int) {
	m.rtpSessions.Set(float64(count))
}

// SetSIPTransactions sets the number of active SIP transactions
func (m *MetricsServer) SetSIPTransactions(count int) {
	m.sipTransactions.Set(float64(count))
}
