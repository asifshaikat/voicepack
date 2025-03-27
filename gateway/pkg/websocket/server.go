// pkg/websocket/server.go
package websocket

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"gateway/pkg/common"
	"gateway/pkg/sip"
	"gateway/pkg/storage"

	"go.uber.org/zap"
)

// Server implements a WebSocket server for WebRTC clients
type Server struct {
	config         ServerConfig
	upgrader       websocket.Upgrader
	httpServer     *http.Server
	storage        storage.StateStorage
	registry       *common.GoroutineRegistry
	handler        SIPHandler
	logger         *zap.Logger
	circuitBreaker *common.CircuitBreaker
	domainProxy    *DomainRewriteProxy

	mu               sync.RWMutex
	connections      map[string]*ClientConnection
	wg               sync.WaitGroup
	backendHealth    map[string]bool
	backendMu        sync.RWMutex
	startTime        time.Time
	totalConnections int
	isLeader         bool
	mux              *http.ServeMux
}

// ServerConfig defines the configuration for the WebSocket server
type ServerConfig struct {
	BindAddr                string
	CertFile                string
	KeyFile                 string
	MaxConnections          int
	ReadTimeout             time.Duration
	WriteTimeout            time.Duration
	IdleTimeout             time.Duration
	EnableIPv4Only          bool
	ServerName              string   // Kept for backward compatibility
	BackendServers          []string // List of WebSocket backend servers
	FailoverThreshold       int      // Number of errors before failover
	DisableSIPProcessing    bool     // General flag
	DisableWSSIPProcessing  bool     // Specific to WebSocket SIP processing
	DisableUDPSIPProcessing bool     // Specific to UDP SIP processing

	// Testing mode fields
	VerboseLogging           bool          // Enable verbose logging for testing
	SimulateBackendDelay     time.Duration // Artificial delay for backend responses
	TestingModeEnabled       bool          // Whether testing mode is active
	UseInternalHealthHandler bool          // Controls whether to register internal health handler
}

// SIPHandler handles SIP messages from WebSocket clients
type SIPHandler interface {
	HandleMessage(msg sip.Message, addr net.Addr) error
}

// ClientConnection represents a WebSocket client connection
type ClientConnection struct {
	ID             string
	Conn           *websocket.Conn
	RemoteAddr     string
	LocalAddr      string
	Protocol       string
	CreateTime     time.Time
	LastActivity   time.Time
	SIPAddress     string
	BackendConn    *websocket.Conn // For proxy scenarios
	BackendAddr    string
	CurrentBackend string // Currently active backend URL
	ErrorCount     int    // Count errors for failover threshold
	LastSIPCallID  string // For session tracking
	LastSIPFromTag string // For session tracking
	LastSIPToTag   string // For session tracking
	closed         bool
	closeMu        sync.Mutex
	clientDone     chan struct{}
	backendDone    chan struct{}
	cancelFunc     context.CancelFunc // manage connection context
}

// NewServer creates a new WebSocket server
func NewServer(config ServerConfig, storage storage.StateStorage, logger *zap.Logger) (*Server, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	// Set defaults
	if config.MaxConnections <= 0 {
		config.MaxConnections = 1000
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = 30 * time.Second
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = 30 * time.Second
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = 120 * time.Second
	}
	if config.ServerName == "" {
		config.ServerName = "WebRTC-SIP-Gateway"
	}
	if config.FailoverThreshold <= 0 {
		config.FailoverThreshold = 1
	}

	// Initialize backend servers from ServerName if not provided
	if len(config.BackendServers) == 0 && config.ServerName != "" {
		config.BackendServers = []string{config.ServerName}
	}

	logger.Info("Creating WebSocket server with configuration",
		zap.String("bindAddr", config.BindAddr),
		zap.String("serverName", config.ServerName),
		zap.Strings("backendServers", config.BackendServers),
		zap.Int("failoverThreshold", config.FailoverThreshold),
		zap.Bool("disableSIPProcessing", config.DisableSIPProcessing),
		zap.Bool("disableWSSIPProcessing", config.DisableWSSIPProcessing))

	// Create health map for all backends
	backendHealth := make(map[string]bool)
	for _, backend := range config.BackendServers {
		backendHealth[backend] = true // Initially assume all backends are healthy
	}

	server := &Server{
		config:        config,
		storage:       storage,
		registry:      common.NewGoroutineRegistry(logger),
		logger:        logger,
		connections:   make(map[string]*ClientConnection),
		backendHealth: backendHealth,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for now
			},
			Subprotocols: []string{"sip"},
		},
		circuitBreaker: common.NewCircuitBreaker(
			"websocket",
			common.CircuitBreakerConfig{
				FailureThreshold: 5,
				ResetTimeout:     30 * time.Second,
				HalfOpenMaxReqs:  3,
			},
			logger,
		),
		startTime: time.Now(),
		mux:       http.NewServeMux(),
	}

	server.domainProxy = NewDomainRewriteProxy(logger)

	return server, nil
}

// SetSIPHandler sets the handler for SIP messages
func (s *Server) SetSIPHandler(handler SIPHandler) {
	s.handler = handler
}

// UpdateBackendHealth updates the health status of a backend server
func (s *Server) UpdateBackendHealth(backend string, healthy bool) {
	s.backendMu.Lock()
	defer s.backendMu.Unlock()

	s.backendHealth[backend] = healthy
	s.logger.Info("Backend health status updated",
		zap.String("backend", backend),
		zap.Bool("healthy", healthy))
}

// CheckBackendHealth checks the health of all configured backends
func (s *Server) CheckBackendHealth(ctx context.Context) {
	for _, backend := range s.config.BackendServers {
		healthy := s.testBackendConnection(backend)
		s.UpdateBackendHealth(backend, healthy)
	}
}

