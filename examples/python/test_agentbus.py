#!/usr/bin/env python3
"""Unit tests for agentbus.py that need no broker.

    python3 -m unittest test_agentbus     (from examples/python)
"""

import json
import logging
import threading
import time
import unittest
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

import agentbus
from agentbus import Agent, check_event_name, expired, inbox_topic, parse_ts, task_topic

# Tests provoke errors on purpose; keep their tracebacks out of the output.
logging.getLogger("agentbus").propagate = False
logging.getLogger("agentbus").addHandler(logging.NullHandler())


class ParseTs(unittest.TestCase):
    def test_forms(self):
        base = datetime(2026, 11, 15, 19, 44, 36, tzinfo=timezone.utc)
        cases = {
            "2026-11-15T19:44:36Z": base,
            "2026-11-15T19:44:36+00:00": base,
            "2026-11-15T12:44:36-07:00": base,
            "2026-11-16T01:14:36+05:30": base,
            "2026-11-15t19:44:36z": base,
            "2026-11-15T19:44:36.1Z": base.replace(microsecond=100000),
            "2026-11-15T19:44:36.12Z": base.replace(microsecond=120000),
            "2026-11-15T19:44:36.123Z": base.replace(microsecond=123000),
            "2026-11-15T19:44:36.1234Z": base.replace(microsecond=123400),
            "2026-11-15T19:44:36.12345Z": base.replace(microsecond=123450),
            "2026-11-15T19:44:36.123456Z": base.replace(microsecond=123456),
            "2026-11-15T19:44:36.1234567Z": base.replace(microsecond=123456),
            "2026-11-15T19:44:36.12345678Z": base.replace(microsecond=123456),
            "2026-11-15T19:44:36.123456789Z": base.replace(microsecond=123456),
            "2026-11-15T12:44:36.1674-07:00": base.replace(microsecond=167400),
            "2026-11-15T12:44:36.123456789-07:00": base.replace(microsecond=123456),
            "2026-11-15T21:44:36.5+02:00": base.replace(microsecond=500000),
        }
        for ts, want in cases.items():
            with self.subTest(ts=ts):
                got = parse_ts(ts)
                self.assertIsNotNone(got.tzinfo, "must be aware")
                self.assertEqual(got, want)

    def test_offset_kept(self):
        self.assertEqual(parse_ts("2026-11-15T12:44:36-07:00").utcoffset(), timedelta(hours=-7))

    def test_rejects(self):
        for ts in ("", "2026-11-15", "2026-11-15T19:44:36", "2026-11-15T19:44:36.Z",
                   "2026-11-15T19:44:36+0700", "2026-11-15T19:44:36+24:00", "yesterday", None, 12):
            with self.subTest(ts=ts), self.assertRaises(ValueError):
                parse_ts(ts)


class Expired(unittest.TestCase):
    @staticmethod
    def ts(delta: timedelta, tz: timezone, digits: int = 0) -> str:
        t = (datetime.now(timezone.utc) + delta).astimezone(tz)
        s = t.strftime("%Y-%m-%dT%H:%M:%S")
        if digits:
            s += "." + f"{t.microsecond:06d}000"[:digits]
        off = t.strftime("%z")
        return s + ("Z" if off == "+0000" else f"{off[:3]}:{off[3:]}")

    def test_cases(self):
        minus7, plus530 = timezone(timedelta(hours=-7)), timezone(timedelta(hours=5, minutes=30))
        for tz in (timezone.utc, minus7, plus530):
            for digits in (0, 3, 4, 9):
                with self.subTest(tz=tz, digits=digits):
                    fresh = {"ts": self.ts(timedelta(seconds=-1), tz, digits), "ttl_ms": 60000}
                    stale = {"ts": self.ts(timedelta(seconds=-120), tz, digits), "ttl_ms": 60000}
                    self.assertFalse(expired(fresh), fresh)
                    self.assertTrue(expired(stale), stale)

    def test_no_ttl_never_expires(self):
        self.assertFalse(expired({"ts": "2000-01-01T00:00:00Z"}))
        self.assertFalse(expired({"ts": "garbage", "ttl_ms": 0}))

    def test_bad_input_raises_value_error(self):
        for env in ({"ts": "garbage", "ttl_ms": 1000}, {"ttl_ms": 1000}, {"ts": "2026-11-15T19:44:36Z", "ttl_ms": "x"}):
            with self.subTest(env=env), self.assertRaises(ValueError):
                expired(env)


