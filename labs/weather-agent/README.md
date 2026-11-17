# weather agent (lab solution)

The finished agent from the [agentbus labs](../README.md). It answers
forecast requests from the National Weather Service (US) or Open-Meteo
(everywhere else), and current conditions from Open-Meteo.

| Handler | Trigger | Action |
|---|---|---|
| forecast | task `weather.forecast`, and inbox op `forecast` | `scripts/forecast.py` (Python, standard library only) |
| current | inbox op `current` | `handlers/current.go` (Go, compiled in) |
| ping | inbox op `ping` | a shell command |

Run these from this directory, with a broker running (`agentbus serve`):

```bash
agentbus agent build .                       # dist/weather-OS-ARCH
agentbus config agent add weather --capability weather.forecast
AGENTBUS_URL=tcp://127.0.0.1:1883 AGENTBUS_TOKEN_FILE=~/.config/agentbus/tokens/weather.token \
  dist/weather-$(go env GOOS)-$(go env GOARCH) -v
agentbus task -c weather.forecast --body '{"location":"Seattle, WA","days":1}'   # in another terminal
```

The token path is the default config directory's. With `AGENTBUS_CONFIG`
set, as in the labs, the token is in `tokens/` beside that config.

Unlike lab 6, `current` has no `allow_from` here, so any agent may call
it.

`go.mod` builds against this checkout (`replace ../..`). Outside the
repository, delete the replace line and `go mod tidy`.
