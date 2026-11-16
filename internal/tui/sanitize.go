package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Clean makes untrusted text (agent IDs, presence descriptions, event
// names, headers, error reasons) safe to print on one line of a terminal:
// control characters, including ESC, DEL, the C1 range, newlines and tabs,
// become visible escapes like \x1b, so they can neither move the cursor,
// recolor or retitle the terminal, nor forge extra rows of output.
func Clean(s string) string { return clean(s, false) }

// CleanBlock is Clean for multi-line text such as a message body: it
// keeps newlines and tabs and escapes every other control character.
func CleanBlock(s string) string { return clean(s, true) }

func clean(s string, block bool) string {
	if !needsClean(s, block) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case block && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func needsClean(s string, block bool) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
			(r < 0x20 && !(block && (r == '\n' || r == '\t'))) {
			return true
		}
		i += size
	}
	return false
}

// cleanAll applies Clean to each string.
func cleanAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = Clean(s)
	}
	return out
}
