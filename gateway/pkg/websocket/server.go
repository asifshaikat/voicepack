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

	mu          sync.RWMutex
	connections map[string]*ClientConnection
	wg          sync.WaitGroup
}

// ServerConfig defines the configuration for the WebSocket server
type ServerConfig struct {
	BindAddr             string
	CertFile             string
	KeyFile              string
	MaxConnections       int
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
	EnableIPv4Only       bool
	ServerName           string
	DisableSIPProcessing bool        // General flag
	DisableWSSIPProcessing bool      // Specific to WebSocket SIP processing
}

// SIPHandler handles SIP messages from WebSocket clients
type SIPHandler interface {
	HandleMessage(msg sip.Message, addr net.Addr) error
}

// ClientConnection represents a WebSocket client connection
type ClientConnection struct {
	ID           string
	Conn         *websocket.Conn
	RemoteAddr   string
	LocalAddr    string
	Protocol     string
	CreateTime   time.Time
	LastActivity time.Time
	SIPAddress   string
	BackendConn  *websocket.Conn // For proxy scenarios
	BackendAddr  string
	closed       bool
	closeMu      sync.Mutex
	clientDone   chan struct{}
	backendDone  chan struct{}
	cancelFunc   context.CancelFunc // manage connection context
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

	logger.Info("Creating WebSocket server with configuration",
		zap.String("bindAddr", config.BindAddr),
		zap.String("serverName", config.ServerName),
		zap.Bool("disableSIPProcessing", config.DisableSIPProcessing),
		zap.Bool("disableWSSIPProcessing", config.DisableWSSIPProcessing))

	server := &Server{
		config:      config,
		storage:     storage,
		registry:    common.NewGoroutineRegistry(logger),
		logger:      logger,
		connections: make(map[string]*ClientConnection),
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
	}

	return server, nil
}

// SetSIPHandler sets the handler for SIP messages
func (s *Server) SetSIPHandler(handler SIPHandler) {
	s.handler = handler
}

// Start starts the WebSocket server
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("Starting WebSocket server",
		zap.String("bindAddr", s.config.BindAddr),
		zap.Bool("tlsEnabled", s.config.CertFile != "" && s.config.KeyFile != ""),
		zap.String("backendServer", s.config.ServerName),
		zap.Bool("disableWSSIPProcessing", s.config.DisableWSSIPProcessing))

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWebSocket)

	s.logger.Debug("Registered WebSocket handler at root path (/)")

	s.httpServer = &http.Server{
		Addr:         s.config.BindAddr,
		Handler:      mux,
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

	// Setup backend connection
	s.logger.Debug("Establishing backend connection",
		zap.String("clientID", client.ID),
		zap.String("backendServer", s.config.ServerName),
	)

	// Parse the backend URL
	backendURL := s.config.ServerName
	if !strings.HasPrefix(backendURL, "ws") {
		backendURL = "wss://" + backendURL
	}

	// Create proper headers for backend connection
	backendHeaders := http.Header{}

	// Extract host from backend URL for Origin header
	host := strings.Split(strings.TrimPrefix(strings.TrimPrefix(backendURL, "wss://"), "ws://"), ":")[0]

	// Add essential headers for WebSocket handshake
	backendHeaders.Add("Origin", "https://"+host)
	backendHeaders.Add("Host", host)
	backendHeaders.Add("User-Agent", "QalqulVoiceGateway/1.0")

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
	backendConn, resp, err := dialer.Dial(backendURL, backendHeaders)
	if err != nil {
		fmt.Fprintf(os.Stderr, "CONSOLE ERROR: Backend connection failed: %v\n", err)
		s.logger.Error("Failed to connect to SIP backend",
			zap.String("clientID", client.ID),
			zap.String("backendURL", backendURL),
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

	s.logger.Info("Established backend connection",
		zap.String("clientID", client.ID),
		zap.String("backendURL", backendURL),
	)

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
				optionsMsg := s.buildSipOptionsKeepAlive(client.RemoteAddr, s.config.ServerName)

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

				// Forward to backend if it exists
				if client.BackendConn != nil {
					err = client.BackendConn.WriteMessage(msgType, msg)
					if err != nil {
						s.logger.Error("Failed to forward message to backend",
							zap.String("client", client.ID),
							zap.Error(err),
						)
						errChan <- fmt.Errorf("backend write: %w", err)
						return
					}
					s.logger.Debug("Forwarded message to backend",
						zap.String("client", client.ID),
						zap.String("backend", client.BackendAddr),
						zap.Int("length", len(msg)),
					)
				}

				// Process the message if SIP - THE CRITICAL FIX IS HERE
				if msgType == websocket.TextMessage || msgType == websocket.BinaryMessage {
					sipMsg, err := sip.ParseMessage(msg)
					if err == nil {
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
						errChan <- fmt.Errorf("backend read: %w", err)
					}
					return
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