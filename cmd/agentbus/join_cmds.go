package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/tui"
)

// joinPoll is how often "agentbus join --wait" asks how its request stands.
var joinPoll = 3 * time.Second

func newJoinCmd(a *app) *cobra.Command {
	var joinURL, description, caFile string
	var caps []string
	var wait time.Duration
	c := &cobra.Command{
		Use:   "join",
		Short: "Ask a broker to add this machine as an agent, pending approval",
		Long: `Ask a broker to add --agent, without anyone copying a token to this machine.

join makes a token, keeps it in a token file here, and sends the broker
only its SHA-256, with the agent ID and the capabilities this agent would
like to serve. The request then waits on the broker until an operator
approves it there ("agentbus config join", or the console's Config tab).
The operator decides which capabilities to grant; what you ask for with
--capability is only a request.

Run it again to see how the request stands; it reuses the token file, so
it never asks twice. With --wait it blocks until the request is answered.
Once approved it prints the agent's AGENTBUS_* settings on stdout.

The broker needs join_listen set. Use an https:// URL off-host: over
plain http, someone on the path could swap in their own token.`,
		Example: `  agentbus join --join-url https://bus.example.com:8884 --agent worker-7 --capability summarize
  agentbus join --join-url https://bus.example.com:8884 --agent worker-7 --wait 1h > worker-7.env
  source worker-7.env && agentbus handle --capability summarize --exec ./summarize.sh`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if joinURL == "" {
				return errors.New("--join-url is required (or AGENTBUS_JOIN_URL): the broker's join_listen address, like https://bus.example.com:8884")
			}
			u, err := neturl.Parse(joinURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("--join-url %q: want an http:// or https:// URL", joinURL)
			}
			if a.agent == "" {
				return errors.New("--agent is required (or AGENTBUS_AGENT): the agent ID to ask for")
			}
			if a.token != "" {
				return errors.New("join makes its own token; drop --token (and unset AGENTBUS_TOKEN)")
			}
			if u.Scheme == "http" && !confstore.Loopback(u.Host) {
				lipgloss.Fprintln(a.stderr, tui.WarnStyle.Render("! plain http: someone on the network path could replace this request with their own"))
			}
			hc, err := joinHTTPClient(caFile)
			if err != nil {
				return err
			}
			tokenFile := a.tokenFile
			if tokenFile == "" {
				tokenFile = a.location().TokenFile(a.agent)
			}
			token, err := joinToken(tokenFile)
			if err != nil {
				return err
			}
			host, _ := os.Hostname()
			req := confstore.JoinRequest{ID: a.agent, TokenSHA256: broker.HashToken(token), Capabilities: caps,
				Description: description, Host: host}
			req.Normalize()
			if err := req.Validate(); err != nil {
				return err
			}
			base := strings.TrimSuffix(u.String(), "/")
			base = strings.TrimSuffix(base, "/join")

			ok := tui.OKStyle.Render("✓")
			reply, err := postJoin(cmd.Context(), hc, base+"/join", req)
			if err != nil {
				return err
			}
			if reply.Status == joinPending {
				lipgloss.Fprintf(a.stderr, "%s asked %s to add %s; the token stays in %s\n"+
					"  Waiting for an operator. On the broker: agentbus config join approve %s\n",
					ok, u.Host, req.ID, tokenFile, req.ID)
				if wait <= 0 {
					lipgloss.Fprintln(a.stderr, "  Run this command again to see how the request stands, or add --wait 1h to block until it's answered.")
					return nil
				}
				if reply, err = waitJoin(cmd.Context(), hc, base+"/join/status", req, wait); err != nil {
					return err
				}
			}
			if reply.Status != joinApproved {
				return fmt.Errorf("unexpected answer from %s: status %q", u.Host, reply.Status)
			}
			busURL := a.url
			if busURL == "" {
				scheme := "tcp"
				if reply.TLS {
					scheme = "ssl"
				}
				busURL = scheme + "://" + net.JoinHostPort(u.Hostname(), reply.MQTTPort)
			}
			granted := "no capabilities (it can send, request and receive)"
			if len(reply.Capabilities) > 0 {
				granted = "capabilities " + strings.Join(reply.Capabilities, ", ")
			}
			lipgloss.Fprintf(a.stderr, "%s %s is approved: granted %s\n", ok, req.ID, granted)
			var missing []string
			for _, c := range req.Capabilities {
				if !slices.Contains(reply.Capabilities, c) && !slices.Contains(reply.Capabilities, "*") {
					missing = append(missing, c)
				}
			}
			if len(missing) > 0 {
				lipgloss.Fprintln(a.stderr, tui.WarnStyle.Render("  not granted: "+strings.Join(missing, ", ")))
			}
			fmt.Fprint(a.stdout, confstore.ClientEnv(busURL, req.ID, tokenFile))
			return nil
		},
	}
	c.Flags().StringVar(&joinURL, "join-url", os.Getenv("AGENTBUS_JOIN_URL"), "the broker's join endpoint, like https://bus.example.com:8884 (env AGENTBUS_JOIN_URL)")
	c.Flags().StringArrayVar(&caps, "capability", nil, "task queue this agent would like to serve (repeatable); the operator decides")
	c.Flags().StringVar(&description, "description", "", "a note for the operator about what this agent is")
	c.Flags().DurationVar(&wait, "wait", 0, "block this long for an answer, like 10m (default: send the request and exit)")
	c.Flags().StringVar(&caFile, "ca-file", "", "PEM certificate (or CA) to trust for the join URL, for a broker with a private certificate")
	return c
}

