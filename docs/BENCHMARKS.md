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
  all paths: what bufferbloat costs the other streams of the session.
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
| multipath | 22ms | 22.6ms | 22.4ms | 74.2 | 74.2 | 262.4ms | 1.8035s | 27.3ms | 51ms |
| path0-only | 21.9ms | 22.4ms | 22.2ms | 38.2 | 38.1 | 461.8ms | 3.5391s | 27.6ms | – |
| path1-only | 61.9ms | 62.5ms | 62.2ms | 36.2 | 38.2 | 500.7ms | 3.5769s | 68.6ms | – |

#### hetero

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 11.4ms | 11.9ms | 13.7ms | 107.8 | 110.3 | 243.9ms | 1.3637s | 25.2ms | 50ms |
| path0-only | 11.3ms | 11.8ms | 13.6ms | 9.4 | 9.5 | 1.7995s | 14.2739s | 24.9ms | – |
| path1-only | 101.4ms | 102.5ms | 102ms | 95.5 | 95.6 | 277.6ms | 1.5074s | 106.9ms | – |

#### lossy-direct

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 22ms | 47.7ms | 22.1ms | 90.5 | 92.8 | 218.1ms | 1.4633s | 25.4ms | 50ms |
| path0-only | 22ms | 49.4ms | 22.3ms | 45.5 | 47.4 | 389ms | 2.8749s | 25.5ms | – |
| path1-only | 55ms | 57.4ms | 55.8ms | 45.4 | 47.7 | 406.3ms | 2.8657s | 65.6ms | – |

#### three-paths

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 18ms | 18.4ms | 18.5ms | 112.8 | 110.3 | 232.2ms | 1.1945s | 24.9ms | 51ms |
| path0-only | 17.7ms | 43.4ms | 18ms | 17.1 | 19.6 | 655.2ms | 7.086s | 23.7ms | – |
| path1-only | 47.1ms | 51ms | 49.3ms | 36.2 | 38.0 | 491.4ms | 3.5966s | 60ms | – |
| path2-only | 121.7ms | 146.6ms | 122.1ms | 51.8 | 55.1 | 432.2ms | 2.5693s | 124ms | – |

#### asym-rtt

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 11.4ms | 12ms | 11.7ms | 177.5 | 170.4 | 188.8ms | 889.2ms | 15.5ms | 152ms |
| path0-only | 11.3ms | 11.7ms | 11.6ms | 95.0 | 94.9 | 189.1ms | 1.4249s | 14.8ms | – |
| path1-only | 301.7ms | 302.5ms | 302ms | 77.7 | 78.2 | 477.1ms | 1.7082s | 307.3ms | – |

#### flapping

| run | echo p50 | echo p99 | new conn | download Mbit/s | upload Mbit/s | 2 MB FCT | 16 MB FCT | echo under load | failover stall |
|---|---|---|---|---|---|---|---|---|---|
| multipath | 22.1ms | 22.6ms | 22.4ms | 61.7 | 62.4 | 248.2ms | 1.7449s | 26.8ms | 301ms |
| path0-only | 22ms | 22.6ms | 23.3ms | 25.6 | 26.6 | 374.3ms | 5.3229s | 32.4ms | – |
| path1-only | 61.6ms | 62.5ms | 62.7ms | 35.5 | 33.9 | 528.8ms | 3.0687s | 73.5ms | – |

## Adding a bad path ("do no harm")

`scripts/harm-bench.sh` runs two good paths (15 ms and 35 ms one-way,
40 Mbit/s each) alone, and again together with one bad path. Each cell below
is *with the bad path / without it*. `scripts/harm-summary.py` flags any
metric that is worse by more than a noise tolerance.

