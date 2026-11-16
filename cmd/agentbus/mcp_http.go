package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/mcpbus"
)

// startMCPGateway serves MCP over HTTP at mcp_listen, if set, for remote
// MCP clients (see docs/INTEGRATIONS.md). The gateway reaches the broker
// over loopback as each agent. The returned stop shuts it down.
func startMCPGateway(b *broker.Broker, cfg *broker.Config, log *slog.Logger) (stop func(), addr string, err error) {
	if cfg.MCPListen == "" {
		return func() {}, "", nil
	}
	url, tlsCfg, err := loopbackBroker(b.Addr(), cfg)
	if err != nil {
		return nil, "", err
	}
	glog := log.With("component", "mcp")
	gw := mcpbus.NewGateway(mcpbus.GatewayOptions{
		BrokerURL: url, TLS: tlsCfg, Version: buildVersion(), Logger: glog,
		Lookup: func(token string) (string, bool, bool) {
			a, ok := b.AgentForToken(token)
			return a.ID, a.Observer, ok
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", gw.Handler())
	srv := &http.Server{
		Handler: mux,
		// No WriteTimeout: tool calls stream for up to 10 minutes.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(glog.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.MCPListen)
	if err != nil {
		gw.Close()
		return nil, "", fmt.Errorf("mcp_listen: %w", err)
	}
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			_ = ln.Close()
			gw.Close()
			return nil, "", fmt.Errorf("mcp_listen: load TLS key pair: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			glog.Error("MCP endpoint stopped", "err", err)
		}
	}()
	log.Info("MCP over HTTP", "addr", ln.Addr().String(), "url", confstore.MCPURL(cfg), "tls", cfg.TLSCert != "")
	return func() {
		gw.Close() // ends sessions, so open streams finish
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, ln.Addr().String(), nil
}

// loopbackBroker is how the gateway, in the broker's own process, reaches
// the MQTT listener: over loopback, and with TLS pinned to the broker's
// own certificate when the listener uses TLS.
func loopbackBroker(addr string, cfg *broker.Config) (string, *tls.Config, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", nil, fmt.Errorf("broker address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	hostport := net.JoinHostPort(host, port)
	if cfg.TLSCert == "" {
		return "tcp://" + hostport, nil, nil
	}
	tlsCfg, err := mcpbus.PinnedTLS(cfg.TLSCert)
	if err != nil {
		return "", nil, err
	}
	return "ssl://" + hostport, tlsCfg, nil
}
