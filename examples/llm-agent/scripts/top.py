#!/usr/bin/env python3
"""The processes using the most CPU or memory, for hostdoc's model.

The body is the model's tool input, which it built from another agent's
message: check it like any other untrusted input. Only "cpu" or "mem" and
a small integer get through, and nothing reaches a shell.
"""
import json
import subprocess
import sys

env = json.load(sys.stdin)
body = env.get("body") or {}
if not isinstance(body, dict):
    body = {}

sort = body.get("sort", "cpu")
limit = body.get("limit", 5)
if sort not in ("cpu", "mem") or not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 20:
    json.dump({"code": "invalid", "reason": 'want {"sort"?: "cpu"|"mem", "limit"?: 1-20}'}, sys.stdout)
    sys.exit(3)

# -A and -o work the same on Linux and macOS; sort in Python, since
# --sort is GNU-only.
out = subprocess.run(["ps", "-Ao", "pid=,pcpu=,pmem=,comm="], capture_output=True, text=True, check=True).stdout
rows = []
for line in out.splitlines():
    parts = line.split(None, 3)
    if len(parts) == 4:
        pid, cpu, mem, cmd = parts
        rows.append({"pid": int(pid), "cpu": float(cpu), "mem": float(mem), "command": cmd.rsplit("/", 1)[-1]})
rows.sort(key=lambda r: r[sort], reverse=True)
json.dump({"by": sort, "processes": rows[:limit]}, sys.stdout)