// testBackendConnection makes a test connection to check if the backend is responsive
func (s *Server) testBackendConnection(backend string) bool {
	if backend == "" {
		return false
	}

	// Extract host from the WebSocket URL
	backendURL := backend
	if !strings.HasPrefix(backendURL, "ws") {
		backendURL = "wss://" + backendURL
	}

	// Extract the hostname from the URL
	hostname := strings.TrimPrefix(strings.TrimPrefix(backendURL, "wss://"), "ws://")
	if idx := strings.IndexByte(hostname, ':'); idx > 0 {
		hostname = hostname[:idx]
	}
	if idx := strings.IndexByte(hostname, '/'); idx > 0 {
		hostname = hostname[:idx]
	}

	// Use port 6060 for OpenSIPS health check via SIP OPTIONS
	targetAddress := hostname + ":6060"

	s.logger.Debug("Testing OpenSIPS backend with SIP OPTIONS",
		zap.String("websocket_url", backendURL),
		zap.String("sip_options_target", targetAddress))

	// Create a SIP OPTIONS message
	callID := fmt.Sprintf("health-%d", time.Now().UnixNano())
	branch := fmt.Sprintf("z9hG4bK-%d", time.Now().UnixNano())
	fromTag := fmt.Sprintf("fromTag-%d", time.Now().UnixNano())

	optionsMsg := fmt.Sprintf(
		"OPTIONS sip:%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP health.monitor;branch=%s;rport\r\n"+
			"Max-Forwards: 69\r\n"+
			"From: <sip:proxy@localhost>;tag=%s\r\n"+
			"To: <sip:%s>\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 1 OPTIONS\r\n"+
			"User-Agent: Qalqul-WebSocket-Health-Monitor\r\n"+
			"Allow: INVITE, ACK, CANCEL, OPTIONS, BYE\r\n"+
			"Content-Length: 0\r\n\r\n",
		hostname,
		branch,
		fromTag,
		hostname,
		callID,
	)

	// Get the UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", targetAddress)
	if err != nil {
		s.logger.Debug("Failed to resolve OpenSIPS address for health check",
			zap.String("target", targetAddress),
			zap.Error(err))
		return false
	}

	// Connect to the OpenSIPS server
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		s.logger.Debug("Failed to connect to OpenSIPS for health check",
			zap.String("target", targetAddress),
			zap.Error(err))
		return false
	}
	defer conn.Close()

	// Set a timeout
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send OPTIONS message
	_, err = conn.Write([]byte(optionsMsg))
	if err != nil {
		s.logger.Debug("Failed to send OPTIONS message for health check",
			zap.String("target", targetAddress),
			zap.Error(err))
		return false
	}

	// Read response
	buffer := make([]byte, 4096)
	n, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		s.logger.Debug("Failed to read response for health check",
			zap.String("target", targetAddress),
			zap.Error(err))
		return false
	}

	response := string(buffer[:n])

	// Check for valid responses (200 OK or 405 Method Not Allowed or 403 Forbidden)
	if strings.Contains(response, "SIP/2.0 200") ||
		strings.Contains(response, "SIP/2.0 405") ||
		strings.Contains(response, "SIP/2.0 403") {

		s.logger.Debug("OpenSIPS health check successful",
			zap.String("target", targetAddress),
			zap.String("response_code", response[:15]))
		return true
	}

	s.logger.Debug("OpenSIPS returned unexpected response for health check",
		zap.String("target", targetAddress),
		zap.String("response", response[:100]))
	return false
}

// selectHealthyBackend returns a healthy backend from the configured list
func (s *Server) selectHealthyBackend() string {
	if len(s.config.BackendServers) == 0 {
		// If no backends configured, use the ServerName for backward compatibility
		return s.config.ServerName
	}

	s.backendMu.RLock()
	defer s.backendMu.RUnlock()

	// First try to find a known healthy backend
	for _, backend := range s.config.BackendServers {
		if healthy, exists := s.backendHealth[backend]; exists && healthy {
			return backend
		}
	}

	// If no known healthy backend, try the first one in the list
	return s.config.BackendServers[0]
}

// Start starts the WebSocket server
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("Starting WebSocket server",
		zap.String("bindAddr", s.config.BindAddr),
		zap.Bool("tlsEnabled", s.config.CertFile != "" && s.config.KeyFile != ""),
		zap.Strings("backendServers", s.config.BackendServers),
		zap.Bool("disableWSSIPProcessing", s.config.DisableWSSIPProcessing))

	// Register handlers
	s.mux.HandleFunc("/", s.handleWebSocket)

	// Only register the health endpoint if specifically requested
	if s.config.UseInternalHealthHandler {
		s.mux.HandleFunc("/health", s.handleHealthCheck)
		s.logger.Debug("Registered internal health check endpoint at /health")
	}

	s.logger.Debug("Registered WebSocket handler at root path (/)")

	s.httpServer = &http.Server{
		Addr:         s.config.BindAddr,
		Handler:      s.mux,
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
		IdleTimeout:  s.config.IdleTimeout,
	}

	s.logger.Debug("HTTP server configured",
		zap.String("bindAddr", s.config.BindAddr),
		zap.Duration("readTimeout", s.config.ReadTimeout),
		zap.Duration("writeTimeout", s.config.WriteTimeout),
		zap.Duration("idleTimeout", s.config.IdleTimeout),
	)

	// Start connection health monitoring and cleaner
	s.registry.Go("connection-cleaner", func(ctx context.Context) {
		s.logger.Debug("Connection cleaner goroutine started")
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		s.StartHealthMonitoring(ctx)

		for {
			select {
			case <-ctx.Done():
				s.logger.Debug("Connection cleaner shutting down: context canceled")
				return
			case <-ticker.C:
				s.logger.Debug("Running scheduled connection cleanup")
				s.cleanConnections()
			}
		}
	})

	// Start backend health checking if multiple backends are configured
	if len(s.config.BackendServers) > 0 {
		s.registry.Go("backend-health-checker", func(ctx context.Context) {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					s.logger.Debug("Backend health checker shutting down: context canceled")
					return
				case <-ticker.C:
					s.CheckBackendHealth(ctx)
				}
			}
		})
	}

	// Start the HTTP server
	s.registry.Go("http-server", func(ctx context.Context) {
		s.logger.Debug("Starting HTTP server goroutine")
		var err error

		// Check if we need TLS
		if s.config.CertFile != "" && s.config.KeyFile != "" {
			s.logger.Debug("Using TLS configuration",
				zap.String("certFile", s.config.CertFile),
				zap.String("keyFile", s.config.KeyFile),
			)
			if s.config.EnableIPv4Only {
				// Create IPv4-only listener
				s.logger.Debug("Creating IPv4-only listener")
				ln, err := net.Listen("tcp4", s.config.BindAddr)
				if err != nil {
					s.logger.Error("Failed to create IPv4 listener", zap.Error(err))
					return
				}
				s.logger.Debug("Successfully created IPv4 listener",
					zap.String("localAddress", ln.Addr().String()),
				)

				s.logger.Debug("Loading TLS certificate")
				cert, err := tls.LoadX509KeyPair(s.config.CertFile, s.config.KeyFile)
				if err != nil {
					s.logger.Error("Failed to load TLS certificate", zap.Error(err))
					return
				}
				tlsConfig := &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   tls.VersionTLS12,
				}
				s.httpServer.TLSConfig = tlsConfig

				s.logger.Info("Starting TLS server on IPv4 listener")
				err = s.httpServer.ServeTLS(ln, "", "")
			} else {
				s.logger.Info("Starting TLS server with IPv4/IPv6 support")
				err = s.httpServer.ListenAndServeTLS(s.config.CertFile, s.config.KeyFile)
			}
		} else {
			s.logger.Info("Starting non-TLS server")
			if s.config.EnableIPv4Only {
				s.logger.Debug("Creating IPv4-only listener (non-TLS)")
				ln, err := net.Listen("tcp4", s.config.BindAddr)
				if err != nil {
					s.logger.Error("Failed to create IPv4 listener (non-TLS)",
						zap.Error(err),
					)
					return
				}
				s.logger.Debug("Starting non-TLS server on IPv4 listener")
				err = s.httpServer.Serve(ln)
			} else {
				s.logger.Debug("Starting non-TLS server with IPv4/IPv6 support")
				err = s.httpServer.ListenAndServe()
			}
		}

		// This will execute when the server stops
		if err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server failed with error", zap.Error(err))

			// Check for common binding errors
			if opErr, ok := err.(*net.OpError); ok {
				s.logger.Error("Network operation error details",
					zap.String("op", opErr.Op),
					zap.String("net", opErr.Net),
					zap.Any("addr", opErr.Addr),
					zap.Bool("timeout", opErr.Timeout()),
					zap.Bool("temporary", opErr.Temporary()),
				)
			}
		} else if err == http.ErrServerClosed {
			s.logger.Info("WebSocket server closed normally")
		}
	})

	s.logger.Info("WebSocket server initialization complete")
	return nil
}

