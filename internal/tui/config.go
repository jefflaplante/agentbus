package tui

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

// CloseMsg tells a parent view that an embedded view is done.
type CloseMsg struct{}

type formDoneMsg struct{ aborted bool }

type configMode int

const (
	modeList configMode = iota
	modeForm
	modeConfirm
	modeClient
)

// liveSettings take effect on a running broker without a restart.
var liveSettings = map[string]bool{"rate_per_second": true, "rate_burst": true, "max_clock_skew": true}

// ConfigModel edits the broker config file: agents, their tokens and
// client setup, and broker settings. Every change is validated and saved
// at once; a running broker reloads the file within a second.
type ConfigModel struct {
	loc confstore.Location
	cfg *broker.Config

	// Embedded makes quitting return CloseMsg (to a parent view) instead
	// of ending the program.
	Embedded bool

	tab      int // 0 agents, 1 broker settings
	agents   table.Model
	settings table.Model
	keys     []string // setting keys, in table order

	mode    configMode
	form    *huh.Form
	onForm  func() error // applies a submitted form
	confirm string       // confirmation prompt
	onYes   func() error
	client  string // agent shown in the client setup view
	clientF int    // 0 shell, 1 go, 2 mqtt

	flash    string
	flashErr bool
	width    int
	height   int
	dark     bool

	// form-bound values
	fID, fCaps, fSendTo, fRate, fBurst, fSign, fKex, fValue string
	fObserver                                               bool
}

// NewConfigEditor opens the editor on the config at loc. If focus names an
// agent, it starts with that agent selected.
func NewConfigEditor(loc confstore.Location, focus string) (*ConfigModel, error) {
	cfg, err := confstore.Load(loc.Path)
	if err != nil {
		return nil, err
	}
	m := &ConfigModel{loc: loc, cfg: cfg, width: 110, height: 30, dark: true}
	m.agents = table.New(table.WithColumns(m.agentColumns()), table.WithFocused(true), table.WithHeight(12))
	m.settings = table.New(table.WithColumns(m.settingColumns()), table.WithHeight(12))
	restyleTable(&m.agents)
	restyleTable(&m.settings)
	m.agents.SetWidth(m.width)
	m.settings.SetWidth(m.width)
	m.refresh()
	if i := confstore.Find(cfg, focus); i >= 0 {
		m.agents.SetCursor(i)
	}
	return m, nil
}

func (m *ConfigModel) agentColumns() []table.Column {
	w := []int{16, 26, 20, 12, 8} // AGENT ROLE SEND-TO RATE TOKEN
	if m.width < 110 {
		w = []int{12, 18, 14, 9, 7}
	}
	used := 6 * 2
	for _, n := range w {
		used += n
	}
	return []table.Column{
		{Title: "AGENT", Width: w[0]}, {Title: "ROLE", Width: w[1]}, {Title: "SEND TO", Width: w[2]},
		{Title: "RATE", Width: w[3]}, {Title: "TOKEN", Width: w[4]}, {Title: "KEYS", Width: max(6, m.width-used)},
	}
}

func (m *ConfigModel) settingColumns() []table.Column {
	return []table.Column{{Title: "SETTING", Width: 24}, {Title: "VALUE", Width: max(20, m.width-24-12-6)}, {Title: "APPLIES", Width: 12}}
}

// AgentRole summarizes what an agent may do.
func AgentRole(a broker.AgentConfig) string {
	var parts []string
	if a.Observer {
		parts = append(parts, "observer")
	}
	if len(a.Capabilities) > 0 {
		parts = append(parts, "serves "+strings.Join(a.Capabilities, ","))
	}
	if len(parts) == 0 {
		return "client"
	}
	return strings.Join(parts, "; ")
}

// SendTo summarizes an agent's send_to list.
func SendTo(a broker.AgentConfig) string {
	if len(a.SendTo) == 0 || slices.Contains(a.SendTo, "*") {
		return "anyone"
	}
	return strings.Join(a.SendTo, ",")
}

