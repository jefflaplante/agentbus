package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
)

// The Map tab draws the bus as it runs: the broker in the middle, every
// agent around it, and each message the connection sees travelling from
// its sender through the broker to its recipient. A task stops at the
// broker, tagged with its capability, because an observer can't see which
// worker takes it; an event ripples out from the broker to whoever
// subscribes; a refusal flashes the broker and goes back to the sender.
//
// It is drawn in braille: each terminal cell is a 2x4 grid of dots, which
// is enough resolution for curves, rings and moving packets.

const (
	mapFrame     = time.Second / 15 // redraw rate while the Map tab shows
	mapLeg       = 650 * time.Millisecond
	mapStampLife = 2200 * time.Millisecond
	mapRipple    = 900 * time.Millisecond
	mapPulse     = 500 * time.Millisecond
	maxPackets   = 160
	brokerNode   = a2a.BrokerID
)

// mapTab is the Map tab's state: what is in flight and what recently
// happened, all keyed by time so the drawing doesn't depend on frame rate.
type mapTab struct {
	packets []mapPacket
	ripples []time.Time // events reaching the broker
	stamps  []mapStamp  // refusals and task tags beside the broker
	pulses  map[string]time.Time
	extra   []string // agents seen in traffic but not in status or presence
	paused  bool
	frozen  time.Time // the paused moment
	ticking bool
	recent  []mapSeen // for the per-type counts over the last 10s
}

type mapPacket struct {
	typ        a2a.Type
	from, to   string // agent IDs; brokerNode for the broker; to "" stops at the broker
	capability string // for a task
	born       time.Time
	arrived    bool // has reached the broker
}

type mapStamp struct {
	text string
	err  bool
	at   time.Time
}

type mapSeen struct {
	typ a2a.Type
	at  time.Time
}

type mapTickMsg time.Time

func mapTick() tea.Cmd {
	return tea.Tick(mapFrame, func(t time.Time) tea.Msg { return mapTickMsg(t) })
}

// startMap begins the frame ticks if the Map tab needs them.
func (m *Console) startMap() tea.Cmd {
	if m.tab != TabMap || m.bus.ticking || m.bus.paused {
		return nil
	}
	m.bus.ticking = true
	return mapTick()
}

// mapNow is the moment the map draws: the real time, or the paused one.
func (b *mapTab) mapNow() time.Time {
	if b.paused {
		return b.frozen
	}
	return time.Now()
}

// see records one message for the map.
func (b *mapTab) see(it Item, now time.Time) {
	if b.pulses == nil {
		b.pulses = map[string]time.Time{}
	}
	if it.Presence != nil {
		b.pulses[it.Presence.Agent] = now
		b.note(it.Presence.Agent)
		return
	}
	e := it.Env
	if e == nil || b.paused {
		return
	}
	b.recent = append(b.recent, mapSeen{e.Type, now})
	p := mapPacket{typ: e.Type, from: e.From, to: e.To, born: now}
	b.note(e.From)
	b.note(e.To)
	switch {
	case e.Type == a2a.TypeEvent:
		p.to = ""
	case e.Type == a2a.TypeRequest && e.To == "" && e.Capability != "":
		p.to = "" // a task: the broker hands it to one worker we can't see
		p.capability = e.Capability
	}
	if e.From == brokerNode {
		p.born, p.arrived = now.Add(-mapLeg), true // starts at the broker
	}
	b.packets = append(b.packets, p)
	if len(b.packets) > maxPackets {
		b.packets = b.packets[len(b.packets)-maxPackets:]
	}
}

func (b *mapTab) note(id string) {
	if id != "" && id != brokerNode && !slices.Contains(b.extra, id) {
		b.extra = append(b.extra, id)
	}
}