// Stop stops the WebSocket server
func (s *Server) Stop() error {
	s.logger.Info("Stopping WebSocket server")

	// Shutdown the HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.logger.Error("HTTP server shutdown failed", zap.Error(err))
	}

	// Close all connections
	s.closeAllConnections()

	// Wait for all goroutines to finish
	s.wg.Wait()

	// Shutdown registry goroutines
	return s.registry.Shutdown(30 * time.Second)
}

// handleWebSocket handles WebSocket connection requests
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	clientAddr := r.RemoteAddr
	s.logger.Debug("Incoming WebSocket handshake request",
		zap.String("client", clientAddr),
		zap.String("url", r.URL.String()),
		zap.String("path", r.URL.Path),
		zap.String("userAgent", r.UserAgent()),
	)

	// Check if we're at capacity
	s.mu.RLock()
	if len(s.connections) >= s.config.MaxConnections {
		s.mu.RUnlock()
		s.logger.Warn("Connection rejected: too many connections",
			zap.String("client", clientAddr),
			zap.Int("maxConnections", s.config.MaxConnections),
		)
		http.Error(w, "Too many connections", http.StatusServiceUnavailable)
		return
	}
	s.mu.RUnlock()

	// Check circuit breaker
	if !s.circuitBreaker.AllowRequest() {
		s.logger.Warn("Connection rejected: circuit breaker open",
			zap.String("client", clientAddr),
		)
		http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	// Extract client's requested subprotocols
	clientSubProtocols := websocket.Subprotocols(r)

	// Check for SIP-specific parameters
	sipTransport := "wss"
	if r.TLS == nil {
		sipTransport = "ws"
	}
	if transportParam := r.URL.Query().Get("transport"); transportParam != "" {
		sipTransport = transportParam
	}

	s.logger.Debug("SIP protocol details",
		zap.String("client", clientAddr),
		zap.Strings("subprotocols", clientSubProtocols),
		zap.String("sipTransport", sipTransport),
	)

	// Verify it's a valid WebSocket upgrade
	if !websocket.IsWebSocketUpgrade(r) {
		s.logger.Warn("Not a valid WebSocket upgrade request",
			zap.String("client", clientAddr),
		)
		http.Error(w, "Not a WebSocket handshake", http.StatusBadRequest)
		return
	}

	// Decide which subprotocols to use
	var protocols []string
	if len(clientSubProtocols) > 0 {
		protocols = clientSubProtocols
	} else {
		protocols = []string{"sip"}
	}
	s.upgrader.Subprotocols = protocols

	// First, upgrade the connection
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("WebSocket upgrade failed",
			zap.String("remoteAddr", clientAddr),
			zap.Error(err),
		)
		s.circuitBreaker.RecordFailure()
		return
	}

	// Set a reasonable initial deadline for any pending writes
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

	// Create a unique client ID
	clientID := fmt.Sprintf("%s-%d", clientAddr, time.Now().UnixNano())

	// Create a long-lived context for this WebSocket connection
	wsCtx, wsCancel := context.WithCancel(context.Background())

	// Create client connection
	client := &ClientConnection{
		ID:           clientID,
		Conn:         conn,
		RemoteAddr:   clientAddr,
		LocalAddr:    r.Host,
		Protocol:     conn.Subprotocol(),
		CreateTime:   time.Now(),
		LastActivity: time.Now(),
		clientDone:   make(chan struct{}),
		backendDone:  make(chan struct{}),
		cancelFunc:   wsCancel,
		ErrorCount:   0,
	}

	// Store the connection
	s.mu.Lock()
	s.connections[clientID] = client
	s.mu.Unlock()

	// Store in persistent storage
	ctx := context.Background()
	s.storage.Set(ctx, "ws:"+clientID, []byte(clientAddr), 1*time.Hour)

	s.logger.Info("WebSocket connection established",
		zap.String("clientID", clientID),
		zap.String("remoteAddr", clientAddr),
		zap.String("protocol", client.Protocol),
		zap.String("sipTransport", sipTransport),
	)

	// Handle the connection in a background goroutine
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Remove the write deadline for the long-running connection
		conn.SetWriteDeadline(time.Time{})
		s.handleClient(wsCtx, client, sipTransport)
	}()

	s.circuitBreaker.RecordSuccess()
}

