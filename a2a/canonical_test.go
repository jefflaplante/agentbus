package a2a

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// Conformance tests for the canonical form (CONTRACT.md, "Canonical form").
// The float table doubles as an RFC 8785 appendix-A cross-check.

var testTS = time.Date(2026, 11, 15, 16, 4, 5, 123000000, time.UTC)

func testEnvelope(body any) (*Envelope, error) {
	e := &Envelope{V: Version, ID: "t1", Type: TypeEvent, From: "alice", TS: testTS}
	if err := e.SetBody(body); err != nil {
		return nil, err
	}
	return e, nil
}

// canonBody returns the canonical serialization of just the value v,
// wrapped in a one-key body object so scalar bodies extract cleanly.
func canonBody(t *testing.T, v any) string {
	t.Helper()
	e, err := testEnvelope(map[string]any{"n": v})
	if err != nil {
		t.Fatalf("SetBody(%v): %v", v, err)
	}
	c, err := e.Canonical()
	if err != nil {
		t.Fatalf("Canonical(%v): %v", v, err)
	}
	s := string(c)
	i := strings.Index(s, `"body":{"n":`)
	if i < 0 {
		t.Fatalf("no body in %s", s)
	}
	rest := s[i+12:]
	// body sorts before "from", which every envelope carries: cut there.
	if j := strings.Index(rest, `},"from":`); j >= 0 {
		return rest[:j]
	}
	t.Fatalf("malformed canonical output: %s", s)
	return ""
}

