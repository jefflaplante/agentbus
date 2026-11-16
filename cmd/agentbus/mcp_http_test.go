package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jefflaplante/agentbus/broker"
)

// writeCert writes a self-signed certificate for host (and nothing else:
// no 127.0.0.1) and its key, and returns their paths.
func writeCert(t *testing.T, host string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return (&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}).RoundTrip(r)
}

// With TLS on, serve's MCP endpoint speaks HTTPS, and the gateway reaches
// the broker's TLS listener over loopback with the certificate pinned,
// though the certificate names bus.example.com and not 127.0.0.1.
func TestServeMCPOverHTTPSWithTLSBroker(t *testing.T) {
	certFile, keyFile := writeCert(t, "bus.example.com")
	tok, hash := broker.NewToken()
	cfg := &broker.Config{Listen: "127.0.0.1:0", TLSCert: certFile, TLSKey: keyFile, MCPListen: "127.0.0.1:0",
		Agents: []broker.AgentConfig{{ID: "claude-code", TokenSHA256: hash}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := broker.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	stop, addr, err := startMCPGateway(b, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: "https://" + addr + "/mcp", HTTPClient: &http.Client{Transport: bearerRT{tok}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("MCP over HTTPS: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "agents"})
	if err != nil || res.IsError {
		t.Fatalf("agents: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "claude-code (online)") {
		t.Errorf("agents: %s", text)
	}
}

func TestLoopbackBroker(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:1883": "tcp://127.0.0.1:1883", "0.0.0.0:1883": "tcp://127.0.0.1:1883",
		"[::]:1883": "tcp://127.0.0.1:1883", ":1883": "tcp://127.0.0.1:1883", "10.0.0.5:1883": "tcp://10.0.0.5:1883",
	} {
		got, tlsCfg, err := loopbackBroker(addr, &broker.Config{})
		if err != nil || got != want || tlsCfg != nil {
			t.Errorf("%s: %s %v %v, want %s", addr, got, tlsCfg, err, want)
		}
	}
}

// config mcp prints setup for each client without the token in it.
func TestConfigMCP(t *testing.T) {
	isolate(t)
	if _, _, err := runCLI(t, "", "config", "init"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", "config", "agent", "add", "claude-code"); err != nil {
		t.Fatal(err)
	}
	tokPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentbus", "tokens", "claude-code.token")
	tok, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	secret := strings.TrimSpace(string(tok))

	// No mcp_listen: stdio.
	out, _, err := runCLI(t, "", "config", "mcp", "claude-code")
	if err != nil || !strings.Contains(out, "-- agentbus mcp") || !strings.Contains(out, "AGENTBUS_TOKEN_FILE") {
		t.Fatalf("stdio claude: %v\n%s", err, out)
	}
	if _, _, err := runCLI(t, "", "config", "mcp", "claude-code", "--transport", "http"); err == nil ||
		!strings.Contains(err.Error(), "mcp_listen") {
		t.Errorf("http without mcp_listen: %v", err)
	}

	if _, _, err := runCLI(t, "", "config", "set", "mcp_listen", "127.0.0.1:8765"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ client, want string }{
		{"claude", `--transport http agentbus http://127.0.0.1:8765/mcp`},
		{"hermes", `Authorization: "Bearer ${AGENTBUS_TOKEN}"`},
		{"openclaw", `"transport":"streamable-http"`},
		{"json", `"type": "http"`},
	} {
		out, _, err := runCLI(t, "", "config", "mcp", "claude-code", "--client", c.client)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", c.client, err, out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("%s: the token itself is in the output", c.client)
		}
	}
	if _, _, err := runCLI(t, "", "config", "mcp", "claude-code", "--client", "emacs"); err == nil {
		t.Error("unknown client accepted")
	}
}
