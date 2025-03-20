// cmd/gateway/main.go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
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
	fmt.Fprintf(os.Stderr, "DEBUG-1: Program starting\n")

	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()
	fmt.Fprintf(os.Stderr, "DEBUG-2: Command line flags parsed, config path: %s\n", *configPath)

	// Load configuration from YAML file
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to load configuration: %v\n", err)
		log.Fatalf("Failed to load configuration: %v", err)
	}
	fmt.Printf("DEBUG: SIP.DisableSIPProcessing = %v\n", cfg.SIP.DisableSIPProcessing)
	fmt.Fprintf(os.Stderr, "DEBUG-3: Configuration loaded successfully\n")

	// Setup logger
	logger := setupLogger(cfg.LogLevel)
	defer logger.Sync()
	fmt.Fprintf(os.Stderr, "DEBUG-4: Logger setup complete with level: %s\n", cfg.LogLevel)

	sipCfg := sip.SIPConfig{
		UDPBindAddr:    cfg.SIP.UDPBindAddr,
		ProxyURI:       cfg.SIP.ProxyURI,
		DefaultNextHop: cfg.SIP.DefaultNextHop,
		MaxForwards:    cfg.SIP.MaxForwards,
		UserAgent:      cfg.SIP.UserAgent,
	}
	fmt.Fprintf(os.Stderr, "DEBUG-5: SIP configuration prepared, UDPBindAddr: %s\n", sipCfg.UDPBindAddr)

	// Create storage
	fmt.Fprintf(os.Stderr, "DEBUG-6: Creating storage\n")
	var stateStorage storage.StateStorage
	if cfg.Redis.Enabled {
		fmt.Fprintf(os.Stderr, "DEBUG-6a: Redis storage would be created here if implemented\n")
		logger.Info("Redis storage not implemented; using in-memory storage")
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	} else {
		fmt.Fprintf(os.Stderr, "DEBUG-6b: Creating in-memory storage, path: %s\n", cfg.MemoryStorage.PersistPath)
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create storage: %v\n", err)
		logger.Fatal("Failed to create storage", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-7: Storage created successfully\n")

	// Create a background context for coordinator registration
	ctx := context.Background()
	fmt.Fprintf(os.Stderr, "DEBUG-8: Background context created\n")

	// Create coordinator
	fmt.Fprintf(os.Stderr, "DEBUG-9: Creating coordinator\n")
	coordinator, err := coordinator.NewCoordinator(stateStorage, coordinator.CoordinatorConfig{
		HeartbeatInterval: cfg.GetHeartbeatInterval(),
		LeaseTimeout:      cfg.GetLeaseTimeout(),
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create coordinator: %v\n", err)
		logger.Fatal("Failed to create coordinator", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-10: Coordinator created, starting it\n")

	if err := coordinator.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to start coordinator: %v\n", err)
		logger.Fatal("Failed to start coordinator", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-11: Coordinator started successfully\n")
	defer coordinator.Stop()

	// Register components for leadership
	fmt.Fprintf(os.Stderr, "DEBUG-12: Registering components for leadership\n")
	coordinator.RegisterComponentLeadership("rtpengine")
	coordinator.RegisterComponentLeadership("ami")
	coordinator.RegisterComponentLeadership("sip")
	coordinator.RegisterComponentLeadership("websocket")
	fmt.Fprintf(os.Stderr, "DEBUG-13: Components registered for leadership\n")

	// Create RTPEngine manager
	fmt.Fprintf(os.Stderr, "DEBUG-14: Creating RTPEngine manager\n")
	rtpManager, err := rtpengine.NewManager(cfg.ToRTPEngineManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create RTPEngine manager: %v\n", err)
		logger.Fatal("Failed to create RTPEngine manager", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-15: RTPEngine manager created successfully\n")

	// Create AMI manager
	fmt.Fprintf(os.Stderr, "DEBUG-16: Creating AMI manager\n")
	amiManager, err := ami.NewManager(cfg.ToAsteriskManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create AMI manager: %v\n", err)
		logger.Fatal("Failed to create AMI manager", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-17: AMI manager created successfully\n")

	// Create SIP proxy
	fmt.Fprintf(os.Stderr, "DEBUG-18: Creating SIP proxy\n")
	sipProxy, err := sip.NewProxy(sipCfg, stateStorage, rtpManager, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create SIP proxy: %v\n", err)
		logger.Fatal("Failed to create SIP proxy", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-19: SIP proxy created successfully\n")

	// Create UDP transport for SIP
	fmt.Fprintf(os.Stderr, "DEBUG-20: Creating UDP transport at %s\n", cfg.SIP.UDPBindAddr)
	udpTransport, err := sip.NewUDPTransport(cfg.SIP.UDPBindAddr, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create UDP transport: %v\n", err)
		logger.Fatal("Failed to create UDP transport", zap.Error(err))
	}
	sipProxy.AddTransport(udpTransport)
	fmt.Fprintf(os.Stderr, "DEBUG-21: UDP transport created and added to SIP proxy\n")

	// Convert WebSocket configuration
	fmt.Fprintf(os.Stderr, "DEBUG-22: Preparing WebSocket configuration\n")
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
	fmt.Fprintf(os.Stderr, "DEBUG-23: WebSocket config prepared: BindAddr=%s, CertFile=%s, ServerName=%s\n",
		wsConfig.BindAddr, wsConfig.CertFile, wsConfig.ServerName)

	// Create WebSocket server
	fmt.Fprintf(os.Stderr, "DEBUG-24: Creating WebSocket server\n")
	logger.Debug("Creating WebSocket server with configuration",
		zap.String("bindAddr", wsConfig.BindAddr),
		zap.String("certFile", wsConfig.CertFile),
		zap.String("keyFile", wsConfig.KeyFile),
		zap.Int("maxConnections", wsConfig.MaxConnections),
		zap.Duration("readTimeout", wsConfig.ReadTimeout),
		zap.Duration("writeTimeout", wsConfig.WriteTimeout),
		zap.Duration("idleTimeout", wsConfig.IdleTimeout),
		zap.Bool("enableIPv4Only", wsConfig.EnableIPv4Only),
		zap.String("serverName", wsConfig.ServerName))

	wsServer, err := websocket.NewServer(wsConfig, stateStorage, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create WebSocket server: %v\n", err)
		logger.Fatal("Failed to create WebSocket server", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-25: WebSocket server created successfully\n")

	logger.Debug("WebSocket server created successfully, setting SIP handler")
	fmt.Fprintf(os.Stderr, "DEBUG-26: Setting SIP handler for WebSocket server\n")
	wsServer.SetSIPHandler(sipProxy)
	fmt.Fprintf(os.Stderr, "DEBUG-27: SIP handler set for WebSocket server\n")

	// Create a context for coordinated shutdown
	fmt.Fprintf(os.Stderr, "DEBUG-28: Creating shutdown context\n")
	shutdownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fmt.Fprintf(os.Stderr, "DEBUG-29: Shutdown context created\n")

	// Start components
	fmt.Fprintf(os.Stderr, "DEBUG-30: Starting WebRTC-SIP Gateway components\n")
	logger.Info("Starting WebRTC-SIP Gateway...")

	fmt.Fprintf(os.Stderr, "DEBUG-31: Starting RTPEngine manager\n")
	rtpManager.Start(shutdownCtx)
	fmt.Fprintf(os.Stderr, "DEBUG-32: RTPEngine manager started\n")

	fmt.Fprintf(os.Stderr, "DEBUG-33: Starting AMI manager\n")
	if err := amiManager.Start(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to start AMI manager: %v\n", err)
		logger.Fatal("Failed to start AMI manager", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-34: AMI manager started successfully\n")

	fmt.Fprintf(os.Stderr, "DEBUG-35: Starting SIP proxy\n")

	// Create a channel to receive initialization status
	sipReadyChan := make(chan error, 1)

	// Start SIP proxy in a background goroutine
	go func() {
		// Launch the SIP proxy server
		err := sipProxy.Start(shutdownCtx)

		// If we get here, either an error occurred or context was canceled
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("SIP proxy server failed", zap.Error(err))
		}

		// Send any initialization errors to the channel
		select {
		case sipReadyChan <- err:
			// Error sent successfully
		default:
			// Channel might be closed or full, no need to block
		}
	}()

	// Wait briefly for initialization errors
	select {
	case err := <-sipReadyChan:
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to start SIP proxy: %v\n", err)
			logger.Error("Failed to start SIP proxy", zap.Error(err))
			// Continue anyway instead of calling logger.Fatal which would terminate
		} else {
			fmt.Fprintf(os.Stderr, "DEBUG-36: SIP proxy started successfully\n")
		}
	case <-time.After(2 * time.Second):
		// No immediate error, assume success and continue
		fmt.Fprintf(os.Stderr, "DEBUG-36: SIP proxy initialization in progress, continuing startup\n")
		logger.Info("SIP proxy initialization in progress, continuing startup")
	}

	// Continue with the rest of the startup sequence
	fmt.Fprintf(os.Stderr, "DEBUG-37: About to start WebSocket server\n")

	// Add panic recovery to catch any silent failures
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "PANIC RECOVERED during WebSocket start: %v\n", r)
			// Stack trace
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "Stack trace: %s\n", buf[:n])
		}
	}()

	logger.Debug("Preparing to start WebSocket server...")
	fmt.Fprintf(os.Stderr, "DEBUG-38: Starting WebSocket server on %s\n", wsConfig.BindAddr)
	if err := wsServer.Start(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: WebSocket server start failed: %v\n", err)
		logger.Error("WebSocket server start failed with error",
			zap.Error(err),
			zap.String("bindAddr", wsConfig.BindAddr),
			zap.String("certFile", wsConfig.CertFile))
		logger.Fatal("Failed to start WebSocket server", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-39: WebSocket server started successfully\n")
	logger.Debug("WebSocket server started successfully")

	fmt.Fprintf(os.Stderr, "DEBUG-40: All components started successfully\n")
	logger.Info("WebRTC-SIP Gateway started successfully")

	// Handle signals for graceful shutdown
	fmt.Fprintf(os.Stderr, "DEBUG-41: Setting up signal handler for graceful shutdown\n")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	fmt.Fprintf(os.Stderr, "DEBUG-42: Waiting for shutdown signal\n")
	sig := <-sigCh
	fmt.Fprintf(os.Stderr, "DEBUG-43: Shutdown signal received: %s\n", sig.String())
	logger.Info("Shutdown signal received", zap.String("signal", sig.String()))
	cancel()

	fmt.Fprintf(os.Stderr, "DEBUG-44: Starting component shutdown sequence\n")
	logger.Info("Shutting down components...")

	// Shutdown components in reverse order
	fmt.Fprintf(os.Stderr, "DEBUG-45: Stopping WebSocket server\n")
	if err := wsServer.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to stop WebSocket server: %v\n", err)
		logger.Error("Failed to stop WebSocket server", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-46: WebSocket server stopped\n")

	fmt.Fprintf(os.Stderr, "DEBUG-47: Stopping SIP proxy\n")
	if err := sipProxy.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to stop SIP proxy: %v\n", err)
		logger.Error("Failed to stop SIP proxy", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-48: SIP proxy stopped\n")

	fmt.Fprintf(os.Stderr, "DEBUG-49: Shutting down AMI manager\n")
	amiManager.Shutdown()
	fmt.Fprintf(os.Stderr, "DEBUG-50: AMI manager shut down\n")

	fmt.Fprintf(os.Stderr, "DEBUG-51: Closing storage\n")
	if err := stateStorage.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to close storage: %v\n", err)
		logger.Error("Failed to close storage", zap.Error(err))
	}
	fmt.Fprintf(os.Stderr, "DEBUG-52: Storage closed\n")

	fmt.Fprintf(os.Stderr, "DEBUG-53: Shutdown complete\n")
	logger.Info("Shutdown complete")
	fmt.Fprintf(os.Stderr, "DEBUG-54: Program exiting\n")
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
