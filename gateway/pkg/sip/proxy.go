// gateway/pkg/sip/proxy.go
package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

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

	// Create a SIP client without binding to a specific local address
	client, err := sipgo.NewClient(ua)
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
		log.Debug("route() called - forwarding logic", "method", req.Method.String())

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
			log.Error("Failed to create client transaction in route()",
				"error", err,
				"destination", dstAddr)
			reply(tx, req, 500, "")
			return
		}
		defer clTx.Terminate()

		log.Debug("Starting transaction in route()",
			"method", req.Method.String(),
			"destination", dstAddr)

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
					log.Error("Failed to forward response in route()", "error", err)
				}
			case <-clTx.Done():
				if err := clTx.Err(); err != nil {
					log.Error("Client transaction ended with error in route()", "error", err)
				}
				return
			case ack := <-tx.Acks():
				log.Info("Proxying ACK",
					"method", req.Method.String(),
					"dstAddr", dstAddr)
				ack.SetDestination(dstAddr)
				if wErr := client.WriteRequest(ack); wErr != nil {
					log.Error("ACK forward failed", "error", wErr)
				}
			case <-tx.Done():
				if err := tx.Err(); err != nil {
					if errors.Is(err, sip.ErrTransactionCanceled) {
						// We might send CANCEL downstream
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
						log.Error("Server transaction ended with error in route()", "error", err)
					}
				}
				log.Debug("Transaction done in route()", "method", req.Method.String())
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
		log.Debug(fmt.Sprintf("Registered %s -> %s (OnRegister handler)", uri.User, addr))

		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		uri.UriParams = sip.NewParams()
		uri.UriParams.Add("transport", req.Transport())
		if err := tx.Respond(res); err != nil {
			log.Error("Failed to respond 200 to REGISTER in OnRegister", "error", err)
		}
	})

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	})

	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		dstAddr := getDestination(req)
		if dstAddr == "" {
			log.Debug("OnAck: no destination found, skipping")
			return
		}
		req.SetDestination(dstAddr)
		if err := client.WriteRequest(req, sipgo.ClientRequestAddVia); err != nil {
			log.Error("ACK forward failed in OnAck", "error", err)
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
	UDPBindAddr          string // e.g., "127.0.0.1:5060"
	ProxyURI             string
	DefaultNextHop       string
	MaxForwards          int
	UserAgent            string
	DisableSIPProcessing bool
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
	config     SIPConfig
}

// NewProxy creates a new SIP proxy instance based on the provided configuration.
func NewProxy(
	cfg SIPConfig,
	storage storage.StateStorage,
	rtpEngine *rtpengine.Manager,
	logger *zap.Logger,
) (*Proxy, error) {

	srv, client, registry := setupSipProxy(cfg.DefaultNextHop, cfg.UDPBindAddr)
	if srv == nil {
		return nil, errors.New("failed to create SIP server")
	}

	// Force DisableSIPProcessing to true regardless of the loaded value.
	cfg.DisableSIPProcessing = true

	logger.Warn("NewProxy called",
		zap.String("UDPBindAddr", cfg.UDPBindAddr),
		zap.String("DefaultNextHop", cfg.DefaultNextHop),
		zap.Bool("DisableSIPProcessing", cfg.DisableSIPProcessing),
	)

	logger.Warn("Successfully created sipgo.Server + sipgo.Client",
		zap.String("DefaultNextHop", cfg.DefaultNextHop),
		zap.Bool("DisableSIPProcessing", cfg.DisableSIPProcessing),
	)

	return &Proxy{
		server:     srv,
		client:     client,
		registry:   registry,
		logger:     logger,
		storage:    storage,
		rtpEngine:  rtpEngine,
		transports: []Transport{},
		proxyDst:   cfg.DefaultNextHop,
		config:     cfg, // now cfg.DisableSIPProcessing is always true
	}, nil
}

// AddTransport adds a transport to the SIP proxy.
func (p *Proxy) AddTransport(t Transport) {
	p.logger.Debug("AddTransport called", zap.String("bindAddr", t.(*UDPTransport).bindAddr))
	p.transports = append(p.transports, t)
}

// Start starts the SIP proxy server.
func (p *Proxy) Start(ctx context.Context) error {
	p.logger.Warn("Proxy.Start() called",
		zap.String("UDPBindAddr", p.config.UDPBindAddr),
		zap.String("DefaultNextHop", p.config.DefaultNextHop),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)

	go func() {
		err := p.server.ListenAndServe(ctx, "udp", "")
		if err != nil && err != context.Canceled {
			p.logger.Error("SIP server failed", zap.Error(err))
		}
	}()

	return nil
}

// Stop stops the SIP proxy server.
func (p *Proxy) Stop() error {
	p.logger.Info("Stop() called on SIP proxy")
	// If needed, gracefully shut down p.server here
	return nil
}

// HandleMessage implements the websocket.SIPHandler interface to process SIP messages from WebSocket connections.
func (p *Proxy) HandleMessage(msg Message, addr net.Addr) error {
	clientAddr := addr.String()

	// Log the method and disable-flag for every inbound message
	p.logger.Warn("HandleMessage invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)

	if req, ok := msg.(*Request); ok {
		method := req.Method.String()
		p.logger.Debug("Decoded SIP request",
			zap.String("method", method),
			zap.String("clientAddr", clientAddr),
		)

		switch method {
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
				zap.String("method", method),
				zap.String("client", clientAddr))
		}
	} else if resp, ok := msg.(*Response); ok {
		p.logger.Debug("Decoded SIP response",
			zap.Int("status", resp.StatusCode),
			zap.String("clientAddr", clientAddr),
			zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
		)
		return p.handleResponse(resp, clientAddr)
	}

	return nil
}