// advance retires what has finished by now, and turns packets that
// reached the broker into ripples and stamps.
func (b *mapTab) advance(now time.Time) {
	keep := b.packets[:0]
	for _, p := range b.packets {
		age := now.Sub(p.born)
		if age >= mapLeg && !p.arrived {
			p.arrived = true
			switch {
			case p.typ == a2a.TypeEvent:
				b.ripples = append(b.ripples, p.born.Add(mapLeg))
				if len(b.ripples) > 3 { // a burst of events is still three rings
					b.ripples = b.ripples[len(b.ripples)-3:]
				}
			case p.to == "":
				b.stamps = append(b.stamps, mapStamp{text: "⇢ " + Clean(p.capability), at: p.born.Add(mapLeg)})
			}
		}
		if p.to != "" && age >= 2*mapLeg {
			b.pulses[p.to] = p.born.Add(2 * mapLeg)
			continue
		}
		if p.to == "" && age >= mapLeg {
			continue
		}
		keep = append(keep, p)
	}
	b.packets = keep
	b.ripples = slices.DeleteFunc(b.ripples, func(t time.Time) bool { return now.Sub(t) > mapRipple })
	b.stamps = slices.DeleteFunc(b.stamps, func(s mapStamp) bool { return now.Sub(s.at) > mapStampLife })
	b.recent = slices.DeleteFunc(b.recent, func(s mapSeen) bool { return now.Sub(s.at) > 10*time.Second })
}

// refusal adds the stamp for a broker error envelope.
func (b *mapTab) refusal(e *a2a.Envelope, now time.Time) {
	if e == nil || e.Type != a2a.TypeError || e.From != brokerNode || b.paused {
		return
	}
	code, _ := errorParts(e)
	if code == "" {
		code = "refused"
	}
	b.stamps = append(b.stamps, mapStamp{text: "✗ " + code, err: true, at: now})
}

func (m *Console) mapUpdate(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	b := &m.bus
	switch k.String() {
	case "p", "space":
		if b.paused {
			// Resume where it froze: shift everything by the pause.
			d := time.Since(b.frozen)
			for i := range b.packets {
				b.packets[i].born = b.packets[i].born.Add(d)
			}
			for i := range b.ripples {
				b.ripples[i] = b.ripples[i].Add(d)
			}
			for i := range b.stamps {
				b.stamps[i].at = b.stamps[i].at.Add(d)
			}
			b.paused = false
			return m.startMap()
		}
		b.paused, b.frozen = true, time.Now()
	case "c":
		b.packets, b.ripples, b.stamps = nil, nil, nil
	}
	return nil
}

func (m *Console) mapHelp() string {
	if m.bus.paused {
		return "p resume · c clear"
	}
	return "p pause · c clear"
}

// ============================== drawing ==============================

// mapInk is one color the map draws with. Bright is for packet heads and
// labels, dim for trails and spokes.
type mapInk int

const (
	inkNone mapInk = iota
	inkSpoke
	inkBroker
	inkBrokerHot
	inkRequest
	inkRequestDim
	inkResponse
	inkResponseDim
	inkEvent
	inkEventDim
	inkError
	inkErrorDim
	numInks
)

func mapStyles() [numInks]lipgloss.Style {
	dim := func(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(c)) }
	var s [numInks]lipgloss.Style
	if pal == darkPalette {
		s[inkSpoke] = dim("#3A3A4A")
		s[inkRequestDim], s[inkResponseDim] = dim("#2F5C86"), dim("#1F7350")
		s[inkEventDim], s[inkErrorDim] = dim("#87426C"), dim("#8A3A3A")
		s[inkBroker] = dim("#7E6BE0")
	} else {
		s[inkSpoke] = dim("#CFCFD8")
		s[inkRequestDim], s[inkResponseDim] = dim("#90B8E0"), dim("#8CC9A8")
		s[inkEventDim], s[inkErrorDim] = dim("#E0A0C4"), dim("#E8A0A0")
		s[inkBroker] = dim("#9C8BEA")
	}
	s[inkBrokerHot] = lipgloss.NewStyle().Foreground(pal.red).Bold(true)
	s[inkRequest] = lipgloss.NewStyle().Foreground(pal.blue).Bold(true)
	s[inkResponse] = lipgloss.NewStyle().Foreground(pal.green).Bold(true)
	s[inkEvent] = lipgloss.NewStyle().Foreground(pal.magenta).Bold(true)
	s[inkError] = lipgloss.NewStyle().Foreground(pal.red).Bold(true)
	return s
}

