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
| `BenchmarkTask` | One sender sends a request to a task queue, a worker answers it, and the sender waits for the reply | `tasks/s`; `roundtrip` is the time until the reply arrives |

Every case runs with `data_dir=off` and `data_dir=on`, and with 1, 8 and
100 sender agents. `BenchmarkTask` has one worker on the queue, and with
100 senders it also runs with 4 and 16 workers. Latencies are reported
as p50 and p99 in milliseconds. `ns/op` is wall time divided by the
number of messages, so it is the inverse of `msgs/s`.

A task round trip stores two messages, the task and the reply, so with
`data_dir` on it costs about twice what an inbox message does.

The messages are split evenly between the senders. With 100 senders and
the default 300 messages each sender sends 3, so those cases measure a
short burst from many agents at once and say little about a rate held
for minutes.

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

Both machines were measured on 2026-11-17 with `BENCHTIME=600x`, one run
per case. Throughput moved by about 15% between runs of the same code on
the Linux host, so treat a smaller difference as noise.

- **Mac:** MacBook Pro, Apple M3 Pro, internal SSD.
- **Linux:** an unprivileged Proxmox LXC container (Ubuntu 24.10, kernel
  6.14.11-2-pve, Intel Xeon D-2123IT, limited to 2 CPUs, 2 GiB of
  memory), with the data directory on a ZFS dataset on an NVMe SSD.

### With `data_dir` on

| Case | Mac | p50 / p99 | Linux | p50 / p99 |
|---|---|---|---|---|
| Inbox, 1 sender | 79 msgs/s | 14 / 20 ms | 126 msgs/s | 7.6 / 17 ms |
| Inbox, 8 senders | 489 msgs/s | 16 / 22 ms | 753 msgs/s | 10 / 18 ms |
| Inbox, 100 senders | 5,176 msgs/s | 17 / 36 ms | 5,545 msgs/s | 19 / 35 ms |
| Task, 1 sender, 1 worker | 59 tasks/s | 16 / 22 ms | 95 tasks/s | 10 / 18 ms |
| Task, 8 senders, 1 worker | 68 tasks/s | 117 / 136 ms | 119 tasks/s | 68 / 87 ms |
| Task, 100 senders, 1 worker | 68 tasks/s | 1,447 / 1,573 ms | 108 tasks/s | 909 / 1,007 ms |
| Task, 100 senders, 4 workers | 226 tasks/s | 426 / 642 ms | 311 tasks/s | 273 / 481 ms |
| Task, 100 senders, 16 workers | 782 tasks/s | 92 / 213 ms | 1,032 tasks/s | 69 / 177 ms |

Latencies are delivery for Inbox and round trip for Task.

The disk sets these numbers. Every commit ends in an fsync, and with one
sender a message waited about 13 ms for the disk on the Mac (Go's fsync
uses `F_FULLFSYNC` on macOS) and about 8 ms on the Linux host. Three
things follow:

- **One connection moves one packet at a time through the disk.** After
  each PUBLISH the broker waits until what that packet wrote is on disk
  before it reads the same client's next packet. That is the 79 and 126
  msgs/s with one sender.
- **Connections share commits.** Everything queued while one fsync runs
  goes into the next commit, so throughput grows with the number of
  agents publishing: about 6 times from 1 sender to 8.
- **One worker limits a task queue.** Every reply is a PUBLISH from the
  worker, so one worker answers one task per wait for the disk however
  many senders there are. Throughput grew nearly in proportion to the number
  of workers: 68, 226 and 782 tasks/s on the Mac with 1, 4 and 16.

Connecting 100 agents to a broker with `data_dir` on took 5.1 seconds on
the Mac and 3.6 seconds on the Linux host. That was measured once with a
temporary test that isn't in the repository.

### With `data_dir` off

| Case | Mac | Linux |
|---|---|---|
| Inbox, 1 sender | 19,243 msgs/s | 6,959 msgs/s |
| Inbox, 8 senders | 27,710 msgs/s | 8,429 msgs/s |
| Inbox, 100 senders | 48,112 msgs/s | 8,096 msgs/s |
| Task, 1 sender, 1 worker | 5,392 tasks/s | 2,120 tasks/s |
| Task, 8 senders, 1 worker | 14,279 tasks/s | 3,599 tasks/s |
| Task, 100 senders, 1 worker | 20,082 tasks/s | 3,464 tasks/s |
| Task, 100 senders, 4 workers | 24,624 tasks/s | 3,461 tasks/s |
| Task, 100 senders, 16 workers | 21,845 tasks/s | 2,966 tasks/s |

These cases finish in well under a second at 600 messages, so they vary
a lot between runs. Without `data_dir` the limit is CPU, and the Linux
host has 2 cores.

## How the store got here