// AgentRate summarizes an agent's rate override.
func AgentRate(a broker.AgentConfig) string {
	if a.RatePerSecond == 0 && a.RateBurst == 0 {
		return "default"
	}
	return fmt.Sprintf("%g/s b%d", a.RatePerSecond, a.RateBurst)
}

func (m *ConfigModel) refresh() {
	rows := make([]table.Row, 0, len(m.cfg.Agents))
	for _, a := range m.cfg.Agents {
		tok := "missing"
		if _, ok := confstore.ReadToken(m.loc, a.ID); ok {
			tok = "ok"
		}
		keys := ""
		if a.Keys != nil {
			keys = "pinned"
		}
		rows = append(rows, table.Row{a.ID, AgentRole(a), SendTo(a), AgentRate(a), tok, keys})
	}
	m.agents.SetRows(rows)

	eff := *m.cfg
	eff.ApplyDefaults()
	raw := settingValues(&eff)
	m.keys = m.keys[:0]
	var srows []table.Row
	for _, k := range settingOrder {
		applies := "restart"
		if liveSettings[k] {
			applies = "live"
		}
		m.keys = append(m.keys, k)
		srows = append(srows, table.Row{k, raw[k], applies})
	}
	m.settings.SetRows(srows)
}

// settingOrder is the order settings appear in the editor.
var settingOrder = []string{
	"listen", "tls_cert", "tls_key", "allow_insecure", "data_dir", "audit_file", "audit_max_bytes", "audit_keep",
	"rate_per_second", "rate_burst", "max_clock_skew", "max_envelope_bytes", "max_queued", "session_expiry",
	"dedupe_window", "max_connections", "max_connections_per_ip", "connect_timeout", "status_interval",
}

func settingValues(c *broker.Config) map[string]string {
	return map[string]string{
		"listen": c.Listen, "tls_cert": c.TLSCert, "tls_key": c.TLSKey, "allow_insecure": strconv.FormatBool(c.AllowInsecure),
		"data_dir": c.DataDir, "audit_file": c.AuditFile, "audit_max_bytes": strconv.FormatInt(c.AuditMaxBytes, 10),
		"audit_keep": strconv.Itoa(c.AuditKeep), "rate_per_second": strconv.FormatFloat(c.RatePerSecond, 'g', -1, 64),
		"rate_burst": strconv.Itoa(c.RateBurst), "max_clock_skew": dur(c.MaxClockSkew),
		"max_envelope_bytes": strconv.Itoa(c.MaxEnvelopeBytes), "max_queued": strconv.Itoa(c.MaxQueued),
		"session_expiry": dur(c.SessionExpiry), "dedupe_window": dur(c.DedupeWindow),
		"max_connections": strconv.Itoa(c.MaxConnections), "max_connections_per_ip": strconv.Itoa(c.MaxConnectionsPerIP),
		"connect_timeout": dur(c.ConnectTimeout), "status_interval": dur(c.StatusInterval),
	}
}

func dur(d broker.Duration) string {
	b, _ := d.MarshalJSON()
	return strings.Trim(string(b), `"`)
}

// save validates and writes cfg; on success it becomes the editor's config.
func (m *ConfigModel) save(cfg *broker.Config) error {
	if err := confstore.Save(m.loc.Path, cfg); err != nil {
		return err
	}
	m.cfg = cfg
	m.refresh()
	return nil
}

func (m *ConfigModel) clone() *broker.Config {
	c := *m.cfg
	c.Agents = slices.Clone(m.cfg.Agents)
	return &c
}

func (m *ConfigModel) selectedAgent() (broker.AgentConfig, bool) {
	i := m.agents.Cursor()
	if i < 0 || i >= len(m.cfg.Agents) {
		return broker.AgentConfig{}, false
	}
	return m.cfg.Agents[i], true
}