// handleClient processes messages from a WebSocket client
func (s *Server) handleClient(ctx context.Context, client *ClientConnection, sipTransport string) {
	defer s.closeConnection(client)

	// Setup error channel
	errChan := make(chan error, 3) // handle multiple errors

	// Select backend based on health status
	backendURL := s.selectHealthyBackend()
	if backendURL == "" {
		// This is a serious error - no backends available
		s.logger.Error("No healthy backends available",
			zap.String("clientID", client.ID))
		return
	}

	// Setup backend connection
	s.logger.Debug("Establishing backend connection",
		zap.String("clientID", client.ID),
		zap.String("backend", backendURL),
	)

	// Connect to backend
	backendConn, resp, err := s.connectToBackend(backendURL, client)
	if err != nil {
		s.logger.Error("Failed to connect to backend",
			zap.String("clientID", client.ID),
			zap.String("backend", backendURL),
			zap.Error(err),
		)
		// Enhanced error logging
		if resp != nil {
			s.logger.Error("Backend response details",
				zap.Int("statusCode", resp.StatusCode),
				zap.String("status", resp.Status),
				zap.Any("headers", resp.Header),
			)
		}
		return // will close the client connection
	}

	// Store the backend connection
	client.BackendConn = backendConn
	client.BackendAddr = backendURL
	client.CurrentBackend = backendURL

	s.logger.Info("Established backend connection",
		zap.String("clientID", client.ID),
		zap.String("backend", backendURL),
	)

	// Set up domain mapping for this client
	// Extract target domain from backend URL
	targetDomain := ""
	if strings.Contains(client.CurrentBackend, "qa.qalqul.io") {
		targetDomain = "qa.qalqul.io"
	} else if strings.Contains(client.CurrentBackend, "businessoperations.qalqul.io") {
		targetDomain = "businessoperations.qalqul.io"
	}

	// Create domain mapping
	if targetDomain != "" {
		// Map both platform and businessoperations domains to the target domain
		s.domainProxy.SetDomainMapping(client.ID, "platform.qalqul.io", targetDomain)
		s.domainProxy.SetDomainMapping(client.ID, "businessoperations.qalqul.io", targetDomain)

		s.logger.Info("Set up domain mappings for client",
			zap.String("clientID", client.ID),
			zap.String("targetDomain", targetDomain))
	}

	//-------------------------------------------------------------------
	// 1) Keepalive: SIP OPTIONS to the WebSocket client
	//-------------------------------------------------------------------
	clientOptionsTicker := time.NewTicker(30 * time.Second)
	defer clientOptionsTicker.Stop()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-ctx.Done():
				s.logger.Debug("Keepalive routine stopping - context done",
					zap.String("client", client.ID),
				)
				return
			case <-client.clientDone:
				s.logger.Debug("Keepalive routine stopping - client done",
					zap.String("client", client.ID),
				)
				return
			case <-clientOptionsTicker.C:
				// Build & send SIP OPTIONS to the WebSocket client
				optionsMsg := s.buildSipOptionsKeepAlive(client.RemoteAddr, backendURL)

				client.closeMu.Lock()
				if client.closed {
					client.closeMu.Unlock()
					return
				}
				if err := client.Conn.WriteMessage(websocket.TextMessage, []byte(optionsMsg)); err != nil {
					s.logger.Debug("Failed to send SIP OPTIONS keepalive to client",
						zap.String("error", err.Error()),
						zap.String("client", client.ID),
					)
					client.closeMu.Unlock()
					return
				}
				client.closeMu.Unlock()

				s.logger.Debug("Sent SIP OPTIONS keepalive to client",
					zap.String("client", client.ID),
				)
			}
		}
	}()

	//-------------------------------------------------------------------
	// 2) Process messages from client to backend
	//-------------------------------------------------------------------
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(client.clientDone)

		for {
			select {
			case <-ctx.Done():
				s.logger.Debug("Client reader stopping - context done",
					zap.String("client", client.ID),
				)
				return
			default:
				// Read message with a reasonable deadline
				client.Conn.SetReadDeadline(time.Now().Add(2 * time.Hour))
				msgType, msg, err := client.Conn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						s.logger.Debug("Client closed connection normally",
							zap.String("client", client.ID),
						)
					} else {
						s.logger.Debug("WebSocket read error",
							zap.String("client", client.ID),
							zap.Error(err),
						)
						errChan <- fmt.Errorf("client read: %w", err)
					}
					return
				}

				// Update activity timestamp
				client.LastActivity = time.Now()

				msgTypeStr := msgTypeToString(msgType)
				s.logger.Debug("Received message from client",
					zap.String("client", client.ID),
					zap.String("msgType", msgTypeStr),
					zap.Int("length", len(msg)),
				)

				// Extract SIP state for session tracking (if text or binary message)
				if (msgType == websocket.TextMessage || msgType == websocket.BinaryMessage) && len(msg) > 0 {
					sipMsg, parseErr := sip.ParseMessage(msg)
					if parseErr == nil && sipMsg != nil {
						s.extractSIPState(sipMsg, client)
					}
				}
				// Add this code to rewrite domains before forwarding to backend
				if msgType == websocket.TextMessage && len(msg) > 0 {
					// Rewrite the domains in the message
					msg = s.domainProxy.RewriteClientToBackend(client.ID, msg)
				}

				// Forward to backend if it exists
				if client.BackendConn != nil {
					err = client.BackendConn.WriteMessage(msgType, msg)
					if err != nil {
						s.logger.Error("Failed to forward message to backend",
							zap.String("client", client.ID),
							zap.String("backend", client.CurrentBackend),
							zap.Error(err),
						)

						// Increment error count
						client.ErrorCount++

						// Check if we need to failover based on threshold
						if client.ErrorCount >= s.config.FailoverThreshold {
							s.logger.Warn("Backend error threshold reached, attempting failover",
								zap.String("clientID", client.ID),
								zap.String("currentBackend", client.CurrentBackend),
								zap.Int("errorCount", client.ErrorCount),
								zap.Int("threshold", s.config.FailoverThreshold))

							// Mark current backend as unhealthy
							s.UpdateBackendHealth(client.CurrentBackend, false)

							// Try to connect to a new backend
							newBackendURL := s.selectHealthyBackend()
							if newBackendURL != "" && newBackendURL != client.CurrentBackend {
								newConn, _, connErr := s.connectToBackend(newBackendURL, client)
								if connErr == nil {
									s.logger.Info("Successfully connected to new backend after failover",
										zap.String("clientID", client.ID),
										zap.String("oldBackend", client.CurrentBackend),
										zap.String("newBackend", newBackendURL))

									// Close old connection
									client.BackendConn.Close()

									// Update connection info
									client.BackendConn = newConn
									client.BackendAddr = newBackendURL
									client.CurrentBackend = newBackendURL
									client.ErrorCount = 0

									// Retry sending the message
									if retryErr := newConn.WriteMessage(msgType, msg); retryErr != nil {
										s.logger.Error("Failed to forward message to new backend after failover",
											zap.String("clientID", client.ID),
											zap.Error(retryErr))

										// Continue despite error - next message might succeed
									} else {
										s.logger.Debug("Successfully forwarded message to new backend after failover",
											zap.String("clientID", client.ID),
											zap.String("backend", newBackendURL))
									}

									// Continue with new backend
									continue
								} else {
									s.logger.Error("Failed to connect to new backend during failover",
										zap.String("clientID", client.ID),
										zap.String("newBackend", newBackendURL),
										zap.Error(connErr))
								}
							} else {
								s.logger.Error("No alternative backend available for failover",
									zap.String("clientID", client.ID),
									zap.String("currentBackend", client.CurrentBackend))
							}
						}

						// Continue despite error - connection will be closed if needed
						errChan <- fmt.Errorf("backend write: %w", err)
						return
					}

					// Reset error count on successful message
					client.ErrorCount = 0

					s.logger.Debug("Forwarded message to backend",
						zap.String("client", client.ID),
						zap.String("backend", client.BackendAddr),
						zap.Int("length", len(msg)),
					)
				}

				// Process the message if SIP
				if (msgType == websocket.TextMessage || msgType == websocket.BinaryMessage) && len(msg) > 0 {
					sipMsg, parseErr := sip.ParseMessage(msg)
					if parseErr == nil {
						// Extract SIP address if available
						if req, ok := sipMsg.(*sip.Request); ok {
							if req.From() != nil {
								client.SIPAddress = req.From().Address.String()

								// THIS IS THE FIX: Check DisableWSSIPProcessing specifically for WebSocket SIP processing
								if s.handler != nil && !s.config.DisableWSSIPProcessing {
									// create virtual address for the client
									addr := &websocketAddr{
										clientID: client.ID,
										network:  sipTransport,
										address:  client.RemoteAddr,
									}

									s.logger.Debug("Processing SIP message from WebSocket",
										zap.String("clientID", client.ID),
										zap.String("method", req.Method.String()),
										zap.Bool("DisableWSSIPProcessing", s.config.DisableWSSIPProcessing))

									if err := s.handler.HandleMessage(sipMsg, addr); err != nil {
										// Log error but don't fail connection
										s.logger.Error("SIP handler error",
											zap.String("clientID", client.ID),
											zap.Error(err),
										)
									}
								} else {
									s.logger.Debug("Skipping SIP processing (disabled by config)",
										zap.String("clientID", client.ID),
										zap.Bool("DisableWSSIPProcessing", s.config.DisableWSSIPProcessing))
								}
							}
						}
					}
				}
			}
		}
	}()

	//-------------------------------------------------------------------
	// 3) Process messages from backend to client
	//-------------------------------------------------------------------
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(client.backendDone)

		for {
			select {
			case <-ctx.Done():
				s.logger.Debug("Backend reader stopping - context done",
					zap.String("client", client.ID),
				)
				return
			case <-client.clientDone:
				s.logger.Debug("Backend reader stopping - client done",
					zap.String("client", client.ID),
				)
				return
			default:
				// Read message from backend
				client.BackendConn.SetReadDeadline(time.Now().Add(2 * time.Hour))
				msgType, msg, err := client.BackendConn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						s.logger.Debug("Backend closed connection normally",
							zap.String("client", client.ID),
							zap.String("backend", client.BackendAddr),
						)
					} else {
						s.logger.Debug("Backend read error",
							zap.String("client", client.ID),
							zap.String("backend", client.BackendAddr),
							zap.Error(err),
						)

						// Increment error count
						client.ErrorCount++

						// Check if we need to failover based on threshold
						if client.ErrorCount >= s.config.FailoverThreshold {
							s.logger.Warn("Backend read error threshold reached, attempting failover",
								zap.String("clientID", client.ID),
								zap.String("currentBackend", client.CurrentBackend),
								zap.Int("errorCount", client.ErrorCount),
								zap.Int("threshold", s.config.FailoverThreshold))

							// Mark current backend as unhealthy
							s.UpdateBackendHealth(client.CurrentBackend, false)

							// Try to connect to a new backend
							newBackendURL := s.selectHealthyBackend()
							if newBackendURL != "" && newBackendURL != client.CurrentBackend {
								newConn, _, connErr := s.connectToBackend(newBackendURL, client)
								if connErr == nil {
									s.logger.Info("Successfully connected to new backend after read error",
										zap.String("clientID", client.ID),
										zap.String("oldBackend", client.CurrentBackend),
										zap.String("newBackend", newBackendURL))

									// Update connection info
									client.BackendConn = newConn
									client.BackendAddr = newBackendURL
									client.CurrentBackend = newBackendURL
									client.ErrorCount = 0

									// Continue with the new backend
									continue
								}
							}
						}

						errChan <- fmt.Errorf("backend read: %w", err)
					}
					return
				}

				// Process the message if SIP (before forwarding to client)
				if (msgType == websocket.TextMessage || msgType == websocket.BinaryMessage) && len(msg) > 0 {
					sipMsg, parseErr := sip.ParseMessage(msg)
					if parseErr == nil {
						// Extract SIP state for session tracking
						s.extractSIPState(sipMsg, client)

						// Check if it's a CANCEL or BYE message - these must be forwarded
						isCriticalMethod := false
						if req, ok := sipMsg.(*sip.Request); ok {
							method := req.Method.String()
							if method == "CANCEL" || method == "BYE" {
								isCriticalMethod = true
								s.logger.Debug("Critical SIP method detected from backend",
									zap.String("clientID", client.ID),
									zap.String("method", method),
									zap.String("backend", client.CurrentBackend))
							}
						}

						// Add SIP processing for messages from backend to client
						if (s.handler != nil && !s.config.DisableWSSIPProcessing) || isCriticalMethod {
							// Create virtual address for the backend
							addr := &websocketAddr{
								clientID: "backend-" + client.CurrentBackend,
								network:  "wss",
								address:  client.CurrentBackend,
							}

							s.logger.Debug("Processing SIP message from backend",
								zap.String("clientID", client.ID),
								zap.String("backend", client.CurrentBackend),
								zap.String("msgType", msgTypeToString(msgType)))

							if resp, ok := sipMsg.(*sip.Response); ok {
								s.logger.Debug("SIP response from backend",
									zap.String("clientID", client.ID),
									zap.Int("statusCode", resp.StatusCode),
									zap.String("reason", resp.Reason))
							}

							// Pass through SIP handler
							if err := s.handler.HandleMessage(sipMsg, addr); err != nil {
								// Log error but don't fail connection
								s.logger.Error("SIP handler error for backend message",
									zap.String("clientID", client.ID),
									zap.String("backend", client.CurrentBackend),
									zap.Error(err))
							}
						} else {
							s.logger.Debug("Skipping SIP processing for backend message (disabled by config)",
								zap.String("clientID", client.ID),
								zap.Bool("DisableWSSIPProcessing", s.config.DisableWSSIPProcessing))
						}
					} else {
						s.logger.Debug("Failed to parse SIP message from backend",
							zap.String("clientID", client.ID),
							zap.String("backend", client.CurrentBackend),
							zap.Error(parseErr))
					}
				}

				// Forward to client
				client.closeMu.Lock()
				if client.closed {
					client.closeMu.Unlock()
					return
				}
				err = client.Conn.WriteMessage(msgType, msg)
				client.closeMu.Unlock()
				if err != nil {
					s.logger.Debug("Failed to forward backend message to client",
						zap.String("client", client.ID),
						zap.Error(err),
					)
					errChan <- fmt.Errorf("client write: %w", err)
					return
				}

				s.logger.Debug("Forwarded message from backend to client",
					zap.String("client", client.ID),
					zap.String("msgType", msgTypeToString(msgType)),
					zap.Int("length", len(msg)),
				)
			}
		}
	}()

	// Wait for first error or context cancellation
	select {
	case <-ctx.Done():
		s.logger.Debug("Context done, closing WebSocket",
			zap.String("client", client.ID),
		)
		return
	case err := <-errChan:
		s.logger.Debug("WebSocket error, closing connection",
			zap.String("client", client.ID),
			zap.Error(err),
		)
		return
	}
}

