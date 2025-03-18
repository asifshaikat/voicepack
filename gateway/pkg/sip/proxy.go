// pkg/sip/proxy.go
package sip

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/jart/gosip/sip"
	"go.uber.org/zap"

	"gateway/pkg/common"
	"gateway/pkg/rtpengine"
	"gateway/pkg/storage"
)

// Transport defines the interface for SIP transports
type Transport interface {
	Start(ctx context.Context) error
	Stop() error
	Send(msg sip.Message, addr net.Addr) error
	SetHandler(handler TransportHandler)
}

// TransportHandler handles incoming SIP messages
type TransportHandler interface {
	HandleMessage(msg sip.Message, addr net.Addr) error
}

// Proxy implements a stateful SIP proxy
type Proxy struct {
	config         ProxyConfig
	transports     []Transport
	storage        storage.StateStorage
	rtpEngine      rtpengine.Manager
	logger         *zap.Logger
	registry       *common.GoroutineRegistry
	requestHandler RequestHandler
	circuitBreaker *common.CircuitBreaker

	mu       sync.RWMutex
	serverTx map[string]*sip.ServerTransaction
	clientTx map[string]*sip.ClientTransaction
	dialogs  map[string]*Dialog
}

// ProxyConfig defines the configuration for the SIP proxy
type ProxyConfig struct {
	ProxyURI       string
	DefaultNextHop string
	MaxForwards    int
	UserAgent      string
}

// RequestHandler defines the interface for custom SIP request handling
type RequestHandler interface {
	HandleRequest(req *sip.Request, tx *sip.ServerTransaction) error
}

// Dialog represents a SIP dialog
type Dialog struct {
	CallID     string
	FromTag    string
	ToTag      string
	State      string
	Route      []string
	CreateTime time.Time
	UpdateTime time.Time
	ExpireTime time.Time
}

// NewProxy creates a new SIP proxy
func NewProxy(config ProxyConfig, storage storage.StateStorage, rtpEngine rtpengine.Manager, logger *zap.Logger) (*Proxy, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	// Set defaults
	if config.MaxForwards <= 0 {
		config.MaxForwards = 70
	}

	if config.UserAgent == "" {
		config.UserAgent = "WebRTC-SIP Gateway"
	}

	p := &Proxy{
		config:     config,
		transports: make([]Transport, 0),
		storage:    storage,
		rtpEngine:  rtpEngine,
		logger:     logger,
		registry:   common.NewGoroutineRegistry(logger),
		serverTx:   make(map[string]*sip.ServerTransaction),
		clientTx:   make(map[string]*sip.ClientTransaction),
		dialogs:    make(map[string]*Dialog),
		circuitBreaker: common.NewCircuitBreaker("sip-proxy", common.CircuitBreakerConfig{
			FailureThreshold: 5,
			ResetSeconds:     30,
			HalfOpenMax:      3,
		}, logger),
	}

	return p, nil
}

// AddTransport adds a SIP transport to the proxy
func (p *Proxy) AddTransport(transport Transport) {
	transport.SetHandler(p)
	p.transports = append(p.transports, transport)
}

// SetRequestHandler sets a custom request handler
func (p *Proxy) SetRequestHandler(handler RequestHandler) {
	p.requestHandler = handler
}

// Start starts the proxy
func (p *Proxy) Start(ctx context.Context) error {
	if len(p.transports) == 0 {
		return fmt.Errorf("no transports configured")
	}

	// Start all transports
	for _, transport := range p.transports {
		if err := transport.Start(ctx); err != nil {
			return fmt.Errorf("failed to start transport: %w", err)
		}
	}

	// Start dialog cleanup
	p.registry.Go("dialog-cleanup", func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.cleanupDialogs()
			}
		}
	})

	return nil
}

// Stop stops the proxy
func (p *Proxy) Stop() error {
	// Stop all transports
	for _, transport := range p.transports {
		if err := transport.Stop(); err != nil {
			p.logger.Error("Failed to stop transport", zap.Error(err))
		}
	}

	// Stop goroutines
	return p.registry.Shutdown(30 * time.Second)
}

// HandleMessage implements TransportHandler
func (p *Proxy) HandleMessage(msg sip.Message, addr net.Addr) error {
	switch m := msg.(type) {
	case *sip.Request:
		return p.handleRequest(m, addr)
	case *sip.Response:
		return p.handleResponse(m, addr)
	default:
		return fmt.Errorf("unknown message type")
	}
}

