package broker

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// guardedListener is a TCP (optionally TLS) listener for mochi that limits
// connections before authentication: a deadline for the client to send
// CONNECT (mochi sets none until CONNECT is parsed), and caps on open
// sockets overall and per remote IP.
type guardedListener struct {
	id, address string
	tlsConfig   *tls.Config
	timeout     time.Duration
	maxTotal    int
	maxPerIP    int
	log         *slog.Logger

	listen net.Listener
	end    atomic.Bool
	// disconnectAll, when set, closes this listener's clients in place of
	// mochi's closeListenerClients (see closeListenerClients in broker.go).
	disconnectAll func(id string)
	// gate holds Accept until the broker has finished restoring state.
	// Connections wait in the kernel backlog meanwhile.
	gate     chan struct{}
	gateOnce sync.Once

	mu         sync.Mutex
	total      int
	perIP      map[string]int
	pending    int            // connections that haven't logged in yet
	pendingIP  map[string]int // the same, per IP key
	maxPending int
	maxPendIP  int
	conns      map[*trackedConn]struct{} // every open connection, for Close
	lastWarn   time.Time                 // "limit reached" logs, throttled
	refused    int                       // refusals since the last warning

	// unread counts accepted connections mochi hasn't read from yet. mochi
	// sets up the client (reading server state that Close writes) before
	// its first Read, so waiting for this to drain before mochi's Close
	// orders the two. See stopAccepting.
	unread sync.WaitGroup
}

// trackedConn marks the first Read, by which time mochi has finished
// setting up the client for this connection.
type trackedConn struct {
	net.Conn
	once     sync.Once
	done     func()
	authOnce sync.Once
	onAuth   func()
	// Guarded by the listener's mu.
	ip       string
	accepted time.Time
	authed   bool
}

// authenticated moves the connection out of the pre-auth budget.
func (c *trackedConn) authenticated() {
	if c.onAuth != nil {
		c.authOnce.Do(c.onAuth)
	}
}

func (c *trackedConn) Read(p []byte) (int, error) {
	c.once.Do(c.done)
	return c.Conn.Read(p)
}

func (c *trackedConn) Close() error {
	c.once.Do(c.done)
	return c.Conn.Close()
}

func newGuardedListener(id, address string, tlsConfig *tls.Config, cfg *Config) *guardedListener {
	return &guardedListener{
		id: id, address: address, tlsConfig: tlsConfig,
		timeout:  time.Duration(cfg.ConnectTimeout),
		maxTotal: cfg.MaxConnections, maxPerIP: cfg.MaxConnectionsPerIP,
		perIP: map[string]int{}, pendingIP: map[string]int{},
		conns: map[*trackedConn]struct{}{},
		// Connections that haven't authenticated get a quarter of each
		// budget, so idle or slow sockets from anyone who can reach the
		// port can't take the slots configured agents need.
		maxPending: max(8, cfg.MaxConnections/4),
		maxPendIP:  max(2, cfg.MaxConnectionsPerIP/4),
		gate:       make(chan struct{}),
	}
}

// open lets Serve start accepting connections.
func (l *guardedListener) open() { l.gateOnce.Do(func() { close(l.gate) }) }

func (l *guardedListener) ID() string       { return l.id }
func (l *guardedListener) Protocol() string { return "tcp" }

func (l *guardedListener) Address() string {
	if l.listen != nil {
		return l.listen.Addr().String()
	}
	return l.address
}

func (l *guardedListener) Init(log *slog.Logger) error {
	l.log = log
	var err error
	if l.tlsConfig != nil {
		l.listen, err = tls.Listen("tcp", l.address, l.tlsConfig)
	} else {
		l.listen, err = net.Listen("tcp", l.address)
	}
	return err
}

func (l *guardedListener) Serve(establish listeners.EstablishFn) {
	<-l.gate
	for {
		if l.end.Load() {
			return
		}
		conn, err := l.listen.Accept()
		if err != nil {
			return
		}
		ip := ipKey(remoteIP(conn))
		tc := &trackedConn{Conn: conn, done: l.unread.Done, ip: ip, accepted: time.Now()}
		tc.onAuth = func() { l.authed(tc) }
		// acquire re-checks end under l.mu and registers with unread there,
		// so stopAccepting can't finish waiting between the two.
		if refused := l.acquire(ip, tc); refused != "" {
			l.warnRefused(ip, refused)
			_ = conn.Close()
			if l.end.Load() {
				return
			}
			continue
		}
		// mochi replaces this with a keepalive-based deadline once CONNECT
		// is read, so it only bounds the pre-auth phase.
		_ = conn.SetDeadline(time.Now().Add(l.timeout))
		go func() {
			defer l.release(ip, tc)
			defer tc.once.Do(tc.done)
			if err := establish(l.id, tc); err != nil && !isRoutineClose(err) {
				l.log.Info("connection ended with error", "remote", ip, "err", err)
			}
		}()
	}
}