func TestCanonicalFloats(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.0, `0`},
		{-0.0, `0`}, // negative zero collapses per JCS
		{42.3, `42.3`},
		{1.0, `1`}, // integer-valued double: no ".0"
		{0.0001, `0.0001`},
		{1e-7, `1e-7`},     // first exponent-required value below 1
		{1e-6, `0.000001`}, // last plain-decimal value below 1
		{1e20, `100000000000000000000`},
		{1e21, `1e+21`}, // first exponent-required value above
		{9.999999999999999e20, `999999999999999900000`},
		{float64(9007199254740993), `9007199254740992`}, // 2^53+1 parses to 2^53
		{333333333.334, `333333333.334`},
		{-1.7976931348623157e308, `-1.7976931348623157e+308`},
	}
	for _, tc := range cases {
		if got := canonBody(t, tc.in); got != tc.want {
			t.Errorf("float %v -> %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalBodyFromWireNumbers(t *testing.T) {
	// Bodies that arrive as raw JSON keep JCS rules regardless of the
	// sender's spelling: non-canonical literals re-serialize canonically.
	cases := []struct{ raw, want string }{
		{`42.30`, `42.3`},
		{`42.10`, `42.1`},
		{`1e2`, `100`},
		{`1E+2`, `100`},
		{`0.000001`, `0.000001`},
		{`1000000000000000000000`, `1e+21`},            // 10^21 is exact as a double
		{`9007199254740992`, `9007199254740992`},       // 2^53: canonical already
		{`1152921504606847000`, `1152921504606847000`}, // 2^60, in its one integer spelling
	}
	for _, tc := range cases {
		e, err := testEnvelope(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("SetBody(%s): %v", tc.raw, err)
		}
		c, err := e.Canonical()
		if err != nil {
			t.Fatalf("Canonical(%s): %v", tc.raw, err)
		}
		if !strings.Contains(string(c), `"body":`+tc.want+`,`) {
			t.Errorf("raw %s -> %s, want body %s", tc.raw, c, tc.want)
		}
	}
}

func TestCanonicalRefuses(t *testing.T) {
	cases := []string{
		`1e400`,                   // beyond a double's range: refuse, don't round
		`-1e400`,                  //
		`1e-400`,                  // underflows to 0: refuse, don't round
		`9007199254740993`,        // same double as 9007199254740992: two values, one signature
		`1152921504606846976`,     // 2^60 exactly, but its one integer spelling is 1152921504606847000
		`1000000000000000000001`,  // from 1e21 up only the exact value is accepted
		`[-12345678901234567890]`, //
		`{"a":1} trailing`,        // two values where one is required
		`{"a":1,"a":2}`,           // duplicate key: receivers disagree on which wins
		`{"a":1,"\u0061":2}`,      // the same key, escaped
		`{"o":{"k":1,"k":1}}`,     // nested
		`"\ud800"`,                // unpaired high surrogate
		`"\udfff"`,                // unpaired low surrogate
		`"\ud800\u0041"`,          // high surrogate followed by a non-surrogate
		"\"pay \xff bob\"",        // invalid UTF-8
	}
	for _, raw := range cases {
		e, err := testEnvelope(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("SetBody(%s): %v", raw, err)
		}
		if _, err := e.Canonical(); err == nil {
			t.Errorf("Canonical accepted %s", raw)
		}
	}
}

func TestCanonicalReencodeInvariant(t *testing.T) {
	// Canonical must be a function of the parsed envelope: after the
	// broker re-encodes (Parse then Marshal), the bytes are unchanged.
	// Body deliberately carries <&> and non-ASCII: the broker's re-encode
	// HTML-escapes and the canonical form must not care (parse unescapes,
	// Canonical writes < > & raw).
	e, err := testEnvelope(map[string]any{
		"z": 1, "a": true, "m": []any{0.5, "s", nil},
		"nested": map[string]any{"k": 42.30, "html": "<script>&</script>", "café": "né"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.To = "bob"
	e.Priority = PriorityHigh
	e.Headers = map[string]string{"session": "tg:1", "trace": "abc"}
	first, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}

	wire, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parsed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("canonical form not stable across re-encode:\n first: %s\nsecond: %s", first, second)
	}
}

func TestCanonicalTSNormalization(t *testing.T) {
	// +00:00 and Z spellings of the same instant canonicalize identically.
	z := &Envelope{V: Version, ID: "t1", Type: TypeEvent, From: "alice", TS: testTS}
	offset := *z
	loc := time.FixedZone("utcish", 0)
	offset.TS = time.Date(2026, 11, 15, 16, 4, 5, 123000000, loc)

	zc, err := z.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	oc, err := offset.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(zc) != string(oc) {
		t.Errorf("ts spellings differ:\n Z:   %s\n +00: %s", zc, oc)
	}
	if !strings.Contains(string(zc), `"ts":"2026-11-15T16:04:05.123Z"`) {
		t.Errorf("ts not normalized to RFC3339Nano UTC: %s", zc)
	}
}

func TestCanonicalKeyOrderAndOmission(t *testing.T) {
	// Keys sorted by UTF-16 code units (RFC 8785); absent fields omitted;
	// headers in.
	e, _ := testEnvelope(map[string]any{"b": 1, "a": 2})
	e.To = "bob"
	e.Headers = map[string]string{"zz": "1", "aa": "2"}
	c, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	s := string(c)
	want := `{"body":{"a":2,"b":1},"from":"alice","headers":{"aa":"2","zz":"1"},"id":"t1","to":"bob","ts":"2026-11-15T16:04:05.123Z","type":"event","v":1`
	if !strings.HasPrefix(s, want) {
		t.Errorf("got  %s\nwant prefix %s", s, want)
	}
	if strings.Contains(s, "ttl_ms") || strings.Contains(s, "priority") {
		t.Errorf("absent fields must be omitted: %s", s)
	}
}

func TestCanonicalByteExactStrings(t *testing.T) {
	// No Unicode normalization: NFD and NFC spellings stay distinct, and
	// neither is rewritten. (é as U+00E9 vs e + U+0301.)
	nfc, _ := testEnvelope(json.RawMessage(`{"s":"café"}`))
	nfd, _ := testEnvelope(json.RawMessage(`{"s":"cafe\u0301"}`))
	c1, err1 := nfc.Canonical()
	c2, err2 := nfd.Canonical()
	if err1 != nil || err2 != nil {
		t.Fatalf("canonical: %v / %v", err1, err2)
	}
	if string(c1) == string(c2) {
		t.Errorf("byte-distinct strings must canonicalize distinctly:\n %s\n %s", c1, c2)
	}
	if strings.Contains(string(c2), "caf\u00e9") {
		t.Errorf("NFD string was normalized to NFC: %s", c2)
	}
}

func TestCanonicalNoHTMLEscaping(t *testing.T) {
	e, _ := testEnvelope(json.RawMessage(`{"s":"<&>"}`))
	c, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(c), `\u003c`) || strings.Contains(string(c), `\u0026`) {
		t.Errorf("HTML escaping leaked into canonical form: %s", c)
	}
	if !strings.Contains(string(c), `<&>`) {
		t.Errorf("raw characters missing: %s", c)
	}
}

func TestCanonicalIntBounds(t *testing.T) {
	// MaxTTL stays an exact integer on the wire and in canonical form.
	e, _ := testEnvelope(nil)
	e.TTLMillis = MaxTTL.Milliseconds()
	c, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c), `"ttl_ms":86400000`) {
		t.Errorf("ttl_ms not exact: %s", c)
	}
	if math.Abs(float64(MaxTTL.Milliseconds())-86400000) > 0 { // keeps math imported if table trimmed
		t.Error("ttl drift")
	}
}

