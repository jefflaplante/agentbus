#!/usr/bin/env python3
"""Play one terminal scene inside an asciinema recording.

  runner.py SCENE.json STATE_DIR BEATLOG.json

SCENE.json is a storyboard scene with each beat's narration length added
(`dur`, seconds). The runner shows a prompt, types each command at a
human pace, runs it for real with the shared environment, and keeps each
beat on screen at least as long as its narration. It writes the time
each beat started (seconds since the runner began) to BEATLOG.json, so
the narration can be placed exactly over the recording.

STATE_DIR carries the environment, working directory and background
processes from one scene to the next.
"""
import json
import os
import random
import re
import shlex
import signal
import subprocess
import sys
import time

GREEN, BLUE, DIM, BOLD, RESET = "\033[32m", "\033[34m", "\033[2m", "\033[1m", "\033[0m"


def load(path, default):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return default


def save(path, value):
    with open(path, "w") as f:
        json.dump(value, f)


class Runner:
    def __init__(self, scene, state_dir):
        self.scene, self.state_dir = scene, state_dir
        st = load(os.path.join(state_dir, "state.json"), {})
        self.env = st.get("env") or dict(os.environ)
        self.cwd = st.get("cwd") or self.env["LAB"]
        self.bg = st.get("bg", {})  # name -> pid
        self.t0 = time.monotonic()
        self.marks = []

    def persist(self):
        save(os.path.join(self.state_dir, "state.json"), {"env": self.env, "cwd": self.cwd, "bg": self.bg})

    def now(self):
        return time.monotonic() - self.t0

    def prompt(self):
        lab = self.env["LAB"]
        here = os.path.relpath(self.cwd, lab)
        where = "agentbus-lab" if here == "." else "agentbus-lab/" + here
        sys.stdout.write(f"{GREEN}{where}{RESET} {BOLD}${RESET} ")
        sys.stdout.flush()

    def type(self, text, fast=False):
        lines = text.split("\n")
        per_char = 0.012 if fast or len(text) > 160 else 0.035
        for i, line in enumerate(lines):
            if i > 0:
                sys.stdout.write("\r\n" + (f"{DIM}>{RESET} " if not fast else ""))
            for ch in line:
                sys.stdout.write(ch)
                sys.stdout.flush()
                time.sleep(per_char + random.uniform(0, per_char * 0.6))
        time.sleep(0.35)
        sys.stdout.write("\r\n")
        sys.stdout.flush()

    def shell(self, cmd, quiet=False):
        """Run cmd with bash, applying `export` and `cd` to the shared state."""
        m = re.fullmatch(r"\s*export\s+([A-Za-z_][A-Za-z0-9_]*)=(.*)", cmd)
        if m:
            value = subprocess.run(["bash", "-c", f"printf %s {m.group(2)}"], env=self.env, cwd=self.cwd,
                                   capture_output=True, text=True).stdout
            self.env[m.group(1)] = value
            return
        m = re.fullmatch(r"\s*cd\s+(\S+)\s*", cmd)
        if m:
            self.cwd = os.path.normpath(os.path.join(self.cwd, os.path.expandvars(m.group(1))))
            return
        out = subprocess.DEVNULL if quiet else None
        subprocess.run(["bash", "-c", cmd], env=self.env, cwd=self.cwd, stdout=out, stderr=out)

    def start_bg(self, name, cmd):
        # Its own session, so stop_bg can signal the whole process group.
        p = subprocess.Popen(["bash", "-c", cmd], env=self.env, cwd=self.cwd,
                             stdin=subprocess.DEVNULL, start_new_session=True)
        self.bg[name] = p.pid

    def stop_bg(self, name):
        pid = self.bg.pop(name, None)
        if pid:
            try:
                os.killpg(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            for _ in range(100):  # wait for the agent to drain and exit
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    break
                time.sleep(0.1)

    def beat(self, b):
        start = self.now()
        self.marks.append(start)
        dur = b.get("dur", 0.0)
        if b.get("hide"):
            self.shell(b["hide"], quiet=True)
        action = next((k for k in ("run", "bg", "stop") if k in b), None)
        if action:
            time.sleep(0.5)
            self.prompt()
            time.sleep(0.4)
            shown = b.get("show")
            if action == "run":
                self.type(shown or b["run"], fast=b.get("fast", False))
                self.shell(b["run"])
            elif action == "bg":
                self.type(shown or b["bg"][1] + " &")
                self.start_bg(*b["bg"])
            else:
                self.type(shown or "kill " + b["stop"])
                self.stop_bg(b["stop"])
            if b.get("hide_after"):
                self.shell(b["hide_after"], quiet=True)
            time.sleep(b.get("hold", 0.8))
        # Stay on this beat until its narration has finished.
        left = start + dur + 0.4 - self.now()
        if left > 0:
            time.sleep(left)

    def play(self):
        sys.stdout.write("\033[2J\033[H")
        title = self.scene.get("title", "")
        if title:
            sys.stdout.write(f"{BLUE}{BOLD}# {title}{RESET}\r\n\r\n")
        sys.stdout.flush()
        time.sleep(0.8)
        for b in self.scene["beats"]:
            self.beat(b)
        self.prompt()
        time.sleep(1.5)
        self.persist()
        return self.marks


def main():
    scene_path, state_dir, log_path = sys.argv[1:4]
    with open(scene_path) as f:
        scene = json.load(f)
    r = Runner(scene, state_dir)
    marks = r.play()
    save(log_path, {"marks": marks, "end": r.now()})


if __name__ == "__main__":
    main()
