package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Canonical form, for signing (CONTRACT.md, "Canonical form").
//
// End-to-end signatures are on the roadmap (docs/SECURITY.md, "Not done
// yet"); they need a byte-exact answer to "what exactly is signed?". This
// file is that answer, so that the Go client, the Python example
// (examples/python/agentbus.py, canonical()), a future broker check and any
// independent signer agree on the bytes. testdata/canonical-vectors.json
// holds the cases every implementation must reproduce.
//
// The canonical form is the JSON Canonicalization Scheme, RFC 8785, applied
// to the parsed envelope, plus two envelope rules:
//
//   - It is a function of the parsed envelope, not of the wire bytes it
//     arrived on. The broker re-encodes every accepted envelope, so a
//     signature over the sender's original bytes would not survive the
//     hop. Canonical(Parse(env.Marshal())) == Canonical(env).
//   - ts is normalized: UTC, RFC 3339 with a Z suffix, fractional digits
//     trimmed to what is present.
//
// Everything else is RFC 8785, so an off-the-shelf JCS library produces
// the same bytes from the same JSON object: keys sorted by UTF-16 code
// units, strings escaped as ECMAScript's JSON.stringify does, numbers in
// ECMAScript's shortest round-trip form.
//
// Canonical refuses input that two receivers could read differently, so
// that one signature can never cover two meanings: invalid UTF-8, unpaired
// surrogate escapes, duplicate object keys, more than one integer literal
// for one double, and numbers outside a double's range.

// maxSafeInteger is the largest integer every double holds exactly, 2^53-1.
const maxSafeInteger = 1<<53 - 1

// Canonical returns the canonical byte form of e (see the comment above and
// CONTRACT.md, "Canonical form"). Same parsed envelope in, same bytes out,
// in every implementation that follows the contract.
func (e *Envelope) Canonical() ([]byte, error) {
	fields, err := e.canonicalFields()
	if err != nil {
		return nil, fmt.Errorf("a2a: canonical: %w", err)
	}
	var b bytes.Buffer
	if err := writeObject(&b, fields); err != nil {
		return nil, fmt.Errorf("a2a: canonical: %w", err)
	}
	return b.Bytes(), nil
}

// canonicalFields is every envelope field that is present, keyed by its
// wire name. A future sig field is left out by construction.
func (e *Envelope) canonicalFields() (map[string]any, error) {
	if e.V < -maxSafeInteger || e.V > maxSafeInteger || e.TTLMillis < -maxSafeInteger || e.TTLMillis > maxSafeInteger {
		return nil, errors.New("v and ttl_ms must be within ±(2^53-1)")
	}
	ts := e.TS.UTC()
	if y := ts.Year(); y < 0 || y > 9999 {
		return nil, fmt.Errorf("ts year %d has no RFC 3339 form", y)
	}
	m := map[string]any{
		"v":    json.Number(strconv.Itoa(e.V)),
		"id":   e.ID,
		"type": string(e.Type),
		"from": e.From,
		"ts":   ts.Format(time.RFC3339Nano),
	}
	opt := map[string]string{
		"to": e.To, "correlation_id": e.CorrelationID, "reply_to": e.ReplyTo,
		"capability": e.Capability, "priority": string(e.Priority), "event": e.Event,
		"content_type": e.ContentType, "enc": e.Enc,
	}
	for k, v := range opt {
		if v != "" {
			m[k] = v
		}
	}
	if e.TTLMillis != 0 {
		m["ttl_ms"] = json.Number(strconv.FormatInt(e.TTLMillis, 10))
	}
	if len(e.Headers) > 0 {
		h := make(map[string]any, len(e.Headers))
		for k, v := range e.Headers {
			h[k] = v
		}
		m["headers"] = h
	}
	if len(e.Body) > 0 {
		v, err := parseCanonical(e.Body)
		if err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
		m["body"] = v
	}
	return m, nil
}

// checkIJSON reports whether data is one I-JSON value (RFC 7493): valid
// UTF-8, no unpaired surrogate escapes, no duplicate object keys.
func checkIJSON(data []byte) error {
	_, err := parseCanonical(data)
	return err
}

// parseCanonical parses raw as exactly one JSON value, refusing what has no
// single reading: invalid UTF-8, unpaired surrogate escapes, duplicate keys.
// Numbers stay json.Number so writeNumber sees the literal.
func parseCanonical(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8")
	}
	if err := checkSurrogates(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the first JSON value")
	}
	return v, nil
}

