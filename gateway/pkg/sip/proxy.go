// gateway/pkg/sip/proxy.go
package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"go.uber.org/zap"

	"gateway/pkg/rtpengine"
	"gateway/pkg/storage"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ------------------------------
// Internal Types and Helpers
// ------------------------------

// Registry provides a simple user -> address mapping in memory.
type Registry struct {
	mu    sync.RWMutex
	store map[string]string
}

// NewRegistry creates a new registration store.
func NewRegistry() *Registry {
	return &Registry{
		store: make(map[string]string),
	}
}

// Add registers a user to a given address.
func (r *Registry) Add(user, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store[user] = addr
}

// Get retrieves the address for a given user, or an empty string if none.
func (r *Registry) Get(user string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store[user]
}

// setupSipProxy builds a SIP proxy server that listens on the given ip
// and forwards requests to proxydst if not found in the registry.
func setupSipProxy(proxydst, ip string) (*sipgo.Server, *sipgo.Client, *Registry) {
	// Use slog.Default() for simple logging.
	log := slog.Default()
	host, port, _ := sip.ParseAddr(ip)

	// Build a user agent (UA)
	ua, err := sipgo.NewUA()
	if err != nil {
		log.Error("Failed to create UA", "error", err)
		return nil, nil, nil
	}

	// Create the main SIP server
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Error("Failed to create server", "error", err)
		return nil, nil, nil
	}

	// Create a SIP client (for forwarding requests downstream)
	client, err := sipgo.NewClient(ua, sipgo.WithClientAddr(ip))
	if err != nil {
		log.Error("Failed to create client", "error", err)
		return nil, nil, nil
	}

	registry := NewRegistry()

	// getDestination returns the final host:port to proxy calls to.
	var getDestination = func(req *sip.Request) string {
		toHeader := req.To()
		if toHeader == nil || toHeader.Address.Host == "" {
			return proxydst
		}
		dst := registry.Get(toHeader.Address.User)
		if dst == "" {
			return proxydst
		}
		return dst
	}

	// reply sends a response with the provided code and reason.
	var reply = func(tx sip.ServerTransaction, req *sip.Request, code int, reason string) {
		resp := sip.NewResponseFromRequest(req, code, reason, nil)
		resp.SetDestination(req.Source())
		if err := tx.Respond(resp); err != nil {
			log.Error("Failed to send response", "error", err)
		}
	}

	// route handles generic forwarding logic.
	var route = func(req *sip.Request, tx sip.ServerTransaction) {
		dstAddr := getDestination(req)
		if dstAddr == "" {
			reply(tx, req, 404, "Not found")
			return
		}
		ctx := context.Background()
		req.SetDestination(dstAddr)
		clTx, err := client.TransactionRequest(ctx, req,
			sipgo.ClientRequestAddVia,
			sipgo.ClientRequestAddRecordRoute,
		)
		if err != nil {
			log.Error("Failed to create client transaction", "error", err)
			reply(tx, req, 500, "")
			return
		}
		defer clTx.Terminate()

		log.Debug("Starting transaction", "method", req.Method.String())
		for {
			select {
			case res, more := <-clTx.Responses():
				if !more {
					return
				}
				// Force the final destination to the original source.
				res.SetDestination(req.Source())
				// Remove the topmost Via header.
				res.RemoveHeader("Via")
				if err := tx.Respond(res); err != nil {
					log.Error("Failed to forward response", "error", err)
				}
			case <-clTx.Done():
				if err := clTx.Err(); err != nil {
					log.Error("Client transaction ended with error", "error", err)
				}
				return
			case ack := <-tx.Acks():
				log.Info("Proxying ACK", "method", req.Method.String(), "dstAddr", dstAddr)
				ack.SetDestination(dstAddr)
				if wErr := client.WriteRequest(ack); wErr != nil {
					log.Error("ACK forward failed", "error", wErr)
				}
			case <-tx.Done():
				if err := tx.Err(); err != nil {
					if errors.Is(err, sip.ErrTransactionCanceled) {
						if req.IsInvite() {
							r := newCancelRequest(req)
							res, cErr := client.Do(ctx, r)
							if cErr != nil {
								log.Error("Canceling downstream transaction failed", "error", cErr)
							} else if res.StatusCode != 200 {
								log.Error("Downstream CANCEL returned non-200 code", "code", res.StatusCode)
							}
						}
					} else {
						log.Error("Server transaction ended with error", "error", err)
					}
				}
				log.Debug("Transaction done", "method", req.Method.String())
				return
			}
		}
	}

	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		contactHeader := req.Contact()
		if contactHeader == nil || contactHeader.Address.Host == "" {
			reply(tx, req, 404, "Missing address of record")
			return
		}
		uri := contactHeader.Address
		if uri.Host == host && uri.Port == port {
			reply(tx, req, 401, "Contact address not provided")
			return
		}
		addr := uri.Host + ":" + strconv.Itoa(uri.Port)
		registry.Add(uri.User, addr)
		log.Debug(fmt.Sprintf("Registered %s -> %s", uri.User, addr))
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		uri.UriParams = sip.NewParams()
		uri.UriParams.Add("transport", req.Transport())
		if err := tx.Respond(res); err != nil {
			log.Error("Failed to respond 200 to REGISTER", "error", err)
		}
	})

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	})

	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		dstAddr := getDestination(req)
		if dstAddr == "" {
			return
		}
		req.SetDestination(dstAddr)
		if err := client.WriteRequest(req, sipgo.ClientRequestAddVia); err != nil {
			log.Error("ACK forward failed", "error", err)
			reply(tx, req, 500, "")
		}
	})

	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	})

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	})

	return srv, client, registry
}

