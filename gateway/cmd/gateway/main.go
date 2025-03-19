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
	"gateway/pkg/common"
	"gateway/pkg/config"
	"gateway/pkg/coordinator"
	"gateway/pkg/rtpengine"
	"gateway/pkg/sip"
	"gateway/pkg/storage"
	"gateway/pkg/websocket"
)

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration from YAML file
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Setup logger
	logger := setupLogger(cfg.LogLevel)
	defer logger.Sync()
	sipCfg := sip.SIPConfig{
		UDPBindAddr:    cfg.SIP.UDPBindAddr,
		ProxyURI:       cfg.SIP.ProxyURI,
		DefaultNextHop: cfg.SIP.DefaultNextHop,
		MaxForwards:    cfg.SIP.MaxForwards,
		UserAgent:      cfg.SIP.UserAgent,
	}

	// Create storage
	var stateStorage storage.StateStorage
	if cfg.Redis.Enabled {
		// Uncomment and implement if Redis storage is available:
		// stateStorage, err = storage.NewRedisStorage(cfg.Redis, logger)
		// if err != nil {
		//     logger.Fatal("Failed to create Redis storage", zap.Error(err))
		// }
		logger.Info("Redis storage not implemented; using in-memory storage")
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	} else {
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	}
	if err != nil {
		logger.Fatal("Failed to create storage", zap.Error(err))
	}

	// Create a background context for coordinator registration
	ctx := context.Background()

	// Create coordinator using configuration getters for heartbeat and lease timeouts
	coordinator, err := coordinator.NewCoordinator(stateStorage, coordinator.CoordinatorConfig{
		HeartbeatInterval: cfg.GetHeartbeatInterval(),
		LeaseTimeout:      cfg.GetLeaseTimeout(),
	}, logger)
	if err != nil {
		logger.Fatal("Failed to create coordinator", zap.Error(err))
	}
	if err := coordinator.Start(ctx); err != nil {
		logger.Fatal("Failed to start coordinator", zap.Error(err))
	}
	defer coordinator.Stop()

	// Register components for leadership
	coordinator.RegisterComponentLeadership("rtpengine")
	coordinator.RegisterComponentLeadership("ami")
	coordinator.RegisterComponentLeadership("sip")
	coordinator.RegisterComponentLeadership("websocket")

	// Create RTPEngine manager using the converted configuration
	rtpManager, err := rtpengine.NewManager(cfg.ToRTPEngineManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	if err != nil {
		logger.Fatal("Failed to create RTPEngine manager", zap.Error(err))
	}

	// Create AMI manager using the converted configuration
	amiManager, err := ami.NewManager(cfg.ToAsteriskManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	if err != nil {
		logger.Fatal("Failed to create AMI manager", zap.Error(err))
	}

	// Create SIP proxy (make sure sip.NewProxy is exported from your SIP package)
	sipProxy, err := sip.NewProxy(sipCfg, stateStorage, rtpManager, logger)
	if err != nil {
		logger.Fatal("Failed to create SIP proxy", zap.Error(err))
	}

	// Create UDP transport for SIP (ensure sip.NewUDPTransport is exported)
	udpTransport, err := sip.NewUDPTransport(cfg.SIP.UDPBindAddr, logger)
	if err != nil {
		logger.Fatal("Failed to create UDP transport", zap.Error(err))
	}
	sipProxy.AddTransport(udpTransport)

	// Convert WebSocket configuration from config.WebSocketConfig using getters
	wsConfig := websocket.ServerConfig{
		BindAddr:       cfg.WebSocket.BindAddr,
		CertFile:       cfg.WebSocket.CertFile,
		KeyFile:        cfg.WebSocket.KeyFile,
		MaxConnections: cfg.WebSocket.MaxConnections,
		ReadTimeout:    cfg.GetWebSocketReadTimeout(),
		WriteTimeout:   cfg.GetWebSocketWriteTimeout(),
		IdleTimeout:    cfg.GetWebSocketIdleTimeout(),
		EnableIPv4Only: cfg.WebSocket.EnableIPv4Only,
		ServerName:     cfg.WebSocket.ServerName,
	}

	// Create WebSocket server
	wsServer, err := websocket.NewServer(wsConfig, stateStorage, logger)
	if err != nil {
		logger.Fatal("Failed to create WebSocket server", zap.Error(err))
	}
	// Set SIP handler for WebSocket (assuming SIP proxy implements the SIPHandler interface)
	wsServer.SetSIPHandler(sipProxy)

	// Create a context for coordinated shutdown
	shutdownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start components
	logger.Info("Starting WebRTC-SIP Gateway...")

	rtpManager.Start(shutdownCtx)
	if err := amiManager.Start(shutdownCtx); err != nil {
		logger.Fatal("Failed to start AMI manager", zap.Error(err))
	}
	if err := sipProxy.Start(shutdownCtx); err != nil {
		logger.Fatal("Failed to start SIP proxy", zap.Error(err))
	}
	if err := wsServer.Start(shutdownCtx); err != nil {
		logger.Fatal("Failed to start WebSocket server", zap.Error(err))
	}

	logger.Info("WebRTC-SIP Gateway started successfully")

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("Shutdown signal received", zap.String("signal", sig.String()))
	cancel()

	logger.Info("Shutting down components...")

	// Shutdown components in reverse order
	if err := wsServer.Stop(); err != nil {
		logger.Error("Failed to stop WebSocket server", zap.Error(err))
	}
	if err := sipProxy.Stop(); err != nil {
		logger.Error("Failed to stop SIP proxy", zap.Error(err))
	}
	amiManager.Shutdown()
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
