package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// startMetrics serves /metrics, /healthz and /readyz on addr. It returns
// once the address is bound.
func (b *Broker) startMetrics(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics_listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", b.serveHealth)
	mux.HandleFunc("GET /readyz", b.serveReady)
	mux.HandleFunc("GET /metrics", b.serveMetrics)
	b.metrics = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	b.metricsAddr = ln.Addr().String()
	go func() {
		if err := b.metrics.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.log.Error("metrics server", "err", err)
		}
	}()
	return nil
}

func (b *Broker) stopMetrics() {
	if b.metrics == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.metrics.Shutdown(ctx)
}

// MetricsAddr is the bound metrics address, or "" when metrics are off.
func (b *Broker) MetricsAddr() string { return b.metricsAddr }

// serveHealth answers 200 while the broker process is serving.
func (b *Broker) serveHealth(w http.ResponseWriter, _ *http.Request) {
	if b.hook.closing.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

// serveReady answers 200 once the broker accepts connections and its last
// config reload (if any) succeeded.
func (b *Broker) serveReady(w http.ResponseWriter, _ *http.Request) {
	switch {
	case b.hook.closing.Load():
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	case !b.ready.Load():
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	b.reload.mu.Lock()
	lastErr := b.reload.lastErr
	b.reload.mu.Unlock()
	if lastErr != "" {
		http.Error(w, "last config reload failed: "+lastErr, http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

// serveMetrics writes Prometheus text format. Label values come from the
// config (agent IDs, capabilities) and fixed code lists, never from
// message content, so a sender can't create unbounded series.
func (b *Broker) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	st := b.Status()
	var m promWriter

	m.help("agentbus_build_info", "gauge", "Build information.")
	m.sample("agentbus_build_info", 1, "version", st.Version, "mochi_version", st.Mochi)
	m.help("agentbus_uptime_seconds", "gauge", "Seconds since the broker started.")
	m.sample("agentbus_uptime_seconds", float64(st.Uptime))

	m.help("agentbus_envelopes_accepted_total", "counter", "Envelopes accepted and forwarded.")
	m.sample("agentbus_envelopes_accepted_total", float64(st.Decisions.Accepted))
	m.help("agentbus_envelopes_duplicate_total", "counter", "Envelopes dropped as duplicates.")
	m.sample("agentbus_envelopes_duplicate_total", float64(st.Decisions.Duplicates))
	m.help("agentbus_envelopes_rejected_total", "counter", "Publishes rejected, by code.")
	codes := make([]string, 0, len(st.Decisions.Rejected))
	for c := range st.Decisions.Rejected {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	for _, c := range codes {
		m.sample("agentbus_envelopes_rejected_total", float64(st.Decisions.Rejected[c]), "code", c)
	}
	m.help("agentbus_auth_failures_total", "counter", "Failed logins.")
	m.sample("agentbus_auth_failures_total", float64(st.Decisions.AuthFailures))
	m.help("agentbus_wills_refused_total", "counter", "Connections refused for carrying a Will.")
	m.sample("agentbus_wills_refused_total", float64(st.Decisions.WillsRefused))

	m.help("agentbus_agent_online", "gauge", "1 if the agent is connected.")
	m.help("agentbus_agent_session", "gauge", "1 if the agent has a session (connected or waiting).")
	m.help("agentbus_agent_queued", "gauge", "Messages in flight or queued for the agent.")
	m.help("agentbus_agent_accepted_total", "counter", "Envelopes accepted from the agent.")
	m.help("agentbus_agent_rejected_total", "counter", "Publishes rejected from the agent.")
	for _, a := range st.Agents {
		m.sample("agentbus_agent_online", b2f(a.State == "online"), "agent", a.ID)
		m.sample("agentbus_agent_session", b2f(a.State != "none"), "agent", a.ID)
		m.sample("agentbus_agent_queued", float64(a.Queued), "agent", a.ID)
		m.sample("agentbus_agent_accepted_total", float64(a.Accepted), "agent", a.ID)
		m.sample("agentbus_agent_rejected_total", float64(a.Rejected), "agent", a.ID)
	}

	m.help("agentbus_requests_open", "gauge", "Requests accepted and not yet answered or expired.")
	m.help("agentbus_requests_total", "counter", "Requests that finished, by outcome.")
	m.help("agentbus_request_duration_seconds", "histogram", "Time from request to accepted reply.")
	for _, r := range st.Requests {
		m.sample("agentbus_requests_open", float64(r.Open), "target", r.Target)
		for _, o := range []struct {
			name string
			n    int64
		}{{"answered", r.Answered}, {"errored", r.Errored}, {"expired", r.Expired}, {"evicted", r.Evicted}} {
			m.sample("agentbus_requests_total", float64(o.n), "target", r.Target, "outcome", o.name)
		}
		for i, n := range r.Buckets {
			le := "+Inf"
			if i < len(latencyBounds) {
				le = strconv.FormatFloat(latencyBounds[i], 'g', -1, 64)
			}
			m.sample("agentbus_request_duration_seconds_bucket", float64(n), "target", r.Target, "le", le)
		}
		var count int64
		if len(r.Buckets) > 0 {
			count = r.Buckets[len(r.Buckets)-1]
		}
		m.sample("agentbus_request_duration_seconds_sum", r.LatencySum, "target", r.Target)
		m.sample("agentbus_request_duration_seconds_count", float64(count), "target", r.Target)
	}

	t := st.Traffic
	for _, g := range []struct {
		name, kind, help string
		v                int64
	}{
		{"agentbus_clients_connected", "gauge", "MQTT clients connected.", t.ClientsConnected},
		{"agentbus_sessions", "gauge", "Agent sessions, connected or waiting.", t.Sessions},
		{"agentbus_subscriptions", "gauge", "MQTT subscriptions.", t.Subscriptions},
		{"agentbus_retained", "gauge", "Retained messages.", t.Retained},
		{"agentbus_inflight", "gauge", "QoS 1 messages in flight or queued.", t.Inflight},
		{"agentbus_inflight_dropped_total", "counter", "Messages mochi dropped for a full queue.", t.InflightDropped},
		{"agentbus_mqtt_messages_received_total", "counter", "MQTT packets received.", t.MessagesReceived},
		{"agentbus_mqtt_messages_sent_total", "counter", "MQTT packets sent.", t.MessagesSent},
		{"agentbus_mqtt_bytes_received_total", "counter", "Bytes received.", t.BytesReceived},
		{"agentbus_mqtt_bytes_sent_total", "counter", "Bytes sent.", t.BytesSent},
	} {
		m.help(g.name, g.kind, g.help)
		m.sample(g.name, float64(g.v))
	}

	m.help("agentbus_config_reloads_total", "counter", "Successful config reloads.")
	m.sample("agentbus_config_reloads_total", float64(st.Config.Reloads))
	m.help("agentbus_config_last_reload_failed", "gauge", "1 if the last config reload failed.")
	m.sample("agentbus_config_last_reload_failed", b2f(st.Config.LastError != ""))
	m.help("agentbus_config_restart_needed", "gauge", "1 if a changed setting needs a restart.")
	m.sample("agentbus_config_restart_needed", b2f(len(st.Config.RestartNeeded) > 0))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, m.String())
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// promWriter builds Prometheus text exposition format.
type promWriter struct{ strings.Builder }

func (p *promWriter) help(name, kind, help string) {
	fmt.Fprintf(p, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample writes one line; labels are name, value pairs.
func (p *promWriter) sample(name string, v float64, labels ...string) {
	p.WriteString(name)
	if len(labels) > 0 {
		p.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				p.WriteByte(',')
			}
			fmt.Fprintf(p, "%s=%q", labels[i], escapeLabel(labels[i+1]))
		}
		p.WriteByte('}')
	}
	p.WriteByte(' ')
	p.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	p.WriteByte('\n')
}

// escapeLabel keeps a label value to what %q writes as valid Prometheus:
// printable ASCII. Agent IDs and capabilities already are.
func escapeLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}