// connectToBackend establishes a connection to a backend server
func (s *Server) connectToBackend(backendURL string, client *ClientConnection) (*websocket.Conn, *http.Response, error) {
	// Ensure backendURL starts with ws:// or wss://
	if !strings.HasPrefix(backendURL, "ws") {
		backendURL = "wss://" + backendURL
	}

	// Create proper headers for backend connection
	backendHeaders := http.Header{}
	host := strings.Split(strings.TrimPrefix(strings.TrimPrefix(backendURL, "wss://"), "ws://"), ":")[0]
	backendHeaders.Add("Origin", "https://"+host)
	backendHeaders.Add("Host", host)
	backendHeaders.Add("User-Agent", "QalqulVoiceGateway/1.0")

	// Add SIP session info if available for session continuity during failover
	if client.LastSIPCallID != "" {
		backendHeaders.Add("X-SIP-Call-ID", client.LastSIPCallID)
	}
	if client.LastSIPFromTag != "" {
		backendHeaders.Add("X-SIP-From-Tag", client.LastSIPFromTag)
	}
	if client.LastSIPToTag != "" {
		backendHeaders.Add("X-SIP-To-Tag", client.LastSIPToTag)
	}

	s.logger.Debug("Connecting to backend with headers",
		zap.String("backendURL", backendURL),
		zap.Any("headers", backendHeaders),
	)

	// Create dialer with TLS config
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // consider making configurable
		},
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{client.Protocol}, // match client subprotocol
	}

	fmt.Fprintf(os.Stderr, "CONSOLE: Attempting backend connection to %s\n", backendURL)

	// Connect to backend
	conn, resp, err := dialer.Dial(backendURL, backendHeaders)
	if err != nil {
		fmt.Fprintf(os.Stderr, "CONSOLE ERROR: Backend connection failed: %v\n", err)
		return nil, resp, err
	}

	return conn, resp, nil
}

