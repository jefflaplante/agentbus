#!/usr/bin/env python3
"""Build the agentbus lab video: narration, terminal recordings, diagrams.

  make_video.py all        every stage below, in order
  make_video.py tts        narrate every beat (Kokoro, offline)      -> build/audio/
  make_video.py record     record the terminal scenes for real        -> build/casts/
  make_video.py render     one MP4 per scene                          -> build/scenes/
  make_video.py assemble   join the scenes, add chapters              -> out/agentbus-lab.mp4
  make_video.py script     write SCRIPT.md from the storyboard

Needs: asciinema, agg, ffmpeg and rsvg-convert on PATH; a Python with
kokoro-onnx and soundfile (TTS_PYTHON), and the Kokoro model files
(KOKORO_DIR, holding kokoro-v1.0.onnx and voices-v1.0.bin); Go; and
Claude Code, signed in, for the last terminal scene. The weather agent
calls api.weather.gov and open-meteo.com, so it needs the network.

The recording runs a real broker and agents in /tmp/agentbus-lab, and
reaches this checkout through the short path /tmp/agentbus-src, so
nothing machine-specific (like your home directory) appears on screen.
"""
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import wave

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
BUILD = os.path.join(HERE, "build")
OUT = os.path.join(HERE, "out")
LAB = "/tmp/agentbus-lab"
SRC_LINK = "/tmp/agentbus-src"
VOICE = os.environ.get("VOICE", "af_heart")
SPEED = float(os.environ.get("SPEED", "1.0"))
W, H, FPS = 1920, 1080, 30
COLS, ROWS = 100, 30

sys.path.insert(0, HERE)
import storyboard  # noqa: E402


def sh(*args, **kw):
    print("+", " ".join(str(a) for a in args)[:200], flush=True)
    subprocess.run([str(a) for a in args], check=True, **kw)


def beats(scene):
    """Normalize a scene's beats to dicts with a "say" key."""
    return [b if isinstance(b, dict) else {"say": b} for b in scene["beats"]]


def spoken(text):
    """The narration as the voice should say it (see storyboard.PRONOUNCE)."""
    for pattern, say in storyboard.PRONOUNCE:
        text = re.sub(r"\b" + pattern + r"\b", say, text, flags=re.IGNORECASE)
    return text


def audio_path(scene_id, i):
    return os.path.join(BUILD, "audio", f"{scene_id}-{i:02d}.wav")


def wav_seconds(path):
    with wave.open(path) as w:
        return w.getnframes() / w.getframerate()


# ---- tts -------------------------------------------------------------------

def tts():
    os.makedirs(os.path.join(BUILD, "audio"), exist_ok=True)
    jobs = []
    for s in storyboard.SCENES:
        for i, b in enumerate(beats(s)):
            if b["say"]:
                jobs.append((audio_path(s["id"], i), spoken(b["say"])))
    todo = []
    for path, text in jobs:
        key = hashlib.sha256(f"{VOICE}|{SPEED}|{text}".encode()).hexdigest()
        stamp = path + ".key"
        if os.path.exists(path) and os.path.exists(stamp) and open(stamp).read() == key:
            continue
        todo.append({"path": path, "text": text, "key": key})
    if todo:
        kdir = os.environ.get("KOKORO_DIR", "")
        py = os.environ.get("TTS_PYTHON", "python3")
        code = r"""
import json, sys
import soundfile as sf
from kokoro_onnx import Kokoro
kdir, voice, speed = sys.argv[1], sys.argv[2], float(sys.argv[3])
k = Kokoro(f"{kdir}/kokoro-v1.0.onnx", f"{kdir}/voices-v1.0.bin")
for job in json.load(sys.stdin):
    samples, sr = k.create(job["text"], voice=voice, speed=speed, lang="en-us")
    sf.write(job["path"], samples, sr)
    open(job["path"] + ".key", "w").write(job["key"])
    print("narrated", job["path"].rsplit("/", 1)[-1], round(len(samples) / sr, 1), "s", flush=True)
"""
        subprocess.run([py, "-c", code, kdir, VOICE, str(SPEED)], input=json.dumps(todo), text=True, check=True)
    durs = {}
    for s in storyboard.SCENES:
        for i, b in enumerate(beats(s)):
            p = audio_path(s["id"], i)
            durs[f"{s['id']}-{i:02d}"] = wav_seconds(p) if b["say"] else 0.0
    json.dump(durs, open(os.path.join(BUILD, "durations.json"), "w"), indent=1)
    total = sum(durs.values())
    print(f"narration: {len(jobs)} beats, {total / 60:.1f} minutes")


# ---- record ----------------------------------------------------------------