func (m *ConfigModel) say(format string, a ...any) {
	m.flash, m.flashErr = fmt.Sprintf(format, a...), false
}
func (m *ConfigModel) fail(err error) { m.flash, m.flashErr = err.Error(), true }

func (m *ConfigModel) Init() tea.Cmd { return tea.RequestBackgroundColor }

func (m *ConfigModel) quit() tea.Cmd {
	if m.Embedded {
		return func() tea.Msg { return CloseMsg{} }
	}
	return tea.Quit
}

func (m *ConfigModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if bg, ok := msg.(tea.BackgroundColorMsg); ok {
		m.dark = bg.IsDark()
		SetDark(m.dark)
		restyleTable(&m.agents)
		restyleTable(&m.settings)
		return m, nil
	}
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = ws.Width, ws.Height
		m.agents.SetColumns(m.agentColumns())
		m.settings.SetColumns(m.settingColumns())
		m.agents.SetWidth(ws.Width)
		m.settings.SetWidth(ws.Width)
		m.agents.SetHeight(max(4, ws.Height-9))
		m.settings.SetHeight(max(4, ws.Height-9))
	}

	switch m.mode {
	case modeForm:
		if done, ok := msg.(formDoneMsg); ok {
			m.mode = modeList
			if !done.aborted && m.onForm != nil {
				if err := m.onForm(); err != nil {
					m.fail(err)
				}
			}
			return m, nil
		}
		// huh only binds ctrl+c to abort; esc should cancel too.
		if k, ok := msg.(tea.KeyPressMsg); ok && (k.String() == "esc" || k.String() == "ctrl+c") {
			m.mode, m.onForm = modeList, nil
			m.say("canceled, nothing saved")
			return m, nil
		}
		f, cmd := m.form.Update(msg)
		if ff, ok := f.(*huh.Form); ok {
			m.form = ff
		}
		return m, cmd
	case modeConfirm:
		if k, ok := msg.(tea.KeyPressMsg); ok {
			switch k.String() {
			case "y", "Y":
				m.mode = modeList
				if err := m.onYes(); err != nil {
					m.fail(err)
				}
			case "n", "N", "esc", "q":
				m.mode = modeList
				m.say("canceled")
			}
		}
		return m, nil
	case modeClient:
		return m.updateClient(msg)
	}

	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, m.forwardTable(msg)
	}
	switch k.String() {
	case "q", "esc", "ctrl+c":
		return m, m.quit()
	case "tab", "shift+tab":
		m.tab = 1 - m.tab
		if m.tab == 0 {
			m.agents.Focus()
			m.settings.Blur()
		} else {
			m.settings.Focus()
			m.agents.Blur()
		}
		return m, nil
	}
	if m.tab == 1 {
		if k.String() == "enter" || k.String() == "e" {
			return m, m.editSetting()
		}
		return m, m.forwardTable(msg)
	}
	switch k.String() {
	case "a", "n":
		return m, m.agentForm(nil)
	case "enter", "e":
		if a, ok := m.selectedAgent(); ok {
			return m, m.agentForm(&a)
		}
	case "c":
		if a, ok := m.selectedAgent(); ok {
			m.mode, m.client, m.clientF = modeClient, a.ID, 0
		}
	case "d", "x", "delete", "backspace":
		if a, ok := m.selectedAgent(); ok {
			m.askDelete(a.ID)
		}
	case "r":
		if a, ok := m.selectedAgent(); ok {
			m.askRotate(a.ID)
		}
	default:
		return m, m.forwardTable(msg)
	}
	return m, nil
}

func (m *ConfigModel) forwardTable(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.tab == 0 {
		m.agents, cmd = m.agents.Update(msg)
	} else {
		m.settings, cmd = m.settings.Update(msg)
	}
	return cmd
}

