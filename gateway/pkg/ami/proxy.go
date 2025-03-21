// pkg/ami/proxy.go
package ami

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// AMIProxyServer handles proxying between clients and Asterisk
type AMIProxyServer struct {
	listeners      map[string]net.Listener
	backendManager *BackendManager
	clients        map[net.Conn]clientInfo
	clientsMutex   sync.Mutex
	logger         *zap.Logger
	stopChan       chan struct{}
	coordinator    interface {
		IsLeader(components ...string) bool
	}
	running      bool
	runningMutex sync.Mutex
}

type clientInfo struct {
	reader        *bufio.Reader
	writer        *bufio.Writer
	authenticated bool
	username      string
	sourceAddr    string
}

// ProxyConfig holds configuration for the AMI proxy
type ProxyConfig struct {
	OriginalAddresses []string
	AmiConfigs        []Config
}

// NewProxyServer creates a new AMI proxy server
func NewProxyServer(config ProxyConfig, coordinator interface {
	IsLeader(components ...string) bool
}, logger *zap.Logger) (*AMIProxyServer, error) {
	backendManager, err := NewBackendManager(config.AmiConfigs, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create backend manager: %w", err)
	}

	server := &AMIProxyServer{
		listeners:      make(map[string]net.Listener),
		backendManager: backendManager,
		clients:        make(map[net.Conn]clientInfo),
		logger:         logger,
		stopChan:       make(chan struct{}),
		coordinator:    coordinator,
		running:        false,
	}

	// Create listeners for all original addresses
	for _, addr := range config.OriginalAddresses {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			// Just log an error but continue with other addresses
			logger.Error("Failed to listen on original AMI address",
				zap.String("address", addr),
				zap.Error(err))
			continue
		}
		server.listeners[addr] = listener
		logger.Info("Created AMI proxy listener", zap.String("address", addr))
	}

	if len(server.listeners) == 0 {
		return nil, fmt.Errorf("failed to create any listeners for original AMI addresses")
	}

	return server, nil
}

// Start begins accepting connections on all listeners
func (s *AMIProxyServer) Start() error {
	s.runningMutex.Lock()
	if s.running {
		s.runningMutex.Unlock()
		return nil
	}
	s.running = true
	s.runningMutex.Unlock()

	// Start accept loops for each listener
	for addr, listener := range s.listeners {
		go s.acceptLoop(addr, listener)
	}

	s.logger.Info("AMI proxy server started",
		zap.Int("listenerCount", len(s.listeners)))
	return nil
}

// Stop shuts down the server
func (s *AMIProxyServer) Stop() error {
	s.runningMutex.Lock()
	if !s.running {
		s.runningMutex.Unlock()
		return nil
	}
	s.running = false
	s.runningMutex.Unlock()

	close(s.stopChan)

	// Close all listeners
	for addr, listener := range s.listeners {
		if err := listener.Close(); err != nil {
			s.logger.Error("Error closing listener",
				zap.String("address", addr),
				zap.Error(err))
		}
	}

	// Close all client connections
	s.clientsMutex.Lock()
	for conn := range s.clients {
		conn.Close()
	}
	s.clientsMutex.Unlock()

	s.logger.Info("AMI proxy server stopped")
	return nil
}

// acceptLoop accepts new connections from a specific listener
func (s *AMIProxyServer) acceptLoop(addr string, listener net.Listener) {
	s.logger.Info("Starting accept loop for AMI address", zap.String("address", addr))

	for {
		select {
		case <-s.stopChan:
			return
		default:
			// Only accept connections if we're the leader
			if !s.coordinator.IsLeader("ami") {
				time.Sleep(1 * time.Second)
				continue
			}

			conn, err := listener.Accept()
			if err != nil {
				// Check if server is shutting down
				select {
				case <-s.stopChan:
					return
				default:
					s.logger.Error("Error accepting connection",
						zap.String("address", addr),
						zap.Error(err))
					time.Sleep(100 * time.Millisecond)
					continue
				}
			}

			// Register new client with the source address
			s.clientsMutex.Lock()
			s.clients[conn] = clientInfo{
				reader:     bufio.NewReader(conn),
				writer:     bufio.NewWriter(conn),
				sourceAddr: addr,
			}
			s.clientsMutex.Unlock()

			// Handle client in a goroutine
			go s.handleClient(conn)
		}
	}
}

