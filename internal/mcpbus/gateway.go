package mcpbus

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jefflaplante/agentbus/client"
)

// GatewayOptions configure a Gateway.
type GatewayOptions struct {
	// BrokerURL and TLS are how the gateway reaches the broker, as each
	// agent it serves: usually the broker's own listener over loopback.
	BrokerURL string
	TLS       *tls.Config
	// Lookup maps a bearer token to the agent it belongs to, in the config
	// as it is now. observer is true for observer (admin) identities,
	// which the gateway refuses.
	Lookup func(token string) (agent string, observer bool, ok bool)

	Version string
	Logger  *slog.Logger

	// IdleClose is how long an agent's bus connection stays open with no
	// HTTP requests and nothing held for it. Default 15 minutes.
	IdleClose time.Duration
	// SessionTimeout closes MCP sessions that send nothing for this long.
	// Default 30 minutes.
	SessionTimeout time.Duration
	// Progress and KeepAlive keep long waits alive through proxies; see
	// ServerOptions. Defaults 20s and 25s.
	Progress, KeepAlive time.Duration
	// Sweep is how often idle connections and revoked tokens are checked.
	// Default 5 seconds.
	Sweep time.Duration
}

// Gateway serves the bus to remote MCP clients over Streamable HTTP. Each
// request carries an agent's token as a bearer token. The gateway keeps
// one bus connection per agent, shared by all of that agent's MCP
// sessions, so two clients with one token don't knock each other off the
// bus, and the agent's inbox outlives any one session.
//
// The gateway is a client of the broker, not part of it: it connects as
// the agent with the token the caller presented, so the broker checks
// everything it sends exactly as for any other agent.
type Gateway struct {
	o       GatewayOptions
	handler http.Handler

	mu     sync.Mutex
	conns  map[string]*agentConn
	locks  map[string]*sync.Mutex // one per agent, held while connecting
	closed bool

	fails *failThrottle
	stop  chan struct{}
	done  chan struct{}
}

// agentConn is one agent's bus connection and everything that hangs off it.
type agentConn struct {
	agent, token string
	c            *client.Client
	ib           *Inbox
	srv          *mcp.Server
	log          *slog.Logger

	mu       sync.Mutex
	inflight int // HTTP requests in progress, including open streams
	lastUsed time.Time
	closed   bool
}

type connKey struct{}

const scopeAgent = "agentbus:agent"

// NewGateway starts a gateway. Serve its Handler at /mcp, and Close it
// when done.
func NewGateway(o GatewayOptions) *Gateway {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.IdleClose <= 0 {
		o.IdleClose = 15 * time.Minute
	}
	if o.SessionTimeout <= 0 {
		o.SessionTimeout = 30 * time.Minute
	}
	if o.Progress <= 0 {
		o.Progress = 20 * time.Second
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 25 * time.Second
	}
	if o.Sweep <= 0 {
		o.Sweep = 5 * time.Second
	}
	g := &Gateway{o: o, conns: map[string]*agentConn{}, locks: map[string]*sync.Mutex{},
		fails: newFailThrottle(20, time.Minute), stop: make(chan struct{}), done: make(chan struct{})}

	streamable := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if ac, ok := r.Context().Value(connKey{}).(*agentConn); ok {
			return ac.srv
		}
		return nil
	}, &mcp.StreamableHTTPOptions{
		EventStore:     mcp.NewMemoryEventStore(nil), // a dropped stream can resume with Last-Event-ID
		SessionTimeout: o.SessionTimeout,
		Logger:         o.Logger,
	})
	bearer := auth.RequireBearerToken(g.verify, &auth.RequireBearerTokenOptions{
		Scopes: []string{scopeAgent}, AllowMissingExpiration: true, // agentbus tokens don't expire
	})
	g.handler = http.NewCrossOriginProtection().Handler(g.throttle(bearer(g.attach(streamable))))
	go g.sweep()
	return g
}

// Handler is the MCP endpoint.
func (g *Gateway) Handler() http.Handler { return g.handler }

// Close disconnects every agent and stops the gateway.
func (g *Gateway) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	conns := make([]*agentConn, 0, len(g.conns))
	for _, ac := range g.conns {
		conns = append(conns, ac)
	}
	g.mu.Unlock()
	close(g.stop)
	<-g.done
	for _, ac := range conns {
		g.drop(ac, "gateway stopping")
	}
}

// Agents lists the agents with an open bus connection, for status and tests.
func (g *Gateway) Agents() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.conns))
	for id := range g.conns {
		out = append(out, id)
	}
	return out
}

// verify maps a bearer token to an agent. Observers authenticate but get
// no scope, so the bearer middleware refuses them with 403.
func (g *Gateway) verify(_ context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
	agent, observer, ok := g.o.Lookup(token)
	if !ok {
		g.fails.add(remoteIP(r))
		g.o.Logger.Warn("MCP request with an unknown token", "remote", remoteIP(r))
		return nil, fmt.Errorf("%w: not an agentbus agent token", auth.ErrInvalidToken)
	}
	info := &auth.TokenInfo{UserID: agent, Extra: map[string]any{"token": token}}
	if observer {
		g.o.Logger.Warn("observer identity refused over MCP HTTP", "agent", agent, "remote", remoteIP(r))
	} else {
		info.Scopes = []string{scopeAgent}
	}
	return info, nil
}