class EventNames(unittest.TestCase):
    def test_valid(self):
        for name in ("done", "jobs/finished", "a/b/c", "x" * 128):
            check_event_name(name)

    def test_invalid(self):
        for name in ("", "jobs/+", "jobs/#", "#", "a\x00b", "a//b", "/a", "a/", "a\nb", "x" * 129, "é" * 65, None):
            with self.subTest(name=name), self.assertRaises(ValueError):
                check_event_name(name)

    def test_emit_publishes_nothing_for_bad_name(self):
        agent, fake = make_agent()
        with self.assertRaises(ValueError):
            agent.emit("jobs/#", {})
        self.assertEqual(fake.published, [])

    def test_bad_targets(self):
        agent, fake = make_agent()
        for call in (lambda: agent.send("+", {}), lambda: agent.request("a/b", {}), lambda: agent.task("#", {})):
            with self.assertRaises(ValueError):
                call()
        self.assertEqual(fake.published, [])


class Seen(unittest.TestCase):
    def test_dedupe(self):
        agent, _ = make_agent()
        self.assertTrue(agent._first_seen("a/1"))
        self.assertFalse(agent._first_seen("a/1"))

    def test_expires_by_age(self):
        agent, _ = make_agent()
        agent._first_seen("a/1")
        agent._seen["a/1"] -= Agent.SEEN_TTL + 1  # pretend it's old
        self.assertTrue(agent._first_seen("a/2"))  # pruned on insert
        self.assertNotIn("a/1", agent._seen)
        self.assertTrue(agent._first_seen("a/1"))

    def test_hard_cap(self):
        agent, _ = make_agent()
        agent.SEEN_MAX = 100
        for i in range(1000):
            agent._first_seen(f"a/{i}")
        self.assertLessEqual(len(agent._seen), 100)
        self.assertIn("a/999", agent._seen)
        self.assertNotIn("a/0", agent._seen)