func typeInks(t a2a.Type) (head, trail mapInk) {
	switch t {
	case a2a.TypeResponse:
		return inkResponse, inkResponseDim
	case a2a.TypeEvent:
		return inkEvent, inkEventDim
	case a2a.TypeError:
		return inkError, inkErrorDim
	}
	return inkRequest, inkRequestDim
}

// brailleCanvas is a grid of terminal cells, each a 2x4 block of dots,
// with one color per cell and text that can overwrite cells.
type brailleCanvas struct {
	cols, rows int
	dots       []uint8
	ink        []mapInk
	text       []rune
	textStyle  []lipgloss.Style
	hasText    []bool
}

func newCanvas(cols, rows int) *brailleCanvas {
	n := cols * rows
	return &brailleCanvas{cols: cols, rows: rows, dots: make([]uint8, n), ink: make([]mapInk, n),
		text: make([]rune, n), textStyle: make([]lipgloss.Style, n), hasText: make([]bool, n)}
}

var brailleBit = [4][2]uint8{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}

// dot sets one dot. A cell takes the ink of its strongest dot, so a packet
// passing over a spoke shows in the packet's color.
func (c *brailleCanvas) dot(x, y float64, ink mapInk) {
	px, py := int(math.Round(x)), int(math.Round(y))
	if px < 0 || py < 0 || px >= c.cols*2 || py >= c.rows*4 {
		return
	}
	i := (py/4)*c.cols + px/2
	c.dots[i] |= brailleBit[py%4][px%2]
	if ink > c.ink[i] {
		c.ink[i] = ink
	}
}

func (c *brailleCanvas) line(x0, y0, x1, y1 float64, ink mapInk, every int) {
	n := int(math.Max(math.Abs(x1-x0), math.Abs(y1-y0)))
	for i := 0; i <= n; i++ {
		if every > 1 && i%every != 0 {
			continue
		}
		t := 0.0
		if n > 0 {
			t = float64(i) / float64(n)
		}
		c.dot(x0+(x1-x0)*t, y0+(y1-y0)*t, ink)
	}
}

func (c *brailleCanvas) ring(cx, cy, r float64, ink mapInk, phase float64, dash bool) {
	steps := int(math.Max(24, r*7))
	for i := 0; i < steps; i++ {
		a := float64(i)/float64(steps)*2*math.Pi + phase
		if dash && int(float64(i)/float64(steps)*16)%2 == 1 {
			continue
		}
		c.dot(cx+math.Cos(a)*r, cy+math.Sin(a)*r, ink)
	}
}

// write puts text into cells starting at column col, clipped to the canvas.
func (c *brailleCanvas) write(col, row int, s string, st lipgloss.Style) {
	if row < 0 || row >= c.rows {
		return
	}
	for _, r := range s {
		if col >= 0 && col < c.cols {
			i := row*c.cols + col
			c.text[i], c.textStyle[i], c.hasText[i] = r, st, true
		}
		col++
	}
}

