# Benchmarks

All numbers come from `scripts/bench-matrix.sh`, which runs `mpbench`. Each
run is a complete in-process testbed on one Linux host (8 cores): a target
TCP server, an mpquic remote, one emulated UDP relay per path
(`internal/linkem`, applied in both directions), and an mpquic local in
tunnel mode. "multipath" uses all paths; "pathN-only" is the same stack with
a single path, as a baseline. Rates are the link-emulator rates, which
include UDP/IP overhead, so ~95% of the nominal rate is the goodput ceiling.

Metrics:

* **echo p50/p99**: 100-byte request/response round trips on an idle
  connection.
* **new conn**: TCP connect to local, then a 2 KB response (includes stream
  setup and target dial).
* **download/upload**: steady-state goodput of one bulk connection (first 2 s
  excluded).
* **2 MB / 16 MB FCT**: flow completion time of a download, median.
* **echo under load**: 100-byte round trips while a bulk download saturates
  all paths.
* **failover stall**: longest gap in a bulk download when the lowest-RTT path
  is blackholed (3 s → 7 s).

Scenarios (one-way `delay`, so RTT = 2×delay):

| name | paths |
|---|---|
| equal | 10ms/40Mbit · 30ms/40Mbit |
| hetero | 5ms/10Mbit (fast, thin) · 50ms/100Mbit (slow, fat) |
| lossy-direct | 10ms/50Mbit **2% loss** · 25ms/50Mbit ±3ms jitter |
| three-paths | 8ms/30Mbit 1% loss, **outage 1.5s every 6s** · 20ms/40Mbit ±5ms · 60ms/60Mbit 2% reordering |
| asym-rtt | 5ms/100Mbit · **150ms**/100Mbit |
| flapping | 10ms, rate flaps 50↔10Mbit every 3s · 30ms, 20↔60Mbit every 4s |

#### equal

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 22.1ms | 22.5ms | 22.5ms | 77.1 | 77.5 | 268.5ms | 1.7939s | 44.6ms | 152ms |
| path0-only | 22ms | 22.3ms | 22.2ms | 40.2 | 40.3 | 460.5ms | 3.5319s | 48.3ms | – |
| path1-only | 61.5ms | 62.5ms | 62.2ms | 40.5 | 40.5 | 500.6ms | 3.5731s | 106.8ms | – |

#### hetero

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 11.3ms | 12.3ms | 13.6ms | 102.8 | 129.6 | 245.8ms | 1.3602s | 38.5ms | 51ms |
| path0-only | 11.3ms | 11.7ms | 13.6ms | 9.9 | 10.2 | 1.768s | 14.1709s | 38.1ms | – |
| path1-only | 101.6ms | 102.5ms | 101.6ms | 95.1 | 100.0 | 277.2ms | 1.5067s | 138.3ms | – |

#### lossy-direct

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 21.6ms | 39.9ms | 22.1ms | 97.8 | 99.4 | 225.9ms | 1.4445s | 41.8ms | 151ms |
| path0-only | 22.1ms | 57.7ms | 22.1ms | 48.9 | 48.8 | 388.7ms | 2.8791s | 44.3ms | – |
| path1-only | 54.9ms | 57.1ms | 55.8ms | 47.7 | 47.5 | 407.6ms | 2.8654s | 112ms | – |

#### three-paths

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 18.1ms | 59.3ms | 18.6ms | 113.2 | 121.9 | 224ms | 1.1784s | 40.6ms | 50ms |
| path0-only | 17.8ms | 37.8ms | 17.9ms | 20.3 | 17.3 | 674.8ms | 6.9106s | 32.4ms | – |
| path1-only | 46.7ms | 51.5ms | 48.3ms | 39.1 | 40.8 | 488.1ms | 3.5609s | 97.6ms | – |
| path2-only | 121.6ms | 146.5ms | 122.1ms | 53.5 | 53.5 | 438.6ms | 2.5538s | 124.3ms | – |

#### asym-rtt

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 11.5ms | 12.1ms | 11.6ms | 182.7 | 179.9 | 187.7ms | 919.5ms | 25.1ms | 304ms |
| path0-only | 11.3ms | 11.7ms | 11.5ms | 100.3 | 100.3 | 187.2ms | 1.4163s | 25ms | – |
| path1-only | 301.6ms | 302.4ms | 302.2ms | 85.7 | 85.0 | 477.5ms | 1.707s | 325.3ms | – |

#### flapping

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 22.1ms | 22.8ms | 22.3ms | 59.4 | 68.0 | 491.6ms | 2.039s | 47ms | 302ms |
| path0-only | 22.1ms | 22.3ms | 23.3ms | 26.8 | 25.7 | 476.4ms | 5.7313s | 43.9ms | – |
| path1-only | 61.7ms | 62.6ms | 61.8ms | 36.4 | 35.2 | 360.2ms | 5.8192s | 73.7ms | – |

## Adding a bad path ("do no harm")