func (m *ConfigModel) startForm(title string, fields []huh.Field, apply func() error) tea.Cmd {
	m.form = huh.NewForm(huh.NewGroup(fields...).Title(title)).
		WithTheme(huh.ThemeFunc(huh.ThemeCharm)).WithShowHelp(true).WithWidth(min(m.width-4, 80))
	m.form.SubmitCmd = func() tea.Msg { return formDoneMsg{} }
	m.form.CancelCmd = func() tea.Msg { return formDoneMsg{aborted: true} }
	m.onForm, m.mode = apply, modeForm
	return m.form.Init()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, p)
	}
	return out
}

func validList(allowStar bool) func(string) error {
	return func(s string) error {
		for _, p := range splitList(s) {
			if !(allowStar && p == "*") && !a2a.ValidName(p) {
				return fmt.Errorf("%q: use a-z 0-9 . _ -", p)
			}
		}
		return nil
	}
}

func validNumber(s string) error {
	if s == "" {
		return nil
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return errors.New("a number, or blank for the broker default")
	}
	return nil
}

// agentForm adds an agent (a == nil) or edits one.
func (m *ConfigModel) agentForm(a *broker.AgentConfig) tea.Cmd {
	adding := a == nil
	if adding {
		a = &broker.AgentConfig{}
	}
	m.fID, m.fCaps, m.fSendTo, m.fObserver = a.ID, strings.Join(a.Capabilities, ", "), strings.Join(a.SendTo, ", "), a.Observer
	m.fRate, m.fBurst, m.fSign, m.fKex = "", "", "", ""
	if a.RatePerSecond != 0 {
		m.fRate = strconv.FormatFloat(a.RatePerSecond, 'g', -1, 64)
	}
	if a.RateBurst != 0 {
		m.fBurst = strconv.Itoa(a.RateBurst)
	}
	if a.Keys != nil {
		m.fSign, m.fKex = a.Keys.Sign, a.Keys.KEX
	}

	var fields []huh.Field
	title := "Edit agent " + a.ID
	if adding {
		title = "Add an agent"
		fields = append(fields, huh.NewInput().Title("Agent ID").Description("1-64 of a-z 0-9 . _ -; also its MQTT client ID and username").
			Value(&m.fID).Validate(func(s string) error {
			if !a2a.ValidName(s) {
				return errors.New("use 1-64 of a-z 0-9 . _ -")
			}
			if confstore.Find(m.cfg, s) >= 0 {
				return errors.New("already exists")
			}
			return nil
		}))
	}
	fields = append(fields,
		huh.NewInput().Title("Capabilities").Description("task queues it may consume, comma-separated; * for all; blank for none").
			Value(&m.fCaps).Validate(validList(true)),
		huh.NewInput().Title("May send to").Description("agents whose inboxes it may message, comma-separated; blank for anyone").
			Value(&m.fSendTo).Validate(validList(true)),
		huh.NewConfirm().Title("Observer?").Description("read all traffic and broker status: for admin and monitoring tools only").
			Value(&m.fObserver),
		huh.NewInput().Title("Rate per second").Description("blank for the broker default").Value(&m.fRate).Validate(validNumber),
		huh.NewInput().Title("Burst").Description("blank for the broker default").Value(&m.fBurst).Validate(validNumber),
		huh.NewInput().Title("Pinned signing key").Description("Ed25519 public key, base64; optional").Value(&m.fSign),
		huh.NewInput().Title("Pinned key-exchange key").Description("X25519 public key, base64; optional").Value(&m.fKex),
	)
	orig := a.ID
	return m.startForm(title, fields, func() error {
		next := broker.AgentConfig{ID: strings.TrimSpace(m.fID), Capabilities: splitList(m.fCaps), SendTo: splitList(m.fSendTo),
			Observer: m.fObserver}
		if m.fRate != "" {
			next.RatePerSecond, _ = strconv.ParseFloat(m.fRate, 64)
		}
		if m.fBurst != "" {
			b, _ := strconv.ParseFloat(m.fBurst, 64)
			next.RateBurst = int(b)
		}
		if m.fSign != "" || m.fKex != "" {
			next.Keys = &a2a.Keys{Sign: strings.TrimSpace(m.fSign), KEX: strings.TrimSpace(m.fKex)}
		}
		cfg := m.clone()
		if adding {
			file, err := confstore.AddAgent(cfg, m.loc, next)
			if err != nil {
				return err
			}
			if err := m.save(cfg); err != nil {
				_ = os.Remove(file)
				return err
			}
			m.agents.SetCursor(len(m.cfg.Agents) - 1)
			m.say("added %s · token in %s · press c for client setup", next.ID, file)
			return nil
		}
		i := confstore.Find(cfg, orig)
		next.TokenSHA256 = cfg.Agents[i].TokenSHA256
		cfg.Agents[i] = next
		if err := m.save(cfg); err != nil {
			return err
		}
		m.say("saved %s · applied to the running broker", next.ID)
		return nil
	})
}