func (c *brailleCanvas) String(styles [numInks]lipgloss.Style) string {
	var out strings.Builder
	for r := 0; r < c.rows; r++ {
		var run strings.Builder
		runInk := mapInk(-1)
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if runInk <= inkNone {
				out.WriteString(run.String())
			} else {
				out.WriteString(styles[runInk].Render(run.String()))
			}
			run.Reset()
		}
		for col := 0; col < c.cols; col++ {
			i := r*c.cols + col
			if c.hasText[i] {
				flush()
				runInk = -1
				out.WriteString(c.textStyle[i].Render(string(c.text[i])))
				continue
			}
			ink, ch := c.ink[i], ' '
			if c.dots[i] != 0 {
				ch = rune(0x2800 + int(c.dots[i]))
			} else {
				ink = inkNone
			}
			if ink != runInk {
				flush()
				runInk = ink
			}
			run.WriteRune(ch)
		}
		flush()
		if r < c.rows-1 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// mapNode is an agent's place on the map, in dot coordinates.
type mapNode struct {
	id     string
	x, y   float64
	online bool
}

// layout puts the broker in the middle and the agents on an ellipse
// around it, in name order so each keeps its place as traffic comes and
// goes.
func (m *Console) layout(cols, rows int) (bx, by float64, nodes []mapNode) {
	w, h := float64(cols*2), float64(rows*4)
	bx, by = w/2, h/2
	online := map[string]bool{}
	ids := []string{}
	for _, r := range m.rows() {
		ids = append(ids, r.id)
		online[r.id] = r.state() == "online" || r.state() == string(a2a.StatusOnline)
	}
	for _, id := range m.bus.extra {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	// Leave room for labels on both sides, and a row above and below.
	rx := math.Max(10, w/2-float64(longest(ids)*2)-8)
	ry := math.Max(6, h/2-8)
	for i, id := range ids {
		a := -math.Pi/2 + float64(i)/float64(max(len(ids), 1))*2*math.Pi
		nodes = append(nodes, mapNode{id: id, x: bx + math.Cos(a)*rx, y: by + math.Sin(a)*ry, online: online[id]})
	}
	return bx, by, nodes
}

func longest(ids []string) int {
	n := 6
	for _, id := range ids {
		n = max(n, min(ansi.StringWidth(id), 18))
	}
	return n
}

func (m *Console) renderMap(w, h int) string {
	b := &m.bus
	now := b.mapNow()
	b.advance(now)
	styles := mapStyles()

	counts := map[a2a.Type]int{}
	for _, s := range b.recent {
		counts[s.typ]++
	}
	head := AccentStyle.Render("Map") + "  " + SubtleStyle.Render("last 10s: ") +
		typeStyle(a2a.TypeRequest).UnsetWidth().Render(fmt.Sprintf("%d req", counts[a2a.TypeRequest])) + SubtleStyle.Render(" · ") +
		typeStyle(a2a.TypeResponse).UnsetWidth().Render(fmt.Sprintf("%d reply", counts[a2a.TypeResponse])) + SubtleStyle.Render(" · ") +
		typeStyle(a2a.TypeEvent).UnsetWidth().Render(fmt.Sprintf("%d event", counts[a2a.TypeEvent])) + SubtleStyle.Render(" · ") +
		typeStyle(a2a.TypeError).UnsetWidth().Render(fmt.Sprintf("%d refused or failed", counts[a2a.TypeError]))
	if b.paused {
		head += WarnStyle.Render("  · paused")
	}
	lines := []string{ansi.Truncate(head, w, "…")}

	cols, rows := w, h-len(lines)
	if rows < 8 || cols < 30 {
		return strings.Join(append(lines, SubtleStyle.Render("  make the window larger to see the map")), "\n")
	}
	c := newCanvas(cols, rows)
	bx, by, nodes := m.layout(cols, rows)
	pos := map[string][2]float64{brokerNode: {bx, by}}
	for _, n := range nodes {
		pos[n.id] = [2]float64{n.x, n.y}
	}

	// spokes, dotted
	for _, n := range nodes {
		c.line(n.x, n.y, bx, by, inkSpoke, 3)
	}

	// the broker: two dashed rings turning opposite ways; red after a refusal
	t := float64(now.UnixMilli()%100000) / 1000
	ring := inkBroker
	for _, s := range b.stamps {
		if s.err && now.Sub(s.at) < 600*time.Millisecond {
			ring = inkBrokerHot
		}
	}
	c.ring(bx, by, 10, ring, t*0.9, true)
	c.ring(bx, by, 15, ring, -t*0.6, true)
	for _, r := range b.ripples {
		k := float64(now.Sub(r)) / float64(mapRipple)
		c.ring(bx, by, 16+k*(math.Min(bx, by)*0.95-16), inkEventDim, 0, k > 0.5) // stays on the canvas
	}

	// packets: a dim braille trail, and a bright mark at the head
	type headMark struct {
		col, row int
		ink      mapInk
	}
	var heads []headMark
	for _, p := range b.packets {
		head, trail := typeInks(p.typ)
		for k := 8; k >= 1; k-- {
			if x, y, ok := packetAt(p, now.Add(-time.Duration(k)*35*time.Millisecond), pos); ok {
				c.dot(x, y, trail)
			}
		}
		if x, y, ok := packetAt(p, now, pos); ok {
			heads = append(heads, headMark{int(x / 2), int(y / 4), head})
		}
	}
	for _, h := range heads {
		c.write(h.col, h.row, "•", styles[h.ink])
	}

	// labels: the broker, then each agent beside its node
	bc, br := int(bx/2), int(by/4)
	c.write(bc-3, br, "broker", lipgloss.NewStyle().Foreground(pal.accent).Bold(true))
	for _, n := range nodes {
		col, row := int(n.x/2), int(n.y/4)
		mark, st := "○", SubtleStyle
		if n.online {
			mark, st = "●", OnlineStyle
		}
		if at, ok := b.pulses[n.id]; ok && now.Sub(at) < mapPulse {
			mark, st = "◉", lipgloss.NewStyle().Foreground(pal.text).Bold(true)
		}
		c.write(col, row, mark, st)
		label := ansi.Truncate(Clean(n.id), 18, "…")
		lw := ansi.StringWidth(label)
		if n.x < bx-1 {
			c.write(col-1-lw, row, label, TextStyle)
		} else {
			c.write(col+2, row, label, TextStyle)
		}
	}

	// stamps stack above the broker, newest nearest
	for i, s := range slicesBackward(b.stamps, 3) {
		st := lipgloss.NewStyle().Foreground(pal.blue)
		if s.err {
			st = ErrStyle
		}
		c.write(bc-ansi.StringWidth(s.text)/2, br-4-i, s.text, st)
	}

	if len(nodes) == 0 {
		c.write(2, 1, "no agents yet: the map fills in as agents come online and send messages", SubtleStyle)
	}
	lines = append(lines, c.String(styles))
	return strings.Join(lines, "\n")
}

// packetAt is where p is at time at: on the way to the broker, then on
// the way from it. It reports false before p starts and after it ends.
func packetAt(p mapPacket, at time.Time, pos map[string][2]float64) (float64, float64, bool) {
	age := at.Sub(p.born)
	if age < 0 {
		return 0, 0, false
	}
	b := pos[brokerNode]
	from, ok := pos[p.from]
	if !ok {
		from = b
	}
	if age < mapLeg {
		return curve(from, b, ease(float64(age)/float64(mapLeg)), 0.25)
	}
	if p.to == "" || age >= 2*mapLeg {
		return 0, 0, false
	}
	to, ok := pos[p.to]
	if !ok {
		return 0, 0, false
	}
	return curve(b, to, ease(float64(age-mapLeg)/float64(mapLeg)), -0.25)
}

// curve is a point on a gentle arc from a to b.
func curve(a, b [2]float64, t, bend float64) (float64, float64, bool) {
	mx, my := (a[0]+b[0])/2, (a[1]+b[1])/2
	cx, cy := mx-(b[1]-a[1])*bend, my+(b[0]-a[0])*bend
	u := 1 - t
	return u*u*a[0] + 2*u*t*cx + t*t*b[0], u*u*a[1] + 2*u*t*cy + t*t*b[1], true
}

func ease(t float64) float64 {
	if t < 0.5 {
		return 2 * t * t
	}
	return 1 - math.Pow(-2*t+2, 2)/2
}

// slicesBackward is the last n of s, newest first.
func slicesBackward[T any](s []T, n int) []T {
	out := []T{}
	for i := len(s) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s[i])
	}
	return out
}