// RFC 8785 sorts keys by UTF-16 code units, not UTF-8 bytes: U+1F600 is
// the surrogate pair D83D DE00, which sorts before U+FB01.
func TestCanonicalKeyOrderUTF16(t *testing.T) {
	e, _ := testEnvelope(json.RawMessage(`{"\ufb01":1,"\ud83d\ude00":2,"a":3}`))
	c, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c), `"body":{"a":3,"`+"\U0001F600"+`":2,"`+"\uFB01"+`":1}`) {
		t.Errorf("not in UTF-16 order: %s", c)
	}
}

// Strings escape as JSON.stringify does: short escapes, lowercase \u00xx
// for other controls, everything else raw, U+2028 and U+2029 included.
func TestCanonicalStringEscapes(t *testing.T) {
	e, _ := testEnvelope(json.RawMessage(`"\b\f\n\r\t\u0001\u001f\"\\/\u007f\u2028\u2029"`))
	c, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	want := `"body":"\b\f\n\r\t\u0001\u001f\"\\/` + "\u007f\u2028\u2029" + `"`
	if !strings.Contains(string(c), want) {
		t.Errorf("got %s\nwant %s", c, want)
	}
}

// A header value that isn't valid UTF-8 is refused, not rewritten.
func TestCanonicalRefusesInvalidHeader(t *testing.T) {
	e, _ := testEnvelope(nil)
	e.Headers = map[string]string{"k": "\xff"}
	if _, err := e.Canonical(); err == nil {
		t.Error("invalid UTF-8 header value accepted")
	}
}

// Parse refuses what isn't I-JSON, so the broker never forwards a message
// two receivers could read differently.
func TestParseRefusesNonIJSON(t *testing.T) {
	base := `{"v":1,"id":"t1","type":"event","from":"alice","event":"x.y","ts":"2026-11-15T16:04:05Z"`
	for _, wire := range []string{
		base + `,"to":"bob","to":"mallory"}`,
		base + `,"headers":{"k":"\ud800"}}`,
		base + `,"body":{"a":1,"a":2}}`,
		base + ",\"headers\":{\"k\":\"\xc3\"}}",
	} {
		if _, err := Parse([]byte(wire)); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) = %v, want ErrMalformed", wire, err)
		}
	}
	if _, err := Parse([]byte(base + `,"body":"\ud83d\ude00"}`)); err != nil {
		t.Errorf("a valid surrogate pair was refused: %v", err)
	}
}

// testdata/canonical-vectors.json is shared with the Python
// implementation (examples/python/test_canonical.py).
func TestCanonicalVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/canonical-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Vectors []struct {
			Name, Wire, WireBase64, Canonical string
			Error                             bool
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, v := range doc.Vectors {
		wire := []byte(v.Wire)
		if v.WireBase64 != "" {
			if wire, err = base64.StdEncoding.DecodeString(v.WireBase64); err != nil {
				t.Fatal(err)
			}
		}
		var got []byte
		e, err := Parse(wire)
		if err == nil {
			got, err = e.Canonical()
		}
		switch {
		case v.Error && err == nil:
			t.Errorf("%s: accepted, produced %s", v.Name, got)
		case !v.Error && err != nil:
			t.Errorf("%s: %v", v.Name, err)
		case !v.Error && string(got) != v.Canonical:
			t.Errorf("%s:\n got %s\nwant %s", v.Name, got, v.Canonical)
		case !v.Error:
			// Canonical output is itself a valid envelope with the same
			// canonical form: verifiers can canonicalize what they receive.
			again, err := Parse(got)
			if err == nil {
				got, err = again.Canonical()
			}
			if err != nil || string(got) != v.Canonical {
				t.Errorf("%s: not idempotent: %s %v", v.Name, got, err)
			}
		}
	}
	if len(doc.Vectors) < 20 {
		t.Errorf("only %d vectors", len(doc.Vectors))
	}
}