// extractSIPState extracts key session identifiers from SIP messages
func (s *Server) extractSIPState(sipMsg sip.Message, client *ClientConnection) {
	if req, ok := sipMsg.(*sip.Request); ok {
		// Extract Call-ID
		if callID := req.CallID(); callID != nil {
			client.LastSIPCallID = callID.Value()
		}

		// Extract From tag
		if from := req.From(); from != nil && from.Params != nil {
			if tag, exists := from.Params.Get("tag"); exists && tag != "" {
				client.LastSIPFromTag = tag
			}
		}

		// Extract To tag
		if to := req.To(); to != nil && to.Params != nil {
			if tag, exists := to.Params.Get("tag"); exists && tag != "" {
				client.LastSIPToTag = tag
			}
		}

		// Store SIP identity in storage for recovery purposes
		if client.LastSIPCallID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			stateKey := fmt.Sprintf("sip:session:%s", client.LastSIPCallID)
			stateData := map[string]string{
				"clientID":  client.ID,
				"callID":    client.LastSIPCallID,
				"fromTag":   client.LastSIPFromTag,
				"toTag":     client.LastSIPToTag,
				"backend":   client.CurrentBackend,
				"timestamp": fmt.Sprintf("%d", time.Now().Unix()),
			}

			jsonBytes, err := json.Marshal(stateData)
			if err == nil {
				s.storage.Set(ctx, stateKey, jsonBytes, 1*time.Hour)
			}
		}
	}
}

