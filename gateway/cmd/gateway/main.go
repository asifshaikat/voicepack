// cmd/gateway/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"gateway/pkg/ami"
	"gateway/pkg/api"
	"gateway/pkg/common"
	"gateway/pkg/config"
	"gateway/pkg/coordinator"
	"gateway/pkg/log"
	"gateway/pkg/metrics"
	"gateway/pkg/rtpengine"
	"gateway/pkg/sip"
	"gateway/pkg/storage"
	"gateway/pkg/websocket"
)

// Version is set during build
var Version = "dev"

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration from YAML file
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Setup logger using config
	logger := setupLogger(cfg.LogLevel, &cfg.Logging)
	defer logger.Sync()

	// Set up standardized logging
	nodeID := cfg.HighAvailability.NodeID
	if nodeID == "" {
		nodeID, _ = os.Hostname()
	}

	// Create standardized logger but use the standard logger for consistency
	stdLogger, err := log.NewLogger(log.Config{
		Development: cfg.TestingMode.Enabled,
		Level:       getZapLevel(cfg.LogLevel),
		NodeID:      nodeID,
		Version:     Version,
	})
	if err != nil {
		logger.Fatal("Failed to create standard logger", zap.Error(err))
	}
	// Use stdLogger for logging high availability events
	_ = stdLogger

	logger.Info("program starting",
		zap.String("config_path", *configPath),
		zap.String("version", Version),
		zap.String("node_id", nodeID))

	logger.Info("configuration loaded",
		zap.Bool("sip_processing_disabled", cfg.SIP.DisableSIPProcessing))

	// Initialize metrics
	metrics.InitMetrics(Version, nodeID)

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

	// Set up leader status metric
	go func() {
		ticker := time.NewTicker(time.Second * 5)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				metrics.SetLeaderStatus(coordinator.IsLeader())
			}
		}
	}()

	// Log testing mode configuration if enabled
	if cfg.TestingMode.Enabled {
		logger.Info("TESTING MODE ENABLED",
			zap.Bool("websocket", cfg.TestingMode.EnableWebSocket),
			zap.Bool("sip", cfg.TestingMode.EnableSIP),
			zap.Bool("rtp", cfg.TestingMode.EnableRTP),
			zap.Bool("ami", cfg.TestingMode.EnableAMI),
			zap.Bool("verbose_logging", cfg.TestingMode.VerboseLogging),
			zap.Bool("log_sip_messages", cfg.TestingMode.LogSIPMessages),
			zap.Int("backend_delay_ms", cfg.TestingMode.SimulateBackendDelay))
	}

	// Create and start the RTPEngine manager if enabled
	var rtpManager *rtpengine.Manager
	if !cfg.TestingMode.Enabled || cfg.TestingMode.EnableRTP {
		logger.Info("creating RTPEngine manager")
		var err error
		rtpManager, err = rtpengine.NewManager(cfg.ToRTPEngineManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
		if err != nil {
			logger.Fatal("failed to create RTPEngine manager", zap.Error(err))
		}
		logger.Info("RTPEngine manager created successfully")
	} else {
		logger.Info("RTPEngine manager disabled in testing mode")
		mockConfig := rtpengine.ManagerConfig{
			Engines: []rtpengine.Config{},
		}
		rtpManager, _ = rtpengine.NewManager(mockConfig, logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	}

	// Create and start AMI manager if enabled
	var amiManager *ami.Manager
	if !cfg.TestingMode.Enabled || cfg.TestingMode.EnableAMI {
		logger.Info("creating AMI manager")
		var err error
		amiManager, err = ami.NewManager(cfg.ToAsteriskManagerConfig(), logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
		if err != nil {
			logger.Fatal("failed to create AMI manager", zap.Error(err))
		}
		logger.Info("AMI manager created successfully")
	} else {
		logger.Info("AMI manager disabled in testing mode")
		mockConfig := ami.ManagerConfig{
			Clients: []ami.Config{},
		}
		amiManager, _ = ami.NewManager(mockConfig, logger, common.NewGoroutineRegistry(logger), stateStorage, coordinator)
	}

	// Create SIP proxy if enabled
	var sipProxy *sip.Proxy
	var udpTransport sip.Transport
	if !cfg.TestingMode.Enabled || cfg.TestingMode.EnableSIP {
		logger.Info("creating SIP proxy")
		sipCfg := sip.SIPConfig{
			UDPBindAddr:             cfg.SIP.UDPBindAddr,
			ProxyURI:                cfg.SIP.ProxyURI,
			DefaultNextHop:          cfg.SIP.DefaultNextHop,
			MaxForwards:             cfg.SIP.MaxForwards,
			UserAgent:               cfg.SIP.UserAgent,
			DisableUDPSIPProcessing: cfg.SIP.DisableUDPSIPProcessing,
			DisableWSSIPProcessing:  cfg.SIP.DisableWSSIPProcessing,
		}

		// Add verbose logging for SIP messages if enabled in testing mode
		if cfg.TestingMode.Enabled && cfg.TestingMode.LogSIPMessages {
			sipCfg.LogFullMessages = true
		}

		logger.Info("SIP configuration prepared",
			zap.String("UDPBindAddr", sipCfg.UDPBindAddr),
			zap.String("DefaultNextHop", sipCfg.DefaultNextHop),
			zap.Bool("DisableUDPSIPProcessing", sipCfg.DisableUDPSIPProcessing),
			zap.Bool("DisableWSSIPProcessing", sipCfg.DisableWSSIPProcessing))

		var err error
		sipProxy, err = sip.NewProxy(sipCfg, stateStorage, rtpManager, logger)
		if err != nil {
			logger.Fatal("failed to create SIP proxy", zap.Error(err))
		}
		logger.Info("SIP proxy created successfully")

		// Create UDP transport for SIP
		logger.Info("creating UDP transport at", zap.String("address", cfg.SIP.UDPBindAddr))
		var transportErr error
		udpTransport, transportErr = sip.NewUDPTransport(cfg.SIP.UDPBindAddr, logger)
		if transportErr != nil {
			logger.Fatal("failed to create UDP transport", zap.Error(transportErr))
		}
		sipProxy.AddTransport(udpTransport)
		logger.Info("UDP transport created and added to SIP proxy")
	} else {
		logger.Info("SIP proxy disabled in testing mode")
		sipCfg := sip.SIPConfig{
			UDPBindAddr:          "127.0.0.1:0",
			DisableSIPProcessing: true,
		}
		sipProxy, _ = sip.NewProxy(sipCfg, stateStorage, rtpManager, logger)
	}

	// Create WebSocket server if enabled
	var wsServer *websocket.Server
	if !cfg.TestingMode.Enabled || cfg.TestingMode.EnableWebSocket {
		// Convert WebSocket configuration
		logger.Info("preparing WebSocket configuration")
		// Add to main.go where you create the WebSocket server config
		wsConfig := websocket.ServerConfig{
			BindAddr:               cfg.WebSocket.BindAddr,
			CertFile:               cfg.WebSocket.CertFile,
			KeyFile:                cfg.WebSocket.KeyFile,
			MaxConnections:         cfg.WebSocket.MaxConnections,
			ReadTimeout:            cfg.GetWebSocketReadTimeout(),
			WriteTimeout:           cfg.GetWebSocketWriteTimeout(),
			IdleTimeout:            cfg.GetWebSocketIdleTimeout(),
			EnableIPv4Only:         cfg.WebSocket.EnableIPv4Only,
			ServerName:             cfg.WebSocket.ServerName,
			BackendServers:         cfg.WebSocket.BackendServers,
			FailoverThreshold:      cfg.WebSocket.FailoverThreshold,
			DisableSIPProcessing:   cfg.WebSocket.DisableSIPProcessing,
			DisableWSSIPProcessing: cfg.WebSocket.DisableWSSIPProcessing,
			//DomainRewriteEnabled:   cfg.WebSocket.DomainRewriteEnabled, // Add this line
		}

		// Add testing-specific configuration
		if cfg.TestingMode.Enabled {
			wsConfig.VerboseLogging = cfg.TestingMode.VerboseLogging
			wsConfig.SimulateBackendDelay = time.Duration(cfg.TestingMode.SimulateBackendDelay) * time.Millisecond
			wsConfig.TestingModeEnabled = true
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

		var err error
		wsServer, err = websocket.NewServer(wsConfig, stateStorage, logger)
		if err != nil {
			logger.Fatal("failed to create WebSocket server", zap.Error(err))
		}
		logger.Info("WebSocket server created successfully")

		logger.Debug("WebSocket server created successfully, setting SIP handler")
		logger.Info("setting SIP handler for WebSocket server")
		wsServer.SetSIPHandler(sipProxy)
		logger.Info("SIP handler set for WebSocket server")

		// Register status and metrics endpoints
		logger.Info("registering status and metrics endpoints")
		api.RegisterStatusEndpoints(
			wsServer.GetHTTPHandler(),
			amiManager,
			rtpManager,
			wsServer,
			coordinator,
			nil, // We don't have a health monitor yet
			stateStorage,
			logger,
			nodeID,
			Version,
		)

		// Start metrics server
		// Using the WebSocket port + 1 for metrics
		metricsPort := 8081 // Default metrics port
		metricsAddr := fmt.Sprintf(":%d", metricsPort)

		go metrics.StartMetricsServer(metricsAddr, logger)

		logger.Info("status and metrics endpoints registered")
	} else {
		logger.Info("WebSocket server disabled in testing mode")
	}

	// Create a context for coordinated shutdown
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())

	// Start each component if it was created
	if rtpManager != nil && (!cfg.TestingMode.Enabled || cfg.TestingMode.EnableRTP) {
		logger.Info("Starting RTPEngine manager")
		rtpManager.Start(shutdownCtx)
	}

	if amiManager != nil && (!cfg.TestingMode.Enabled || cfg.TestingMode.EnableAMI) {
		logger.Info("Starting AMI manager")
		if err := amiManager.Start(shutdownCtx); err != nil {
			logger.Fatal("Failed to start AMI manager", zap.Error(err))
		}
	}

	if sipProxy != nil && (!cfg.TestingMode.Enabled || cfg.TestingMode.EnableSIP) {
		logger.Info("Starting SIP proxy server")
		if err := sipProxy.Start(shutdownCtx); err != nil {
			logger.Fatal("Failed to start SIP proxy", zap.Error(err))
		}
	}

	if wsServer != nil && (!cfg.TestingMode.Enabled || cfg.TestingMode.EnableWebSocket) {
		logger.Info("Starting WebSocket server")
		if err := wsServer.Start(shutdownCtx); err != nil {
			logger.Fatal("Failed to start WebSocket server", zap.Error(err))
		}
	}

	// Create a channel to receive initialization status
	initCh := make(chan error, 1)
	go func() {
		// This channel will be used in the future for coordinated initialization
		initCh <- nil
	}()

	// Wait for initialization to complete
	err = <-initCh
	if err != nil {
		logger.Fatal("failed to initialize gateway", zap.Error(err))
	}

	logger.Info("WebRTC-SIP Gateway initialized successfully")
	logger.Info("Access status page at: http://<host>:8080/status")
	logger.Info("Access metrics at: http://<host>:8081/metrics")
	logger.Info("Access health endpoint at: http://<host>:8080/health")

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-sigCh
	logger.Info("shutdown signal received", zap.String("signal", sig.String()))
	cancelShutdown()

	logger.Info("starting component shutdown sequence")

	// Shutdown wait time
	waitDuration := time.Duration(cfg.ShutdownWaitSeconds) * time.Second
	logger.Info("waiting for components to shut down",
		zap.Duration("timeout", waitDuration))

	// Create context with timeout for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), waitDuration)
	defer cancel()

	// Shut down components in reverse order
	if wsServer != nil {
		logger.Info("stopping WebSocket server")
		wsServer.Stop()
	}

	if sipProxy != nil {
		logger.Info("stopping SIP proxy")
		sipProxy.Stop()
	}

	if amiManager != nil {
		logger.Info("stopping AMI manager")
		amiManager.Shutdown()
	}

	if rtpManager != nil {
		logger.Info("stopping RTPEngine manager")
		rtpManager.Stop()
	}

	// Wait for all services to stop
	// This would typically be a more complex process with coordination

	logger.Info("all components shut down, exiting")
}

// getZapLevel converts a string level to zapcore.Level
func getZapLevel(level string) zapcore.Level {
	switch level {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// setupLogger creates a logger with the specified configuration
func setupLogger(level string, logConfig *config.LogConfig) *zap.Logger {
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

	// Create encoder config
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "message",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// For console output, add colors to level
	if logConfig.UseConsole {
		encoderConfig.EncodeLevel = zapcore.LowercaseColorLevelEncoder
	} else {
		encoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder
	}

	// Create cores slice to store all our outputs
	var cores []zapcore.Core

	// Add console output if configured
	if logConfig.UseConsole {
		consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)
		consoleCore := zapcore.NewCore(
			consoleEncoder,
			zapcore.AddSync(os.Stdout),
			logLevel,
		)
		cores = append(cores, consoleCore)

		// Add JSON stderr output if both console and JSON are enabled
		if logConfig.UseJSON {
			jsonEncoder := zapcore.NewJSONEncoder(encoderConfig)
			stderrCore := zapcore.NewCore(
				jsonEncoder,
				zapcore.AddSync(os.Stderr),
				logLevel,
			)
			cores = append(cores, stderrCore)
		}
	}

	// Add file output if configured
	if logConfig.LogFile != "" || logConfig.Directory != "" {
		// Ensure the log directory exists
		logDir := logConfig.Directory
		if err := ensureDirectory(logDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating log directory: %v\n", err)
		}

		// Determine the log file path
		var logFilePath string
		if logConfig.LogFile != "" {
			// Use specified log file
			if filepath.IsAbs(logConfig.LogFile) {
				logFilePath = logConfig.LogFile
			} else {
				logFilePath = filepath.Join(logDir, logConfig.LogFile)
			}
		} else {
			// Construct filename with optional date-time
			filename := "voiceproxy.log"
			if logConfig.IncludeDateTime {
				timestamp := time.Now().Format("2006-01-02_15-04-05")
				filename = "voiceproxy_" + timestamp + ".log"
			}
			logFilePath = filepath.Join(logDir, filename)
		}

		// Create lumberjack writer for log rotation
		lumberjackLogger := &lumberjack.Logger{
			Filename:   logFilePath,
			MaxSize:    logConfig.MaxSize, // MB
			MaxBackups: logConfig.MaxBackups,
			MaxAge:     logConfig.MaxAge, // days
			Compress:   logConfig.Compress,
			LocalTime:  true,
		}

		// Create an encoder based on the format preference
		var fileEncoder zapcore.Encoder
		if logConfig.UseJSON {
			fileEncoder = zapcore.NewJSONEncoder(encoderConfig)
		} else {
			fileEncoder = zapcore.NewConsoleEncoder(encoderConfig)
		}

		// Create a core for file output
		fileCore := zapcore.NewCore(
			fileEncoder,
			zapcore.AddSync(lumberjackLogger),
			logLevel,
		)
		cores = append(cores, fileCore)

		// Log the file setup
		fmt.Fprintf(os.Stdout, "Logging to file: %s\n", logFilePath)
	}

	// Combine all cores
	core := zapcore.NewTee(cores...)

	// Create logger
	logger := zap.New(core,
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)

	return logger
}

// ensureDirectory ensures the specified directory exists
func ensureDirectory(dir string) error {
	return os.MkdirAll(dir, 0755)
}