func newCancelRequest(inviteRequest *sip.Request) *sip.Request {
	cancelReq := sip.NewRequest(sip.CANCEL, inviteRequest.Recipient)
	cancelReq.AppendHeader(sip.HeaderClone(inviteRequest.Via()))
	cancelReq.AppendHeader(sip.HeaderClone(inviteRequest.From()))
	cancelReq.AppendHeader(sip.HeaderClone(inviteRequest.To()))
	cancelReq.AppendHeader(sip.HeaderClone(inviteRequest.CallID()))
	sip.CopyHeaders("Route", inviteRequest, cancelReq)
	cancelReq.SetSource(inviteRequest.Source())
	cancelReq.SetDestination(inviteRequest.Destination())
	return cancelReq
}

// ------------------------------
// Exported Types and Functions
// ------------------------------

// SIPConfig represents the SIP proxy configuration.
type SIPConfig struct {
	UDPBindAddr    string // e.g., "127.0.0.1:5060"
	ProxyURI       string
	DefaultNextHop string
	MaxForwards    int
	UserAgent      string
}

// Proxy wraps the underlying sipgo.Server and holds additional state.
type Proxy struct {
	server     *sipgo.Server
	client     *sipgo.Client
	registry   *Registry
	logger     *zap.Logger
	storage    storage.StateStorage
	rtpEngine  *rtpengine.Manager
	transports []Transport
	proxyDst   string
}

// NewProxy creates a new SIP proxy instance based on the provided configuration.
// The proxydst is taken from cfg.DefaultNextHop and the IP to bind from cfg.UDPBindAddr.
func NewProxy(cfg SIPConfig, storage storage.StateStorage, rtpEngine *rtpengine.Manager, logger *zap.Logger) (*Proxy, error) {
	srv, client, registry := setupSipProxy(cfg.DefaultNextHop, cfg.UDPBindAddr)
	if srv == nil {
		return nil, errors.New("failed to create SIP server")
	}
	return &Proxy{
		server:     srv,
		client:     client,
		registry:   registry,
		logger:     logger,
		storage:    storage,
		rtpEngine:  rtpEngine,
		transports: make([]Transport, 0),
		proxyDst:   cfg.DefaultNextHop,
	}, nil
}

// AddTransport adds a transport to the SIP proxy.
func (p *Proxy) AddTransport(t Transport) {
	p.transports = append(p.transports, t)
	// Optionally, you can integrate this transport with the underlying server.
}

// Start starts the SIP proxy server.
func (p *Proxy) Start(ctx context.Context) error {
	// Adjust parameters as needed. Here we assume UDP transport.
	return p.server.ListenAndServe(ctx, "udp", "")
}

// Stop stops the SIP proxy server.
func (p *Proxy) Stop() error {
	return nil
}

// HandleMessage implements the websocket.SIPHandler interface to process SIP messages from WebSocket connections
// HandleMessage implements the websocket.SIPHandler interface for processing SIP messages
func (p *Proxy) HandleMessage(msg Message, addr net.Addr) error {
	clientAddr := addr.String()

	// No need to parse the message as it's already parsed

	// Process based on message type
	if req, ok := msg.(*Request); ok {
		// Handle different request types
		switch req.Method.String() {
		case "REGISTER":
			return p.handleRegister(req, clientAddr)
		case "INVITE":
			return p.handleInvite(req, clientAddr)
		case "BYE":
			return p.handleBye(req, clientAddr)
		case "ACK":
			return p.handleAck(req, clientAddr)
		case "CANCEL":
			return p.handleCancel(req, clientAddr)
		case "OPTIONS":
			return p.handleOptions(req, clientAddr)
		default:
			p.logger.Debug("Unhandled SIP method",
				zap.String("method", req.Method.String()),
				zap.String("client", clientAddr))
		}
	} else if resp, ok := msg.(*Response); ok {
		// Handle SIP responses
		return p.handleResponse(resp, clientAddr)
	}

	return nil
}

