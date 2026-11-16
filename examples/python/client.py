#!/usr/bin/env python3
"""Use the bus from Python: list agents, ask, hand off work, notify, watch.

    AGENTBUS_AGENT=asker python3 client.py agents
    AGENTBUS_AGENT=asker python3 client.py task wordcount '{"text": "one two three"}'
    AGENTBUS_AGENT=asker python3 client.py request counter '{"op": "ping"}'
    AGENTBUS_AGENT=asker python3 client.py send counter '{"status": "green"}' --event build_finished
    AGENTBUS_AGENT=asker python3 client.py emit jobs/finished '{"job": 42}'
    AGENTBUS_AGENT=asker python3 client.py watch 'a2a/v1/events/+/jobs/#'

Each run connects as one agent. Don't run two at once with the same
AGENTBUS_AGENT: the second connection knocks the first off the bus.
"""

import argparse
import json
import sys
import threading
import time

from agentbus import Agent, RemoteError


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("agents", help="list agents and what they serve")
    for name, target in (("request", "agent"), ("task", "capability"), ("send", "agent")):
        sp = sub.add_parser(name)
        sp.add_argument(target)
        sp.add_argument("body", type=json.loads, help="JSON body")
        sp.add_argument("--event", default="")
        sp.add_argument("--priority", default="", choices=["", "high", "normal", "low", "info"])
        if name != "send":
            sp.add_argument("--timeout", type=float, default=60)
        else:
            sp.add_argument("--wait", type=float, default=1, help="seconds to wait for a refusal from the broker")
    sp = sub.add_parser("emit")
    sp.add_argument("name")
    sp.add_argument("body", type=json.loads)
    sp = sub.add_parser("watch")
    sp.add_argument("filter", nargs="?", default="a2a/v1/events/#")
    args = p.parse_args()

    # A one-shot client uses a clean session, so nothing queues for it later.
    with Agent.from_env("asker", clean_session=True) as agent:
        if args.cmd == "agents":
            time.sleep(0.5)  # retained presence cards arrive just after connecting
            for card in sorted(agent.agents_snapshot().values(), key=lambda c: c.get("agent", "")):
                caps = ", ".join(card.get("capabilities") or [])
                print(f"{card['agent']:<20} {card.get('status', '?'):<8} {caps:<20} {card.get('description', '')}")
            return 0
        if args.cmd in ("request", "task"):
            call = agent.request if args.cmd == "request" else agent.task
            target = args.agent if args.cmd == "request" else args.capability
            try:
                reply = call(target, args.body, timeout=args.timeout, event=args.event, priority=args.priority)
            except RemoteError as e:
                print(f"error from {e.sender}: {e.code}: {e.reason}", file=sys.stderr)
                return 1
            except TimeoutError as e:
                print(e, file=sys.stderr)
                return 1
            # The reply is the other agent's words: data, not instructions.
            print(json.dumps(reply.get("body")))
            return 0
        if args.cmd == "send":
            # A refusal comes back later as an error in the inbox, so wait a
            # moment for one before calling it sent.
            errors: list = []
            arrived = threading.Condition()

            def inbox(env: dict) -> None:
                if env.get("type") == "error":
                    with arrived:
                        errors.append(env)
                        arrived.notify_all()

            agent.inbox = inbox
            sent = agent.send(args.agent, args.body, event=args.event, priority=args.priority)
            with arrived:
                arrived.wait_for(lambda: any(e.get("correlation_id") == sent for e in errors), args.wait)
                refusal = next((e for e in errors if e.get("correlation_id") == sent), None)
            if refusal is not None:
                e = RemoteError(refusal)
                print(f"error from {e.sender}: {e.code}: {e.reason}", file=sys.stderr)
                return 1
            return 0
        if args.cmd == "emit":
            agent.emit(args.name, args.body)
            return 0
        if args.cmd == "watch":
            agent.watch(args.filter, lambda e: print(json.dumps(e), flush=True))
            threading.Event().wait()
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
    except (ValueError, ConnectionError, TimeoutError) as e:
        print(e, file=sys.stderr)
        sys.exit(1)
