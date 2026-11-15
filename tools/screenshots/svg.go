package main

import (
	"fmt"
	"hash/crc32"
	"html"
	"image/color"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// cell style state while walking ANSI text.
type sgr struct {
	fg, bg string // hex colors, "" for default
	bold   bool
}

// span is a stretch of text in one style, starting at column col.
type span struct {
	col, width int
	text       string
	st         sgr
}

const (
	charW    = 8.6  // px per terminal column
	lineH    = 18.0 // px per row
	fontSize = 14.0
	padX     = 16.0
	padTop   = 44.0 // room for the title bar
	padBot   = 14.0
	termBg   = "#16161E"
	termFg   = "#D6D6E0"
	barBg    = "#24242E"
)

// toSVG renders ANSI terminal output as an SVG terminal window titled
// title, cols columns wide.
func toSVG(title, content string, cols int) string {
	// A unique clip id, so several screenshots can be inlined in one page.
	clipID := fmt.Sprintf("term-%08x", crc32.ChecksumIEEE([]byte(title+content)))
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	w := padX*2 + float64(cols)*charW
	h := padTop + float64(len(lines))*lineH + padBot

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`+"\n", w, h, w, h)
	fmt.Fprintf(&b, `<style>text{font-family:"SF Mono",Menlo,Monaco,Consolas,"DejaVu Sans Mono",monospace;font-size:%.0fpx;white-space:pre}</style>`+"\n", fontSize)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" rx="10" fill="%s"/>`+"\n", w, h, termBg)
	fmt.Fprintf(&b, `<path d="M0 10a10 10 0 0 1 10-10h%.0fa10 10 0 0 1 10 10v20H0z" fill="%s"/>`+"\n", w-20, barBg)
	for i, c := range []string{"#FF5F57", "#FEBC2E", "#28C840"} {
		fmt.Fprintf(&b, `<circle cx="%d" cy="15" r="6" fill="%s"/>`+"\n", 20+i*20, c)
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="20" fill="#9A9AAA" text-anchor="middle">%s</text>`+"\n", w/2, html.EscapeString(title))
	// Clip to the terminal's columns, as a real terminal would.
	fmt.Fprintf(&b, `<clipPath id="%s"><rect x="%.1f" y="%.0f" width="%.1f" height="%.0f"/></clipPath><g clip-path="url(#%s)">`+"\n",
		clipID, padX, padTop, float64(cols)*charW, h-padTop, clipID)

	for i, line := range lines {
		y := padTop + float64(i)*lineH
		for _, r := range parseLine(line) {
			x := padX + float64(r.col)*charW
			rw := float64(r.width) * charW
			if r.st.bg != "" {
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.0f" fill="%s"/>`+"\n", x, y, rw, lineH, r.st.bg)
			}
			if strings.TrimSpace(r.text) == "" {
				continue
			}
			fg := r.st.fg
			if fg == "" {
				fg = termFg
			}
			weight := ""
			if r.st.bold {
				weight = ` font-weight="bold"`
			}
			// textLength pins each run to its cells so columns line up
			// whatever monospace font the viewer has.
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="%s"%s textLength="%.1f" lengthAdjust="spacingAndGlyphs">%s</text>`+"\n",
				x, y+lineH-5, fg, weight, rw, html.EscapeString(r.text))
		}
	}
	b.WriteString("</g>\n</svg>\n")
	return b.String()
}

// parseLine splits one line of ANSI text into styled runs.
func parseLine(line string) []span {
	var runs []span
	var cur sgr
	var text strings.Builder
	col, start := 0, 0
	flush := func() {
		if text.Len() > 0 {
			runs = append(runs, span{col: start, width: col - start, text: text.String(), st: cur})
			text.Reset()
		}
		start = col
	}
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				flush()
				cur = applySGR(cur, line[i+2:j])
			}
			i = j + 1
			continue
		}
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == ']' { // OSC: skip to BEL or ST
			j := i + 2
			for j < len(line) && line[j] != 0x07 && !(line[j] == 0x1b && j+1 < len(line) && line[j+1] == '\\') {
				j++
			}
			if j < len(line) && line[j] == 0x1b {
				j++
			}
			i = j + 1
			continue
		}
		r, size := decodeRune(line[i:])
		text.WriteString(string(r))
		col += max(1, ansi.StringWidth(string(r)))
		i += size
	}
	flush()
	return runs
}

func decodeRune(s string) (rune, int) {
	for i, r := range s {
		if i == 0 {
			return r, len(string(r))
		}
	}
	return 0, 1
}

func applySGR(st sgr, params string) sgr {
	if params == "" {
		return sgr{}
	}
	p := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	for i := 0; i < len(p); i++ {
		n, _ := strconv.Atoi(p[i])
		switch {
		case n == 0:
			st = sgr{}
		case n == 1:
			st.bold = true
		case n == 22:
			st.bold = false
		case n == 39:
			st.fg = ""
		case n == 49:
			st.bg = ""
		case (n == 38 || n == 48) && i+1 < len(p):
			var c string
			switch p[i+1] {
			case "2":
				if i+4 < len(p) {
					r, _ := strconv.Atoi(p[i+2])
					g, _ := strconv.Atoi(p[i+3])
					bl, _ := strconv.Atoi(p[i+4])
					c = hex(color.RGBA{uint8(r), uint8(g), uint8(bl), 255})
					i += 4
				}
			case "5":
				if i+2 < len(p) {
					idx, _ := strconv.Atoi(p[i+2])
					c = ansi256(idx)
					i += 2
				}
			}
			if n == 38 {
				st.fg = c
			} else {
				st.bg = c
			}
		case n >= 30 && n <= 37:
			st.fg = ansi256(n - 30)
		case n >= 90 && n <= 97:
			st.fg = ansi256(n - 90 + 8)
		case n >= 40 && n <= 47:
			st.bg = ansi256(n - 40)
		case n >= 100 && n <= 107:
			st.bg = ansi256(n - 100 + 8)
		}
	}
	return st
}

func hex(c color.RGBA) string { return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B) }

// ansi256 maps an xterm 256-color index to hex.
func ansi256(i int) string {
	base := []string{"#1E1E28", "#FF6B6B", "#3DD68C", "#F5D547", "#5EB1FF", "#F07AC2", "#56D4DD", "#D6D6E0",
		"#6C6C7A", "#FF8A8A", "#6BE3A7", "#F8E27A", "#8CC8FF", "#F59FD3", "#83E3EA", "#FFFFFF"}
	switch {
	case i < 16:
		return base[i]
	case i < 232:
		i -= 16
		lv := []int{0, 95, 135, 175, 215, 255}
		return hex(color.RGBA{uint8(lv[i/36]), uint8(lv[(i/6)%6]), uint8(lv[i%6]), 255})
	default:
		v := uint8(8 + (i-232)*10)
		return hex(color.RGBA{v, v, v, 255})
	}
}