func (m *ConfigModel) askDelete(id string) {
	m.mode = modeConfirm
	extra := ""
	if id == confstore.AdminID {
		extra = " status and dashboard use admin by default."
	}
	m.confirm = fmt.Sprintf("Remove agent %s and its token file? A running broker disconnects it.%s", id, extra)
	m.onYes = func() error {
		cfg := m.clone()
		i := confstore.Find(cfg, id)
		cfg.Agents = slices.Delete(cfg.Agents, i, i+1)
		if err := m.save(cfg); err != nil {
			return err
		}
		_ = os.Remove(m.loc.TokenFile(id))
		m.say("removed %s", id)
		return nil
	}
}

func (m *ConfigModel) askRotate(id string) {
	m.mode = modeConfirm
	m.confirm = fmt.Sprintf("Give %s a new token? Its current token stops working and a running broker disconnects it.", id)
	m.onYes = func() error {
		tok, hash := broker.NewToken()
		cfg := m.clone()
		cfg.Agents[confstore.Find(cfg, id)].TokenSHA256 = hash
		if err := m.save(cfg); err != nil {
			return err
		}
		if err := confstore.WriteToken(m.loc.TokenFile(id), tok); err != nil {
			return fmt.Errorf("config saved but writing the token file failed (rotate again): %w", err)
		}
		m.say("new token for %s in %s", id, m.loc.TokenFile(id))
		return nil
	}
}

func (m *ConfigModel) editSetting() tea.Cmd {
	i := m.settings.Cursor()
	if i < 0 || i >= len(m.keys) {
		return nil
	}
	key := m.keys[i]
	eff := *m.cfg
	eff.ApplyDefaults()
	m.fValue = settingValues(&eff)[key]
	applies := "Takes effect after a restart."
	if liveSettings[key] {
		applies = "Applies to a running broker at once."
	}
	return m.startForm("Edit "+key, []huh.Field{
		huh.NewInput().Title(key).Description(applies + " Durations look like 10m or 72h.").Value(&m.fValue),
	}, func() error {
		cfg := m.clone()
		if err := confstore.Set(cfg, key, m.fValue); err != nil {
			return err
		}
		if err := m.save(cfg); err != nil {
			return err
		}
		m.say("%s = %s · %s", key, m.fValue, strings.ToLower(strings.TrimSuffix(applies, ".")))
		return nil
	})
}

