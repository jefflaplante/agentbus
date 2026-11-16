package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/broker"
)

// StatusMsg delivers a broker status snapshot.
type StatusMsg broker.Status

type statusClosedMsg struct{}

func waitForStatus(ch <-chan broker.Status) tea.Cmd {
	return func() tea.Msg {
		st, ok := <-ch
		if !ok {
			return statusClosedMsg{}
		}
		return StatusMsg(st)
	}
}

// Rates are per-second message rates between two snapshots.
type Rates struct{ In, Out float64 }

// RatesBetween computes message rates from prev to cur.
func RatesBetween(prev, cur broker.Status) Rates {
	dt := cur.Time.Sub(prev.Time).Seconds()
	if dt <= 0 || prev.Time.IsZero() {
		return Rates{}
	}
	return Rates{
		In:  float64(cur.Traffic.MessagesReceived-prev.Traffic.MessagesReceived) / dt,
		Out: float64(cur.Traffic.MessagesSent-prev.Traffic.MessagesSent) / dt,
	}
}

// requestSummary totals request outcomes across targets: open, answered,
// errored and expired, with expired highlighted since it means no reply.
func requestSummary(rs []broker.RequestStats) string {
	var open int
	var ok, errored, expired int64
	for _, r := range rs {
		open += r.Open
		ok += r.Answered
		errored += r.Errored
		expired += r.Expired
	}
	exp := fmt.Sprintf("%d expired", expired)
	if expired > 0 {
		exp = ErrStyle.Render(exp)
	}
	return fmt.Sprintf("%d open · %d ok · %d err · ", open, ok, errored) + exp
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// BrokerSummary is a one-line broker summary for the dashboard header.
func BrokerSummary(st broker.Status) string {
	var rej int64
	for _, n := range st.Decisions.Rejected {
		rej += n
	}
	return fmt.Sprintf("broker up %s · %d clients · %d accepted · %d rejected · %d queued",
		time.Duration(st.Uptime)*time.Second, st.Traffic.ClientsConnected, st.Decisions.Accepted, rej, st.Traffic.Inflight)
}

// tailTrunc keeps the end of s (the useful part of a path) within width.
func tailTrunc(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return "…" + string(r[len(r)-width+1:])
}
