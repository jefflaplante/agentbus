# Lab 6: Extend it

**Goal:** add a Go handler for current conditions, limit who may call
it, and see the request metrics the broker keeps.

## 1. A Go handler

**Terminal 2:**

```bash
cp $REPO/labs/weather-agent/handlers/current.go $LAB/weather/handlers/
```

Read it. A Go handler is a function registered by name in `init`:

```go
func init() { agent.Register("current", current) }

func current(ctx context.Context, e *a2a.Envelope) (any, error) { ... }
```

- **The return value** is the reply body.
- **`client.Errorf("not_found", ...)`** chooses the error code, as exit 3
  does for scripts.
- **`ctx`** ends at the request's deadline, and every HTTP call uses it,
  so a slow upstream can't outlive the request.
- **To add another Go handler,** drop another file in `handlers/`.

## 2. Add it to the manifest, with an allow-list

```bash
cat >> $LAB/weather/agent.yaml <<'EOF'
  - inbox: true
    op: current
    go: current
    timeout: 20s
    allow_from: [me, claude-code]
    description: '{"op": "current", "location": string}; replies current temperature, wind and conditions'
EOF
agentbus agent build $LAB/weather
```

`allow_from` lets only `me` and `claude-code` use this handler. `from` is
checked by the broker, so the list can be trusted.

## 3. Redeploy

**Terminal 3:** stop the old binary (`ctrl+c`), then:

```bash
cp $LAB/weather/dist/weather-$(go env GOOS)-$(go env GOARCH) ./weather
./weather -v
```

The running handlers drained before the old one stopped, so nothing in
flight was lost.

## 4. Try it

**Terminal 2:**

```bash
agentbus request --to weather --body '{"op":"current","location":"Seattle, WA"}'
AGENTBUS_AGENT=hello agentbus request --to weather --body '{"op":"current","location":"Seattle, WA"}'   # forbidden
```

The first, as `me`, gets the conditions. The second, as `hello`, is
refused by the agent:

```text
✓ response from weather in 600ms
{
  "conditions": "Partly cloudy",
  "location": "Seattle, Washington, US",
  "source": "Open-Meteo (open-meteo.com)",
  "temperature_f": 51.8,
  "time": "2026-11-16T12:00",
  "wind_mph": 4.6
}
✗ forbidden from weather after 250ms
  hello may not use inbox:current
```

Ask Claude too, in a new session so it reads the updated description:
*"What's it like in Seattle right now?"*

## 5. Request metrics

Every request the broker routes is tracked: open, answered, errored,
expired, and how long replies took.

```bash
AGENTBUS_AGENT=admin agentbus status
```

Terminal 2 runs as `me`, and only an observer may read the broker's
status, so this command names `admin`. Without it you get
`broker status needs an observer identity`.

The console's Overview tab has a `requests` row in the Decisions panel,
and a "Requests by target" table with latencies. As JSON:

```bash
AGENTBUS_AGENT=admin agentbus status --plain | python3 -c 'import json,sys; [print(r) for r in json.load(sys.stdin).get("requests", [])]'
```

```text
{'target': 'agent:weather', 'open': 0, 'answered': 3, 'errored': 2, 'expired': 0, 'evicted': 0, 'p50_seconds': 0.1, 'p95_seconds': 0.5}
{'target': 'task:weather.forecast', 'open': 0, 'answered': 4, 'errored': 3, 'expired': 0, 'evicted': 0, 'p50_seconds': 0.5, 'p95_seconds': 0.5}
...
```

For Prometheus, add a metrics listener and restart the broker. A
listener address is one of the few settings the broker can't apply live;
terminal 1 logs `restart_needed=[metrics_listen]` when you set it.

```bash
agentbus config set metrics_listen 127.0.0.1:9464
# terminal 1: ctrl+c, then agentbus serve again
curl -s 127.0.0.1:9464/metrics | grep agentbus_requests_total
curl -s 127.0.0.1:9464/healthz
```

The weather agent in terminal 3 reconnects on its own when the broker
comes back.

A capability whose `expired` count climbs has workers that are slow,
stuck or offline. That's the alert you'd want on the weather agent.

## 6. Optional: give it a model

Needs an Anthropic API key, or a local model server (see the note at the
end). It isn't part of `check.sh`.

Add a handler that answers questions in plain words, with the forecast
script as its tool. **Terminal 2:**

```bash
cat >> $LAB/weather/agent.yaml <<'EOF'
  - inbox: true
    op: ask
    timeout: 120s
    description: '{"op": "ask", "question": string}; replies {"answer": string}'
    llm:
      provider: anthropic
      model: claude-haiku-5-5
      system: |
        Answer weather questions. Look the forecast up with the forecast
        tool before you answer; never guess. Reply {"answer": "..."}.
      tools: [forecast_tool]
      reply: json
  - name: forecast_tool
    script: scripts/forecast.py
    description: The forecast for a place, from public weather services
    params:
      type: object
      required: [location]
      properties:
        location: {type: string, description: '"City, ST" or "City, Country"'}
        days: {type: integer, minimum: 1, maximum: 7}
EOF
agentbus agent build $LAB/weather
```

`forecast_tool` has no trigger. Only the model can run it, and it runs
the script you wrote in lab 3. The model reads the question, picks a
location and the number of days, and calls the script with them as the
body.

**Terminal 3:** stop the old binary, then start the new one with the key:

```bash
cp $LAB/weather/dist/weather-$(go env GOOS)-$(go env GOARCH) ./weather
ANTHROPIC_API_KEY=... ./weather -v
```

**Terminal 2:**

```bash
agentbus request --to weather --timeout 2m \
  --body '{"op": "ask", "question": "Do I need an umbrella in Seattle, WA tomorrow?"}'
```

Terminal 3 shows a `handled handler=forecast_tool` line for the tool call,
then a `model run` line with the turns and tokens it took.

For a local model instead, replace the `provider` and `model` lines with
`provider: openai`, `base_url: http://localhost:11434/v1` and
`model: qwen3:8b`. Run Ollama first, and leave out the key. See
[Model-backed handlers](../docs/AGENTS.md#model-backed-handlers) for the
rest, and [examples/llm-agent](../examples/llm-agent) for a bigger agent.

## More to try

- Add an event handler to the manifest: `event: a2a/v1/events/+/alerts/#`
  with `run: logger -t weather "alert from $AGENTBUS_FROM"`. Then send it
  something with `agentbus emit --name alerts/storm --body '{"level":"high"}'`.
- Run a second weather agent under another identity with the same
  `weather.forecast` capability, and send several tasks. The broker
  spreads them between the two.
- Swap `forecast.py` for a version that calls your own internal service.
  Nothing else changes.

## Clean up

[Lab 7](07-join.md) is optional and uses this broker. If you're doing
it, do it before you clean up.

Stop everything with `ctrl+c` in each terminal, then:

```bash
rm -rf $LAB
```

**Done when:** `current` answers you, refuses `hello`, and the broker
shows request counts for `task:weather.forecast`.
