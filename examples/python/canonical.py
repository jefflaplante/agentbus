"""The canonical form of an envelope: the exact bytes a signature covers.

This is CONTRACT.md, "Canonical form", in Python, written separately from
the Go version (a2a/canonical.go). Both pass a2a/testdata/canonical-vectors.json
(test_canonical.py runs it here). Standard library only.

    canonical(wire_bytes) -> bytes      # raises CanonicalError when refused

The canonical form is RFC 8785 (JCS) applied to the parsed envelope, with
ts normalized to UTC and Z. Input that two receivers could read differently
is refused: invalid UTF-8, unpaired surrogates, duplicate keys, more than
one integer literal for one double, numbers outside a double's range.
"""

import json
import math
import re
from datetime import datetime, timedelta, timezone
from decimal import Decimal

MAX_SAFE_INTEGER = 2**53 - 1

# Envelope fields, as the Go struct has them. Unknown fields are dropped,
# as Go's parser drops them; empty optional fields count as absent.
_STRING_FIELDS = ("id", "type", "from")
_OPTIONAL_STRINGS = ("to", "correlation_id", "reply_to", "capability", "priority",
                     "event", "content_type", "enc")

_TS = re.compile(r"^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?([Zz]|[+-]\d{2}:\d{2})$")


class CanonicalError(ValueError):
    """The envelope has no canonical form."""


class _Number:
    """A JSON number literal, kept as written until serialized."""

    def __init__(self, literal: str):
        self.literal = literal


class _Object(dict):
    """A parsed object that remembers whether a key repeated."""

    duplicate = None


def _pairs(pairs):
    obj = _Object()
    for k, v in pairs:
        if k in obj and obj.duplicate is None:
            obj.duplicate = k
        obj[k] = v
    return obj


def canonical(wire: bytes) -> bytes:
    """Return the canonical bytes of the envelope in wire, or raise."""
    try:
        text = wire.decode("utf-8")  # strict: invalid UTF-8 is refused
    except UnicodeDecodeError as e:
        raise CanonicalError(f"invalid UTF-8: {e}") from None
    try:
        env = json.loads(text, object_pairs_hook=_pairs, parse_int=_Number,
                         parse_float=_Number, parse_constant=_refuse_constant)
    except json.JSONDecodeError as e:
        raise CanonicalError(f"not JSON: {e}") from None
    if not isinstance(env, dict):
        raise CanonicalError("an envelope is a JSON object")
    _check(env)  # anywhere: duplicate keys, unpaired surrogates

    out = {}
    for k in ("v",):
        out[k] = _int_field(env, k, required=True)
    for k in _STRING_FIELDS:
        v = env.get(k)
        if not isinstance(v, str):
            raise CanonicalError(f"{k} must be a string")
        out[k] = v
    out["ts"] = _normalize_ts(env.get("ts"))
    for k in _OPTIONAL_STRINGS:
        v = env.get(k)
        if v not in (None, ""):
            if not isinstance(v, str):
                raise CanonicalError(f"{k} must be a string")
            out[k] = v
    ttl = _int_field(env, "ttl_ms", required=False)
    if ttl:
        out["ttl_ms"] = ttl
    headers = env.get("headers")
    if headers:
        if not isinstance(headers, dict) or not all(isinstance(v, str) for v in headers.values()):
            raise CanonicalError("headers must map strings to strings")
        out["headers"] = dict(headers)
    if "body" in env:
        out["body"] = env["body"]  # JSON null stays: "body":null is present
    return _serialize(out).encode("utf-8")


def _refuse_constant(name):
    raise CanonicalError(f"{name} is not JSON")


def _check(v):
    if isinstance(v, _Object) and v.duplicate is not None:
        raise CanonicalError(f"duplicate key {v.duplicate!r}")
    if isinstance(v, str):
        _check_string(v)
    elif isinstance(v, dict):
        for k, vv in v.items():
            _check_string(k)
            _check(vv)
    elif isinstance(v, list):
        for vv in v:
            _check(vv)


def _check_string(s: str):
    # json.loads keeps "\ud800" as a lone surrogate; it has no UTF-8 form.
    try:
        s.encode("utf-8")
    except UnicodeEncodeError:
        raise CanonicalError(f"unpaired surrogate in {s!r}") from None