// Implementation of net.Addr for WebSocket clients
type websocketAddr struct {
	clientID string
	network  string
	address  string
}

func (a *websocketAddr) Network() string {
	return a.network
}

func (a *websocketAddr) String() string {
	return a.address
}

// SendMessage sends a message to a WebSocket client
func (s *Server) SendMessage(ctx context.Context, clientID string, msg []byte) error {
	ctx, cancel := common.QuickTimeout(context.Background())
	defer cancel()

	s.mu.RLock()
	client, ok := s.connections[clientID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("client not found: %s", clientID)
	}

	client.closeMu.Lock()
	defer client.closeMu.Unlock()

	if client.closed {
		return fmt.Errorf("connection closed")
	}

	client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return client.Conn.WriteMessage(websocket.TextMessage, msg)
}

// PrepareForFailover notifies clients to prepare for potential failover
func (s *Server) PrepareForFailover() {
	s.logger.Info("Preparing clients for potential failover")

	s.mu.RLock()
	clients := make([]*ClientConnection, 0, len(s.connections))
	for _, client := range s.connections {
		clients = append(clients, client)
	}
	s.mu.RUnlock()

	// Generate a failover notification message
	failoverData := map[string]interface{}{
		"event":     "system-notification",
		"type":      "failover-preparation",
		"message":   "Server maintenance imminent, please prepare for reconnection",
		"timestamp": time.Now().Unix(),
	}

	jsonBytes, err := json.Marshal(failoverData)
	if err != nil {
		s.logger.Error("Failed to encode failover message", zap.Error(err))
		return
	}

	// Notify each client
	for _, client := range clients {
		client.closeMu.Lock()
		if !client.closed {
			client.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := client.Conn.WriteMessage(websocket.TextMessage, jsonBytes)
			if err != nil {
				s.logger.Warn("Failed to send failover notification",
					zap.String("clientID", client.ID),
					zap.Error(err),
				)
			}
		}
		client.closeMu.Unlock()
	}
}

// RedirectClient instructs a specific client to reconnect to a new server
func (s *Server) RedirectClient(clientID string, newServerAddr string) error {
	s.mu.RLock()
	client, ok := s.connections[clientID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("client not found: %s", clientID)
	}

	// Create redirect message
	redirectData := map[string]interface{}{
		"event":      "system-redirect",
		"serverAddr": newServerAddr,
		"timestamp":  time.Now().Unix(),
	}

	jsonBytes, err := json.Marshal(redirectData)
	if err != nil {
		return fmt.Errorf("failed to encode redirect message: %w", err)
	}

	// Send the message
	client.closeMu.Lock()
	defer client.closeMu.Unlock()

	if client.closed {
		return fmt.Errorf("connection closed")
	}

	client.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return client.Conn.WriteMessage(websocket.TextMessage, jsonBytes)
}

// StartHealthMonitoring begins monitoring this WebSocket server's health
func (s *Server) StartHealthMonitoring(ctx context.Context) {
	s.registry.Go("ws-health-monitor", func(ctx context.Context) {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Update connection count metric
				s.mu.RLock()
				connectionCount := len(s.connections)
				s.mu.RUnlock()

				// Store health info in storage for other instances
				healthData := map[string]interface{}{
					"addr":        s.config.BindAddr,
					"connections": connectionCount,
					"timestamp":   time.Now().Unix(),
					"status":      "healthy",
				}

				healthBytes, err := json.Marshal(healthData)
				if err != nil {
					s.logger.Error("Failed to encode health data", zap.Error(err))
					continue
				}

				hostname, _ := os.Hostname()
				healthKey := fmt.Sprintf("ws-health:%s", hostname)

				storeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err = s.storage.Set(storeCtx, healthKey, healthBytes, 30*time.Second)
				cancel()

				if err != nil {
					s.logger.Error("Failed to store health data", zap.Error(err))
				}
			}
		}
	})
}

// HasBackends returns true if there are backend servers configured
func (s *Server) HasBackends() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.config.BackendServers) > 0
}

// HasHealthyBackends returns true if at least one backend is healthy
func (s *Server) HasHealthyBackends() bool {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()

	// Check if there are any backends configured
	if len(s.backendHealth) == 0 {
		return false
	}

	// Check if at least one backend is healthy
	for _, healthy := range s.backendHealth {
		if healthy {
			return true
		}
	}

	return false
}

// closeConnection closes a client connection
func (s *Server) closeConnection(client *ClientConnection) {
	client.closeMu.Lock()
	if client.closed {
		client.closeMu.Unlock()
		return
	}
	client.closed = true

	// Cancel the connection context
	if client.cancelFunc != nil {
		client.cancelFunc()
	}

	client.closeMu.Unlock()

	// Send close message
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Connection closed")
	client.Conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(5*time.Second))

	// Close the WebSocket
	client.Conn.Close()

	// Close backend connection
	if client.BackendConn != nil {
		client.BackendConn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(5*time.Second))
		client.BackendConn.Close()
	}

	// Remove from connections map
	s.mu.Lock()
	delete(s.connections, client.ID)
	s.mu.Unlock()

	// Clean up storage
	ctx := context.Background()
	s.storage.Delete(ctx, "ws:"+client.ID)

	s.logger.Info("WebSocket connection closed",
		zap.String("clientID", client.ID),
		zap.String("remoteAddr", client.RemoteAddr),
	)
}

// closeAllConnections closes all active connections
func (s *Server) closeAllConnections() {
	s.mu.Lock()
	clients := make([]*ClientConnection, 0, len(s.connections))
	for _, client := range s.connections {
		clients = append(clients, client)
	}
	s.mu.Unlock()

	s.logger.Info("Closing all WebSocket connections",
		zap.Int("connectionCount", len(clients)),
	)

	for _, client := range clients {
		s.closeConnection(client)
	}
}

