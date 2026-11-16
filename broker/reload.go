package broker

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/mochi-mqtt/server/v2/packets"
)

// ReloadResult says what a config reload changed.
type ReloadResult struct {
	Added, Removed, Changed []string // agent IDs
	Disconnected            []string // agents dropped because they were removed or their token changed
	RestartNeeded           []string // settings that changed but only take effect on restart
}

func (r ReloadResult) String() string {
	return fmt.Sprintf("added %v, removed %v, changed %v, disconnected %v, restart needed for %v",
		r.Added, r.Removed, r.Changed, r.Disconnected, r.RestartNeeded)
}

// reloadInfo is what Status reports about the config file.
type reloadInfo struct {
	mu            sync.Mutex
	path          string
	loadedAt      time.Time
	reloads       int
	lastErr       string
	restartNeeded []string
}

// Reload applies a new config to the running broker. Agents (their
// tokens, permissions, rates and keys), the broker-wide rate limit and the
// clock-skew window take effect immediately. Everything else (listener,
// TLS, storage, connection and queue limits, intervals) keeps its current
// value and is listed in RestartNeeded.
func (b *Broker) Reload(next *Config) (ReloadResult, error) {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	return b.reloadLocked(next)
}

func (b *Broker) reloadLocked(next *Config) (ReloadResult, error) {
	next.ApplyDefaults()
	if err := next.Validate(); err != nil {
		return ReloadResult{}, err
	}
	cur := b.hook.state().cfg

	// Start from the running config; take only the reloadable fields.
	eff := *cur
	eff.Agents = slices.Clone(next.Agents)
	eff.RatePerSecond, eff.RateBurst, eff.MaxClockSkew = next.RatePerSecond, next.RateBurst, next.MaxClockSkew

	var res ReloadResult
	for _, f := range []struct {
		name     string
		cur, nxt any
	}{
		{"listen", cur.Listen, next.Listen}, {"tls_cert", cur.TLSCert, next.TLSCert}, {"tls_key", cur.TLSKey, next.TLSKey},
		{"allow_insecure", cur.AllowInsecure, next.AllowInsecure}, {"data_dir", cur.DataDir, next.DataDir},
		{"audit_file", cur.AuditFile, next.AuditFile}, {"audit_max_bytes", cur.AuditMaxBytes, next.AuditMaxBytes},
		{"audit_keep", cur.AuditKeep, next.AuditKeep}, {"max_envelope_bytes", cur.MaxEnvelopeBytes, next.MaxEnvelopeBytes},
		{"max_queued", cur.MaxQueued, next.MaxQueued}, {"session_expiry", cur.SessionExpiry, next.SessionExpiry},
		{"max_connections", cur.MaxConnections, next.MaxConnections},
		{"max_connections_per_ip", cur.MaxConnectionsPerIP, next.MaxConnectionsPerIP},
		{"connect_timeout", cur.ConnectTimeout, next.ConnectTimeout}, {"dedupe_window", cur.DedupeWindow, next.DedupeWindow},
		{"status_interval", cur.StatusInterval, next.StatusInterval},
		{"metrics_listen", cur.MetricsListen, next.MetricsListen},
		{"mcp_listen", cur.MCPListen, next.MCPListen},
		{"join_listen", cur.JoinListen, next.JoinListen},
	} {
		if !reflect.DeepEqual(f.cur, f.nxt) {
			res.RestartNeeded = append(res.RestartNeeded, f.name)
		}
	}

	old := map[string]AgentConfig{}
	for _, a := range cur.Agents {
		old[a.ID] = a
	}
	var drop []string
	for _, a := range eff.Agents {
		prev, ok := old[a.ID]
		switch {
		case !ok:
			res.Added = append(res.Added, a.ID)
		case !reflect.DeepEqual(prev, a):
			res.Changed = append(res.Changed, a.ID)
			if prev.TokenSHA256 != a.TokenSHA256 {
				drop = append(drop, a.ID)
			}
		}
		delete(old, a.ID)
	}
	for id := range old {
		res.Removed = append(res.Removed, id)
		drop = append(drop, id)
	}
	slices.Sort(res.Removed)

	st := newHookState(&eff)
	b.hook.st.Store(st)
	// A login that authenticated against the old config but hasn't
	// finished setting up its session is caught in OnSessionEstablished,
	// which checks the login again against this state.
	for _, id := range drop {
		if cl, ok := b.server.Clients.Get(id); ok && !cl.Closed() {
			_ = b.server.DisconnectClient(cl, packets.ErrNotAuthorized)
			res.Disconnected = append(res.Disconnected, id)
		}
	}
	b.hook.applyReload(st, res.Removed, res.Changed)
	return res, nil
}

// ReloadFrom loads path and applies it with Reload, recording the outcome
// for Status.
func (b *Broker) ReloadFrom(path string) (ReloadResult, error) {
	// Held across the read and the apply, so a SIGHUP and the file watcher
	// can't apply an older read after a newer one.
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	cfg, err := LoadConfig(path)
	if err == nil {
		var res ReloadResult
		res, err = b.reloadLocked(cfg)
		if err == nil {
			b.reload.mu.Lock()
			b.reload.path, b.reload.loadedAt, b.reload.lastErr = path, time.Now().UTC(), ""
			b.reload.reloads++
			b.reload.restartNeeded = res.RestartNeeded
			b.reload.mu.Unlock()
			b.log.Info("config reloaded", "path", path, "added", res.Added, "removed", res.Removed,
				"changed", res.Changed, "disconnected", res.Disconnected, "restart_needed", res.RestartNeeded)
			return res, nil
		}
	}
	b.reload.mu.Lock()
	b.reload.lastErr = err.Error()
	b.reload.mu.Unlock()
	b.log.Error("config reload failed; keeping the running config", "path", path, "err", err)
	return ReloadResult{}, err
}

// SetConfigPath records which file the running config came from.
func (b *Broker) SetConfigPath(path string) {
	b.reload.mu.Lock()
	b.reload.path, b.reload.loadedAt = path, time.Now().UTC()
	b.reload.mu.Unlock()
}

// WatchConfig reloads path whenever its modification time or size
// changes, checking every interval until ctx ends. Edits from
// `agentbus config`, or any editor, take effect within one interval.
func (b *Broker) WatchConfig(ctx context.Context, path string, interval time.Duration) {
	b.SetConfigPath(path)
	last := fileStamp(path)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st := fileStamp(path); st != last && st != "" {
			last = st
			_, _ = b.ReloadFrom(path)
		}
	}
}

func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
}