// Helper methods with the proper signature

func (p *Proxy) handleRegister(req *Request, clientAddr string) error {
	contactHeader := req.Contact()
	if contactHeader == nil || contactHeader.Address.Host == "" {
		p.logger.Error("Missing address of record in REGISTER",
			zap.String("client", clientAddr))
		return errors.New("missing address of record")
	}

	uri := contactHeader.Address
	host, port, _ := net.SplitHostPort(clientAddr)
	portNum, _ := strconv.Atoi(port)

	if uri.Host == host && uri.Port == portNum {
		p.logger.Error("Contact address not provided in REGISTER",
			zap.String("client", clientAddr))
		return errors.New("contact address not provided")
	}

	addr := uri.Host + ":" + strconv.Itoa(uri.Port)
	p.registry.Add(uri.User, addr)
	p.logger.Debug("Registered user via WebSocket",
		zap.String("user", uri.User),
		zap.String("address", addr))

	// Forward to server if needed
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleInvite(req *Request, clientAddr string) error {
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleBye(req *Request, clientAddr string) error {
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleAck(req *Request, clientAddr string) error {
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleCancel(req *Request, clientAddr string) error {
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleOptions(req *Request, clientAddr string) error {
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleResponse(resp *Response, clientAddr string) error {
	// Process SIP responses if needed
	p.logger.Debug("Received SIP response via WebSocket",
		zap.Int("status", resp.StatusCode),
		zap.String("client", clientAddr))

	// Forward response if needed
	return nil
}

func (p *Proxy) forwardRequest(req *Request, clientAddr string) error {
	// Determine destination
	dstAddr := p.getDestination(req)
	if dstAddr == "" {
		p.logger.Error("No destination found for request",
			zap.String("method", req.Method.String()),
			zap.String("client", clientAddr))
		return errors.New("no destination found")
	}

	req.SetDestination(dstAddr)

	// Create context for the request
	ctx := context.Background()

	// Forward the request using the client
	clTx, err := p.client.TransactionRequest(ctx, req,
		sipgo.ClientRequestAddVia,
		sipgo.ClientRequestAddRecordRoute,
	)
	if err != nil {
		p.logger.Error("Failed to create client transaction for WebSocket request",
			zap.Error(err),
			zap.String("client", clientAddr))
		return err
	}
	defer clTx.Terminate()

	p.logger.Debug("Forwarded WebSocket SIP request",
		zap.String("method", req.Method.String()),
		zap.String("client", clientAddr),
		zap.String("destination", dstAddr))

	// Could handle responses here, but that would require a mechanism to
	// send responses back via WebSocket to the original sender
	return nil
}
func (p *Proxy) getDestination(req *sip.Request) string {
	toHeader := req.To()
	if toHeader == nil || toHeader.Address.Host == "" {
		return p.proxyDst
	}
	dst := p.registry.Get(toHeader.Address.User)
	if dst == "" {
		return p.proxyDst
	}
	return dst
}

// Transport defines an interface for sending SIP messages.
type Transport interface {
	Send(msg sip.Message, addr net.Addr) error
}

// UDPTransport is a minimal implementation of a UDP transport.
type UDPTransport struct {
	bindAddr string
	logger   *zap.Logger
	conn     *net.UDPConn
}

// NewUDPTransportImpl creates a new UDPTransport.
func NewUDPTransportImpl(bindAddr string, logger *zap.Logger) (*UDPTransport, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", bindAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	return &UDPTransport{
		bindAddr: bindAddr,
		logger:   logger,
		conn:     conn,
	}, nil
}

// Send sends a SIP message over UDP.
func (t *UDPTransport) Send(msg sip.Message, addr net.Addr) error {
	data := []byte(msg.String())
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return errors.New("invalid UDP address")
	}
	_, err := t.conn.WriteToUDP(data, udpAddr)
	return err
}

// NewUDPTransport creates a new UDP transport and returns it as a Transport interface.
func NewUDPTransport(bindAddr string, logger *zap.Logger) (Transport, error) {
	return NewUDPTransportImpl(bindAddr, logger)
}
