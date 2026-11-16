#!/usr/bin/env python3
"""A service agent in Python: the same agent as examples/service in Go.

It serves the wordcount task queue, answers {"op": "ping"} in its inbox,
logs notes sent to it, and watches other agents' jobs/... events.

    agentbus config agent add counter --capability wordcount
    AGENTBUS_AGENT=counter python3 worker.py
"""

import logging
import signal
import threading

from agentbus import Agent, ReplyError

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("worker")


def wordcount(env: dict) -> dict:
    """One task from the shared queue. The return value is the reply body."""
    body = env.get("body")
    if not isinstance(body, dict) or not isinstance(body.get("text"), str) or not body["text"]:
        # The caller sees this code and reason, so say what to change.
        raise ReplyError("invalid", 'body must be {"text": "..."}')
    return {"words": len(body["text"].split())}


def inbox(env: dict):
    """Everything sent to this agent directly. Only requests get a reply."""
    typ = env.get("type")
    if typ == "request":
        body = env.get("body") if isinstance(env.get("body"), dict) else {}
        if body.get("op") == "ping":
            return {"op": "pong"}
        raise ReplyError("invalid", f"unknown op {body.get('op')!r}; try {{\"op\": \"ping\"}}")
    if typ == "event":
        log.info("note from %s: event=%s priority=%s", env["from"], env.get("event"), env.get("priority"))
    elif typ == "error":
        log.warning("error envelope from %s: %s", env["from"], env.get("body"))
    return None  # ignored for anything but a request


def main() -> None:
    agent = Agent.from_env(
        "counter",
        description='wordcount: body {"text": string}, replies {"words": int}. Inbox: {"op": "ping"}.',
        tasks={"wordcount": wordcount},  # before connect, so queued tasks have a handler
        inbox=inbox,
        max_concurrent=4,
    )
    with agent.connect():
        agent.watch("a2a/v1/events/+/jobs/#", lambda e: log.info("job event from %s: %s", e["from"], e.get("event")))
        log.info("serving as %s", agent.id)
        stop = threading.Event()
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        stop.wait()


if __name__ == "__main__":
    main()