class Replies(unittest.TestCase):
    def deliver(self, agent, fake, topic, env):
        msg = SimpleNamespace(topic=topic, payload=json.dumps(env).encode(), mid=len(fake.acks) + 100, qos=1)
        agent._on_message(fake, None, msg)
        return msg.mid

    def test_unencodable_body_gets_handler_error(self):
        agent, fake = make_agent(tasks={"cap": lambda env: {"when": datetime.now(), "set": {1}}})
        mid = self.deliver(agent, fake, task_topic("cap"), request(capability="cap"))
        fake.wait_acked(mid)
        (topic, env), = fake.published
        self.assertEqual(topic, inbox_topic("asker"))
        self.assertEqual(env["type"], "error")
        self.assertEqual(env["body"], {"code": "handler_error", "reason": "reply body could not be encoded"})

    def test_nan_body_is_not_sent(self):
        agent, fake = make_agent(tasks={"cap": lambda env: {"x": float("nan")}})
        fake.wait_acked(self.deliver(agent, fake, task_topic("cap"), request(capability="cap")))
        self.assertEqual(fake.published[0][1]["body"]["code"], "handler_error")

    def test_good_body(self):
        agent, fake = make_agent(tasks={"cap": lambda env: {"ok": 1}})
        fake.wait_acked(self.deliver(agent, fake, task_topic("cap"), request(capability="cap")))
        self.assertEqual(fake.published[0][1]["type"], "response")

    def test_negative_offset_and_garbage_ts_are_acked(self):
        agent, fake = make_agent(inbox=lambda env: {"ok": 1})
        env = request(ts="2026-11-15T12:44:36.1674-07:00", ttl_ms=1000)  # long expired
        mid = self.deliver(agent, fake, inbox_topic("me"), env)
        self.assertIn(mid, fake.acks)
        mid = self.deliver(agent, fake, inbox_topic("me"), request(ts="not a time", ttl_ms=1000))
        self.assertIn(mid, fake.acks)
        self.assertEqual(fake.published, [])

    def test_unexpected_exception_is_logged_and_acked(self):
        agent, fake = make_agent()
        agent._handle_message = lambda msg, ack: 1 / 0
        with self.assertLogs("agentbus", "ERROR"):
            mid = self.deliver(agent, fake, inbox_topic("me"), request())
        self.assertIn(mid, fake.acks)

    def test_busy_does_not_wait_on_network_thread(self):
        release = threading.Event()
        agent, fake = make_agent(tasks={"cap": lambda env: release.wait(5) and {"ok": 1}}, max_concurrent=1)
        fake.puback = False  # nothing is ever acknowledged
        agent._mqtt._thread = threading.current_thread()  # this test plays paho's network thread
        self.deliver(agent, fake, task_topic("cap"), request(capability="cap"))
        start = time.monotonic()
        self.deliver(agent, fake, task_topic("cap"), request(capability="cap"))
        self.assertLess(time.monotonic() - start, 1)
        self.assertEqual(fake.published[-1][1]["body"]["code"], "busy")
        agent._mqtt._thread = None
        fake.puback = True
        release.set()

    def test_no_handler_reply(self):
        agent, fake = make_agent()
        agent._mqtt._thread = threading.current_thread()
        fake.puback = False
        self.deliver(agent, fake, inbox_topic("me"), request())
        self.assertEqual(fake.published[-1][1]["body"]["code"], "no_handler")

    def test_broker_error_without_inbox_is_logged(self):
        agent, fake = make_agent()
        err = {"v": 1, "id": "e1", "type": "error", "from": "$broker", "ts": agentbus.now_ts(),
               "correlation_id": "zzz", "body": {"code": "denied", "reason": "nope"}}
        with self.assertLogs("agentbus", "WARNING") as cm:
            self.deliver(agent, fake, inbox_topic("me"), err)
        self.assertIn("$broker", "".join(cm.output))

    def test_close_drains_and_refuses_new_work(self):
        release = threading.Event()
        agent, fake = make_agent(tasks={"cap": lambda env: release.wait(5) and {"ok": 1}})
        first = self.deliver(agent, fake, task_topic("cap"), request(capability="cap"))
        closer = threading.Thread(target=agent.close)
        closer.start()
        time.sleep(0.2)
        self.assertTrue(closer.is_alive(), "close() must wait for the running handler")
        self.assertFalse(fake.disconnected)
        self.deliver(agent, fake, task_topic("cap"), request(capability="cap"))
        self.assertEqual(fake.published[-1][1]["body"]["code"], "busy")
        release.set()
        closer.join(5)
        self.assertIn(first, fake.acks)
        self.assertEqual(fake.published[-1][1]["type"], "response")
        self.assertTrue(fake.disconnected)

    def test_close_times_out(self):
        agent, fake = make_agent(tasks={"cap": lambda env: time.sleep(2)})
        self.deliver(agent, fake, task_topic("cap"), request(capability="cap"))
        start = time.monotonic()
        with self.assertLogs("agentbus", "WARNING"):
            agent.close(timeout=0.3)
        self.assertLess(time.monotonic() - start, 1.5)
        self.assertTrue(fake.disconnected)


class Publish(unittest.TestCase):
    def test_offline_is_queued_not_error(self):
        agent, fake = make_agent()
        fake.rc = agentbus.mqtt.MQTT_ERR_NO_CONN
        agent.emit("jobs/x", {})  # no exception

    def test_timeout_raises(self):
        agent, fake = make_agent()
        fake.puback = False
        with self.assertRaises(TimeoutError):
            agent._publish("a2a/v1/events/me/x", agent._envelope("event", {}), timeout=0.2)


