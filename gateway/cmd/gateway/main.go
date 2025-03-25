// cmd/gateway/main.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"gateway/pkg/ami"
	"gateway/pkg/common"
	"gateway/pkg/config"
	"gateway/pkg/coordinator"
	"gateway/pkg/health"
	"gateway/pkg/rtpengine"
	"gateway/pkg/sip"
	"gateway/pkg/storage"
	"gateway/pkg/websocket"
)

func main() {
	logger := setupLogger("debug")
	defer logger.Sync()

	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()
	
	logger.Info("program starting",
		zap.String("config_path", *configPath))

	// Load configuration from YAML file
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logger.Fatal("failed to load configuration",
			zap.Error(err))
	}

	logger.Info("configuration loaded",
		zap.Bool("sip_processing_disabled", cfg.SIP.DisableSIPProcessing))

		sipCfg := sip.SIPConfig{
			UDPBindAddr:             cfg.SIP.UDPBindAddr,
			ProxyURI:                cfg.SIP.ProxyURI,
			DefaultNextHop:          cfg.SIP.DefaultNextHop,
			MaxForwards:             cfg.SIP.MaxForwards,
			UserAgent:               cfg.SIP.UserAgent,
			DisableUDPSIPProcessing: cfg.SIP.DisableUDPSIPProcessing,
			DisableWSSIPProcessing:  cfg.SIP.DisableWSSIPProcessing,
		}
		logger.Info("SIP configuration prepared",
		zap.String("UDPBindAddr", sipCfg.UDPBindAddr),
		zap.String("DefaultNextHop", sipCfg.DefaultNextHop),
		zap.Bool("DisableUDPSIPProcessing", sipCfg.DisableUDPSIPProcessing),
		zap.Bool("DisableWSSIPProcessing", sipCfg.DisableWSSIPProcessing))
	// Create storage
	logger.Info("creating storage")
	var stateStorage storage.StateStorage
	if cfg.Redis.Enabled {
		logger.Info("Redis storage not implemented; using in-memory storage")
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	} else {
		logger.Info("creating in-memory storage",
			zap.String("path", cfg.MemoryStorage.PersistPath))
		stateStorage, err = storage.NewMemoryStorage(storage.MemoryConfig{
			MaxKeys:         cfg.MemoryStorage.MaxKeys,
			CleanupInterval: time.Duration(cfg.MemoryStorage.CleanupIntervalSeconds) * time.Second,
			PersistPath:     cfg.MemoryStorage.PersistPath,
		}, logger)
	}
	if err != nil {
		logger.Fatal("failed to create storage", zap.Error(err))
	}
	logger.Info("storage created successfully")

	// Create a background context for coordinator registration
	ctx := context.Background()
	logger.Info("background context created")

	// Create coordinator
	logger.Info("creating coordinator")
	coordinator, err := coordinator.NewCoordinator(stateStorage, coordinator.CoordinatorConfig{
		HeartbeatInterval: cfg.GetHeartbeatInterval(),
		LeaseTimeout:      cfg.GetLeaseTimeout(),
	}, logger)
	if err != nil {
		logger.Fatal("failed to create coordinator", zap.Error(err))
	}
	logger.Info("coordinator created, starting it")

	if err := coordinator.Start(ctx); err != nil {
		logger.Fatal("failed to start coordinator", zap.Error(err))
	}
	logger.Info("coordinator started successfully")
	defer coordinator.Stop()

	// Register components for leadership
	logger.Info("registering components for leadership")
	coordinator.RegisterComponentLeadership("rtpengine")
	coordinator.RegisterComponentLeadership("ami")
	coordinator.RegisterComponentLeadership("sip")
	coordinator.RegisterComponentLeadership("websocket")
	logger.Info("components registered for leadership")

	// Create RTPEngine manager
	logger.Info("creating RTPEngine manager")
	rtpManager, err := rtpengine.NewManager(cfg.ToRTPEngineManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	if err != nil {
		logger.Fatal("failed to create RTPEngine manager", zap.Error(err))
	}
	logger.Info("RTPEngine manager created successfully")

	// Create AMI manager
	logger.Info("creating AMI manager")
	amiManager, err := ami.NewManager(cfg.ToAsteriskManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	if err != nil {
		logger.Fatal("failed to create AMI manager", zap.Error(err))
	}
	logger.Info("AMI manager created successfully")

	// Create SIP proxy
	logger.Info("creating SIP proxy")
	sipProxy, err := sip.NewProxy(sipCfg, stateStorage, rtpManager, logger)
	if err != nil {
		logger.Fatal("failed to create SIP proxy", zap.Error(err))
	}
	logger.Info("SIP proxy created successfully")

	// Create UDP transport for SIP
	logger.Info("creating UDP transport at", zap.String("address", cfg.SIP.UDPBindAddr))
	udpTransport, err := sip.NewUDPTransport(cfg.SIP.UDPBindAddr, logger)
	if err != nil {
		logger.Fatal("failed to create UDP transport", zap.Error(err))
	}
	sipProxy.AddTransport(udpTransport)
	logger.Info("UDP transport created and added to SIP proxy")

	// Convert WebSocket configuration
	logger.Info("preparing WebSocket configuration")
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
	logger.Info("WebSocket config prepared",
		zap.String("bindAddr", wsConfig.BindAddr),
		zap.String("certFile", wsConfig.CertFile),
		zap.String("serverName", wsConfig.ServerName))

	// Create WebSocket server
	logger.Info("creating WebSocket server")
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
		logger.Fatal("failed to create WebSocket server", zap.Error(err))
	}
	logger.Info("WebSocket server created successfully")

	logger.Debug("WebSocket server created successfully, setting SIP handler")
	logger.Info("setting SIP handler for WebSocket server")
	wsServer.SetSIPHandler(sipProxy)
	logger.Info("SIP handler set for WebSocket server")

	// Create a context for coordinated shutdown
	logger.Info("creating shutdown context")
	shutdownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger.Info("shutdown context created")

	// Start components
	logger.Info("starting WebRTC-SIP Gateway components")
	logger.Info("Starting WebRTC-SIP Gateway...")

	logger.Info("starting RTPEngine manager")
	rtpManager.Start(shutdownCtx)
	logger.Info("RTPEngine manager started")

	logger.Info("starting AMI manager")
	if err := amiManager.Start(shutdownCtx); err != nil {
		logger.Fatal("failed to start AMI manager", zap.Error(err))
	}
	logger.Info("AMI manager started successfully")

	logger.Info("starting SIP proxy")

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
			logger.Error("failed to start SIP proxy", zap.Error(err))
			// Continue anyway instead of calling logger.Fatal which would terminate
		} else {
			logger.Info("SIP proxy started successfully")
		}
	case <-time.After(2 * time.Second):
		// No immediate error, assume success and continue
		logger.Info("SIP proxy initialization in progress, continuing startup")
		logger.Info("SIP proxy initialization in progress, continuing startup")
	}

	// Continue with the rest of the startup sequence
	logger.Info("about to start WebSocket server")

	// Add panic recovery to catch any silent failures
	defer func() {
		if r := recover(); r != nil {
			logger.Error("PANIC RECOVERED during WebSocket start", zap.Any("panic", r))
			// Stack trace
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			logger.Error("Stack trace", zap.String("trace", string(buf[:n])))
		}
	}()

	logger.Debug("Preparing to start WebSocket server...")
	logger.Info("starting WebSocket server on", zap.String("bindAddr", wsConfig.BindAddr))
	if err := wsServer.Start(shutdownCtx); err != nil {
		logger.Error("WebSocket server start failed with error",
			zap.Error(err),
			zap.String("bindAddr", wsConfig.BindAddr),
			zap.String("certFile", wsConfig.CertFile))
		logger.Fatal("failed to start WebSocket server", zap.Error(err))
	}
	logger.Info("WebSocket server started successfully")
	logger.Info("setting up health monitoring system")

	// Extract domain from DefaultNextHop
	opensipsHost := cfg.SIP.DefaultNextHop
	if idx := strings.LastIndex(opensipsHost, ":"); idx > 0 {
		opensipsHost = opensipsHost[:idx]
	}

	healthConfig := health.HealthConfig{
		OpenSIPSAddress: opensipsHost,                 // Using just the domain part
		CheckInterval:   15 * time.Second,             // Check every 15 seconds
		EnableFailover:  cfg.HighAvailability.Enabled, // Use HA config to determine if failover is enabled
	}

	logger.Debug("Health monitor configuration",
		zap.String("opensipsAddress", healthConfig.OpenSIPSAddress),
		zap.Duration("checkInterval", healthConfig.CheckInterval),
		zap.Bool("enableFailover", healthConfig.EnableFailover))

	healthMonitor := health.NewHealthMonitor(
		udpTransport, // SIP transport for OpenSIPS checks
		amiManager,   // AMI manager for Asterisk checks
		rtpManager,   // RTP manager for RTPEngine checks
		wsServer,     // WebSocket server for notifications
		coordinator,  // Coordinator for leadership
		stateStorage, // Storage for health history
		logger.Named("health"),
		healthConfig,
	)

	// Start health monitoring
	logger.Info("starting health monitoring")

	if err := healthMonitor.Start(shutdownCtx); err != nil {
		logger.Error("failed to start health monitoring", zap.Error(err))
	} else {
		logger.Info("health monitoring started successfully")
	}

	// Set up HTTP server for health checks
	logger.Info("setting up HTTP server for health endpoints")

	// Create HTTP server mux
	httpMux := http.NewServeMux()

	// Register health endpoint
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("Received health check request",
			zap.String("remoteAddr", r.RemoteAddr),
			zap.String("userAgent", r.UserAgent()))

		// Get all component health statuses
		status := healthMonitor.GetAllComponentHealth()

		// Calculate overall health
		allHealthy := true
		for _, comp := range status {
			if comp.Status != health.StatusHealthy {
				allHealthy = false
				logger.Debug("Unhealthy component detected",
					zap.String("component", comp.Component),
					zap.String("status", string(comp.Status)),
					zap.String("message", comp.Message))
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !allHealthy {
			w.WriteHeader(http.StatusServiceUnavailable) // 503
			logger.Debug("Returning unhealthy status (503)")
		} else {
			logger.Debug("Returning healthy status (200)")
		}

		// Prepare response
		statusText := "unhealthy"
		if allHealthy {
			statusText = "healthy"
		}
		response := map[string]interface{}{
			"status":     statusText,
			"components": status,
			"timestamp":  time.Now(),
		}

		// Send response
		if err := json.NewEncoder(w).Encode(response); err != nil {
			logger.Error("failed to encode health response", zap.Error(err))
		}
	})

	// Add component-specific health endpoints
	httpMux.HandleFunc("/health/opensips", func(w http.ResponseWriter, r *http.Request) {
		status := healthMonitor.GetComponentHealth("opensips")
		w.Header().Set("Content-Type", "application/json")

		if status == nil || status.Status != health.StatusHealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(status)
	})

	httpMux.HandleFunc("/health/asterisk", func(w http.ResponseWriter, r *http.Request) {
		status := healthMonitor.GetComponentHealth("asterisk")
		w.Header().Set("Content-Type", "application/json")

		if status == nil || status.Status != health.StatusHealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(status)
	})

	httpMux.HandleFunc("/health/rtpengine", func(w http.ResponseWriter, r *http.Request) {
		status := healthMonitor.GetComponentHealth("rtpengine")
		w.Header().Set("Content-Type", "application/json")

		if status == nil || status.Status != health.StatusHealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(status)
	})

	// Create HTTP server
	httpServer := &http.Server{
		Addr:    ":8080", // Health check port
		Handler: httpMux,
	}

	// Start HTTP server in a goroutine
	logger.Info("starting HTTP server for health endpoints", zap.String("address", ":8080"))

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", zap.Error(err))
		}
	}()

	logger.Info("HTTP server for health endpoints started")
	logger.Info("WebRTC-SIP Gateway started successfully")

	// Handle signals for graceful shutdown
	logger.Info("setting up signal handler for graceful shutdown")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	logger.Info("waiting for shutdown signal")
	sig := <-sigCh
	logger.Info("shutdown signal received", zap.String("signal", sig.String()))
	cancel()

	logger.Info("starting component shutdown sequence")
	logger.Info("Shutting down components...")
	logger.Info("shutting down HTTP server")
	httpShutdownCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpCancel()
	if err := httpServer.Shutdown(httpShutdownCtx); err != nil {
		logger.Error("failed to gracefully shut down HTTP server", zap.Error(err))
	}
	logger.Info("HTTP server shut down")
	// Shutdown components in reverse order
	logger.Info("stopping WebSocket server")
	if err := wsServer.Stop(); err != nil {
		logger.Error("failed to stop WebSocket server", zap.Error(err))
	}
	logger.Info("WebSocket server stopped")

	logger.Info("stopping SIP proxy")
	if err := sipProxy.Stop(); err != nil {
		logger.Error("failed to stop SIP proxy", zap.Error(err))
	}
	logger.Info("SIP proxy stopped")

	logger.Info("shutting down AMI manager")
	amiManager.Shutdown()
	logger.Info("AMI manager shut down")

	logger.Info("closing storage")
	if err := stateStorage.Close(); err != nil {
		logger.Error("failed to close storage", zap.Error(err))
	}
	logger.Info("storage closed")

	logger.Info("shutdown complete")
	logger.Info("WebRTC-SIP Gateway started successfully")
	logger.Info("program exiting")
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

	// Create custom console encoder config
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "message",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseColorLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// Create console core for prettier development output
	consoleCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		zapcore.AddSync(os.Stdout),
		logLevel,
	)

	// Create JSON core for structured logging
	jsonConfig := encoderConfig
	jsonConfig.EncodeLevel = zapcore.LowercaseLevelEncoder
	jsonCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(jsonConfig),
		zapcore.AddSync(os.Stderr),
		logLevel,
	)

	// Combine both cores
	core := zapcore.NewTee(consoleCore, jsonCore)

	// Create logger
	logger := zap.New(core,
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)

	return logger
}