// handleClient processes a client connection
func (s *AMIProxyServer) handleClient(conn net.Conn) {
	defer func() {
		conn.Close()
		s.clientsMutex.Lock()
		delete(s.clients, conn)
		s.clientsMutex.Unlock()
		s.logger.Debug("Client disconnected", zap.String("remote", conn.RemoteAddr().String()))
	}()

	// Send initial AMI banner
	s.clientsMutex.Lock()
	info := s.clients[conn]
	s.clientsMutex.Unlock()

	banner := "Asterisk Call Manager/1.4\r\n"
	_, err := info.writer.WriteString(banner)
	if err != nil {
		s.logger.Error("Failed to send banner", zap.Error(err))
		return
	}
	info.writer.Flush()

	// Register event handler for this client
	activeBackend := s.backendManager.GetActiveClient()
	clientConn := conn // Capture for closure

	// Register wildcard handler to forward all events to this client
	activeBackend.RegisterEventHandler("*", func(event map[string]string) {
		s.clientsMutex.Lock()
		if clientInfo, exists := s.clients[clientConn]; exists {
			// Only send events to authenticated clients
			if !clientInfo.authenticated {
				s.clientsMutex.Unlock()
				return
			}

			// Format event as AMI message
			var buf strings.Builder
			for k, v := range event {
				buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
			}
			buf.WriteString("\r\n")

			_, err := clientInfo.writer.WriteString(buf.String())
			if err == nil {
				clientInfo.writer.Flush()
			}
		}
		s.clientsMutex.Unlock()
	})

	// Main client loop - read and process commands
	for {
		// Read client request (action)
		action, err := s.readAction(conn)
		if err != nil {
			s.logger.Debug("Client connection closed",
				zap.String("remote", conn.RemoteAddr().String()),
				zap.Error(err))
			return
		}

		// Handle login action specially
		if strings.EqualFold(action["Action"], "login") {
			s.handleLoginAction(conn, action)
			continue
		}

		// Check if client is authenticated
		s.clientsMutex.Lock()
		authenticated := s.clients[conn].authenticated
		s.clientsMutex.Unlock()

		if !authenticated {
			s.sendErrorResponse(conn, "Not authenticated")
			continue
		}

		// Forward action to backend
		response, err := s.backendManager.SendAction(action)
		if err != nil {
			s.sendErrorResponse(conn, err.Error())
			continue
		}

		// Send response to client
		s.sendResponse(conn, response)
	}
}

// readAction reads an AMI action from the client
func (s *AMIProxyServer) readAction(conn net.Conn) (map[string]string, error) {
	s.clientsMutex.Lock()
	info, exists := s.clients[conn]
	if !exists {
		s.clientsMutex.Unlock()
		return nil, fmt.Errorf("client not found")
	}
	reader := info.reader
	s.clientsMutex.Unlock()

	action := make(map[string]string)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break // End of action
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue // Invalid line
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		action[key] = value
	}

	return action, nil
}

// handleLoginAction processes a login action
func (s *AMIProxyServer) handleLoginAction(conn net.Conn, action map[string]string) {
	username := action["Username"]
	secret := action["Secret"]

	if username == "" || secret == "" {
		s.sendErrorResponse(conn, "Missing credentials")
		return
	}

	// Forward login to backend to verify credentials
	activeClient := s.backendManager.GetActiveClient()

	// Create a new login action for the backend
	loginAction := map[string]string{
		"Action":   "Login",
		"Username": username,
		"Secret":   secret,
	}

	response, err := activeClient.SendAction(loginAction)
	if err != nil || response["Response"] != "Success" {
		s.sendErrorResponse(conn, "Authentication failed")
		return
	}

	// Mark client as authenticated
	s.clientsMutex.Lock()
	info := s.clients[conn]
	info.authenticated = true
	info.username = username
	s.clients[conn] = info
	s.clientsMutex.Unlock()

	// Send success response
	s.sendResponse(conn, map[string]string{
		"Response": "Success",
		"Message":  "Authentication accepted",
	})
}

// sendErrorResponse sends an error response to the client
func (s *AMIProxyServer) sendErrorResponse(conn net.Conn, message string) {
	s.sendResponse(conn, map[string]string{
		"Response": "Error",
		"Message":  message,
	})
}

// sendResponse sends a response to the client
func (s *AMIProxyServer) sendResponse(conn net.Conn, response map[string]string) {
	s.clientsMutex.Lock()
	info, exists := s.clients[conn]
	if !exists {
		s.clientsMutex.Unlock()
		return
	}
	writer := info.writer
	s.clientsMutex.Unlock()

	// Format response
	var buf strings.Builder
	for k, v := range response {
		buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	buf.WriteString("\r\n")

	_, err := writer.WriteString(buf.String())
	if err != nil {
		s.logger.Error("Failed to send response", zap.Error(err))
		return
	}
	writer.Flush()
}
