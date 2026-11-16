#!/usr/bin/env python3
"""weather.forecast: the forecast for a place, from public weather APIs.

Request body:  {"location": "Seattle, WA", "days": 3}      (days optional, 1-7)
Reply body:    {"location": "Seattle, Washington, US", "source": "...",
                "periods": [{"name", "temperature", "unit", "summary", "detail", "wind"}]}

US locations use the National Weather Service (api.weather.gov), which has
written forecasts. Anywhere else, or if NWS is unavailable, Open-Meteo.
Both are free and need no API key. Places are found with Open-Meteo's
geocoding API.

The agentbus contract: the envelope JSON arrives on stdin and the reply
goes to stdout. To refuse with an error the requester sees, print
{"code": ..., "reason": ...} and exit 3. stderr stays in the agent's log.
Standard library only, so it runs on any host with python3.
"""
import json
import os
import re
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request

USER_AGENT = "agentbus-weather-lab/1.0"  # api.weather.gov requires a User-Agent
TIMEOUT = 10  # seconds per upstream call; the handler's own timeout is 30s

US_STATES = {
    "AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California",
    "CO": "Colorado", "CT": "Connecticut", "DE": "Delaware", "DC": "District of Columbia",
    "FL": "Florida", "GA": "Georgia", "HI": "Hawaii", "ID": "Idaho", "IL": "Illinois",
    "IN": "Indiana", "IA": "Iowa", "KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana",
    "ME": "Maine", "MD": "Maryland", "MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota",
    "MS": "Mississippi", "MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada",
    "NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico", "NY": "New York",
    "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma", "OR": "Oregon",
    "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina", "SD": "South Dakota",
    "TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont", "VA": "Virginia",
    "WA": "Washington", "WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming",
}

# WMO weather codes, for Open-Meteo forecasts.
WMO = {
    0: "Clear", 1: "Mainly clear", 2: "Partly cloudy", 3: "Overcast", 45: "Fog", 48: "Freezing fog",
    51: "Light drizzle", 53: "Drizzle", 55: "Heavy drizzle", 56: "Freezing drizzle", 57: "Freezing drizzle",
    61: "Light rain", 63: "Rain", 65: "Heavy rain", 66: "Freezing rain", 67: "Freezing rain",
    71: "Light snow", 73: "Snow", 75: "Heavy snow", 77: "Snow grains", 80: "Rain showers",
    81: "Rain showers", 82: "Violent rain showers", 85: "Snow showers", 86: "Heavy snow showers",
    95: "Thunderstorm", 96: "Thunderstorm with hail", 99: "Thunderstorm with heavy hail",
}

# The location comes from another agent: accept a plain place name only.
LOCATION_RE = re.compile(r"^[A-Za-z][A-Za-z .,'-]{0,99}$")


class Refuse(Exception):
    """An error the requester should see: code and reason."""

    def __init__(self, code, reason):
        super().__init__(reason)
        self.code, self.reason = code, reason


def tls_context():
    """A verifying TLS context. Some Python builds (python.org's on macOS,
    minimal containers) ship without CA certificates; then use the
    operating system's bundle. Verification is never turned off."""
    ctx = ssl.create_default_context()
    if ctx.cert_store_stats().get("x509_ca", 0) == 0:
        for bundle in ("/etc/ssl/cert.pem", "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt"):
            if os.path.exists(bundle):
                ctx.load_verify_locations(cafile=bundle)
                break
    return ctx


TLS = tls_context()


def get_json(url, headers=None):
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, **(headers or {})})
    with urllib.request.urlopen(req, timeout=TIMEOUT, context=TLS) as resp:
        return json.load(resp)