// joinToken reads the token in file, or makes one and writes it there
// first, so the token behind a request is never lost.
func joinToken(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read token file: %w", err)
	}
	tok, _ := broker.NewToken()
	if err := confstore.WriteToken(file, tok); err != nil {
		return "", fmt.Errorf("write token file: %w", err)
	}
	return tok, nil
}

func joinHTTPClient(caFile string) (*http.Client, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, Proxy: http.ProxyFromEnvironment}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("--ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca-file %s: no PEM certificate", caFile)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}, nil
}

// postJoin sends req to a join endpoint. A 404 from /join/status comes
// back as status "unknown"; any other refusal is an error.
func postJoin(ctx context.Context, hc *http.Client, url string, req confstore.JoinRequest) (joinReply, error) {
	var reply joinReply
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(hr)
	if err != nil {
		return reply, fmt.Errorf("join: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(data, &reply); err != nil || (reply.Status == "" && reply.Error == "") {
		return reply, fmt.Errorf("join: %s answered %s, not a join reply; is join_listen set on the broker, and is this its address?",
			url, resp.Status)
	}
	if reply.Status == "" {
		return reply, fmt.Errorf("join refused: %s", tui.Clean(reply.Error))
	}
	return reply, nil
}

// waitJoin polls until the request is approved, or fails when it is
// denied, expires, or the wait runs out.
func waitJoin(ctx context.Context, hc *http.Client, url string, req confstore.JoinRequest, wait time.Duration) (joinReply, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	t := time.NewTicker(joinPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return joinReply{}, fmt.Errorf("no answer for %s after %s; the request is still waiting, so run this again later", req.ID, wait)
		case <-t.C:
		}
		reply, err := postJoin(ctx, hc, url, req)
		switch {
		case err != nil && ctx.Err() != nil:
			continue // the wait ran out mid-request: report that
		case err != nil:
			return reply, err
		case reply.Status == joinUnknown:
			return reply, fmt.Errorf("the request for %s was denied, or expired unanswered", req.ID)
		case reply.Status == joinApproved:
			return reply, nil
		}
	}
}