def record():
    durs = json.load(open(os.path.join(BUILD, "durations.json")))
    casts = os.path.join(BUILD, "casts")
    state = os.path.join(BUILD, "state")
    os.makedirs(casts, exist_ok=True)
    stop_background(state)
    shutil.rmtree(state, ignore_errors=True)
    os.makedirs(state)
    shutil.rmtree(LAB, ignore_errors=True)
    os.makedirs(LAB)
    if os.path.islink(SRC_LINK) or os.path.exists(SRC_LINK):
        os.remove(SRC_LINK)
    os.symlink(REPO, SRC_LINK)
    sh("go", "build", "-o", os.path.join(REPO, "bin", "agentbus"), "./cmd/agentbus", cwd=REPO,
       env={**os.environ, "GOWORK": "off"})

    env = {k: v for k, v in os.environ.items() if not k.startswith("AGENTBUS_")}
    env.update({"LAB": LAB, "REPO": SRC_LINK, "TERM": "xterm-256color", "COLORTERM": "truecolor",
                "PATH": os.path.join(SRC_LINK, "bin") + os.pathsep + env["PATH"], "GOWORK": "off"})
    json.dump({"env": env, "cwd": LAB, "bg": {}}, open(os.path.join(state, "state.json"), "w"))
    try:
        for s in storyboard.SCENES:
            if s["kind"] != "terminal":
                continue
            scene = dict(s)
            scene["beats"] = [dict(b, dur=durs[f"{s['id']}-{i:02d}"]) for i, b in enumerate(beats(s))]
            spath = os.path.join(state, s["id"] + ".json")
            json.dump(scene, open(spath, "w"))
            log = os.path.join(casts, s["id"] + ".beats.json")
            cmd = f"python3 {os.path.join(HERE, 'runner.py')} {spath} {state} {log}"
            sh("asciinema", "rec", "--headless", "--overwrite", "--quiet", "-f", "asciicast-v2",
               "--window-size", f"{COLS}x{ROWS}", "-c", cmd, os.path.join(casts, s["id"] + ".cast"), env=env)
    finally:
        stop_background(state)


def stop_background(state):
    p = os.path.join(state, "state.json")
    if not os.path.exists(p):
        return
    st = json.load(open(p))
    for name, pid in st.get("bg", {}).items():
        try:
            os.killpg(pid, 15)
            print("stopped", name)
        except (ProcessLookupError, PermissionError):
            pass
    st["bg"] = {}
    json.dump(st, open(p, "w"))


# ---- render ----------------------------------------------------------------

LEAD, GAP, TAIL = 0.8, 0.5, 1.2  # diagram scenes: silence before, between and after beats


def render():
    durs = json.load(open(os.path.join(BUILD, "durations.json")))
    os.makedirs(os.path.join(BUILD, "scenes"), exist_ok=True)
    os.makedirs(os.path.join(BUILD, "frames"), exist_ok=True)
    for s in storyboard.SCENES:
        if s["kind"] == "diagram":
            render_diagram(s, durs)
        else:
            render_terminal(s, durs)


def render_diagram(s, durs):
    png = os.path.join(BUILD, "frames", s["id"] + ".png")
    sh("rsvg-convert", "-w", W * 2, "-h", H * 2, "-o", png, os.path.join(HERE, "diagrams", s["svg"]))
    clips, t = [], LEAD
    for i, b in enumerate(beats(s)):
        d = durs[f"{s['id']}-{i:02d}"]
        clips.append((audio_path(s["id"], i), t))
        t += d + GAP
    total = t - GAP + TAIL
    audio = mix_narration(s["id"], clips, total)
    frames = int(total * FPS)
    vf = (f"zoompan=z='1+0.04*on/{frames}':x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'"
          f":d=1:s={W}x{H}:fps={FPS},"
          f"fade=t=in:st=0:d=0.5,fade=t=out:st={total - 0.5:.2f}:d=0.5,format=yuv420p")
    encode(s["id"], ["-loop", "1", "-framerate", FPS, "-i", png], vf, audio, total)


