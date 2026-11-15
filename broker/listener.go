package broker

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
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
	// gate holds Accept until the broker has finished restoring state.
	// Connections wait in the kernel backlog meanwhile.
	gate     chan struct{}
	gateOnce sync.Once

	mu    sync.Mutex
	total int
	perIP map[string]int

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
	once sync.Once
	done func()
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
		perIP: map[string]int{},
		gate:  make(chan struct{}),
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
		ip := remoteIP(conn)
		if !l.acquire(ip) {
			l.log.Warn("connection refused: limit reached", "remote", ip)
			_ = conn.Close()
			continue
		}
		// mochi replaces this with a keepalive-based deadline once CONNECT
		// is read, so it only bounds the pre-auth phase.
		_ = conn.SetDeadline(time.Now().Add(l.timeout))
		l.unread.Add(1)
		tc := &trackedConn{Conn: conn, done: l.unread.Done}
		go func() {
			defer l.release(ip)
			defer tc.once.Do(tc.done)
			if err := establish(l.id, tc); err != nil && !isRoutineClose(err) {
				l.log.Info("connection ended with error", "remote", ip, "err", err)
			}
		}()
	}
}

func (l *guardedListener) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxTotal || l.perIP[ip] >= l.maxPerIP {
		return false
	}
	l.total++
	l.perIP[ip]++
	return true
}

func (l *guardedListener) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

// stopAccepting stops new connections and waits (up to wait) for mochi to
// finish setting up the ones already accepted.
func (l *guardedListener) stopAccepting(wait time.Duration) {
	l.end.Store(true)
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
	closeClients(l.id)
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