// newConfigJoinCmd is "agentbus config join": the operator's side.
func newConfigJoinCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "join",
		Short: "List, approve and deny agents that asked to join",
		Long: `Answer join requests: clients that ran "agentbus join" against this
broker's join_listen address and are waiting to become agents.

A request names the agent ID the client wants and the capabilities it
would like. Nothing is granted until you approve it, which adds the agent
to the config with the grants you choose; a running broker accepts it
within a second. The client keeps its own token, so there is no token
file for it here. Unanswered requests expire after 24 hours.`,
		Example: `  agentbus config set join_listen 0.0.0.0:8884     # then restart the broker
  agentbus config join                             # who is waiting
  agentbus config join approve worker-7            # grant what it asked for
  agentbus config join approve worker-7 --capability summarize --send-to conduit-home
  agentbus config join approve worker-7 --capability=   # let it join, serve nothing
  agentbus config join deny worker-7`,
		Args: cobra.NoArgs,
	}
	list := func(*cobra.Command, []string) error {
		loc := a.location()
		reqs, err := confstore.Pending(loc)
		if err != nil {
			return err
		}
		if len(reqs) == 0 {
			fmt.Fprintln(a.stderr, "no join requests waiting")
			return nil
		}
		tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tASKS FOR\tFROM\tAGE\tDESCRIPTION")
		for _, r := range reqs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, orDash(strings.Join(r.Capabilities, ",")), orDash(tui.Clean(r.From())),
				time.Since(r.RequestedAt).Round(time.Second), orDash(tui.Clean(r.Description)))
		}
		return tw.Flush()
	}
	c.RunE = list
	ls := &cobra.Command{Use: "list", Short: "List the join requests waiting", Args: cobra.NoArgs, RunE: list}

	var caps, sendTo []string
	var rate float64
	var burst int
	approve := &cobra.Command{
		Use: "approve ID", Short: "Add the agent that asked to join, with the grants you choose", Args: cobra.ExactArgs(1),
		Long: `Approve a join request: add the agent to the config with the token hash
it sent. Without --capability it gets the capabilities it asked for; with
--capability it gets exactly those instead, and --capability= grants none.
It can't be made an observer here; use "agentbus config agent edit".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := a.location()
			req, ok := confstore.FindPending(loc, args[0])
			if !ok {
				return fmt.Errorf("no join request for %q; \"agentbus config join\" lists them", args[0])
			}
			ag := broker.AgentConfig{Capabilities: req.Capabilities, RatePerSecond: rate, RateBurst: burst}
			editAgent(cmd.Flags().Changed, &ag, caps, sendTo, false, rate, burst)
			if _, err := change(loc, func(tx *confstore.Tx) error { return tx.ApproveJoin(req.ID, ag) }); err != nil {
				return err
			}
			ag.ID = req.ID
			lipgloss.Fprintf(a.stderr, "%s approved %s: %s; may send to %s; rate %s\n  A running broker accepts it within a second. Its token stays on %s.\n",
				tui.OKStyle.Render("✓"), ag.ID, tui.AgentRole(ag), tui.SendTo(ag), tui.AgentRate(ag), orDash(tui.Clean(req.From())))
			return nil
		},
	}
	approve.Flags().StringArrayVar(&caps, "capability", nil, "grant these capabilities instead of the ones it asked for (repeatable; empty for none; * for all)")
	approve.Flags().StringArrayVar(&sendTo, "send-to", nil, "agent whose inbox it may message (repeatable; default all)")
	approve.Flags().Float64Var(&rate, "rate", 0, "publish rate limit per second (default: the broker's)")
	approve.Flags().IntVar(&burst, "burst", 0, "publish burst (default: the broker's)")

	deny := &cobra.Command{
		Use: "deny ID", Aliases: []string{"rm", "reject"}, Short: "Discard a join request", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := confstore.RemovePending(a.location(), args[0]); err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s denied %s; nothing was added\n", tui.OKStyle.Render("✓"), args[0])
			return nil
		},
	}
	pendingIDs := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		reqs, err := confstore.Pending(a.location())
		if err != nil || len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var ids []string
		for _, r := range reqs {
			ids = append(ids, r.ID)
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
	approve.ValidArgsFunction, deny.ValidArgsFunction = pendingIDs, pendingIDs
	c.AddCommand(ls, approve, deny)
	return c
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
