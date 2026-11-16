package broker

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/storage/bolt"
	"github.com/mochi-mqtt/server/v2/packets"
	"go.etcd.io/bbolt"
)

// Broker is a running a2a/v1 broker.
type Broker struct {
	server     *mqtt.Server
	hook       *contractHook
	ln         *guardedListener
	log        *slog.Logger
	dedupePath string // "" without data_dir

	reload reloadInfo

	closeOnce  sync.Once
	closeErr   error
	statusDone chan struct{}
}

// New builds a broker from cfg. Call Start to begin listening.
func New(cfg *Config, log *slog.Logger) (*Broker, error) {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	caps := mqtt.NewDefaultServerCapabilities()
	caps.MaximumPacketSize = uint32(cfg.MaxEnvelopeBytes + 1024) // payload plus headers
	caps.MaximumInflight = uint16(min(cfg.MaxQueued, 65535))
	caps.MaximumSessionExpiryInterval = uint32(time.Duration(cfg.SessionExpiry).Seconds())
	caps.MaximumClientWritesPending = 1024
	caps.MaximumQos = 1 // the contract uses QoS 1; mochi mishandles some QoS 2 rejections
	server := mqtt.New(&mqtt.Options{
		InlineClient: true, // the broker publishes presence and error envelopes
		Capabilities: caps,
		Logger:       log.With("component", "mochi"),
	})

	hook := newContractHook(cfg, server, log)
	b := &Broker{server: server, hook: hook, log: log, statusDone: make(chan struct{})}
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("data_dir: %w", err)
		}
		// mochi's bbolt hook persists sessions, subscriptions, queued
		// (in-flight) messages and retained presence cards.
		store := &bolt.Options{
			Path:    filepath.Join(cfg.DataDir, "broker.db"),
			Options: &bbolt.Options{Timeout: 2 * time.Second}, // fail fast if another broker holds the file
		}
		if err := server.AddHook(newBoltStore(), store); err != nil {
			return nil, fmt.Errorf("open %s (is another broker using this data_dir?): %w", store.Path, err)
		}
		b.dedupePath = filepath.Join(cfg.DataDir, "dedupe.json")
		if err := hook.dedup.load(b.dedupePath, time.Now()); err != nil {
			log.Warn("dedupe state not restored", "err", err)
		}
	}
	if cfg.AuditFile != "" {
		audit, err := openAudit(cfg.AuditFile, cfg.AuditMaxBytes, cfg.AuditKeep, log)
		if err != nil {
			return nil, err
		}
		hook.audit = audit
	}
	if err := server.AddHook(hook, nil); err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load TLS key pair: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	} else if !isLoopback(cfg.Listen) {
		log.Warn("plaintext listener on a non-loopback address: agent tokens cross the network in clear",
			"listen", cfg.Listen, "fix", "set tls_cert and tls_key")
	}
	ln := newGuardedListener("tcp", cfg.Listen, tlsConfig, cfg)
	ln.disconnectAll = func(id string) { closeListenerClients(server, id) }
	if err := server.AddListener(ln); err != nil {
		return nil, err
	}
	b.ln = ln
	return b, nil
}

// Start loads any stored state, then begins accepting connections. It
// returns once the listener is bound.
func (b *Broker) Start() error {
	if err := b.server.Serve(); err != nil { // reads the store before returning
		return err
	}
	b.hook.restorePresence()
	b.ln.open()
	go b.publishStatus(time.Duration(b.hook.state().cfg.StatusInterval), b.statusDone)
	return nil
}

// Addr is the bound listener address ("host:port"), useful when Listen
// used port 0.
func (b *Broker) Addr() string { return b.ln.Address() }

// Close stops the broker and disconnects all clients.
// Calling it more than once is safe.
func (b *Broker) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.close() })
	return b.closeErr
}

func (b *Broker) close() error {
	close(b.statusDone)
	b.ln.stopAccepting(time.Second)
	err := b.server.Close() // also closes the bbolt store
	if b.dedupePath != "" {
		if serr := b.hook.dedup.save(b.dedupePath, time.Now()); serr != nil {
			b.log.Warn("dedupe state not saved", "err", serr)
		}
	}
	b.hook.audit.close()
	return err
}

// closeListenerClients replaces mochi's version, whose Clients.GetByListener
// takes the clients read lock twice (it calls Len inside RLock). A client
// disconnecting in between queues a write lock that blocks the second read,
// and shutdown hangs. GetAll takes the lock once.
func closeListenerClients(server *mqtt.Server, id string) {
	for _, cl := range server.Clients.GetAll() {
		if cl.Net.Listener == id && !cl.Closed() {
			_ = server.DisconnectClient(cl, packets.ErrServerShuttingDown)
		}
	}
}