// decodeValue reads one value from dec's token stream, building maps and
// slices, and refuses an object that repeats a key. Keys compare after
// unescaping, so "a" and "a" are the same key.
func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not one JSON value: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				if _, dup := obj[k]; dup {
					return nil, fmt.Errorf("duplicate key %q", k)
				}
				if obj[k], err = decodeValue(dec); err != nil {
					return nil, err
				}
			}
			_, err := dec.Token() // '}'
			return obj, err
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token() // ']'
			return arr, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil // string, json.Number, bool or nil
	}
}

// checkSurrogates refuses \uD800-\uDFFF escapes that aren't a high
// surrogate followed directly by a low one. Go's decoder would turn them
// into U+FFFD, so "\ud800" and "�" would otherwise sign the same.
func checkSurrogates(raw []byte) error {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case !inString:
			inString = c == '"'
		case c == '"':
			inString = false
		case c == '\\' && i+1 < len(raw):
			if raw[i+1] != 'u' {
				i++ // skip the escaped character
				continue
			}
			r, ok := hex4(raw, i+2)
			if !ok {
				return errors.New("bad \\u escape")
			}
			i += 5
			switch {
			case r >= 0xDC00 && r <= 0xDFFF:
				return fmt.Errorf("unpaired surrogate \\u%04x", r)
			case r >= 0xD800 && r <= 0xDBFF:
				lo, ok := hex4(raw, i+3)
				if i+2 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' || !ok || lo < 0xDC00 || lo > 0xDFFF {
					return fmt.Errorf("unpaired surrogate \\u%04x", r)
				}
				i += 6
			}
		}
	}
	return nil
}

func hex4(raw []byte, at int) (rune, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(raw[at:at+4]), 16, 16)
	return rune(n), err == nil
}

func writeValue(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		return writeString(b, t)
	case json.Number:
		return writeNumber(b, string(t))
	case map[string]any:
		return writeObject(b, t)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeValue(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("unsupported value %T", v)
	}
	return nil
}

// writeObject writes keys in RFC 8785 order: by UTF-16 code units.
func writeObject(b *bytes.Buffer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int {
		return slices.Compare(utf16.Encode([]rune(x)), utf16.Encode([]rune(y)))
	})
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writeString(b, k); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := writeValue(b, m[k]); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// writeString escapes as RFC 8785 §3.2.2.2 (ECMAScript JSON.stringify):
// \" and \\, \b \t \n \f \r, other controls as lowercase \u00xx, and every
// other character raw, including /, DEL, U+2028 and U+2029.
func writeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("invalid UTF-8 in %q", s)
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}

// writeNumber writes a JSON number literal in RFC 8785 §3.2.2.3 form,
// refusing literals a double can't stand for: beyond its range, or so
// small it becomes 0.
//
// Beyond ±(2^53-1), many integer literals parse to the same double, and
// Python and other readers that keep integers exact see different values:
// 9007199254740993 and 9007199254740992 would share a signature. So only
// one integer literal per double is accepted (see oneSpelling).
func writeNumber(b *bytes.Buffer, lit string) error {
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("number %s does not fit a double", lit)
	}
	mant, _, _ := strings.Cut(strings.ToLower(lit), "e")
	if f == 0 && strings.ContainsAny(mant, "123456789") {
		return fmt.Errorf("number %s underflows to 0", lit)
	}
	out := formatES6(f)
	if !strings.ContainsAny(lit, ".eE") && (f > maxSafeInteger || f < -maxSafeInteger) && !oneSpelling(lit, f, out) {
		return fmt.Errorf("integer %s is beyond ±(2^53-1) and isn't the one integer spelling of its double; send it as a string", lit)
	}
	b.WriteString(out)
	return nil
}

// oneSpelling reports whether integer literal lit is the one accepted
// integer spelling of its double f: the canonical form out when that is a
// plain integer (so canonical output always re-canonicalizes), else, from
// 1e21 up where the canonical form has an exponent, the exact value.
func oneSpelling(lit string, f float64, out string) bool {
	if !strings.ContainsAny(out, "e") {
		return lit == out
	}
	n, ok := new(big.Int).SetString(lit, 10)
	exact, _ := new(big.Float).SetFloat64(f).Int(nil)
	return ok && n.Cmp(exact) == 0
}

// formatES6 is ECMAScript Number::toString for a finite double: shortest
// round-trip digits, plain between 1e-7 and 1e21, else d.ddde±n.
func formatES6(f float64) string {
	if f == 0 {
		return "0" // +0 and -0
	}
	neg := f < 0
	s := strconv.FormatFloat(math.Abs(f), 'e', -1, 64) // d.ddde±xx
	mant, expStr, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	k, n := len(digits), exp+1 // n: decimal point position
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		out = digits[:1]
		if k > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		out += "e" + sign + strconv.Itoa(abs(n-1))
	}
	if neg {
		out = "-" + out
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