def render_terminal(s, durs):
    casts = os.path.join(BUILD, "casts")
    log = json.load(open(os.path.join(casts, s["id"] + ".beats.json")))
    bs = beats(s)
    narr = [(m, m + durs[f"{s['id']}-{i:02d}"]) for i, m in enumerate(log["marks"]) if bs[i]["say"]]
    cast_in = os.path.join(casts, s["id"] + ".cast")
    cast_out = os.path.join(casts, s["id"] + ".compressed.cast")
    remap = compress_cast(cast_in, cast_out, narr)
    gif = os.path.join(BUILD, "frames", s["id"] + ".gif")
    sh("agg", "--font-family", "SF Mono,Menlo", "--font-size", 30, "--line-height", 1.3,
       "--theme", "github-dark", "--idle-time-limit", 3600, "--fps-cap", FPS, "--last-frame-duration", 0.1,
       cast_out, gif)
    bg = os.path.join(BUILD, "frames", s["id"] + "-bg.png")
    frame_svg = terminal_frame_svg(s.get("title", ""))
    subprocess.run(["rsvg-convert", "-w", str(W), "-h", str(H), "-o", bg], input=frame_svg.encode(), check=True)
    total = remap(log["end"]) + 0.6
    clips = [(audio_path(s["id"], i), remap(m)) for i, m in enumerate(log["marks"]) if bs[i]["say"]]
    audio = mix_narration(s["id"], clips, total)
    # The terminal sits in the window's body: x 80..1840, y 150..1020.
    vf = (f"[1:v]scale=1740:870:force_original_aspect_ratio=decrease,fps={FPS}[term];"
          f"[0:v][term]overlay=x=90+(1740-w)/2:y=150+(870-h)/2:eof_action=repeat,"
          f"fade=t=in:st=0:d=0.4,fade=t=out:st={total - 0.4:.2f}:d=0.4,format=yuv420p")
    encode(s["id"], ["-loop", "1", "-framerate", FPS, "-i", bg, "-ignore_loop", "1", "-i", gif], vf, audio,
           total, complex_filter=True)


def compress_cast(src, dst, narration, max_gap=1.2, keep=0.8, margin=0.3):
    """Shorten idle stretches that no narration covers (Claude thinking,
    a slow build) and return the old-time -> new-time mapping."""
    lines = open(src).read().splitlines()
    header, events = lines[0], [json.loads(x) for x in lines[1:] if x.strip()]
    covered = [(a - margin, b + margin) for a, b in narration]
    removals = []
    times = [0.0] + [e[0] for e in events]
    for a, b in zip(times, times[1:]):
        if b - a <= max_gap:
            continue
        free = [(a, b)]
        for ca, cb in covered:
            nxt = []
            for fa, fb in free:
                if cb <= fa or ca >= fb:
                    nxt.append((fa, fb))
                    continue
                if ca > fa:
                    nxt.append((fa, ca))
                if cb < fb:
                    nxt.append((cb, fb))
            free = nxt
        for fa, fb in free:
            if fb - fa > keep:
                removals.append((fa + keep / 2, fb - keep / 2))

    def remap(t):
        return t - sum(max(0.0, min(t, rb) - ra) for ra, rb in removals)

    with open(dst, "w") as f:
        f.write(header + "\n")
        for e in events:
            f.write(json.dumps([round(remap(e[0]), 6)] + e[1:]) + "\n")
    cut = sum(rb - ra for ra, rb in removals)
    if cut > 0.5:
        print(f"  trimmed {cut:.1f}s of unnarrated waiting")
    return remap


def mix_narration(scene_id, clips, total):
    """One WAV the length of the scene, with each clip at its offset."""
    out = os.path.join(BUILD, "scenes", scene_id + ".wav")
    args = ["ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-t", f"{total:.3f}",
            "-i", "anullsrc=r=48000:cl=stereo"]
    filt, labels = [], ["[0:a]"]
    for n, (path, at) in enumerate(clips, start=1):
        args += ["-i", path]
        ms = max(0, int(at * 1000))
        filt.append(f"[{n}:a]aresample=48000,aformat=channel_layouts=stereo,adelay={ms}|{ms}[c{n}]")
        labels.append(f"[c{n}]")
    filt.append("".join(labels) + f"amix=inputs={len(labels)}:normalize=0:duration=first[a]")
    args += ["-filter_complex", ";".join(filt), "-map", "[a]", out]
    subprocess.run(args, check=True)
    return out


def encode(scene_id, inputs, vf, audio, total, complex_filter=False):
    out = os.path.join(BUILD, "scenes", scene_id + ".mp4")
    n_video = sum(1 for x in inputs if x == "-i")
    args = ["ffmpeg", "-y", "-loglevel", "error"] + [str(x) for x in inputs] + ["-i", audio]
    if complex_filter:
        args += ["-filter_complex", vf + "[v]", "-map", "[v]"]
    else:
        args += ["-vf", vf, "-map", "0:v"]
    args += ["-map", f"{n_video}:a", "-t", f"{total:.3f}", "-r", str(FPS),
             "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p",
             "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-movflags", "+faststart", out]
    print(f"+ encode {scene_id} ({total:.1f}s)", flush=True)
    subprocess.run(args, check=True)