// handleRequest processes SIP requests
func (p *Proxy) handleRequest(req *sip.Request, addr net.Addr) error {
	if !p.circuitBreaker.AllowRequest() {
		p.logger.Warn("Circuit breaker open, rejecting request",
			zap.String("method", string(req.Method)),
			zap.String("callID", req.CallID().Value))
		// Send 503 Service Unavailable
		resp := sip.NewResponse(sip.StatusServiceUnavailable, "Service Unavailable")
		resp.SetDestination(addr)
		// Add Retry-After header
		resp.AppendHeader(&sip.RetryAfterHeader{
			Value: "10",
		})
		for _, transport := range p.transports {
			if err := transport.Send(resp, addr); err != nil {
				p.logger.Error("Failed to send 503 response", zap.Error(err))
			}
		}
		return nil
	}

	// Create server transaction
	branchID := req.Via().Params.Get("branch")
	callID := req.CallID().Value
	txKey := fmt.Sprintf("%s:%s", branchID, callID)

	// Create server transaction if it doesn't exist
	p.mu.Lock()
	tx, exists := p.serverTx[txKey]
	if !exists {
		tx = sip.NewServerTransaction(req)
		p.serverTx[txKey] = tx
	}
	p.mu.Unlock()

	// Process the request
	var err error

	// If we have a custom handler, use it
	if p.requestHandler != nil {
		err = p.requestHandler.HandleRequest(req, tx)
	} else {
		// Default handling based on method
		switch req.Method {
		case sip.INVITE:
			err = p.handleInvite(req, tx)
		case sip.ACK:
			err = p.handleAck(req, tx)
		case sip.BYE:
			err = p.handleBye(req, tx)
		case sip.CANCEL:
			err = p.handleCancel(req, tx)
		case sip.REGISTER:
			err = p.handleRegister(req, tx)
		case sip.OPTIONS:
			err = p.handleOptions(req, tx)
		default:
			err = p.handleDefault(req, tx)
		}
	}

	if err != nil {
		p.logger.Error("Failed to handle request",
			zap.String("method", string(req.Method)),
			zap.Error(err))

		p.circuitBreaker.RecordFailure()

		// Send 500 Internal Server Error
		resp := sip.NewResponse(sip.StatusInternalServerError, "Internal Server Error")
		resp.SetDestination(addr)
		for _, transport := range p.transports {
			if err := transport.Send(resp, addr); err != nil {
				p.logger.Error("Failed to send 500 response", zap.Error(err))
			}
		}
		return err
	}

	p.circuitBreaker.RecordSuccess()
	return nil
}

// handleInvite processes SIP INVITE requests
func (p *Proxy) handleInvite(req *sip.Request, tx *sip.ServerTransaction) error {
	callID := req.CallID().Value
	fromTag := req.From().Tag

	// Extract the SDP
	sdp := extractSDP(req)

	// Process with RTPEngine if we have SDP
	if sdp != "" {
		// Determine if this is from WebRTC
		fromWebRTC := isFromWebRTC(req)

		// Prepare options
		options := make(map[string]interface{})
		if fromWebRTC {
			options["ICE"] = "force"
			options["DTLS"] = "passive"
		}

		// Process with RTPEngine
		ctx := context.Background()
		newSDP, err := p.rtpEngine.ProcessOffer(ctx, callID, fromTag, sdp, options)
		if err != nil {
			p.logger.Error("Failed to process SDP offer",
				zap.String("callID", callID),
				zap.Error(err))
			return err
		}

		// Replace the SDP
		replaceSDP(req, newSDP)
	}

	// Store dialog state
	dialog := &Dialog{
		CallID:     callID,
		FromTag:    fromTag,
		State:      "early",
		CreateTime: time.Now(),
		UpdateTime: time.Now(),
		ExpireTime: time.Now().Add(24 * time.Hour),
	}

	ctx := context.Background()
	if err := p.storage.StoreDialog(ctx, dialog); err != nil {
		p.logger.Error("Failed to store dialog",
			zap.String("callID", callID),
			zap.Error(err))
	}

	// Store in memory as well
	p.mu.Lock()
	p.dialogs[callID] = dialog
	p.mu.Unlock()

	// Add Record-Route header
	req.AppendHeader(&sip.RecordRouteHeader{
		Address: sip.URI{Host: p.config.ProxyURI},
	})

	// Forward the request to the next hop
	return p.forwardRequest(req, tx)
}

// Additional methods for handling other SIP requests...

// handleResponse processes SIP responses
func (p *Proxy) handleResponse(resp *sip.Response, addr net.Addr) error {
	// Process the response
	return nil
}

// forwardRequest forwards a SIP request to the next hop
func (p *Proxy) forwardRequest(req *sip.Request, tx *sip.ServerTransaction) error {
	// Determine next hop
	nextHop := p.determineNextHop(req)

	// Create client transaction
	clientTx := sip.NewClientTransaction(req)

	// Store transaction mapping
	branchID := req.Via().Params.Get("branch")
	callID := req.CallID().Value
	txKey := fmt.Sprintf("%s:%s", branchID, callID)

	p.mu.Lock()
	p.clientTx[txKey] = clientTx
	p.mu.Unlock()

	// Forward the request
	req.SetDestination(nextHop)

	// Send the request via the appropriate transport
	for _, transport := range p.transports {
		if err := transport.Send(req, nextHop); err != nil {
			p.logger.Error("Failed to forward request",
				zap.String("method", string(req.Method)),
				zap.String("nextHop", nextHop.String()),
				zap.Error(err))
			return err
		}
	}

	return nil
}

// determineNextHop finds the next hop for a request
func (p *Proxy) determineNextHop(req *sip.Request) net.Addr {
	// Implementation depends on your routing logic
	return nil // Placeholder
}

// Helper functions
func extractSDP(req *sip.Request) string {
	// Extract SDP from request
	return "" // Placeholder
}

func replaceSDP(req *sip.Request, newSDP string) {
	// Replace SDP in request
}

func isFromWebRTC(req *sip.Request) bool {
	// Determine if request is from WebRTC client
	return false // Placeholder
}