def _int_field(env, key, required):
    v = env.get(key)
    if v is None:
        if required:
            raise CanonicalError(f"{key} is required")
        return 0
    if not isinstance(v, _Number) or not re.fullmatch(r"-?\d+", v.literal):
        raise CanonicalError(f"{key} must be an integer")
    n = int(v.literal)
    if abs(n) > MAX_SAFE_INTEGER:
        raise CanonicalError(f"{key} must be within ±(2^53-1)")
    return n


def _normalize_ts(ts) -> str:
    """UTC, RFC 3339, Z suffix, fraction trimmed of trailing zeros.

    Done on the string, not with datetime, which keeps only microseconds.
    """
    m = _TS.match(ts) if isinstance(ts, str) else None
    if not m:
        raise CanonicalError(f"ts {ts!r} is not RFC 3339")
    y, mo, d, h, mi, s, frac, off = m.groups()
    dt = datetime(int(y), int(mo), int(d), int(h), int(mi), int(s), tzinfo=timezone.utc)
    if off not in ("Z", "z"):
        sign = 1 if off[0] == "+" else -1
        dt -= sign * timedelta(hours=int(off[1:3]), minutes=int(off[4:6]))
    if not 0 <= dt.year <= 9999:
        raise CanonicalError("ts year has no RFC 3339 form")
    frac = (frac or "").rstrip("0")
    return dt.strftime("%Y-%m-%dT%H:%M:%S") + ("." + frac if frac else "") + "Z"


def _serialize(v) -> str:
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, str):
        return _string(v)
    if isinstance(v, _Number):
        return _number(v.literal)
    if isinstance(v, int):  # envelope integers, already range-checked
        return str(v)
    if isinstance(v, dict):
        keys = sorted(v, key=lambda k: k.encode("utf-16-be"))  # UTF-16 code units
        return "{" + ",".join(_string(k) + ":" + _serialize(v[k]) for k in keys) + "}"
    if isinstance(v, list):
        return "[" + ",".join(_serialize(x) for x in v) + "]"
    raise CanonicalError(f"unsupported value {type(v).__name__}")


_SHORT = {'"': '\\"', "\\": "\\\\", "\b": "\\b", "\t": "\\t", "\n": "\\n", "\f": "\\f", "\r": "\\r"}


def _string(s: str) -> str:
    """RFC 8785 §3.2.2.2: JSON.stringify escaping, everything else raw."""
    out = ['"']
    for ch in s:
        if ch in _SHORT:
            out.append(_SHORT[ch])
        elif ord(ch) < 0x20:
            out.append("\\u%04x" % ord(ch))
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _number(literal: str) -> str:
    """RFC 8785 §3.2.2.3, refusing literals a double can't stand for."""
    is_int = re.fullmatch(r"-?\d+", literal) is not None
    try:
        f = float(literal)
    except OverflowError:
        f = math.inf
    if math.isinf(f) or math.isnan(f):
        raise CanonicalError(f"number {literal} does not fit a double")
    mantissa = literal.lower().split("e")[0]
    if f == 0 and any(c in mantissa for c in "123456789"):
        raise CanonicalError(f"number {literal} underflows to 0")
    out = _es6(f)
    if is_int and abs(f) > MAX_SAFE_INTEGER:
        # One integer spelling per double: the canonical form while it is a
        # plain integer, the exact value once it has an exponent (1e21 up).
        ok = literal == out if "e" not in out else int(literal) == int(f)
        if not ok:
            raise CanonicalError(f"integer {literal} isn't the one integer spelling of its double")
    return out


def _es6(f: float) -> str:
    """ECMAScript Number::toString: shortest round-trip digits."""
    if f == 0:
        return "0"
    sign, digits, exp = Decimal(repr(abs(f))).normalize().as_tuple()
    digits = "".join(map(str, digits))
    k = len(digits)
    n = exp + k  # position of the decimal point
    if k <= n <= 21:
        out = digits + "0" * (n - k)
    elif 0 < n <= 21:
        out = digits[:n] + "." + digits[n:]
    elif -6 < n <= 0:
        out = "0." + "0" * -n + digits
    else:
        e = n - 1
        out = digits[0] + ("." + digits[1:] if k > 1 else "") + "e" + ("+" if e >= 0 else "-") + str(abs(e))
    return ("-" if f < 0 else "") + out
