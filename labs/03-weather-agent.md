# Lab 3: The weather agent

**Goal:** an agent called `weather` that answers forecast requests,
written as a Python handler.

## 1. Generate the project

**Terminal 2:**

```bash
agentbus agent init weather --dir $LAB/weather --agentbus-src $REPO \
  --description "Weather forecasts for any city"
cd $LAB/weather
rm scripts/disk.sh scripts/wordcount.py       # template examples we won't use
cp $REPO/labs/weather-agent/scripts/forecast.py scripts/
```

## 2. Read the handler

Open `scripts/forecast.py`. It uses only the standard library, so it runs
on any host with python3. The parts to notice:

| Part | Why it matters |
|---|---|
| `main()` reads `json.load(sys.stdin)`, the whole envelope | The same contract as every script handler (lab 2) |
| `LOCATION_RE` and the `days` check, before anything else | The body comes from another agent. Check its shape first; never pass it anywhere unchecked |
| `Refuse("not_found", ...)` → prints `{"code","reason"}`, exits 3 | The requester gets a precise error instead of "handler failed" |
| `geocode()` | Turns "Seattle, WA" into coordinates with Open-Meteo's geocoding API, matching the state or country |
| `nws_forecast()`, with `open_meteo_forecast()` as the fallback | US places use the National Weather Service, which has written forecasts; anywhere else, or if NWS is down, Open-Meteo |
| `tls_context()` | Some Python builds ship without CA certificates; it falls back to the OS bundle and never turns verification off |

Try it on its own, with no bus involved. A handler is just a program:

```bash
echo '{"from":"me","body":{"location":"Seattle, WA","days":1}}' | python3 scripts/forecast.py
echo '{"from":"me","body":{"location":"Nowhereville, ZZ"}}' | python3 scripts/forecast.py; echo "exit $?"
```

## 3. Write the manifest

Replace `agent.yaml`:

```bash
cat > agent.yaml <<'EOF'
agent: weather
description: Weather forecasts for any city, from public weather services
max_concurrent: 4
timeout: 30s
drain: 15s
handlers:
  - task: weather.forecast
    script: scripts/forecast.py
    description: >-
      body {"location": "City, ST" or "City, Country", "days"?: 1-7 (default 3)};
      replies {"location", "source", "periods": [{"name", "temperature", "unit", "summary", "detail", "wind"}]}
  - inbox: true
    op: forecast
    script: scripts/forecast.py
    description: '{"op": "forecast", "location": string, "days"?: 1-7}; same reply as task weather.forecast'
  - inbox: true
    op: ping
    run: 'printf "{\"pong\": \"%s\"}" "$(hostname)"'
    description: '{"op": "ping"}; replies {"pong": hostname}'
EOF
agentbus agent check
```

The same script serves two triggers:
- **A task** (`weather.forecast`) for anyone who wants a forecast and
  doesn't care which agent produces it.
- **An inbox op** (`forecast`) for callers that ask the `weather` agent by
  name.

The `description`s matter. They're published on the agent's presence
card, and they're how other agents, and Claude in lab 5, learn what body
to send.

## 4. Give it an identity and run it

```bash
agentbus config agent add weather --capability weather.forecast
```

**Terminal 3:**

```bash
cd $LAB/weather
agentbus agent run
```

## 5. Ask for the forecast

**Terminal 2:**

```bash
export AGENTBUS_AGENT=me
agentbus task -c weather.forecast --body '{"location":"Seattle, WA","days":2}'
```

On a terminal the reply is pretty-printed. Expect something like this;
the weather will differ:

```json
{
  "location": "Seattle, Washington, US",
  "source": "National Weather Service (api.weather.gov)",
  "periods": [
    {
      "name": "Tonight",
      "temperature": 43,
      "unit": "F",
      "summary": "Chance Light Rain",
      "detail": "A chance of light rain. Partly cloudy, with a low around 43. ...",
      "wind": "3 to 7 mph SSE"
    },
    ...
```

Now try the other routes, and the failures:

```bash
agentbus request --to weather --body '{"op":"forecast","location":"Paris, France","days":2}'   # Open-Meteo
agentbus request --to weather --body '{"op":"ping"}'
agentbus task -c weather.forecast --body '{"location":"Nowhereville, ZZ"}'       # not_found
agentbus task -c weather.forecast --body '{"location":"x; rm -rf ~"}'            # invalid
agentbus task -c weather.forecast --body '{"location":"Seattle, WA","days":30}'  # invalid
```

Keep `agentbus console --tab traffic` open in terminal 4 to see each
request and reply cross the bus.

## Questions

1. The forecast is offered as a task and as an inbox op. When would a
   caller choose each?
2. Why does the script need its own User-Agent? (Read the NWS API's
   rules, and look at `USER_AGENT`.)
3. What happens if two terminals run `agentbus agent run` for `weather` at
   the same time? Try it, and watch terminal 1.

**Done when:** you get a Seattle forecast from the NWS through the bus,
and a Paris forecast from Open-Meteo.

Leave the agent running for now; lab 4 replaces it.