def geocode(location):
    """Find a place for "City", "City, ST" or "City, Country"."""
    city, _, qualifier = (part.strip() for part in location.partition(","))
    qs = urllib.parse.urlencode({"name": city, "count": 10, "language": "en", "format": "json"})
    try:
        results = get_json("https://geocoding-api.open-meteo.com/v1/search?" + qs).get("results") or []
    except (urllib.error.URLError, TimeoutError, ValueError) as e:
        raise Refuse("upstream_error", f"geocoding failed: {e}")
    if qualifier:
        q = qualifier.lower()
        state = US_STATES.get(qualifier.upper(), "").lower()
        results = [r for r in results if q in (
            (r.get("admin1") or "").lower(), (r.get("country") or "").lower(),
            (r.get("country_code") or "").lower()) or (state and (r.get("admin1") or "").lower() == state)]
    if not results:
        raise Refuse("not_found", f"no place called {location!r}")
    r = results[0]
    name = ", ".join(p for p in (r.get("name"), r.get("admin1"), r.get("country_code")) if p)
    return name, r["latitude"], r["longitude"], r.get("country_code")


def nws_forecast(lat, lon, days):
    point = get_json(f"https://api.weather.gov/points/{lat:.4f},{lon:.4f}",
                     {"Accept": "application/geo+json"})
    forecast = get_json(point["properties"]["forecast"], {"Accept": "application/geo+json"})
    periods = forecast["properties"]["periods"][: days * 2]  # NWS gives day and night periods
    return [{
        "name": p["name"],
        "temperature": p["temperature"],
        "unit": p["temperatureUnit"],
        "summary": p["shortForecast"],
        "detail": p["detailedForecast"],
        "wind": f'{p.get("windSpeed", "")} {p.get("windDirection", "")}'.strip(),
    } for p in periods]


def open_meteo_forecast(lat, lon, days):
    qs = urllib.parse.urlencode({
        "latitude": lat, "longitude": lon, "forecast_days": days, "timezone": "auto",
        "daily": "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,wind_speed_10m_max",
        "temperature_unit": "fahrenheit", "wind_speed_unit": "mph",
    })
    d = get_json("https://api.open-meteo.com/v1/forecast?" + qs)["daily"]
    out = []
    for i, day in enumerate(d["time"]):
        summary = WMO.get(d["weather_code"][i], "Unknown")
        out.append({
            "name": day,
            "temperature": round(d["temperature_2m_max"][i]),
            "unit": "F",
            "summary": summary,
            "detail": (f'{summary}. High {round(d["temperature_2m_max"][i])}F, low {round(d["temperature_2m_min"][i])}F, '
                       f'{d["precipitation_probability_max"][i]}% chance of precipitation.'),
            "wind": f'up to {round(d["wind_speed_10m_max"][i])} mph',
        })
    return out


def forecast(body):
    if not isinstance(body, dict) or not isinstance(body.get("location"), str):
        raise Refuse("invalid", 'body must be {"location": "City, ST", "days"?: 1-7}')
    location = body["location"].strip()
    if not LOCATION_RE.match(location):
        raise Refuse("invalid", "location must be a place name like 'Seattle, WA' (letters, spaces, . , ' -)")
    days = body.get("days", 3)
    if not isinstance(days, int) or isinstance(days, bool) or not 1 <= days <= 7:
        raise Refuse("invalid", "days must be a whole number from 1 to 7")

    name, lat, lon, country = geocode(location)
    if country == "US":
        try:
            return {"location": name, "source": "National Weather Service (api.weather.gov)",
                    "periods": nws_forecast(lat, lon, days)}
        except (urllib.error.URLError, TimeoutError, KeyError, ValueError) as e:
            print(f"NWS failed, falling back to Open-Meteo: {e}", file=sys.stderr)
    try:
        return {"location": name, "source": "Open-Meteo (open-meteo.com)",
                "periods": open_meteo_forecast(lat, lon, days)}
    except (urllib.error.URLError, TimeoutError, KeyError, ValueError) as e:
        raise Refuse("upstream_error", f"forecast failed: {e}")


def main():
    envelope = json.load(sys.stdin)
    try:
        json.dump(forecast(envelope.get("body")), sys.stdout)
    except Refuse as r:
        json.dump({"code": r.code, "reason": r.reason}, sys.stdout)
        sys.exit(3)


if __name__ == "__main__":
    main()