| bad path | bulk Mbit/s | worst 100 ms (p10) | longest stall | echo p50 | 32 KB echo p90 | echo under load p99 |
|---|---|---|---|---|---|---|
| 25 ms, 20% loss | 102 / 74 | 72 / 56 | 42 ms / 31 ms | 31.6 ms / 31.5 ms | 43 ms / 44 ms | 59 ms / 49 ms |
| 150 ms, 10% loss | 102 / 74 | 55 / 54 | 20 ms / 20 ms | 31.5 ms / 31.4 ms | 45 ms / 45 ms | 47 ms / 49 ms |
| 20 ms ± 80 ms jitter | 77 / 74 | 54 / 56 | 31 ms / 22 ms | 31.4 ms / 31.4 ms | 45 ms / 43 ms | 47 ms / 47 ms |
| 10 ms, 1 Mbit/s | 75 / 75 | 51 / 61 | 33 ms / 20 ms | 24.7 ms / 31.4 ms | 44 ms / 45 ms | 90 ms / 48 ms |
| 10 ms, down 1 s of every 3 s | 77 / 74 | 39 / 49 | 21 ms / 22 ms | 22.4 ms / 31.4 ms | 86 ms / 45 ms | 46 ms / 48 ms |
| 10 ms, 3% loss, 40↔2 Mbit/s every 3 s | 92 / 74 | 53 / 51 | 187 ms / 31 ms | 22.2 ms / 31.3 ms | 47 ms / 45 ms | 48 ms / 47 ms |

Throughput never drops, and usually rises by the bad path's usable capacity
(the 80 ms jitter and outage paths add little; between runs of these
scenarios bulk varies by ±10 Mbit/s). A path that fails or changes abruptly
costs transients: a stall of up to ~200 ms, or a slow small request, when it
drops out or collapses. A very thin path can cost tail latency under load.
Afterwards the session settles back, much as MPTCP does after a link change.
No path is excluded by rule. Echo under load (p99) is ~47 ms; before the
bufferbloat fixes in the congestion controller it was ~120 ms (see
docs/CONGESTION.md).

Four lossy paths only (`scripts/lossy-bench.sh`: 20 ms/20%, 30 ms/10%,
40 ms/5%, 60 ms/1% loss, 50 Mbit/s each): about 145–150 Mbit/s download and
upload (an earlier release reached about 100), with echo p50 42 ms. Before the
control plane moved to datagrams, this setup produced frequent false
"path suspect" events; now there are 2–5 per run.

## Reading the results

* **Latency of the shortest path is preserved.** In every scenario the
  multipath echo p50 and new-connection time equal the best single path to
  within ~0.3 ms. Small requests never touch the slow paths (ECF), even when
  the fast path is thin (hetero: 11 ms, versus 102 ms on the fat path).
* **Aggregate throughput.** Bulk goodput is about the sum of the single-path
  goodputs: equal 74 vs 38+36, hetero 108 vs 9+96, lossy-direct 91 vs 46+45,
  three-paths 113 vs 17+36+52, flapping 62 vs 26+36. With extreme RTT
  asymmetry (10 ms vs 300 ms RTT) it is 178 (single paths: 95 and 78).
* **Completion times beat the best single path at all sizes.** Examples: a
  16 MB download takes 1.19 s with three paths, against 2.57 s on the best
  single path. In hetero, 2 MB takes 244 ms, against 278 ms on the fat path
  and 1.80 s on the fast path. Because ECF stops using slow paths near the
  end of a transfer, finishing never waits on the slowest path.
* **Loss and outages are hidden from interactive traffic.** With 2% loss on
  the best path, eager redundant copies on the idle second path keep echo
  tails at or below that path's alone (p99 48 vs 49 ms idle, 41 vs 53 ms
  under load; p99 of 100 samples varies by ~10 ms between runs). With the
  flaky direct path in three-paths, the worst echo was 18 ms, against 1.6 s
  on that path alone. Cutting the best path mid-download stalls the download
  for only 50–300 ms.
* **Under full load**, interactive latency rises by only 4–14 ms over the
  idle echo (equal: 27 vs 22 ms, three-paths: 25 vs 18 ms). It used to
  double: stock BBRv1 kept a standing queue of one BDP on a saturated
  bottleneck. See docs/CONGESTION.md.

## CPU

With unconstrained loopback paths, a single process running both endpoints
and both relays moves ~3.7 Gbit/s download / ~4.0 Gbit/s upload (7 of 8
cores busy). Internet paths, not CPU, are the bottleneck for this use case.

## SOCKS5 vs WireGuard entrypoint

`scripts/bench-wireguard.sh` runs one `mpquic local` with both listeners on
the same multipath session, in Docker. A client container reaches the same
target through SOCKS5 (plain TCP to the local) or through WireGuard (the
Linux kernel implementation, MTU 1420). `scripts/netbench` does the
measuring for both. Throughput is the median of 3 runs, each 10 s after a
2 s warm-up. Latencies come from 1000 round trips (connect: 200 new
connections, timed to the first echoed byte). CPU is the local container's
cgroup usage (100% = one core). Host: Ryzen 7 5700X, 8 cores shared by all
containers.

