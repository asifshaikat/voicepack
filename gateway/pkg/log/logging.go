// pkg/log/logging.go
package log

import (
	"context"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Standard log levels
const (
	DebugLevel = zapcore.DebugLevel
	InfoLevel  = zapcore.InfoLevel
	WarnLevel  = zapcore.WarnLevel
	ErrorLevel = zapcore.ErrorLevel
	FatalLevel = zapcore.FatalLevel
)

// HA Event types
const (
	EventFailover            = "failover"
	EventLeadershipChange    = "leadership_change"
	EventHealthChanged       = "health_changed"
	EventCircuitBreakerState = "circuit_breaker_state"
	EventBackendConnected    = "backend_connected"
	EventBackendDisconnected = "backend_disconnected"
)

// Component types
const (
	ComponentAMI       = "ami"
	ComponentRTPEngine = "rtpengine"
	ComponentWebSocket = "websocket"
	ComponentStorage   = "storage"
	ComponentConfig    = "config"
	ComponentHealth    = "health"
)

// Logger wraps zap.Logger to provide standardized logging
type Logger struct {
	*zap.Logger
	nodeID  string
	version string
}

// Config holds configuration for the logger
type Config struct {
	Development bool
	Level       zapcore.Level
	NodeID      string
	Version     string
}

// NewLogger creates a new Logger with the given configuration
func NewLogger(config Config) (*Logger, error) {
	var zapConfig zap.Config
	if config.Development {
		zapConfig = zap.NewDevelopmentConfig()
	} else {
		zapConfig = zap.NewProductionConfig()
	}

	zapConfig.Level = zap.NewAtomicLevelAt(config.Level)

	zapLogger, err := zapConfig.Build()
	if err != nil {
		return nil, err
	}

	return &Logger{
		Logger:  zapLogger,
		nodeID:  config.NodeID,
		version: config.Version,
	}, nil
}

// With creates a child logger with the given zap fields.
func (l *Logger) With(fields ...zap.Field) *Logger {
	return &Logger{
		Logger:  l.Logger.With(fields...),
		nodeID:  l.nodeID,
		version: l.version,
	}
}

// LogFailoverEvent logs a failover event with standardized fields
func (l *Logger) LogFailoverEvent(ctx context.Context, component string, fromBackend, toBackend string, reason string, duration time.Duration) {
	l.Info("Failover event",
		zap.String("event_type", EventFailover),
		zap.String("component", component),
		zap.String("from_backend", fromBackend),
		zap.String("to_backend", toBackend),
		zap.String("reason", reason),
		zap.Duration("duration", duration),
		zap.String("node_id", l.nodeID),
	)
}

// LogLeadershipChange logs a leadership change event
func (l *Logger) LogLeadershipChange(ctx context.Context, isLeader bool, term int64) {
	l.Info("Leadership change",
		zap.String("event_type", EventLeadershipChange),
		zap.Bool("is_leader", isLeader),
		zap.Int64("term", term),
		zap.String("node_id", l.nodeID),
	)
}

// LogHealthChange logs a health status change event
func (l *Logger) LogHealthChange(ctx context.Context, component string, backend string, isHealthy bool, failCount int) {
	level := InfoLevel
	if !isHealthy {
		level = WarnLevel
	}

	l.Log(level, "Health status changed",
		zap.String("event_type", EventHealthChanged),
		zap.String("component", component),
		zap.String("backend", backend),
		zap.Bool("is_healthy", isHealthy),
		zap.Int("fail_count", failCount),
		zap.String("node_id", l.nodeID),
	)
}

// LogCircuitBreakerStateChange logs a circuit breaker state change
func (l *Logger) LogCircuitBreakerStateChange(ctx context.Context, component string, backend string, state string, reason string) {
	l.Info("Circuit breaker state changed",
		zap.String("event_type", EventCircuitBreakerState),
		zap.String("component", component),
		zap.String("backend", backend),
		zap.String("state", state),
		zap.String("reason", reason),
		zap.String("node_id", l.nodeID),
	)
}

// LogBackendConnection logs a backend connection or disconnection event
func (l *Logger) LogBackendConnection(ctx context.Context, component string, backend string, isConnected bool, reason string) {
	eventType := EventBackendConnected
	if !isConnected {
		eventType = EventBackendDisconnected
	}

	l.Info("Backend connection status changed",
		zap.String("event_type", eventType),
		zap.String("component", component),
		zap.String("backend", backend),
		zap.Bool("connected", isConnected),
		zap.String("reason", reason),
		zap.String("node_id", l.nodeID),
	)
}

// Log logs a message at the specified level
func (l *Logger) Log(level zapcore.Level, msg string, fields ...zap.Field) {
	switch level {
	case DebugLevel:
		l.Debug(msg, fields...)
	case InfoLevel:
		l.Info(msg, fields...)
	case WarnLevel:
		l.Warn(msg, fields...)
	case ErrorLevel:
		l.Error(msg, fields...)
	case FatalLevel:
		l.Fatal(msg, fields...)
	default:
		l.Info(msg, fields...)
	}
}