// acquire admits a new connection, or says why not.
func (l *guardedListener) acquire(ip string, tc *trackedConn) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.end.Load():
		return "shutting down"
	case l.total >= l.maxTotal || l.perIP[ip] >= l.maxPerIP:
		return "connection limit reached"
	}
	// When the pre-auth budget is full, close the oldest connection that
	// still hasn't logged in rather than refusing this one. A real agent
	// logs in within milliseconds, so it isn't the one evicted; a flood of
	// idle sockets only displaces itself.
	if l.pendingIP[ip] >= l.maxPendIP {
		l.evictOldestPending(ip)
	} else if l.pending >= l.maxPending {
		l.evictOldestPending("")
	}
	l.total++
	l.perIP[ip]++
	l.pending++
	l.pendingIP[ip]++
	l.conns[tc] = struct{}{}
	l.unread.Add(1)
	return ""
}

// evictOldestPending closes the oldest connection that hasn't logged in,
// from ip if ip is set. Called with l.mu held. The connection leaves the
// budget now; its goroutine releases its general slot when mochi notices.
func (l *guardedListener) evictOldestPending(ip string) {
	var oldest *trackedConn
	for tc := range l.conns {
		if tc.authed || (ip != "" && tc.ip != ip) {
			continue
		}
		if oldest == nil || tc.accepted.Before(oldest.accepted) {
			oldest = tc
		}
	}
	if oldest == nil {
		return
	}
	l.leavePending(oldest)
	_ = oldest.Conn.Close()
}

// authed moves a connection from the pre-auth budget to the general one.
func (l *guardedListener) authed(tc *trackedConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leavePending(tc)
}

// leavePending takes tc out of the pre-auth budget once. l.mu held.
func (l *guardedListener) leavePending(tc *trackedConn) {
	if tc.authed {
		return
	}
	tc.authed = true
	l.pending--
	if l.pendingIP[tc.ip]--; l.pendingIP[tc.ip] <= 0 {
		delete(l.pendingIP, tc.ip)
	}
}

func (l *guardedListener) release(ip string, tc *trackedConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leavePending(tc) // no-op if it already left the pre-auth budget
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
	delete(l.conns, tc)
}

// warnRefused logs refused connections at most once a second, with a
// count of the ones in between.
func (l *guardedListener) warnRefused(ip, reason string) {
	l.mu.Lock()
	l.refused++
	if time.Since(l.lastWarn) < time.Second {
		l.mu.Unlock()
		return
	}
	n := l.refused
	l.refused, l.lastWarn = 0, time.Now()
	l.mu.Unlock()
	l.log.Warn("connection refused", "remote", ip, "reason", reason, "refused_since_last_warning", n)
}

// stopAccepting stops new connections and waits (up to wait) for mochi to
// finish setting up the ones already accepted.
func (l *guardedListener) stopAccepting(wait time.Duration) {
	l.mu.Lock()
	l.end.Store(true)
	l.mu.Unlock()
	l.open() // release a Serve still waiting at the gate
	if l.listen != nil {
		_ = l.listen.Close()
	}
	done := make(chan struct{})
	go func() { l.unread.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

func (l *guardedListener) Close(closeClients listeners.CloseFn) {
	l.end.Store(true)
	if l.listen != nil {
		_ = l.listen.Close()
	}
	if l.disconnectAll != nil {
		l.disconnectAll(l.id)
	} else {
		closeClients(l.id)
	}
	// Connections still in the handshake aren't mochi clients yet, so the
	// disconnect above can't see them, but mochi's Close waits for them.
	// Close their sockets so shutdown doesn't wait out connect_timeout.
	l.mu.Lock()
	open := make([]*trackedConn, 0, len(l.conns))
	for tc := range l.conns {
		open = append(open, tc)
	}
	l.mu.Unlock()
	for _, tc := range open {
		_ = tc.Conn.Close()
	}
}

// ipKey groups addresses for per-IP limits. An IPv6 host usually holds a
// whole /64, so per-address limits would be trivial to sidestep there.
func ipKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Is4() || addr.Is4In6() {
		return ip
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}

// isRoutineClose reports errors that are a normal end of a connection.
func isRoutineClose(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, packets.CodeDisconnect) ||
		errors.Is(err, packets.ErrSessionTakenOver) || errors.Is(err, packets.ErrServerShuttingDown)
}
