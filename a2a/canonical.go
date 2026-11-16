package a2a

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Canonical form, for signing (CONTRACT.md, "Canonical form").
//
// End-to-end signatures are on the roadmap (docs/SECURITY.md, "Not done
// yet"); they need a byte-exact answer to "what exactly is signed?". This
// file is that answer, so that the Go client, the Python example, a future
// broker check and any independent signer agree on the bytes:
//
//   - The canonical form is a function of the parsed envelope, not of the
//     wire bytes it arrived on. The broker re-encodes every accepted
//     envelope, so a signature over the sender's original bytes would not
//     survive the hop. Canonical(Parse(env.Marshal())) == Canonical(env).
//   - Object keys are sorted by UTF-8 byte order, recursively; separators
//     are compact; no HTML escaping (<, >, & stay raw).
//   - ts is normalized: UTC, RFC 3339 with a Z suffix, fractional digits
//     trimmed to what is present ("...:05Z" never "...:05.000Z"). A +00:00
//     spelling and Z canonicalize identically.
//   - Numbers in the body follow RFC 8785 (JCS) §3.2.2.3: IEEE-754 double,
//     shortest round-trip decimal digits, ES6 number-to-string layout
//     (1e-7 switches to exponent, 1e+21 does on the high end, -0 collapses
//     to 0). An integer literal too large for a double signs as the double
//     it parses to; authors should keep integers within ±(2^53-1).
//   - Strings are byte-exact: no Unicode normalization, case folding or
//     trimming. Two canonically-equivalent but byte-distinct strings sign
//     differently, by design. (Non-normative: emitting NFC makes
//     human-equivalent payloads compare equal before signing.)
//   - Present fields only. A future sig field is excluded from the
//     canonical form by construction: it is not part of the map below.
//   - Values the canonical form cannot represent deterministically return
//     an error; Canonical never silently rewrites something it refuses to
//     stand behind.

// maxSafeInteger is the exact-integer range of an IEEE-754 double
// (RFC 8785: JSON numbers carry double-precision semantics).
const maxSafeInteger = float64(math.MaxInt64 >> uint(64-54)) // 2^53-1

// rawNumber carries a pre-serialized canonical JSON number literal into the
// encoder without re-quoting (encoding/json has no raw-number mode).
type rawNumber string

func (r rawNumber) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// unsignable fails the marshal when a value has no canonical form.
type unsignable struct{ reason string }

func (u unsignable) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("a2a: canonical: %s", u.reason)
}

// Canonical returns the canonical byte form of e (see the package comment
// and CONTRACT.md, "Canonical form"). It is deterministic across
// implementations that follow the contract: same envelope in, same bytes
// out, regardless of how the envelope was first serialized.
func (e *Envelope) Canonical() ([]byte, error) {
	m, err := e.canonicalMap()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("a2a: canonical: %w", err)
	}
	// Encoder.Encode appends '\n'; canonical bytes carry no trailing newline.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// canonicalMap builds the signable map: every envelope field that is
// present, keyed by its wire name, values canonicalized. encoding/json
// marshals map keys in sorted UTF-8 byte order, which is the contract's
// key order.
func (e *Envelope) canonicalMap() (map[string]any, error) {
	m := map[string]any{
		"v":    e.V,
		"id":   e.ID,
		"type": string(e.Type),
		"from": e.From,
		"ts":   e.TS.UTC().Format(time.RFC3339Nano),
	}
	if e.To != "" {
		m["to"] = e.To
	}
	if e.TTLMillis != 0 {
		m["ttl_ms"] = e.TTLMillis // bounded by MaxTTL; exact as an integer
	}
	if e.CorrelationID != "" {
		m["correlation_id"] = e.CorrelationID
	}
	if e.ReplyTo != "" {
		m["reply_to"] = e.ReplyTo
	}
	if e.Capability != "" {
		m["capability"] = e.Capability
	}
	if e.Priority != "" {
		m["priority"] = string(e.Priority)
	}
	if e.Event != "" {
		m["event"] = e.Event
	}
	if e.ContentType != "" {
		m["content_type"] = e.ContentType
	}
	if e.Enc != "" {
		m["enc"] = e.Enc
	}
	if len(e.Headers) > 0 {
		m["headers"] = e.Headers // map[string]string; keys sort on marshal
	}
	if len(e.Body) > 0 {
		v, err := canonicalJSON(e.Body)
		if err != nil {
			return nil, err
		}
		m["body"] = v
	}
	return m, nil
}

// canonicalJSON parses raw as exactly one JSON value and returns it in
// canonical shape: maps and slices rebuilt (sorted keys come from the
// encoder), numbers as rawNumber literals in JCS form, everything else
// as-is.
func canonicalJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("a2a: canonical: body is not one JSON value: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("a2a: canonical: body has trailing data after the first JSON value")
	}
	return canonicalValue(v), nil
}

// canonicalValue rewrites a parsed JSON tree so that encoding/json emits
// contract-canonical bytes: json.Number becomes a rawNumber in JCS layout.
func canonicalValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = canonicalValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = canonicalValue(vv)
		}
		return out
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return unsignable{reason: fmt.Sprintf("number %q does not fit a double", string(t))}
		}
		return canonicalFloat(f)
	case string, bool, nil:
		return t
	default:
		// Parsed JSON only yields the types above; anything else is a
		// programming error, and refusing is better than mangling.
		return unsignable{reason: fmt.Sprintf("unsupported value %T", v)}
	}
}

// canonicalFloat implements RFC 8785 (JCS) §3.2.2.3 number serialization:
// the double's shortest round-trip decimal digits in ES6 ToString layout.
func canonicalFloat(f float64) any {
	switch {
	case math.IsNaN(f) || math.IsInf(f, -1) || math.IsInf(f, 1):
		return unsignable{reason: "NaN and Infinity have no JSON canonical form"}
	case f == 0:
		return rawNumber("0") // +0 and -0 both collapse to 0
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, "eE") {
		// Plain decimal; integer-valued doubles carry no ".0".
		return rawNumber(s)
	}
	// Exponent form: re-lay the digits out per ES6 (JCS) thresholds.
	i := strings.IndexAny(s, "eE")
	mantissa, expStr := s[:i], s[i+1:]
	exp, err := strconv.Atoi(expStr)
	if err != nil {
		return unsignable{reason: fmt.Sprintf("float %v: bad exponent %q", f, expStr)}
	}
	digits := strings.Replace(mantissa, ".", "", 1)
	digits = strings.TrimPrefix(digits, "-")
	neg := strings.HasPrefix(mantissa, "-")
	k := len(digits)
	p := exp + 1 // decimal point position in the plain form
	var out string
	switch {
	case k <= p && p <= 21:
		out = digits + strings.Repeat("0", p-k) // ES6 step 8: plain integer
	case 0 < p && p <= 21:
		out = digits[:p] + "." + digits[p:] // ES6 step 9: embedded point
	case -6 < p && p <= 0:
		out = "0." + strings.Repeat("0", -p) + digits // ES6 step 10: 0.000ddd
	default:
		var b strings.Builder
		b.WriteString(digits[:1])
		if k > 1 {
			b.WriteString(".")
			b.WriteString(digits[1:])
		}
		we := p - 1
		if we < 0 {
			b.WriteString("e-")
			we = -we
		} else {
			b.WriteString("e+")
		}
		b.WriteString(strconv.Itoa(we))
		out = b.String() // ES6 step 11: scientific
	}
	if neg {
		out = "-" + out
	}
	return rawNumber(out)
}
