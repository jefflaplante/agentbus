# Broker benchmarks

`make bench` measures how many messages the broker moves and how long
each one takes, with `data_dir` off and on. Use it before and after a
change to the broker's storage or delivery path, and run it on the disk
the broker will use in production. Results from one machine say little
about another, because fsync costs very different amounts on different
disks and operating systems.

## What it measures

Each case starts its own broker on loopback, with the rate limit raised
so it doesn't cap the result. Agents connect with persistent sessions,
the way long-running agents connect.

| Benchmark | What each operation is | Metrics |
|---|---|---|
| `BenchmarkInbox` | One sender sends one message to the `sink` agent's inbox | `msgs/s`; `ack` is the time until the sender has the broker's PUBACK; `delivery` is the time until the receiver's handler runs |
| `BenchmarkTask` | One sender sends a request to a task queue, one worker answers it, and the sender waits for the reply | `tasks/s`; `roundtrip` is the time until the reply arrives |

Every case runs with `data_dir=off` and `data_dir=on`, and with 1 and 8
sender agents. Latencies are reported as p50 and p99 in milliseconds.
`ns/op` is wall time divided by the number of messages, so it is the
inverse of `msgs/s`.

A task round trip stores two messages, the task and the reply, so with
`data_dir` on it costs about twice what an inbox message does.

## Running it

```bash
make bench
```

Settings:

| Variable | Default | Meaning |
|---|---|---|
| `BENCHTIME` | `300x` | Messages per case. More gives steadier percentiles. |
| `BENCH` | `.` | Which cases, as a `go test -bench` pattern, e.g. `Inbox/data_dir=on` |
| `TMPDIR` | system default | Where the broker's data directory is created, and so which disk is measured |

To compare two versions, save each run and compare them with
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat):

```bash
make bench BENCHTIME=300x | tee before.txt
```

```bash
make bench BENCHTIME=300x | tee after.txt
```

```bash
benchstat before.txt after.txt
```

Run each side a few times (`go test -count`) if the difference is small.

### On GitHub Actions

The Benchmarks workflow (`.github/workflows/bench.yml`) runs
`make bench` when started by hand from the Actions tab, and writes the
output to the run's summary. A runner's disk is virtualized and shared
with other jobs, so use these numbers only to compare runs with each
other.

### On a Kubernetes cluster

This measures the storage class the broker's volume actually uses.
[examples/kubernetes/bench.yaml](../examples/kubernetes/bench.yaml)
creates a volume and a pod that mounts it at `/data`. Set its
`storageClassName` to the broker's class first.

```bash
make bench-binary
```

```bash
kubectl apply -f examples/kubernetes/bench.yaml
```

```bash
kubectl wait --for=condition=Ready pod/agentbus-bench
```

```bash
kubectl cp bin/broker-bench.test agentbus-bench:/data/broker-bench.test
```

```bash
kubectl exec agentbus-bench -- env TMPDIR=/data /data/broker-bench.test -test.run '^$' -test.bench . -test.benchtime 300x
```

```bash
kubectl delete -f examples/kubernetes/bench.yaml
```

`make bench-binary` builds a Linux test binary for amd64. Use
`make bench-binary BENCH_GOARCH=arm64` for arm64 nodes. The same binary
runs on any Linux host: copy it over and run it with `TMPDIR` set to a
directory on the disk you want to measure.

## Reference results

From a MacBook Pro (Apple M3 Pro, internal SSD) on 2026-11-16, at
commit `e51e49b` with `BENCHTIME=300x`:

| Case | Throughput | p50 | p99 |
|---|---|---|---|
| Inbox, `data_dir` off, 1 sender | 14,375 msgs/s | 0.09 ms delivery | 0.30 ms delivery |
| Inbox, `data_dir` off, 8 senders | 28,873 msgs/s | 0.44 ms delivery | 1.04 ms delivery |
| Inbox, `data_dir` on, 1 sender | 28.6 msgs/s | 60 ms delivery | 70 ms delivery |
| Inbox, `data_dir` on, 8 senders | 32.8 msgs/s | 383 ms delivery | 483 ms delivery |
| Task, `data_dir` off, 1 sender | 5,537 tasks/s | 0.17 ms round trip | 0.40 ms round trip |
| Task, `data_dir` off, 8 senders | 10,991 tasks/s | 0.70 ms round trip | 1.14 ms round trip |
| Task, `data_dir` on, 1 sender | 15.4 tasks/s | 64 ms round trip | 78 ms round trip |
| Task, `data_dir` on, 8 senders | 13.5 tasks/s | 585 ms round trip | 709 ms round trip |

With `data_dir` on, the broker spends four bbolt write transactions on
each delivered message, each with its own fsync. On macOS Go's fsync
uses `F_FULLFSYNC`, which took about 8 ms per write on this machine, so
the broker tops out near 30 messages a second however many agents are
sending. More senders only make each one wait longer. Linux servers
usually fsync much faster, but that hasn't been measured yet.
[ADR-0001](adr/0001-broker-high-availability.md#what-a-replicated-write-costs)
explains where the four writes come from.
