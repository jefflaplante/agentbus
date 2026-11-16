package tui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/table"

	"github.com/jefflaplante/agentbus/a2a"
)

// refreshEvery is how often live views re-read presence.
const refreshEvery = 500 * time.Millisecond

// AgentRows renders presence cards as table rows: agent, status, since,
// capabilities, keys, description.
func AgentRows(cards []a2a.Presence) []table.Row {
	rows := make([]table.Row, 0, len(cards))
	for _, p := range cards {
		keys := ""
		switch {
		case p.Keys != nil && p.KeysPinned:
			keys = "pinned"
		case p.Keys != nil:
			keys = "claimed"
		}
		rows = append(rows, table.Row{
			Clean(p.Agent), Clean(string(p.Status)), p.TS.Local().Format("Jan 02 15:04:05"),
			Clean(strings.Join(p.Capabilities, ",")), keys, Clean(p.Description),
		})
	}
	return rows
}

// Counts returns how many agents are online and offline.
func Counts(cards []a2a.Presence) (online, offline int) {
	for _, p := range cards {
		if p.Status == a2a.StatusOnline {
			online++
		} else {
			offline++
		}
	}
	return
}