// cleanConnections removes inactive connections
func (s *Server) cleanConnections() {
	now := time.Now()
	threshold := now.Add(-30 * time.Minute)

	s.mu.Lock()
	var toClose []*ClientConnection

	for _, client := range s.connections {
		if client.LastActivity.Before(threshold) {
			toClose = append(toClose, client)
		}
	}
	s.mu.Unlock()

	for _, client := range toClose {
		s.logger.Warn("Closing inactive WebSocket connection",
			zap.String("clientID", client.ID),
			zap.Time("lastActivity", client.LastActivity),
			zap.Duration("inactive", now.Sub(client.LastActivity)),
		)
		s.closeConnection(client)
	}
}

// buildSipOptionsKeepAlive generates a SIP OPTIONS message for keepalive
func (s *Server) buildSipOptionsKeepAlive(clientAddr string, serverName string) string {
	callID := fmt.Sprintf("keepalive-%d", time.Now().UnixNano())
	branch := fmt.Sprintf("z9hG4bK-%d", time.Now().UnixNano())
	fromTag := fmt.Sprintf("fromTag-%d", time.Now().UnixNano())

	return fmt.Sprintf(
		`OPTIONS sip:keepalive@%s SIP/2.0
Via: SIP/2.0/WSS %s;branch=%s;rport
Max-Forwards: 70
From: <sip:monitor@%s>;tag=%s
To: <sip:keepalive@%s>
Call-ID: %s
CSeq: 1 OPTIONS
User-Agent: %s
Allow: INVITE, ACK, CANCEL, OPTIONS, BYE, REFER, SUBSCRIBE, NOTIFY, INFO, PUBLISH, MESSAGE
Supported: replaces, timer
Content-Length: 0

`,
		clientAddr,
		serverName,
		branch,
		serverName,
		fromTag,
		clientAddr,
		callID,
		s.config.ServerName,
	)
}

// msgTypeToString returns a string for the WebSocket message type
func msgTypeToString(mt int) string {
	switch mt {
	case websocket.TextMessage:
		return "text"
	case websocket.BinaryMessage:
		return "binary"
	case websocket.CloseMessage:
		return "close"
	case websocket.PingMessage:
		return "ping"
	case websocket.PongMessage:
		return "pong"
	default:
		return "unknown"
	}
}

// handleHealthCheck handles health check requests
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("Received health check request",
		zap.String("remoteAddr", r.RemoteAddr),
		zap.String("userAgent", r.UserAgent()))

	// Check components health
	allBackendsHealthy := s.HasHealthyBackends()

	// Check storage health
	storageHealthy := s.checkStorageHealth()

	// Get connection count
	s.mu.RLock()
	connectionCount := len(s.connections)
	clientStats := s.getClientStats()
	s.mu.RUnlock()

	// Get backend health details
	s.backendMu.RLock()
	backendStatus := make(map[string]map[string]interface{})
	for backend, healthy := range s.backendHealth {
		backendStatus[backend] = map[string]interface{}{
			"healthy":      healthy,
			"last_checked": time.Now().Format(time.RFC3339),
		}
	}
	s.backendMu.RUnlock()

	// Determine overall status
	status := "healthy"
	statusCode := http.StatusOK

	if !allBackendsHealthy {
		status = "unhealthy"
		statusCode = http.StatusServiceUnavailable
	}

	// Create detailed response
	response := map[string]interface{}{
		"status":      status,
		"connections": connectionCount,
		"timestamp":   time.Now().Format(time.RFC3339),
		"backends":    backendStatus,
		"storage": map[string]interface{}{
			"healthy": storageHealthy,
			"type":    "memory", // Change to "redis" if using Redis
		},
		"service":        "WebRTC-SIP Gateway",
		"version":        "1.0",
		"uptime_seconds": int(time.Since(s.startTime).Seconds()),
		"client_stats":   clientStats,
		"is_leader":      s.isLeader,
		"health_details": map[string]interface{}{
			"websocket_server": map[string]interface{}{
				"status": status,
				"reason": getHealthReason(allBackendsHealthy, storageHealthy),
			},
		},
	}

	// Send JSON response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("Failed to encode health response",
			zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

// checkStorageHealth checks if the storage is healthy
func (s *Server) checkStorageHealth() bool {
	if s.storage == nil {
		return false
	}

	// Simple health check - write and read a test value
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	testKey := "health_check_test"
	testValue := []byte(fmt.Sprintf("test_%d", time.Now().UnixNano()))

	err := s.storage.Set(ctx, testKey, testValue, 5*time.Second)
	if err != nil {
		s.logger.Warn("Storage health check failed - set operation failed",
			zap.Error(err))
		return false
	}

	_, err = s.storage.Get(ctx, testKey)
	if err != nil {
		s.logger.Warn("Storage health check failed - get operation failed",
			zap.Error(err))
		return false
	}

	return true
}

// getClientStats returns statistics about connected clients
func (s *Server) getClientStats() map[string]interface{} {
	stats := map[string]interface{}{
		"active":            0,
		"total_since_start": s.totalConnections,
		"registrations":     0,
	}

	// Count active connections
	stats["active"] = len(s.connections)

	// Count SIP registrations if available
	registrations := 0
	for _, client := range s.connections {
		if client.SIPAddress != "" {
			registrations++
		}
	}
	stats["registrations"] = registrations

	return stats
}

// getHealthReason returns a reason string explaining the health status
func getHealthReason(backendsHealthy, storageHealthy bool) string {
	var reasons []string

	if !backendsHealthy {
		reasons = append(reasons, "no healthy backends available")
	}

	if !storageHealthy {
		reasons = append(reasons, "storage not healthy")
	}

	if len(reasons) == 0 {
		return "all systems operational"
	}

	return strings.Join(reasons, ", ")
}

// GetHTTPHandler returns the HTTP handler for the WebSocket server
// so that additional endpoints can be registered.
func (s *Server) GetHTTPHandler() *http.ServeMux {
	return s.mux
}
