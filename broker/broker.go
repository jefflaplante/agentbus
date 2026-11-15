package broker

import (
	"crypto/tls"
	"fmt"
	"log/slog"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// Broker is a running a2a/v1 broker.
type Broker struct {
	server *mqtt.Server
	hook   *contractHook
	ln     *listeners.TCP
}

// New builds a broker from cfg. Call Start to begin listening.
func New(cfg *Config, log *slog.Logger) (*Broker, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	caps := mqtt.NewDefaultServerCapabilities()
	caps.MaximumPacketSize = uint32(cfg.MaxEnvelopeBytes + 1024) // payload plus headers
	server := mqtt.New(&mqtt.Options{
		InlineClient: true, // the broker publishes presence and error envelopes
		Capabilities: caps,
		Logger:       log.With("component", "mochi"),
	})

	hook := newContractHook(cfg, server, log)
	if err := server.AddHook(hook, nil); err != nil {
		return nil, err
	}

	lc := listeners.Config{ID: "tcp", Address: cfg.Listen}
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load TLS key pair: %w", err)
		}
		lc.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	ln := listeners.NewTCP(lc)
	if err := server.AddListener(ln); err != nil {
		return nil, err
	}
	return &Broker{server: server, hook: hook, ln: ln}, nil
}

// Start begins serving. It returns once the listener is bound.
func (b *Broker) Start() error { return b.server.Serve() }

// Addr is the bound listener address ("host:port"), useful when Listen
// used port 0.
func (b *Broker) Addr() string { return b.ln.Address() }

// Close stops the broker and disconnects all clients.
func (b *Broker) Close() error { return b.server.Close() }