// throttle answers 429 to an address with too many recent bad tokens.
func (g *Gateway) throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.fails.blocked(remoteIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many failed attempts; retry later", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// attach finds or opens the agent's bus connection and hands it to the
// MCP handler. The request counts as activity until it returns, so an
// open stream keeps the connection from being closed as idle.
func (g *Gateway) attach(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		token, _ := info.Extra["token"].(string)
		ac, err := g.acquire(r.Context(), info.UserID, token)
		if err != nil {
			g.o.Logger.Warn("MCP gateway could not connect an agent to the bus", "agent", info.UserID, "err", err)
			http.Error(w, "could not connect to the bus as "+info.UserID+": "+err.Error(), http.StatusBadGateway)
			return
		}
		ac.begin()
		defer ac.end()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), connKey{}, ac)))
	})
}

// acquire returns the agent's open connection, opening one if needed.
// Only one connection per agent is ever opened at a time.
func (g *Gateway) acquire(ctx context.Context, agent, token string) (*agentConn, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, errors.New("gateway is stopping")
	}
	lock := g.locks[agent]
	if lock == nil {
		lock = &sync.Mutex{}
		g.locks[agent] = lock
	}
	g.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	g.mu.Lock()
	ac := g.conns[agent]
	g.mu.Unlock()
	if ac != nil && !ac.isClosed() {
		if ac.token == token {
			return ac, nil
		}
		// The token was rotated and this is the new one: the old connection
		// was opened with a token the broker no longer accepts.
		g.drop(ac, "token rotated")
	}

	log := g.o.Logger.With("agent", agent)
	ib := NewInbox()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := client.Connect(cctx, client.Options{
		BrokerURL: g.o.BrokerURL, AgentID: agent, Token: token, TLS: g.o.TLS,
		CleanSession: false, // messages sent while no client is connected wait at the broker
		Inbox:        ib.Handle,
		Description:  "MCP client over HTTP (agentbus gateway)",
		Meta:         map[string]string{"runtime": "agentbus-mcp-http"},
		Logger:       log,
	})
	if err != nil {
		return nil, err
	}
	ac = &agentConn{agent: agent, token: token, c: c, ib: ib, log: log, lastUsed: time.Now(),
		srv: NewServer(c, ib, agent, g.o.Version, &ServerOptions{Progress: g.o.Progress, KeepAlive: g.o.KeepAlive})}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		c.Close()
		return nil, errors.New("gateway is stopping")
	}
	g.conns[agent] = ac
	g.mu.Unlock()
	log.Info("MCP gateway connected an agent to the bus")
	return ac, nil
}

// drop closes an agent's connection and its MCP sessions. Their clients
// get 404 on the next request and start a new session, which reconnects.
func (g *Gateway) drop(ac *agentConn, why string) {
	ac.mu.Lock()
	if ac.closed {
		ac.mu.Unlock()
		return
	}
	ac.closed = true
	ac.mu.Unlock()
	g.mu.Lock()
	if g.conns[ac.agent] == ac {
		delete(g.conns, ac.agent)
	}
	g.mu.Unlock()
	for ss := range ac.srv.Sessions() {
		_ = ss.Close()
	}
	ac.c.Close()
	ac.log.Info("MCP gateway disconnected an agent from the bus", "why", why)
}

// sweep closes connections whose token was revoked or rotated, and ones
// that are idle with nothing held for them.
func (g *Gateway) sweep() {
	defer close(g.done)
	t := time.NewTicker(g.o.Sweep)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
		g.mu.Lock()
		conns := make([]*agentConn, 0, len(g.conns))
		for _, ac := range g.conns {
			conns = append(conns, ac)
		}
		g.mu.Unlock()
		for _, ac := range conns {
			if agent, _, ok := g.o.Lookup(ac.token); !ok || agent != ac.agent {
				g.drop(ac, "token no longer valid")
				continue
			}
			if ac.idleFor() > g.o.IdleClose && ac.ib.Idle() {
				g.drop(ac, "idle")
			}
		}
	}
}

func (ac *agentConn) begin() {
	ac.mu.Lock()
	ac.inflight++
	ac.lastUsed = time.Now()
	ac.mu.Unlock()
}

func (ac *agentConn) end() {
	ac.mu.Lock()
	ac.inflight--
	ac.lastUsed = time.Now()
	ac.mu.Unlock()
}

// idleFor is how long the connection has had no HTTP request in progress.
func (ac *agentConn) idleFor() time.Duration {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.inflight > 0 {
		return 0
	}
	return time.Since(ac.lastUsed)
}

func (ac *agentConn) isClosed() bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.closed
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// failThrottle counts failed authentications per address in a window.
type failThrottle struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string][]time.Time
}

func newFailThrottle(max int, window time.Duration) *failThrottle {
	return &failThrottle{max: max, window: window, seen: map[string][]time.Time{}}
}

func (f *failThrottle) add(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[ip] = append(f.recentLocked(ip), time.Now())
	if len(f.seen) > 10000 { // bound memory under a spray of addresses
		for k := range f.seen {
			if len(f.recentLocked(k)) == 0 {
				delete(f.seen, k)
			}
		}
	}
}

func (f *failThrottle) blocked(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recentLocked(ip)) >= f.max
}

func (f *failThrottle) recentLocked(ip string) []time.Time {
	cut := time.Now().Add(-f.window)
	ts := f.seen[ip]
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(f.seen, ip)
		return nil
	}
	f.seen[ip] = ts
	return ts
}

// PinnedTLS is a TLS config that accepts exactly the certificate in
// certFile, for the gateway to reach its own broker over loopback: the
// broker's certificate names its public host, not 127.0.0.1, so ordinary
// hostname verification can't pass, and pinning is stricter anyway.
func PinnedTLS(certFile string) (*tls.Config, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no PEM certificate", certFile)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("%s: %w", certFile, err)
	}
	want := block.Bytes
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by the exact match below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, want) {
				return errors.New("broker certificate does not match tls_cert")
			}
			return nil
		},
	}, nil
}