// SIP Request Handlers

func (p *Proxy) handleRegister(req *Request, clientAddr string) error {
	p.logger.Warn("handleRegister invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)

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

	// Store the registration info locally (if desired).
	addr := uri.Host + ":" + strconv.Itoa(uri.Port)
	p.registry.Add(uri.User, addr)
	p.logger.Debug("Registered user via WebSocket",
		zap.String("user", uri.User),
		zap.String("address", addr),
		zap.String("clientAddr", clientAddr),
	)

	// If disabled, skip forwarding to local SIP transactions.
	if p.config.DisableSIPProcessing {
		p.logger.Warn("Skipping forwardRequest for REGISTER (DisableSIPProcessing=true)",
			zap.String("client", clientAddr))
		return nil
	}

	// Otherwise, forward to server if needed.
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleInvite(req *Request, clientAddr string) error {
	p.logger.Warn("handleInvite invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleBye(req *Request, clientAddr string) error {
	p.logger.Warn("handleBye invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleAck(req *Request, clientAddr string) error {
	p.logger.Warn("handleAck invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleCancel(req *Request, clientAddr string) error {
	p.logger.Warn("handleCancel invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)
	return p.forwardRequest(req, clientAddr)
}

func (p *Proxy) handleOptions(req *Request, clientAddr string) error {
	p.logger.Warn("handleOptions invoked",
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)
	return p.forwardRequest(req, clientAddr)
}

// SIP Response Handler

func (p *Proxy) handleResponse(resp *Response, clientAddr string) error {
	p.logger.Warn("handleResponse invoked (SIP response)",
		zap.Int("statusCode", resp.StatusCode),
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)
	// If you want to forward responses back somewhere, add logic here.
	return nil
}

// Actually forward the request by creating a local SIP transaction (only if allowed).
func (p *Proxy) forwardRequest(req *Request, clientAddr string) error {

	/* 	// Hard bypass if you want to completely disable local SIP processing.
	   	p.logger.Warn("Hard bypass in forwardRequest: skipping local SIP transaction creation",
	   		zap.String("method", req.Method.String()),
	   		zap.String("clientAddr", clientAddr),
	   	)
	   	return nil */
	p.logger.Warn("forwardRequest called",
		zap.String("method", req.Method.String()),
		zap.String("clientAddr", clientAddr),
		zap.Bool("DisableSIPProcessing", p.config.DisableSIPProcessing),
	)

	// Hard short-circuit if disabled
	if p.config.DisableSIPProcessing {
		p.logger.Warn("Short-circuit: skipping local SIP transaction creation (DisableSIPProcessing=true)",
			zap.String("method", req.Method.String()),
			zap.String("client", clientAddr),
		)
		return nil
	}

	// Determine destination
	dstAddr := p.getDestination(req)
	if dstAddr == "" {
		p.logger.Error("No destination found for request",
			zap.String("method", req.Method.String()),
			zap.String("client", clientAddr))
		return errors.New("no destination found")
	}
	req.SetDestination(dstAddr)

	p.logger.Debug("Proceeding with local SIP transaction creation",
		zap.String("method", req.Method.String()),
		zap.String("client", clientAddr),
		zap.String("dstAddr", dstAddr),
	)

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt transaction with retries
	var clTx sip.ClientTransaction
	var err error
	maxRetries := 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		clTx, err = p.client.TransactionRequest(ctx, req,
			sipgo.ClientRequestAddVia,
			sipgo.ClientRequestAddRecordRoute,
		)
		if err == nil {
			break // success
		}

		if strings.Contains(err.Error(), "bind: address already in use") {
			p.logger.Warn("Port binding issue, retrying in forwardRequest()",
				zap.Int("attempt", attempt+1),
				zap.String("client", clientAddr))
			time.Sleep(200 * time.Millisecond)
			continue
		}

		p.logger.Error("Failed to create client transaction for WebSocket request in forwardRequest()",
			zap.Error(err),
			zap.String("client", clientAddr),
			zap.Int("attempt", attempt+1),
		)
		return err
	}

	if err != nil {
		p.logger.Error("Failed to create client transaction after retries in forwardRequest()",
			zap.Error(err),
			zap.String("client", clientAddr),
			zap.Int("maxRetries", maxRetries))
		return err
	}
	defer clTx.Terminate()

	p.logger.Debug("Forwarded WebSocket SIP request successfully",
		zap.String("method", req.Method.String()),
		zap.String("client", clientAddr),
		zap.String("destination", dstAddr),
	)

	return nil
}

func (p *Proxy) getDestination(req *sip.Request) string {
	toHeader := req.To()
	if toHeader == nil || toHeader.Address.Host == "" {
		p.logger.Debug("getDestination returning proxyDst due to empty To header",
			zap.String("proxyDst", p.proxyDst))
		return p.proxyDst
	}
	dst := p.registry.Get(toHeader.Address.User)
	if dst == "" {
		p.logger.Debug("getDestination: not found in registry, returning proxyDst",
			zap.String("proxyDst", p.proxyDst),
			zap.String("toUser", toHeader.Address.User))
		return p.proxyDst
	}
	p.logger.Debug("getDestination found registry entry", zap.String("finalDst", dst))
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