def terminal_frame_svg(title):
    esc = title.replace("&", "&amp;").replace("<", "&lt;")
    return f"""<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}">
  <defs><linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
    <stop offset="0" stop-color="#0b1220"/><stop offset="1" stop-color="#141b2d"/></linearGradient></defs>
  <rect width="{W}" height="{H}" fill="url(#bg)"/>
  <rect x="70" y="70" width="1780" height="960" rx="18" fill="#171b21" stroke="#30363d" stroke-width="2"/>
  <path d="M70 88 a18 18 0 0 1 18 -18 h1744 a18 18 0 0 1 18 18 v52 h-1780 z" fill="#161b22"/>
  <circle cx="110" cy="106" r="10" fill="#ff5f57"/><circle cx="142" cy="106" r="10" fill="#febc2e"/>
  <circle cx="174" cy="106" r="10" fill="#28c840"/>
  <text x="960" y="115" text-anchor="middle" font-family="Helvetica Neue, Helvetica, Arial" font-size="28"
        fill="#c9d1d9">{esc}</text>
</svg>"""


# ---- assemble --------------------------------------------------------------

def assemble():
    os.makedirs(OUT, exist_ok=True)
    scenes = [os.path.join(BUILD, "scenes", s["id"] + ".mp4") for s in storyboard.SCENES]
    lst = os.path.join(BUILD, "concat.txt")
    with open(lst, "w") as f:
        for p in scenes:
            f.write(f"file '{p}'\n")
    meta = os.path.join(BUILD, "chapters.txt")
    t = 0.0
    with open(meta, "w") as f:
        f.write(";FFMETADATA1\ntitle=" + storyboard.TITLE + "\n")
        for s, p in zip(storyboard.SCENES, scenes):
            d = float(subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration",
                                      "-of", "csv=p=0", p], capture_output=True, text=True, check=True).stdout)
            name = s.get("title") or chapter_name(s)
            f.write(f"[CHAPTER]\nTIMEBASE=1/1000\nSTART={int(t * 1000)}\nEND={int((t + d) * 1000)}\ntitle={name}\n")
            t += d
    out = os.path.join(OUT, "agentbus-lab.mp4")
    # Join, then bring the narration to -16 LUFS (typical for online video);
    # the video stream is copied untouched.
    sh("ffmpeg", "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", lst, "-i", meta,
       "-map_metadata", "1", "-map_chapters", "1", "-map", "0:v", "-map", "0:a", "-c:v", "copy",
       "-af", "loudnorm=I=-16:TP=-1.5:LRA=11", "-c:a", "aac", "-b:a", "160k", "-ar", "48000",
       "-movflags", "+faststart", out)
    sh("ffmpeg", "-y", "-loglevel", "error", "-ss", "4", "-i", os.path.join(BUILD, "scenes", "01-title.mp4"),
       "-frames:v", "1", os.path.join(OUT, "poster.png"))
    print(f"wrote {out} ({t / 60:.1f} minutes)")


def chapter_name(s):
    return {"01-title": "Introduction", "02-why": "Why a bus", "03-map": "What we'll build",
            "05-anatomy": "Anatomy of an agent", "07-deploy": "One binary, no token inside",
            "09-mcp": "Claude Code on the bus", "11-recap": "Recap"}.get(s["id"], s["id"])


# ---- script ----------------------------------------------------------------

def script():
    lines = [f"# {storyboard.TITLE}: video script", "",
             "The narration and on-screen commands of the lab video, generated from",
             "`storyboard.py` by `make_video.py script`. Edit the storyboard, not this file.", ""]
    for n, s in enumerate(storyboard.SCENES, start=1):
        name = s.get("title") or chapter_name(s)
        kind = "diagram: " + s["svg"] if s["kind"] == "diagram" else "terminal"
        lines += [f"## {n}. {name}", "", f"*{kind}*", ""]
        for b in beats(s):
            if b["say"]:
                lines += [f"> {b['say']}", ""]
            cmd = b.get("show") or b.get("run") or (b.get("bg", [None, None])[1] and b["bg"][1] + " &") or \
                ("kill " + b["stop"] if b.get("stop") else None)
            if cmd:
                lines += ["```bash", cmd, "```", ""]
    open(os.path.join(HERE, "SCRIPT.md"), "w").write("\n".join(lines))
    print("wrote SCRIPT.md")


STAGES = {"tts": tts, "record": record, "render": render, "assemble": assemble, "script": script}

if __name__ == "__main__":
    stage = sys.argv[1] if len(sys.argv) > 1 else "all"
    if stage == "all":
        for name in ("script", "tts", "record", "render", "assemble"):
            STAGES[name]()
    else:
        STAGES[stage]()