The broker first used mochi-mqtt's bbolt storage hook, which runs one
transaction, and so one fsync, for every write, and writes four times for
each delivered message. The broker's own store (`broker/store.go`)
replaced it in three steps. Each row is `data_dir` on at `BENCHTIME=600x`.

| Linux | One fsync per write | Group commit | No acknowledgement records | Wait once per packet |
|---|---|---|---|---|
| Inbox, 1 sender | 46 msgs/s | 60 | 118 | 126 |
| Inbox, 8 senders | 52 msgs/s | 334 | 635 | 753 |
| Inbox, 100 senders | 45 msgs/s | 566 | 575 | 5,545 |
| Task, 1 sender | 26 tasks/s | 39 | 66 | 95 |
| Task, 8 senders | 23 tasks/s | 51 | 99 | 119 |
| Task, 100 senders | 23 tasks/s | 57 | 105 | 108 |

| Mac | One fsync per write | Group commit | No acknowledgement records | Wait once per packet |
|---|---|---|---|---|
| Inbox, 1 sender | 29 msgs/s | 39 | 75 | 79 |
| Inbox, 8 senders | 33 msgs/s | 230 | 418 | 489 |
| Inbox, 100 senders | 28 msgs/s | 401 | 435 | 5,176 |
| Task, 1 sender | 16 tasks/s | 18 | 35 | 59 |
| Task, 8 senders | 14 tasks/s | 32 | 61 | 68 |
| Task, 100 senders | 13 tasks/s | 30 | 59 | 68 |

- **Group commit.** One goroutine commits everything that queued during
  the previous fsync in a single transaction.
- **No acknowledgement records.** Around every PUBACK it sends, mochi
  stores the PUBACK and deletes it again. Those records only matter for
  QoS 2, which the broker turns off, and they were two of the four
  writes.
- **Wait once per packet.** Writes are queued, and the broker waits for
  one commit after each PUBLISH or SUBSCRIBE has been processed. Before
  this it waited once per record. A message that goes to many agents
  writes one record per recipient, and every agent's presence card goes
  to every other agent, so connecting 100 agents took 93 seconds on the
  Mac and the broker spent about 90 more writing after they
  disconnected.

More goroutines in the broker would not help. bbolt runs one write
transaction at a time, and with `data_dir` on the process is mostly idle
waiting for the disk. A faster fsync would help: a ZFS pool with a
separate log device, or a disk with power-loss protection.

## What a crash loses

`TestCrashRecovery` (`broker/crash_test.go`) starts the broker as a
separate process. Eight senders publish to an agent that is offline, the
broker is killed with SIGKILL after 200-700 ms, and a new broker on the
same data directory delivers what it has. It does this six times and
fails if any sender loses more than one acknowledged message in one kill.

In three runs on each machine the store reopened after every kill and
the offline agent's session, subscription and queued messages were
there. Six kills lost 34-47 of the 941-2,438 messages the senders had
acknowledgements for, and no sender lost more than one per kill.

Nearly every sender loses one message per kill. mochi sends the PUBACK
before it stores the message, and the record reaches the disk about one
fsync later, so a kill under load usually lands between the two. This
was so before the store changed. A sender that must know its message
arrived needs a reply from the recipient.

`make test` skips this test because it takes about half a minute. Run
it with:

```bash
go test -run TestCrashRecovery -v ./broker
```

The test does not cover power loss. SIGKILL stops the process, and the
kernel keeps what the process had already written. It also checks inbox
messages only.

The test showed one more way to lose an acknowledged message. An agent's
queue holds `max_queued` messages (1,024 by default). When it is full
the broker drops a new message for that agent and still acknowledges it
to the sender.

## Run-ahead, tried and not kept

One more change was built, measured and removed: let each client have up
to 3 packets whose writes aren't on disk, and wait only when a fourth
would join them. The results below are `data_dir` on at
`BENCHTIME=600x`.

| Case | Mac, wait every packet | Mac, run-ahead 3 | Linux, wait every packet | Linux, run-ahead 3 |
|---|---|---|---|---|
| Inbox, 1 sender | 74 msgs/s | 231 | 144 | 442 |
| Inbox, 8 senders | 472 msgs/s | 1,569 | 830 | 3,245 |
| Task, 1 sender, 1 worker | 58 tasks/s | 219 | 107 | 446 |
| Task, 100 senders, 1 worker | 64 tasks/s | 226 | 124 | 377 |
| Task, 100 senders, 4 workers | 206 tasks/s | 817 | 309 | 1,305 |
| Task, 100 senders, 16 workers | 729 tasks/s | 2,628 | 950 | 1,840 |

It was about 3 to 4 times faster for one connection, and the gain
multiplied with the number of workers on the Mac. With 16 workers the
Linux host gained less, probably because it ran out of CPU; that was not
measured.

The crash test showed the cost. With run-ahead 3 a sender lost up to 4
acknowledged messages in one kill, where it loses 1 without, and six
kills lost 88-139 messages, where they lose 34-47 without. Throughput
and loss on a crash both went up about three times. It was left out
because adding workers gets the same throughput without losing more.

