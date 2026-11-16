// Package tui holds the Bubble Tea views behind the agentbus CLI's
// interactive commands: the broker status view, the live agents table,
// the message feed, the request spinner and the dashboard.
//
// It uses Bubble Tea v2, which asks the terminal for its background
// color from inside a running program. Bubble Tea v1 asked at package
// init and waited up to 5 seconds for terminals that never answer,
// which delayed every command, including non-interactive ones.
package tui

import (
	"image/color"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jefflaplante/agentbus/a2a"
)

// palette is one set of colors; there's one for dark and one for light
// terminals.
type palette struct {
	accent, subtle, text, green, red, blue, magenta, yellow color.Color
}

var (
	darkPalette = palette{
		accent: lipgloss.Color("#9F86FF"), subtle: lipgloss.Color("#6C6C6C"), text: lipgloss.Color("#E4E4E4"),
		green: lipgloss.Color("#3DD68C"), red: lipgloss.Color("#FF6B6B"), blue: lipgloss.Color("#5EB1FF"),
		magenta: lipgloss.Color("#F07AC2"), yellow: lipgloss.Color("#F5D547"),
	}
	lightPalette = palette{
		accent: lipgloss.Color("#5A3FD9"), subtle: lipgloss.Color("#8A8A8A"), text: lipgloss.Color("#1F1F1F"),
		green: lipgloss.Color("#18794E"), red: lipgloss.Color("#C62828"), blue: lipgloss.Color("#1565C0"),
		magenta: lipgloss.Color("#AD1457"), yellow: lipgloss.Color("#8D6E00"),
	}
	pal = darkPalette
)

// Styles shared by the views. SetDark rebuilds them for the terminal's
// background; until a view hears back from the terminal they're dark.
var (
	TitleStyle   lipgloss.Style
	SubtleStyle  lipgloss.Style
	TextStyle    lipgloss.Style
	AccentStyle  lipgloss.Style
	OKStyle      lipgloss.Style
	ErrStyle     lipgloss.Style
	WarnStyle    lipgloss.Style
	HelpStyle    lipgloss.Style
	PanelStyle   lipgloss.Style
	OnlineStyle  lipgloss.Style
	OfflineStyle lipgloss.Style
)

func init() { SetDark(true) } // pure assignment; no terminal I/O

// SetDark picks the dark or light palette and rebuilds the styles.
func SetDark(dark bool) {
	pal = lightPalette
	if dark {
		pal = darkPalette
	}
	TitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(pal.accent).Padding(0, 1)
	SubtleStyle = lipgloss.NewStyle().Foreground(pal.subtle)
	TextStyle = lipgloss.NewStyle().Foreground(pal.text)
	AccentStyle = lipgloss.NewStyle().Foreground(pal.accent).Bold(true)
	OKStyle = lipgloss.NewStyle().Foreground(pal.green).Bold(true)
	ErrStyle = lipgloss.NewStyle().Foreground(pal.red).Bold(true)
	WarnStyle = lipgloss.NewStyle().Foreground(pal.yellow)
	HelpStyle = lipgloss.NewStyle().Foreground(pal.subtle).PaddingLeft(1)
	PanelStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(pal.subtle).Padding(0, 1)
	OnlineStyle = lipgloss.NewStyle().Foreground(pal.green)
	OfflineStyle = lipgloss.NewStyle().Foreground(pal.subtle)
}

// Panel chrome. In lipgloss v2 a style's Width and Height include its
// border and padding, so a panel of total width w has w-panelPadX
// columns for content and h-panelPadY rows.
const (
	panelPadX = 4 // 2 border + 2 padding
	panelPadY = 2 // 2 border
)

// handleBackground updates the palette when the terminal reports its
// background color. Views call it from Update.
func handleBackground(msg tea.Msg) bool {
	if bg, ok := msg.(tea.BackgroundColorMsg); ok {
		SetDark(bg.IsDark())
		return true
	}
	return false
}

// fullScreen wraps content as an alt-screen view.
func fullScreen(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// StatusStyle colors an agent status.
func StatusStyle(s a2a.Status) lipgloss.Style {
	if s == a2a.StatusOnline {
		return OnlineStyle
	}
	return OfflineStyle
}

// typeStyle colors an envelope type badge.
func typeStyle(t a2a.Type) lipgloss.Style {
	base := lipgloss.NewStyle().Bold(true).Width(8)
	switch t {
	case a2a.TypeRequest:
		return base.Foreground(pal.blue)
	case a2a.TypeResponse:
		return base.Foreground(pal.green)
	case a2a.TypeEvent:
		return base.Foreground(pal.magenta)
	case a2a.TypeError:
		return base.Foreground(pal.red)
	}
	return base.Foreground(pal.subtle)
}

func priorityStyle(p a2a.Priority) lipgloss.Style {
	switch p {
	case a2a.PriorityHigh:
		return lipgloss.NewStyle().Foreground(pal.red).Bold(true)
	case a2a.PriorityLow, a2a.PriorityInfo:
		return SubtleStyle
	}
	return WarnStyle
}

// restyleTable applies the current palette to a bubbles table.
func restyleTable(t *table.Model) {
	st := table.DefaultStyles()
	st.Header = tableHeaderStyle()
	st.Selected = tableSelectedStyle()
	t.SetStyles(st)
}

func tableHeaderStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Padding(0, 1).
		BorderStyle(lipgloss.NormalBorder()).BorderForeground(pal.subtle).BorderBottom(true)
}

func tableSelectedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(pal.accent)
}
