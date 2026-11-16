package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/a2a"
)

// Inspector shows one message (an envelope, a presence card or a raw
// payload) in full: every header field and the pretty-printed body, in a
// scrollable viewport. Parent views embed it and route keys to it.
type Inspector struct {
	Item   Item
	vp     viewport.Model
	raw    string // the JSON to copy
	width  int
	height int
}

// NewInspector builds an inspector for it, sized w×h.
func NewInspector(it Item, w, h int) *Inspector {
	in := &Inspector{Item: it}
	in.vp = viewport.New(viewport.WithWidth(w), viewport.WithHeight(max(3, h-3)))
	in.Resize(w, h)
	return in
}

// Resize fits the inspector to w×h.
func (in *Inspector) Resize(w, h int) {
	in.width, in.height = w, h
	in.vp.SetWidth(w)
	in.vp.SetHeight(max(3, h-3))
	in.vp.SetContent(in.content())
}

// Update handles scrolling and copy. It reports done when the user leaves.
func (in *Inspector) Update(msg tea.Msg) (done bool, cmd tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc", "q", "enter", "backspace", "left":
			return true, nil
		case "y":
			return false, tea.SetClipboard(in.raw)
		}
	}
	in.vp, cmd = in.vp.Update(msg)
	return false, cmd
}

// View renders the inspector.
func (in *Inspector) View() string {
	title := "message"
	switch {
	case in.Item.Env != nil:
		title = string(in.Item.Env.Type) + " " + in.Item.Env.ID
	case in.Item.Presence != nil:
		title = "presence card of " + in.Item.Presence.Agent
	}
	pct := fmt.Sprintf("%3.0f%%", in.vp.ScrollPercent()*100)
	head := AccentStyle.Render("Inspect "+title) + SubtleStyle.Render("  "+in.Item.Topic+"  "+pct)
	return head + "\n" + in.vp.View() + "\n" + HelpStyle.Render("↑/↓ pgup/pgdn scroll · y copy JSON · esc back")
}

func (in *Inspector) content() string {
	kv := func(k, v string) string {
		if v == "" {
			return ""
		}
		return SubtleStyle.Render(fmt.Sprintf("%-15s", k)) + TextStyle.Render(v) + "\n"
	}
	var b strings.Builder
	switch {
	case in.Item.Env != nil:
		e := in.Item.Env
		raw, _ := json.MarshalIndent(e, "", "  ")
		in.raw = string(raw)
		b.WriteString(kv("type", string(e.Type)))
		b.WriteString(kv("id", e.ID))
		b.WriteString(kv("from", e.From))
		b.WriteString(kv("to", e.To))
		b.WriteString(kv("topic", in.Item.Topic))
		b.WriteString(kv("sent", e.TS.Local().Format("2006-01-02 15:04:05.000")+SubtleStyle.Render("  ("+since(e.TS)+" ago)")))
		if e.TTLMillis > 0 {
			exp := e.TS.Add(time.Duration(e.TTLMillis) * time.Millisecond)
			b.WriteString(kv("ttl", (time.Duration(e.TTLMillis)*time.Millisecond).String()+SubtleStyle.Render("  (expires "+exp.Local().Format("15:04:05")+")")))
		}
		b.WriteString(kv("correlation_id", e.CorrelationID))
		b.WriteString(kv("reply_to", e.ReplyTo))
		b.WriteString(kv("capability", e.Capability))
		if e.Priority != "" {
			b.WriteString(kv("priority", priorityStyle(e.Priority).Render(string(e.Priority))))
		}
		b.WriteString(kv("event", e.Event))
		b.WriteString(kv("content_type", e.ContentType))
		b.WriteString(kv("enc", e.Enc))
		if len(e.Headers) > 0 {
			keys := make([]string, 0, len(e.Headers))
			for k := range e.Headers {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for i, k := range keys {
				label := ""
				if i == 0 {
					label = "headers"
				}
				b.WriteString(kv(label, k+" = "+e.Headers[k]))
			}
		}
		b.WriteString("\n" + AccentStyle.Render("body") + SubtleStyle.Render(fmt.Sprintf("  %d bytes", len(e.Body))) + "\n")
		b.WriteString(prettyJSON(e.Body))
	case in.Item.Presence != nil:
		p := in.Item.Presence
		raw, _ := json.MarshalIndent(p, "", "  ")
		in.raw = string(raw)
		b.WriteString(kv("agent", p.Agent))
		b.WriteString(kv("status", StatusStyle(p.Status).Render(string(p.Status))))
		b.WriteString(kv("since", p.TS.Local().Format("2006-01-02 15:04:05")+SubtleStyle.Render("  ("+since(p.TS)+" ago)")))
		b.WriteString(kv("description", p.Description))
		b.WriteString(kv("capabilities", strings.Join(p.Capabilities, ", ")))
		if p.Keys != nil {
			trust := WarnStyle.Render("claimed by the agent, not pinned by the broker")
			if p.KeysPinned {
				trust = OKStyle.Render("pinned in the broker config")
			}
			b.WriteString(kv("keys", trust))
			b.WriteString(kv("  sign", p.Keys.Sign))
			b.WriteString(kv("  kex", p.Keys.KEX))
		}
		if len(p.Meta) > 0 {
			keys := make([]string, 0, len(p.Meta))
			for k := range p.Meta {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for i, k := range keys {
				label := ""
				if i == 0 {
					label = "meta"
				}
				b.WriteString(kv(label, k+" = "+p.Meta[k]))
			}
		}
	default:
		in.raw = string(in.Item.Raw)
		b.WriteString(kv("topic", in.Item.Topic))
		b.WriteString("\n" + prettyJSON(in.Item.Raw))
	}
	return b.String()
}

func since(t time.Time) string {
	d := time.Since(t)
	if d < time.Second {
		return "just now"
	}
	return d.Round(time.Second).String()
}

// prettyJSON indents JSON and colors keys; anything else is shown as is.
func prettyJSON(b []byte) string {
	if len(b) == 0 {
		return SubtleStyle.Render("(empty)")
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return TextStyle.Render(string(b))
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	var s strings.Builder
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := line[:len(line)-len(trimmed)]
		if strings.HasPrefix(trimmed, `"`) {
			if i := strings.Index(trimmed, `": `); i > 0 {
				s.WriteString(indent + AccentStyle.Render(trimmed[:i+1]) + TextStyle.Render(trimmed[i+1:]) + "\n")
				continue
			}
		}
		s.WriteString(indent + TextStyle.Render(trimmed) + "\n")
	}
	return s.String()
}

// involves reports whether an item concerns agent: sent by it, addressed
// to it, on its topics, or its presence card.
func involves(it Item, agent string) bool {
	switch {
	case it.Env != nil:
		e := it.Env
		if e.From == agent || e.To == agent {
			return true
		}
	case it.Presence != nil:
		return it.Presence.Agent == agent
	}
	t, err := a2a.ParseTopic(it.Topic)
	return err == nil && t.Owner == agent
}
