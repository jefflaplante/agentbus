# The lab video

A narrated walkthrough of the labs, about six and a half minutes long:
- **Diagrams** explain each idea.
- **Terminal scenes** are real recordings of the commands, against a real
  broker and agents.
- **The voice-over** is generated from [storyboard.py](storyboard.py).

The narration and on-screen commands are in [SCRIPT.md](SCRIPT.md).

The video itself isn't checked in; build it with `make_video.py`. Nothing
in the terminal scenes is staged:
- The broker, the weather agent and the forecast are live.
- The two Claude Code answers are whatever Claude says on the day.

## Files

| File | What it is |
|---|---|
| `storyboard.py` | Every scene: narration, diagram, or the commands typed. Edit this, then rebuild. |
| `make_video.py` | The build pipeline, in stages |
| `runner.py` | Plays one terminal scene inside an asciinema recording: types each command, runs it, keeps pace with the narration |
| `diagrams/*.svg` | The diagrams, 1920×1080 |
| `SCRIPT.md` | The narration and commands as a document, generated from the storyboard |

## Building it

You need:
- `asciinema`, `agg`, `ffmpeg` and `rsvg-convert` (`brew install asciinema agg ffmpeg librsvg`)
- Go
- Claude Code, signed in
- network access for the weather APIs
- the [Kokoro](https://github.com/thewh1teagle/kokoro-onnx) text-to-speech model, which runs offline:

```bash
uv venv tts && uv pip install --python tts/bin/python kokoro-onnx soundfile
curl -LO https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx
curl -LO https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin
```

Then:

```bash
export TTS_PYTHON=$PWD/tts/bin/python KOKORO_DIR=$PWD   # where the model files are
python3 make_video.py all       # or one stage: script, tts, record, render, assemble
```

The output goes to `out/agentbus-lab.mp4`, with chapters, plus
`out/poster.png`. The `.cast` files in `build/casts/` play in any
asciinema player.

## How the pieces line up

1. **`tts`** narrates each beat to a WAV and records its length. It
   regenerates only the beats whose text changed. Change the voice with
   `VOICE` (default `af_heart`) and the pace with `SPEED`.
2. **`record`** runs each terminal scene in `asciinema rec --headless`,
   against a fresh workspace in `/tmp/agentbus-lab`.
   - The runner keeps each beat on screen for at least as long as its
     narration, and logs when each beat starts.
   - Scenes share the workspace and background processes, so the broker
     from one scene is still up in the next.
   - The checkout is reached as `/tmp/agentbus-src`, so no home directory
     or username appears on screen.
3. **`render`** builds each scene:
   - **Diagrams:** rendered at 2× with a slow zoom.
   - **Terminal scenes:** waits longer than about a second that no
     narration covers are trimmed (Claude thinking, a long build). Each
     narration clip is placed at its beat's start time on the trimmed
     timeline. The recording is drawn into a window frame.
4. **`assemble`** joins the scenes with a chapter for each, and saves a
   poster frame.

Claude's answers aren't scripted. The narration for the first, unguided
question assumes Claude doesn't use the bus. If, on some run, it does,
record again (`make_video.py record`) or adjust that beat.
