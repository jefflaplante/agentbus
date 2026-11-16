# hostdoc: a model-backed agent

`hostdoc` answers questions about its host in plain language: "is the disk
filling up?", "why is this box slow?". Its one bus-facing handler is an
`llm:` handler. The model reads the question, calls the agent's own
handlers as tools to check, and replies with JSON:

```json
{"answer": "/ is 3% used, with 498 GiB free. Nothing to worry about.",
 "evidence": ["/dev/disk3s1s1  926Gi  12Gi  498Gi  3%  /"]}
```

| Handler | Trigger | Action |
|---|---|---|
| `hostdoc.ask` | task `hostdoc.ask` | `llm:`: Claude Haiku 5.5, with `disk`, `load` and `top` as tools, and the `weather.forecast` queue on the bus |
| `disk` | none (tool only) | `df -Ph` |
| `load` | none (tool only) | `uptime` |
| `top` | none (tool only) | `scripts/top.py`, which takes `{"sort": "cpu"\|"mem", "limit": 1-20}` |

The tools are ordinary handlers, the same kind that answer the bus in any
other agent. They have no trigger, so only the model can run them. The
model decides which to call and when. The handlers do the work, and they
check their input like any other handler: `top.py` refuses anything but
`cpu`, `mem` and a small number, and never builds a shell command.

## Run it

You need a broker (`agentbus serve`) and an Anthropic API key. On the
broker host, add the identity. `hostdoc` serves one capability, and may
hand work to the lab's weather agent if it's running:

```bash
agentbus config agent add hostdoc --capability hostdoc.ask
```

Then, from this directory, run it from source:

```bash
export ANTHROPIC_API_KEY=...          # or set api_key_file in agent.yaml
agentbus agent run -v                 # -v logs each model run and tool call
```

Ask it something, from another terminal:

```bash
agentbus task --capability hostdoc.ask --timeout 2m \
  --body '{"question": "Is any disk more than 80% full? What is using the most memory?"}'
```

With `-v`, the agent logs one line per tool call and one per model run:

```text
level=INFO msg=handled handler=disk from=cli id=87d9... ms=33 ok=true
level=INFO msg=handled handler=top from=cli id=5b9b... ms=41 ok=true
level=INFO msg="model run" handler=hostdoc.ask model=claude-haiku-5-5 turns=2 tokens_in=2637 tokens_out=242 ms=2392 ok=true
```

To see it use the bus, start the [lab weather agent](../../labs/weather-agent)
with an identity granted `weather.forecast`, and ask about the weather:

```bash
agentbus config agent add weather --capability weather.forecast
(cd ../../labs/weather-agent && agentbus agent run -v)   # in another terminal

agentbus task --capability hostdoc.ask --timeout 2m \
  --body '{"question": "Will it rain in Seattle, WA tomorrow?"}'
```

The model hands a `weather.forecast` task to the weather agent, which asks
the National Weather Service, and then answers from that forecast. From a
real run:

```json
{"answer": "No. Tomorrow (Tuesday) is forecast to be mostly sunny with a high near 50°F and no rain mentioned. The rain chances are today (30%) and Wednesday (30%).",
 "evidence": ["weather.forecast (NWS): \"Tuesday\",\"summary\":\"Mostly Sunny\",\"detail\":\"Mostly sunny. High near 50...\"",
              "\"Today\": \"Chance Rain Showers\", chance of precipitation is 30%",
              "\"Wednesday\": \"Chance Rain Showers\", chance of precipitation is 30%"]}
```

```text
weather: level=INFO msg=handled handler=weather.forecast from=hostdoc ms=1283 ok=true
hostdoc: level=INFO msg="model run" handler=hostdoc.ask turns=2 tokens_in=3490 tokens_out=776 ms=5672 ok=true
```

With no weather agent running, the model says that nobody serves
`weather.forecast`, instead of guessing.

## Run it on a local model

[agent.local.yaml](agent.local.yaml) is the same agent pointed at Ollama.
Any OpenAI-compatible server works the same way: vLLM, llama.cpp,
LM Studio or OpenRouter. No key is needed when `base_url` is set:

```bash
ollama pull qwen3:8b
agentbus agent run -f agent.local.yaml -v
```

Pick a model that supports tool calls. Small models follow the
"reply with JSON" instruction less reliably. When one doesn't, the
requester gets `bad_model_output` rather than a broken body.

## Build and deploy it

```bash
agentbus agent build . --target linux/amd64,linux/arm64   # dist/hostdoc-OS-ARCH
```

The binary holds `agent.yaml` and `scripts/`. It doesn't hold the agentbus
token or the model key. On each host:

```bash
install -m 0600 hostdoc.token /etc/agentbus/hostdoc.token
install -m 0600 model.key /etc/agentbus/hostdoc.model-key   # then set api_key_file to it, or
export ANTHROPIC_API_KEY=...                                 # the environment, e.g. systemd EnvironmentFile=
./hostdoc-linux-amd64 -check                                 # manifest, scripts, token and model key
./hostdoc-linux-amd64 -v
```

`-check` reports a missing model key the same way it reports a missing
token. Under systemd, `agentbus agent init`'s unit template shows both
ways to deliver the key: as a credential, with `api_key_file:
/run/credentials/hostdoc.service/model-key`, or as an `EnvironmentFile=`.
See [Deploying](../../docs/AGENTS.md#deploying). To run the local-model variant from a built binary, pass
`-manifest agent.local.yaml`. The scripts still come from the binary.

`go.mod` builds against this checkout (`replace ../..`). Outside the
repository, delete the replace line and `go mod tidy`.

## What it can't do

- **Choose its own model, prompt or tools from a message.** They're in
  the manifest. The question reaches the model as untrusted data.
- **Reach any agent it likes.** `bus:` lets the model see presence cards
  and hand work to `weather.forecast`, nothing else. The broker's
  grants apply on top.
- **Read the model key from a tool.** `ANTHROPIC_API_KEY` is removed from
  every command's environment, as the agentbus token is.
- **Run forever.** Six model calls per question (`max_turns`), 120 seconds
  (`timeout`), two bus hops (`max_hops`), and two questions at once
  (`max_concurrent`).

See [Model-backed handlers](../../docs/AGENTS.md#model-backed-handlers)
for every setting.