**lan**: one direct path, no emulation (overhead / CPU ceiling)

| test | direct | SOCKS5 | WireGuard | local CPU, SOCKS5 → WG |
|---|---|---|---|---|
| download, 1 stream | 27.9 Gbit/s | 2224 Mbit/s | 1236 Mbit/s | 178% → 241% |
| upload, 1 stream | 18.2 Gbit/s | 2075 Mbit/s | 1508 Mbit/s | 190% → 258% |
| download, 4 streams | – | 2042 Mbit/s | 1177 Mbit/s | 185% → 249% |
| upload, 4 streams | – | 2005 Mbit/s | 1415 Mbit/s | 181% → 261% |
| TCP 64 B round trip, p50 / p99 | 0.054 / 0.22 ms | 0.320 / 0.49 ms | 0.455 / 1.10 ms | |
| new TCP connection, p50 / p99 | 0.158 / 0.50 ms | 0.650 / 1.13 ms | 0.773 / 1.51 ms | |
| UDP 64 B round trip, p50 / p99 | 0.039 / 0.10 ms | 0.283 / 0.42 ms | 0.390 / 0.78 ms | |
| UDP 1200 B round trip, p50 / p99 | 0.039 / 0.10 ms | 0.324 / 0.64 ms | 0.402 / 0.77 ms | |

**wan**: two emulated paths through relays, 10 ms and 25 ms one-way, 100
Mbit/s each

| test | SOCKS5 | WireGuard | local CPU, SOCKS5 → WG |
|---|---|---|---|
| download, 1 / 4 streams | 188 / 186 Mbit/s | 187 / 187 Mbit/s | 67% → 104%, 62% → 84% |
| upload, 1 / 4 streams | 186 / 193 Mbit/s | 186 / 187 Mbit/s | 51% → 80%, 50% → 81% |
| TCP 64 B round trip, p50 | 21.77 ms | 21.85 ms | |
| new TCP connection, p50 | 21.51 ms | 21.69 ms | |
| UDP 64 B / 1200 B round trip, p50 | 20.99 / 21.20 ms | 21.15 / 21.38 ms | |

When the paths are the bottleneck, both entrypoints fill them equally, and
WireGuard adds about 0.1–0.2 ms of latency. Without a bottleneck, the
WireGuard listener runs at 55–75% of SOCKS5 throughput and uses about twice
the local CPU per byte (the remote's cost per byte is the same).
Profiling the local during a WireGuard download puts about 25% of its CPU
in wireguard-go's UDP socket I/O (one ~1420-byte packet per send), about
20% in gVisor's TCP stack and only about 3% in ChaCha20-Poly1305.

Simple optimizations tried (4 full runs, judged by the WireGuard/SOCKS5
ratio within each run, since unchanged SOCKS5 drifted by up to ±8% between
runs) changed neither throughput nor latency beyond the noise, so none was
kept:

* gVisor software GSO (TCP sends a burst of up to 32 KB per pass), tun
  batches of 128 packets, one fewer copy per packet: the batches that
  wireguard-go encrypts and sends grew from mostly single packets to about
  25, and gVisor's share of CPU fell from ~21% to ~12%. The serial
  per-peer send path (UDP `sendmmsg`) stayed the limit.
* GRO on packets from the peers, with receive checksums skipped (WireGuard
  authenticates every packet). gVisor's GRO also requires the link to
  advertise `CapabilityRXChecksumOffload`, as it leaves the IPv4 header
  checksum of merged packets stale.

On this testbed all containers share one host, so the time in the local's
send system calls includes the client kernel's receive work. A two-machine
setup would show the local's own limit.

## Reproduce

```sh
scripts/bench-matrix.sh [scenario...]      # writes bench-results/*.txt
scripts/bench-summary.py bench-results     # markdown tables
scripts/harm-bench.sh ./bin/mpbench        # good paths vs. good paths + one bad path
scripts/harm-summary.py                    # flags metrics the bad path made worse
scripts/lossy-bench.sh ./bin/mpbench       # four lossy paths (20/10/5/1%)
mpbench --path delay=20ms,rate=50mbit,loss=1% --path delay=60ms,rate=80mbit -v
scripts/bench-wireguard.sh                 # SOCKS5 vs WireGuard entrypoint (Docker)
```