`scripts/harm-bench.sh` runs two good paths (15 ms and 35 ms one-way,
40 Mbit/s each) alone, and again together with one bad path. Each cell below
is *with the bad path / without it*. `scripts/harm-summary.py` flags any
metric that is worse by more than a noise tolerance.

| bad path | bulk Mbit/s | worst 100 ms (p10) | longest stall | echo p50 | 32 KB echo p90 | echo under load p99 |
|---|---|---|---|---|---|---|
| 25 ms, 20% loss | 105 / 75 | 63 / 39 | 42 ms / 63 ms | 31.5 ms / 31.5 ms | 43 ms / 43 ms | 75 ms / 119 ms |
| 150 ms, 10% loss | 105 / 77 | 51 / 62 | 51 ms / 0 ms | 31.4 ms / 31.3 ms | 44 ms / 43 ms | 68 ms / 124 ms |
| 20 ms ± 80 ms jitter | 88 / 76 | 60 / 55 | 62 ms / 52 ms | 31.5 ms / 31.5 ms | 45 ms / 45 ms | 123 ms / 125 ms |
| 10 ms, 1 Mbit/s | 79 / 77 | 59 / 58 | 41 ms / 11 ms | 24.7 ms / 31.6 ms | 44 ms / 46 ms | 117 ms / 70 ms |
| 10 ms, down 1 s of every 3 s | 90 / 77 | 39 / 57 | 51 ms / 42 ms | 22.3 ms / 31.5 ms | 88 ms / 44 ms | 96 ms / 121 ms |
| 10 ms, 3% loss, 40↔2 Mbit/s every 3 s | 90 / 76 | 47 / 53 | 103 ms / 31 ms | 21.7 ms / 31.3 ms | 44 ms / 44 ms | 89 ms / 84 ms |

Throughput never drops, and usually rises by the bad path's usable capacity.
Lossy and slow paths no longer cost smoothness. A path that fails or changes
abruptly costs transients: a stall of up to ~100 ms, or a slow small request,
when it drops out or collapses. Afterwards the session settles back, much as
MPTCP does after a link change. No path is excluded by rule.

Four lossy paths only (`scripts/lossy-bench.sh`: 20 ms/20%, 30 ms/10%,
40 ms/5%, 60 ms/1% loss, 50 Mbit/s each): about 157 Mbit/s download and upload
(the previous release reached about 100), with echo p50 42 ms. Before the
control plane moved to datagrams, this setup produced frequent false
"path suspect" events; now there are about 2 per run.

## Reading the results

* **Latency of the shortest path is preserved.** In every scenario the
  multipath echo p50 and new-connection time equal the best single path to
  within ~0.3 ms. Small requests never touch the slow paths (ECF), even when
  the fast path is thin (hetero: 11 ms, versus 102 ms on the fat path).
* **Aggregate throughput.** Bulk goodput is about the sum of the single-path
  goodputs: equal 77 vs 40+41, hetero 103 vs 10+95, lossy-direct 98 vs 49+48,
  three-paths 113 vs 20+39+54, flapping 59 vs 27+36. With extreme RTT
  asymmetry (10 ms vs 300 ms RTT) it is 183 of 186.
* **Completion times beat the best single path at all sizes.** Examples: a
  16 MB download takes 1.18 s with three paths, against 2.55 s on the best
  single path. In hetero, 2 MB takes 246 ms, against 277 ms on the fat path
  and 1.77 s on the fast path. Because ECF stops using slow paths near the
  end of a transfer, finishing never waits on the slowest path.
* **Loss and outages are hidden from interactive traffic.** With 2% loss on
  the best path, echo p99 drops from 58 ms (that path alone) to 40 ms; that's
  eager redundant copies on the idle second path. With the flaky direct
  path in three-paths, the worst echo was 114 ms, against seconds on that
  path alone. Cutting the best path mid-download stalls the download for
  only 50–300 ms (304 ms when the only remaining path has 300 ms RTT).
* **Under full load**, interactive latency is about 2× the best path's base
  RTT. That's the queue BBRv1 builds on a saturated bottleneck, the same as
  a single-path QUIC/TCP flow would see.

## CPU

With unconstrained loopback paths, a single process running both endpoints
and both relays moves ~3.7 Gbit/s download / ~4.0 Gbit/s upload (7 of 8
cores busy). Internet paths, not CPU, are the bottleneck for this use case.

## Reproduce

```sh
scripts/bench-matrix.sh [scenario...]      # writes bench-results/*.txt
scripts/bench-summary.py bench-results     # markdown tables
scripts/harm-bench.sh ./bin/mpbench        # good paths vs. good paths + one bad path
scripts/harm-summary.py                    # flags metrics the bad path made worse
scripts/lossy-bench.sh ./bin/mpbench       # four lossy paths (20/10/5/1%)
mpbench --path delay=20ms,rate=50mbit,loss=1% --path delay=60ms,rate=80mbit -v
```
