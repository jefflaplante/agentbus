#!/usr/bin/env python3
"""A stand-in for the weather services the lab's weather agent calls.

`labs/check.sh --mock` runs it and sets WEATHER_API_URL, which points
forecast.py and current.go here instead of at the real services. CI uses
it, so the labs job tests agentbus and not the weather services'
availability from GitHub's runners.

It answers the four calls the agent makes, with fixed data:

  GET /v1/search?name=...                 Open-Meteo geocoding
  GET /points/{lat},{lon}                 api.weather.gov: names the forecast URL
  GET /gridpoints/SEW/125,68/forecast     api.weather.gov: day and night periods
  GET /v1/forecast?daily=... | current=   Open-Meteo forecast and current conditions

It knows Seattle (US, so NWS) and London (GB, so Open-Meteo); any other
name has no results, which the agent reports as not_found. Every forecast
says it is mock data, so nobody mistakes it for a real one.

  labs/mock-weather.py [--port N] [--port-file FILE]

With --port 0 (the default) it picks a free port; --port-file gets the
port number once it is listening. Standard library only.
"""
import argparse
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

PLACES = {
    "seattle": {"name": "Seattle", "admin1": "Washington", "country": "United States", "country_code": "US",
                "latitude": 47.60621, "longitude": -122.33207},
    "london": {"name": "London", "admin1": "England", "country": "United Kingdom", "country_code": "GB",
               "latitude": 51.50853, "longitude": -0.12574},
}

DAYS = ["Today", "Tonight", "Tuesday", "Tuesday Night", "Wednesday", "Wednesday Night", "Thursday",
        "Thursday Night", "Friday", "Friday Night", "Saturday", "Saturday Night", "Sunday", "Sunday Night"]


def nws_periods():
    out = []
    for i, name in enumerate(DAYS):
        night = i % 2 == 1
        temp = 44 if night else 52
        short = "Partly Cloudy" if night else ("Chance Rain Showers" if i % 4 == 0 else "Mostly Sunny")
        out.append({
            "number": i + 1, "name": name, "isDaytime": not night,
            "temperature": temp, "temperatureUnit": "F",
            "windSpeed": "5 mph", "windDirection": "S",
            "shortForecast": short,
            "detailedForecast": f"{short}. {'Low' if night else 'High'} near {temp}. (mock data from labs/mock-weather.py)",
        })
    return out


class Handler(BaseHTTPRequestHandler):
    server_version = "mock-weather/1.0"

    def log_message(self, fmt, *args):  # one line per request, on stderr
        sys.stderr.write("mock-weather: " + (fmt % args) + "\n")

    def send_json(self, body, status=200):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        u = urlparse(self.path)
        q = {k: v[0] for k, v in parse_qs(u.query).items()}
        base = f"http://{self.headers.get('Host', '127.0.0.1')}"

        if u.path == "/v1/search":
            place = PLACES.get(q.get("name", "").strip().lower())
            return self.send_json({"results": [place]} if place else {"generationtime_ms": 0.1})

        if u.path.startswith("/points/"):
            return self.send_json({"properties": {"forecast": base + "/gridpoints/SEW/125,68/forecast"}})

        if u.path == "/gridpoints/SEW/125,68/forecast":
            return self.send_json({"properties": {"periods": nws_periods()}})

        if u.path == "/v1/forecast" and "current" in q:
            return self.send_json({"current": {"time": "2026-11-16T12:00", "temperature_2m": 51.8,
                                               "wind_speed_10m": 4.6, "weather_code": 2}})

        if u.path == "/v1/forecast" and "daily" in q:
            n = max(1, min(7, int(q.get("forecast_days", "3"))))
            return self.send_json({"daily": {
                "time": [f"2026-11-{16 + i:02d}" for i in range(n)],
                "weather_code": [[2, 61, 3, 80, 1, 0, 63][i] for i in range(n)],
                "temperature_2m_max": [50.4 + i for i in range(n)],
                "temperature_2m_min": [41.2 + i for i in range(n)],
                "precipitation_probability_max": [[10, 70, 20, 60, 5, 0, 80][i] for i in range(n)],
                "wind_speed_10m_max": [9.3 + i for i in range(n)],
            }})

        self.send_json({"error": "not found", "path": u.path}, 404)


def main():
    ap = argparse.ArgumentParser(description="Stand-in weather services for labs/check.sh --mock")
    ap.add_argument("--port", type=int, default=0, help="port to listen on (default: any free port)")
    ap.add_argument("--port-file", help="write the port here once listening")
    args = ap.parse_args()
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    port = srv.server_address[1]
    if args.port_file:
        with open(args.port_file + ".tmp", "w") as f:
            f.write(str(port))
        os.replace(args.port_file + ".tmp", args.port_file)  # appears whole, never half-written
    print(f"mock-weather listening on http://127.0.0.1:{port}", file=sys.stderr, flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