func (m *ConfigModel) updateClient(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	url, file := confstore.URL(m.cfg), m.loc.TokenFile(m.client)
	switch k.String() {
	case "esc", "q", "c":
		m.mode = modeList
	case "tab", "right", "l":
		m.clientF = (m.clientF + 1) % 3
	case "shift+tab", "left", "h":
		m.clientF = (m.clientF + 2) % 3
	case "1", "2", "3":
		m.clientF = int(k.String()[0] - '1')
	case "y":
		m.say("copied %s setup to the clipboard", []string{"shell", "Go", "MQTT"}[m.clientF])
		return m, tea.SetClipboard(m.clientText(url, file))
	case "w":
		envFile := strings.TrimSuffix(file, ".token") + ".env"
		if err := os.WriteFile(envFile, []byte(confstore.ClientEnv(url, m.client, file)), 0o600); err != nil {
			m.fail(err)
		} else {
			m.say("wrote %s · source it to act as %s", envFile, m.client)
		}
	}
	return m, nil
}

func (m *ConfigModel) clientText(url, file string) string {
	switch m.clientF {
	case 1:
		return confstore.GoSnippet(url, m.client, file)
	case 2:
		return confstore.MQTTSettings(url, m.client, file)
	}
	return confstore.ClientEnv(url, m.client, file)
}

func (m *ConfigModel) View() tea.View { return fullScreen(m.render()) }

func (m *ConfigModel) render() string {
	header := TitleStyle.Render("agentbus · config") + " " + TextStyle.Render(confstore.Tilde(m.loc.Path)) +
		SubtleStyle.Render(fmt.Sprintf("  %d agents · saved on every change, applied live", len(m.cfg.Agents)))
	header = ansi.Truncate(header, max(m.width, 20), "…")
	tabs := func(name string, on bool) string {
		if on {
			return AccentStyle.Underline(true).Render(name)
		}
		return SubtleStyle.Render(name)
	}
	tabLine := " " + tabs("Agents", m.tab == 0) + "   " + tabs("Broker settings", m.tab == 1)

	var body, help string
	switch m.mode {
	case modeForm:
		body = m.form.View()
		help = "enter next · shift+tab back · esc cancel"
	case modeClient:
		body, help = m.renderClient()
	default:
		if m.tab == 0 {
			body = m.agents.View()
			help = "↑/↓ select · enter edit · a add · c client setup · r rotate token · d remove · tab settings · q quit"
		} else {
			body = m.settings.View()
			help = "↑/↓ select · enter edit · tab agents · q quit"
		}
	}
	if m.mode == modeConfirm {
		help = WarnStyle.Render(m.confirm) + "  " + AccentStyle.Render("y") + SubtleStyle.Render("/") + AccentStyle.Render("n")
	}
	flash := ""
	if m.flash != "" {
		style := OKStyle
		mark := "✓ "
		if m.flashErr {
			style, mark = ErrStyle, "✗ "
		}
		flash = "\n" + ansi.Truncate(" "+style.Render(mark+m.flash), max(m.width, 20), "…")
	}
	return header + "\n" + tabLine + "\n\n" + body + flash + "\n" + ansi.Truncate(HelpStyle.Render(help), max(m.width, 20), "…")
}

func (m *ConfigModel) renderClient() (string, string) {
	url, file := confstore.URL(m.cfg), m.loc.TokenFile(m.client)
	names := []string{"Shell", "Go", "MQTT Explorer / MQTTX"}
	var tabs []string
	for i, n := range names {
		if i == m.clientF {
			tabs = append(tabs, AccentStyle.Underline(true).Render(n))
		} else {
			tabs = append(tabs, SubtleStyle.Render(n))
		}
	}
	tokState := OKStyle.Render("token file present")
	if _, ok := confstore.ReadToken(m.loc, m.client); !ok {
		tokState = ErrStyle.Render("token file missing: press esc, then r to create one")
	}
	box := PanelStyle.Width(min(m.width-2, 100)).Render(TextStyle.Render(m.clientText(url, file)))
	body := AccentStyle.Render("Client setup for "+m.client) + "  " + tokState + "\n\n" +
		" " + strings.Join(tabs, "   ") + "\n" + box + "\n" +
		SubtleStyle.Render(" The token is never shown here: clients read it from "+confstore.Tilde(file))
	return body, "tab switch · y copy · w write " + m.client + ".env · esc back"
}
