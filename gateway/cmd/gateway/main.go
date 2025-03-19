// cmd/gateway/main.go
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"gateway/pkg/ami"
	"gateway/pkg/config"
	"gateway/pkg/rtpengine"
	"gateway/pkg/sip"
	"gateway/pkg/storage"
	"gateway/pkg/websocket"
)

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Setup logger
	logger := setupLogger(cfg.LogLevel)
	defer logger.Sync()

	// Create storage
	var stateStorage storage.StateStorage
	if cfg.Redis.Enabled {
		// Create Redis storage
		stateStorage, err = storage.NewRedisStorage(cfg.Redis, logger)
	} else {
		// Create in-memory storage
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	}

	if err != nil {
		logger.Fatal("Failed to create storage", zap.Error(err))
	}

	// Create coordinator for high availability
	coordinator, err := common.NewCoordinator(stateStorage, common.CoordinatorConfig{
		HeartbeatInterval: 5 * time.Second,
		LeaseTimeout:      15 * time.Second,
	}, logger)
	if err != nil {
		logger.Fatal("Failed to create coordinator", zap.Error(err))
	}

	// Start the coordinator
	if err := coordinator.Start(ctx); err != nil {
		logger.Fatal("Failed to start coordinator", zap.Error(err))
	}
	defer coordinator.Stop()

	// Register components for leadership
	coordinator.RegisterComponentLeadership("rtpengine")
	coordinator.RegisterComponentLeadership("ami")
	coordinator.RegisterComponentLeadership("sip")
	coordinator.RegisterComponentLeadership("websocket")
	// Create RTPEngine manager
	rtpManager, err := rtpengine.NewManager(cfg.RTPEngine, logger, stateStorage)
	if err != nil {
		logger.Fatal("Failed to create RTPEngine manager", zap.Error(err))
	}

	// Create AMI manager
	amiManager, err := ami.NewManager(cfg.Asterisk, logger, stateStorage)
	if err != nil {
		logger.Fatal("Failed to create AMI manager", zap.Error(err))
	}

	// Create SIP proxy
	sipProxy, err := sip.NewProxy(cfg.SIP, stateStorage, rtpManager, logger)
	if err != nil {
		logger.Fatal("Failed to create SIP proxy", zap.Error(err))
	}

	// Create UDP transport for SIP
	udpTransport, err := sip.NewUDPTransport(cfg.SIP.UDPBindAddr, logger)
	if err != nil {
		logger.Fatal("Failed to create UDP transport", zap.Error(err))
	}

	// Add transport to proxy
	sipProxy.AddTransport(udpTransport)

	// Create WebSocket server
	wsServer, err := websocket.NewServer(cfg.WebSocket, stateStorage, logger)
	if err != nil {
		logger.Fatal("Failed to create WebSocket server", zap.Error(err))
	}

	// Set SIP handler for WebSocket
	wsServer.SetSIPHandler(sipProxy)

	// Create context for coordinated shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start components
	logger.Info("Starting WebRTC-SIP Gateway...")

	// Start RTPEngine manager
	rtpManager.Start(ctx)

	// Start AMI manager
	if err := amiManager.Start(ctx); err != nil {
		logger.Fatal("Failed to start AMI manager", zap.Error(err))
	}

	// Start SIP proxy
	if err := sipProxy.Start(ctx); err != nil {
		logger.Fatal("Failed to start SIP proxy", zap.Error(err))
	}

	// Start WebSocket server
	if err := wsServer.Start(ctx); err != nil {
		logger.Fatal("Failed to start WebSocket server", zap.Error(err))
	}

	logger.Info("WebRTC-SIP Gateway started successfully")

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for shutdown signal
	sig := <-sigCh
	logger.Info("Shutdown signal received", zap.String("signal", sig.String()))

	// Cancel context to initiate coordinated shutdown
	cancel()

	// Graceful shutdown
	logger.Info("Shutting down components...")

	// Shutdown components in reverse order
	shutdownTimeout := time.Duration(cfg.ShutdownWaitSeconds) * time.Second

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// Stop WebSocket server
	if err := wsServer.Stop(); err != nil {
		logger.Error("Failed to stop WebSocket server", zap.Error(err))
	}

	// Stop SIP proxy
	if err := sipProxy.Stop(); err != nil {
		logger.Error("Failed to stop SIP proxy", zap.Error(err))
	}

	// Stop AMI manager
	amiManager.Shutdown()

	// Close storage
	if err := stateStorage.Close(); err != nil {
		logger.Error("Failed to close storage", zap.Error(err))
	}

	logger.Info("Shutdown complete")
}

func setupLogger(level string) *zap.Logger {
	var logLevel zapcore.Level

	switch level {
	case "debug":
		logLevel = zapcore.DebugLevel
	case "info":
		logLevel = zapcore.InfoLevel
	case "warn":
		logLevel = zapcore.WarnLevel
	case "error":
		logLevel = zapcore.ErrorLevel
	default:
		logLevel = zapcore.InfoLevel
	}

	config := zap.Config{
		Level:            zap.NewAtomicLevelAt(logLevel),
		Encoding:         "json",
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
		EncoderConfig: zapcore.EncoderConfig{
			MessageKey:     "message",
			LevelKey:       "level",
			TimeKey:        "time",
			NameKey:        "logger",
			CallerKey:      "caller",
			FunctionKey:    zapcore.OmitKey,
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
	}

	logger, _ := config.Build()
	return logger
}
