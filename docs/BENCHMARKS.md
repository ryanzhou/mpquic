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
| multipath | 21.9ms | 22.2ms | 22.4ms | 76.3 | 76.3 | 255.1ms | 1.7873s | 45.1ms | 102ms |
| path0-only | 21.6ms | 22ms | 22.1ms | 37.3 | 38.3 | 460.5ms | 3.5321s | 47.6ms | – |
| path1-only | 61.8ms | 62.4ms | 62.3ms | 38.3 | 38.3 | 500.2ms | 3.5727s | 116.7ms | – |

#### hetero

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 11.2ms | 12ms | 13.6ms | 100.5 | 105.0 | 217.2ms | 1.3436s | 38.2ms | 252ms |
| path0-only | 11.1ms | 11.8ms | 13.5ms | 9.4 | 9.6 | 1.764s | 14.1713s | 38.4ms | – |
| path1-only | 101.6ms | 102.3ms | 101.7ms | 95.7 | 90.8 | 277.6ms | 1.5068s | 144.5ms | – |

#### lossy-direct

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 22ms | 38.4ms | 22ms | 91.3 | 93.7 | 221.4ms | 1.4715s | 42.7ms | 152ms |
| path0-only | 21.7ms | 72.7ms | 22.3ms | 47.8 | 47.2 | 387.1ms | 2.8483s | 43.8ms | – |
| path1-only | 55.2ms | 57.3ms | 55.6ms | 45.7 | 47.8 | 407.8ms | 2.8657s | 109.5ms | – |

#### three-paths

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 17.8ms | 30.4ms | 18.5ms | 113.1 | 112.3 | 226.4ms | 1.1589s | 38.9ms | 102ms |
| path0-only | 17.4ms | 61.5ms | 17.9ms | 19.4 | 18.8 | 689ms | 6.8273s | 37.3ms | – |
| path1-only | 47.4ms | 50.9ms | 48ms | 36.5 | 38.3 | 486.8ms | 3.5598s | 100.5ms | – |
| path2-only | 121.7ms | 146.5ms | 121.9ms | 52.3 | 55.7 | 429ms | 2.5519s | 124.6ms | – |

#### asym-rtt

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 11.1ms | 11.5ms | 11.5ms | 168.8 | 168.5 | 187ms | 843.9ms | 24.9ms | 504ms |
| path0-only | 11.1ms | 11.5ms | 11.4ms | 95.7 | 95.7 | 187.1ms | 1.4167s | 24.8ms | – |
| path1-only | 301.5ms | 302.5ms | 301.7ms | 91.1 | 84.5 | 477.5ms | 1.7447s | 327.7ms | – |

#### flapping

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 21.8ms | 22.4ms | 22.2ms | 56.9 | 57.7 | 268.8ms | 2.8812s | 43.2ms | 252ms |
| path0-only | 21.8ms | 22.3ms | 23ms | 25.5 | 25.9 | 373.1ms | 4.8902s | 46ms | – |
| path1-only | 61.5ms | 62.5ms | 62.3ms | 31.6 | 35.0 | 411.3ms | 5.2455s | 74.6ms | – |

## Reading the results

* **Latency of the shortest path is preserved.** In every scenario the
  multipath echo p50 and new-connection time equal the best single path to
  within ~0.3 ms. Small requests never touch the slow paths (ECF), even when
  the fast path is thin (hetero: 11 ms, versus 102 ms on the fat path).
* **Aggregate throughput.** Bulk goodput is about the sum of the single-path
  goodputs: equal 76 vs 38+38, hetero 100 vs 9+96, lossy-direct 91 vs 48+46,
  three-paths 113 vs 19+37+52, flapping 57 vs 26+32. With extreme RTT
  asymmetry (10 ms vs 300 ms RTT) it is 169 of 187, about 90%. Most of the
  gap there is the slow path's BBR startup, which takes ~2 s at 300 ms RTT.
* **Completion times beat the best single path at all sizes.** Examples: a
  16 MB download takes 1.16 s with three paths, against 2.55 s on the best
  single path. In hetero, 2 MB takes 217 ms, against 278 ms on the fat path
  and 1.76 s on the fast path. Because ECF stops using slow paths near the
  end of a transfer, finishing never waits on the slowest path.
* **Loss and outages are hidden from interactive traffic.** With 2% loss on
  the best path, echo p99 drops from 73 ms (that path alone) to 38 ms; that's
  eager redundant copies on the idle second path. With the flaky direct
  path in three-paths, the worst echo was 34 ms, against 1.55 s on that path
  alone. Cutting the best path mid-download stalls the download for only
  100–250 ms (504 ms when the only remaining path has 300 ms RTT).
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
mpbench --path delay=20ms,rate=50mbit,loss=1% --path delay=60ms,rate=80mbit -v
```