If it is built again, make it a config setting that is off by default,
keep the broker's own publishes waiting on every packet, and raise
`max_queued` in any test that fills an offline agent's queue. The first
crash-test run with run-ahead failed because the senders had become fast
enough to fill it.

## Results before the store changed

These were measured with mochi's storage hook, one fsync per write, at
`BENCHTIME=300x`. They are kept as the baseline that
[ADR-0001](adr/0001-broker-high-availability.md#what-a-replicated-write-costs)
refers to, and for the comparison of Podman with the host.

### macOS, Apple M3 Pro

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

With `data_dir` on, the broker spent four bbolt write transactions on
each delivered message, each with its own fsync. On macOS Go's fsync
uses `F_FULLFSYNC`, which took about 8 ms per write on this machine, so
the broker topped out near 30 messages a second however many agents were
sending. More senders only made each one wait longer.
[ADR-0001](adr/0001-broker-high-availability.md#what-a-replicated-write-costs)
explains where the four writes came from.

### Linux, ZFS on an NVMe SSD

From an unprivileged Proxmox LXC container (Ubuntu 24.10, kernel
6.14.11-2-pve, Intel Xeon D-2123IT, limited to 2 CPUs, 2 GiB of memory)
on 2026-11-17, at commit `aea6fa7` with `BENCHTIME=300x`, one run per
case. The broker's data directory was on a ZFS dataset on an NVMe SSD.

Each case ran twice: with the test binary directly on the host, and with
the same binary inside the `ghcr.io/jefflaplante/agentbus:v0.1.0` image
under rootful Podman 5.0.3, as the image's user, with the data directory
on a named volume on the same ZFS pool. Latencies are p50 / p99, delivery
for Inbox and round trip for Task.

| Case | On the host | p50 / p99 | In Podman | p50 / p99 |
|---|---|---|---|---|
| Inbox, `data_dir` off, 1 sender | 8,272 msgs/s | 0.16 / 0.48 ms | 3,704 msgs/s | 0.40 / 3.92 ms |
| Inbox, `data_dir` off, 8 senders | 10,517 msgs/s | 0.87 / 2.42 ms | 4,916 msgs/s | 3.61 / 13.35 ms |
| Inbox, `data_dir` on, 1 sender | 46.5 msgs/s | 37 / 56 ms | 44.5 msgs/s | 39 / 53 ms |
| Inbox, `data_dir` on, 8 senders | 52.8 msgs/s | 241 / 287 ms | 48.6 msgs/s | 262 / 333 ms |
| Task, `data_dir` off, 1 sender | 2,073 tasks/s | 0.44 / 1.19 ms | 2,133 tasks/s | 0.45 / 0.88 ms |
| Task, `data_dir` off, 8 senders | 4,526 tasks/s | 1.60 / 3.52 ms | 4,503 tasks/s | 1.61 / 3.28 ms |
| Task, `data_dir` on, 1 sender | 26.4 tasks/s | 38 / 53 ms | 24.7 tasks/s | 40 / 57 ms |
| Task, `data_dir` on, 8 senders | 22.7 tasks/s | 349 / 409 ms | 21.2 tasks/s | 371 / 447 ms |

With `data_dir` on, a stored message cost about 21 ms here against
about 35 ms on the MacBook, so this host moved about 1.6 times as many
messages. fsync on this ZFS pool was the limit, as `F_FULLFSYNC` is on
macOS.

Podman costs 4-8% with `data_dir` on. The two Inbox rows with `data_dir`
off look much worse in Podman, but that is noise: at 300 messages those
cases finish in under 100 ms. Rerun with `BENCHTIME=5000x`, three times
each, the `data_dir` off cases came out the same on the host and in
Podman:

| Case, `data_dir` off | On the host | In Podman |
|---|---|---|
| Inbox, 1 sender | 6,896-7,195 msgs/s | 7,179-7,548 msgs/s |
| Inbox, 8 senders | 7,902-8,778 msgs/s | 8,591-9,382 msgs/s |
| Task, 1 sender | 2,019-2,148 tasks/s | 2,164-2,257 tasks/s |
| Task, 8 senders | 3,782-3,943 tasks/s | 3,814-4,109 tasks/s |

The benchmark's broker listens on loopback inside the container, so
these numbers do not include Podman's port forwarding.

To run it in Podman, mount the test binary and a volume, and point
`TMPDIR` at the volume:

```bash
podman run --rm -v ./bin/broker-bench.test:/bench/broker-bench.test:ro -v agentbus-bench:/data -e TMPDIR=/data --entrypoint /bench/broker-bench.test ghcr.io/jefflaplante/agentbus:v0.1.0 -test.run '^$' -test.bench . -test.benchtime 300x
```