class Subscribe(unittest.TestCase):
    def test_refused_subscription_fails_connect(self):
        agent, fake = make_agent(tasks={"cap": lambda env: None})
        agent.SUBSCRIBE_RETRIES = 0
        agent._on_connect(fake, None, None, Code(False), None)
        mid, filters = fake.subscribed[-1]
        codes = [Code(f.startswith("$share/")) for f, _ in filters]
        with self.assertLogs("agentbus", "ERROR"):
            agent._on_subscribe(fake, None, mid, codes, None)
        self.assertTrue(agent._settled.is_set())
        self.assertFalse(agent._connected.is_set())
        self.assertIn("$share/cap/a2a/v1/tasks/cap", agent._connect_error)

    def test_retry_then_success(self):
        agent, fake = make_agent(tasks={"cap": lambda env: None})
        agent._on_connect(fake, None, None, Code(False), None)
        mid, filters = fake.subscribed[-1]
        agent._on_subscribe(fake, None, mid, [Code(f.startswith("$share/")) for f, _ in filters], None)
        self.assertFalse(agent._connected.is_set())
        deadline = time.monotonic() + 2
        while len(fake.subscribed) < 2 and time.monotonic() < deadline:
            time.sleep(0.01)
        mid, filters = fake.subscribed[-1]
        self.assertEqual([f for f, _ in filters], ["$share/cap/a2a/v1/tasks/cap"])
        agent._on_subscribe(fake, None, mid, [Code(False)], None)
        self.assertTrue(agent._connected.is_set())


class Agents(unittest.TestCase):
    def test_snapshot_is_a_copy(self):
        agent, fake = make_agent()
        card = {"v": 1, "agent": "x", "status": "online"}
        msg = SimpleNamespace(topic="a2a/v1/agents/x/presence", payload=json.dumps(card).encode(), mid=1, qos=1)
        agent._on_message(fake, None, msg)
        snap = agent.agents_snapshot()
        self.assertEqual(snap, {"x": card})
        snap.clear()
        self.assertIn("x", agent.agents)

    def test_empty_presence_removes_agent(self):
        agent, fake = make_agent()
        card = {"v": 1, "agent": "x", "status": "online"}
        agent._on_message(fake, None, SimpleNamespace(
            topic="a2a/v1/agents/x/presence", payload=json.dumps(card).encode(), mid=1, qos=1))
        with self.assertNoLogs("agentbus", "WARNING"):
            agent._on_message(fake, None, SimpleNamespace(topic="a2a/v1/agents/x/presence", payload=b"", mid=2, qos=1))
        self.assertEqual(agent.agents_snapshot(), {})
        self.assertIn(2, fake.acks)


class Port(unittest.TestCase):
    def test_tls_default_port(self):
        for url, want in (("ssl://h", 8883), ("mqtts://h", 8883), ("tls://h", 8883), ("tcp://h", 1883), ("ssl://h:9999", 9999)):
            agent = Agent("me", "t", url)
            calls = []
            agent._mqtt = SimpleNamespace(connect=lambda host, port, keepalive: calls.append(port), loop_start=lambda: None)
            agent._settled.set()
            agent._connected.set()
            agent.connect()
            self.assertEqual(calls, [want], url)


# Fakes


class Code:
    def __init__(self, failure: bool):
        self.is_failure = failure


class FakeInfo:
    def __init__(self, rc, published):
        self.rc, self._published = rc, published

    def wait_for_publish(self, timeout):
        if self.rc:
            raise RuntimeError("not connected")
        if not self._published:
            time.sleep(timeout)

    def is_published(self):
        return self._published


class FakeClient:
    """Stands in for both paho's client and the client passed to callbacks."""

    def __init__(self):
        self.published, self.acks, self.subscribed = [], [], []
        self.rc, self.puback, self.disconnected, self._thread = 0, True, False, None
        self._acked = threading.Condition()

    def publish(self, topic, payload, qos=0, retain=False):
        if not retain:
            self.published.append((topic, json.loads(payload)))
        return FakeInfo(self.rc, self.puback)

    def ack(self, mid, qos):
        with self._acked:
            self.acks.append(mid)
            self._acked.notify_all()

    def wait_acked(self, mid, timeout=5):
        with self._acked:
            assert self._acked.wait_for(lambda: mid in self.acks, timeout), f"mid {mid} never acked"

    def subscribe(self, filters):
        mid = len(self.subscribed) + 1
        self.subscribed.append((mid, filters))
        return 0, mid

    def disconnect(self):
        self.disconnected = True

    def loop_stop(self):
        pass


def make_agent(**kw):
    agent = Agent("me", "token", **kw)
    fake = FakeClient()
    agent._mqtt = fake
    return agent, fake


def request(**fields):
    env = {"v": 1, "id": agentbus.secrets.token_hex(8), "type": "request", "from": "asker", "ts": agentbus.now_ts(), "ttl_ms": 60000}
    env.update(fields)
    return env


if __name__ == "__main__":
    unittest.main()
