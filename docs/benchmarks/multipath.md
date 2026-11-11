# Multipath benchmarks

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
[congestion-control.md](congestion-control.md)).

Four lossy paths only (`scripts/lossy-bench.sh`: 20 ms/20%, 30 ms/10%,
40 ms/5%, 60 ms/1% loss, 50 Mbit/s each): about 170–180 Mbit/s download and
upload (145–150 with a BBR per path; an earlier release reached about 100),
with echo p50 42 ms. Before the
control plane moved to datagrams, this setup produced frequent false
"path suspect" events; now there are 2–5 per run.

## Common bottlenecks (an endpoint's line is slower than the relays)

`scripts/shared-bench.sh` puts one link in front of all relays (`mpbench
--shared`: the sending endpoint's uplink) or behind all of them
(`--shared-rx`: the receiving endpoint's downlink; what it drops has already
crossed the relays). The relays themselves are not limited unless noted;
their RTTs are 20/50/80 ms.

The rows compare the two congestion-control designs on the same machine:
**per-path** is the previous release (a BBR per path, its estimate capped by
the session so that the paths add up to what was delivered), **session** is
the current one (one rate controller for the session, the paths pace at
their shares; see [congestion control](../design/congestion.md)). Single
12 s runs, so differences of a few percent mean little.

* **relays carried**: UDP payload arriving at the relays, over the goodput.
  3–4% is the protocol's own overhead; the rest is retransmissions, copies,
  and packets that are dropped further on.
* **dropped**: packets dropped at the common bottleneck.
* **copies**: bytes the session sent again (rescues), as a share of the
  goodput.
* **rates**: what the sender's paths pace at, added up, during the transfer
  (median / 90th percentile, Mbit/s). It should be what the bottleneck
  carries: about 97% of its rate, as rates include UDP/IP headers.
* **echo**: 100-byte round trips during the transfer (idle: ~22 ms; ~30 ms
  with the upload running in the bidirectional scenario).
* **p10**: the 10th percentile of the goodput per 100 ms, i.e. how smooth
  the delivery is.

| scenario | | Mbit/s | relays carried | dropped | copies | rates p50 / p90 | echo p50 / p90 | p10 Mbit/s |
|---|---|---|---|---|---|---|---|---|
| 20 Mbit/s uplink, 300 ms buffer (upload) | per-path | 18.6 | +3.1% | 0.00% | 0.3% | 20 / 20 | 30 / 37 ms | 16 |
| | **session** | 18.9 | +3.0% | 0.00% | 0.0% | 20 / 21 | 30 / 38 ms | 16 |
| 20 Mbit/s uplink, 15 ms buffer (upload) | per-path | 18.5 | +6.4% | 2.52% | 2.9% | 20 / 20 | 30 / 38 ms | 6 |
| | **session** | 18.9 | +5.9% | 1.86% | 1.8% | 19 / 20 | 28 / 37 ms | 12 |
| 50 Mbit/s uplink, relays with 2–4 ms jitter (upload) | per-path | 46.1 | +4.6% | 0.00% | 1.4% | 50 / 50 | 30 / 40 ms | 33 |
| | **session** | 45.5 | +5.7% | 0.00% | 2.5% | 49 / 52 | 32 / 39 ms | 36 |
| 100 Mbit/s downlink, 50 ms buffer | per-path | 93.6 | +3.0% | 0.02% | 0.1% | 100 / 103 | 29 / 35 ms | 78 |
| | **session** | 94.2 | +3.2% | 0.03% | 0.3% | 98 / 106 | 28 / 39 ms | 81 |
| 100 Mbit/s downlink, 10 ms buffer | per-path | 92.1 | +9.6% | 2.76% | 3.4% | 102 / 105 | 30 / 37 ms | 29 |
| | **session** | 93.0 | +7.0% | 1.02% | 1.9% | 98 / 104 | 28 / 34 ms | 32 |
| 100 Mbit/s downlink, 300 ms buffer | per-path | 93.1 | +3.0% | 0.00% | 0.2% | 100 / 103 | 29 / 39 ms | 79 |
| | **session** | 94.1 | +3.0% | 0.00% | 0.1% | 99 / 105 | 29 / 36 ms | 79 |
| 50 Mbit/s **policer** on the downlink | per-path | 45.3 | +21.3% | 9.50% | 8.1% | 51 / 52 | 25 / 35 ms | 15 |
| | **session** | 47.0 | +9.1% | 2.44% | 3.8% | 49 / 49 | 25 / 28 ms | 20 |
| 100 Mbit/s downlink, relays with jitter | per-path | 90.1 | +6.5% | 0.03% | 3.4% | 102 / 103 | 32 / 39 ms | 64 |
| | **session** | 89.6 | +8.2% | 0.16% | 4.7% | 98 / 108 | 33 / 47 ms | 58 |
| 100 Mbit/s downlink, 4 relays of 300/200/200/100 Mbit/s | per-path | 93.5 | +3.4% | 0.11% | 0.4% | 102 / 104 | 26 / 36 ms | 78 |
| | **session** | 96.7 | +3.7% | 0.30% | 0.4% | 97 / 103 | 23 / 44 ms | 83 |
| 100 Mbit/s downlink, 4 parallel downloads | per-path | 95.2 | +3.4% | 0.14% | 0.1% | 101 / 102 | 35 / 58 ms | 42 |
| | **session** | 98.3 | +3.7% | 0.18% | 0.0% | 100 / 108 | 42 / 60 ms | 51 |
| downlink 100 ↔ 40 Mbit/s every 4 s | per-path | 62.6 | +4.7% | 0.85% | 0.2% | 69 / 103 | 29 / 45 ms | 13 |
| | **session** | 60.9 | +10.5% | 2.39% | 0.9% | 49 / 108 | 30 / 97 ms | 7 |
| **100/20 Mbit/s line: a download while an upload runs** | per-path | 92.2 | +3.4% | 0.06% | 0.5% | 101 / 108 | 36 / 47 ms | 77 |
| | **session** | 86.5 | +4.6% | 0.70% | 0.7% | 91 / 102 | 35 / 50 ms | 36 |
| … with `--up 19mbit --down 95mbit` | per-path | 90.8 | +3.3% | 0.00% | 0.4% | 95 / 95 | 33 / 36 ms | 79 |
| | **session** | 92.6 | +3.0% | 0.00% | 0.0% | 95 / 95 | 32 / 35 ms | 82 |
| 50 Mbit/s policer, with `--down 47mbit` | per-path | 44.7 | +2.9% | 0.00% | 0.1% | 47 / 47 | 25 / 29 ms | 42 |
| | **session** | 45.8 | +2.9% | 0.00% | 0.1% | 47 / 47 | 25 / 27 ms | 40 |
| remote's uplink 150 Mbit/s (20 ms), relays 3×100 Mbit/s | per-path | 138.0 | +3.7% | 0.50% | 1.0% | 148 / 159 | 26 / 37 ms | 109 |
| | **session** | 142.2 | +3.7% | 0.67% | 0.9% | 148 / 157 | 27 / 30 ms | 109 |
| remote's uplink 150 Mbit/s (50 ms), relays 3×100 Mbit/s | per-path | 138.9 | +2.8% | 0.03% | 0.2% | 148 / 150 | 26 / 36 ms | 119 |
| | **session** | 141.1 | +2.7% | 0.00% | 0.0% | 148 / 157 | 28 / 32 ms | 120 |
| remote's uplink 150 ↔ 60 Mbit/s every 4 s | per-path | 93.9 | +3.4% | 0.72% | 0.5% | 101 / 148 | 28 / 41 ms | 18 |
| | **session** | 98.6 | +4.6% | 1.29% | 0.3% | 124 / 156 | 29 / 54 ms | 6 |
| remote's uplink 100 Mbit/s, 2 relays, the fastest with 1% loss | per-path | 92.4 | +3.9% | 0.00% | 0.1% | 99 / 100 | 27 / 34 ms | 53 |
| | **session** | 93.8 | +3.5% | 0.00% | 0.1% | 97 / 103 | 26 / 33 ms | 73 |
| control: uplink 400 Mbit/s, not the limit (3×100 Mbit/s) | per-path | 275.9 | +2.7% | 0.00% | 0.0% | 296 / 301 | 25 / 32 ms | 226 |
| | **session** | 285.6 | +2.8% | 0.00% | 0.0% | 294 / 312 | 26 / 31 ms | 242 |
| control: independent paths 3×50 Mbit/s | per-path | 138.7 | +3.0% | 0.00% | 0.0% | 148 / 151 | 26 / 34 ms | 121 |
| | **session** | 143.0 | +2.7% | 0.00% | 0.0% | 147 / 156 | 26 / 29 ms | 121 |
| control: two identical paths, 50 Mbit/s each | per-path | 94.4 | +2.7% | 0.00% | 0.0% | 99 / 100 | 46 / 49 ms | 88 |
| | **session** | 95.3 | +2.7% | 0.00% | 0.0% | 98 / 104 | 46 / 49 ms | 92 |
| control: independent 50 / 30 (2% loss) / 20 Mbit/s | per-path | 92.5 | +4.7% | 0.00% | 0.8% | 98 / 101 | 26 / 33 ms | 74 |
| | **session** | 92.4 | +4.5% | 0.00% | 0.5% | 97 / 104 | 39 / 49 ms | 78 |
| 100 Mbit/s downlink, one relay limited to 20 Mbit/s | per-path | 93.3 | +3.2% | 0.17% | 0.1% | 101 / 102 | 36 / 48 ms | 73 |
| | **session** | 95.0 | +3.1% | 0.08% | 0.2% | 98 / 104 | 30 / 36 ms | 76 |

What the numbers say:

* **Policers and shallow buffers** were the weak spot of the per-path
  design, which followed them as BBR does (by the highest rate they let
  through): 9.5% of the packets were dropped at the 50 Mbit/s policer and
  the relays carried a fifth more than was delivered. The session counts
  loss that comes with the rate and backs off below it: 2–3% dropped in
  12 s runs, 9–10% carried extra, and most of what is still dropped are
  the start-up and the first probes (probes become rare once the ceiling
  is confirmed; see the 30 s run below). A 10–15 ms buffer: 1.0–1.9%
  dropped instead of 2.5–2.8%.
* **Throughput** is the same or a little higher where the bottleneck is
  steady, and so is the latency under load, with a shorter tail in most
  rows (echo p90).
* **A relay slower than its share** (last row) is held to what it carries
  by its own window from the first round trip, and gets its own limit
  within a second: echo 30 / 36 ms instead of 36 / 48.
* **A flapping bottleneck** is where the session is worse. On the
  flapping downlink it delivers about the same on average but less evenly
  (52–65 Mbit/s in six runs against 61–63 in three), and each change costs
  more: 1.3–3.2% dropped against 0.9–1.0%, the longest pause in delivery
  240–530 ms against 160–220 ms, echo p90 52–120 ms against 45–57. One
  rate follows a drop at the next loss or queue that every path shows,
  about a round trip later than the fastest path's own controller did.
* **Both directions loaded** (the 100/20 Mbit/s line): the download is a
  little slower (86–94 against 92–93 Mbit/s in five and four runs) and the
  echo's tail longer (p90 49–68 ms against 44–48); the upload gets the same
  share of the uplink (16 against 15–17 Mbit/s). With `--up` / `--down` set
  there is no difference.
* **Known rates.** With `--up` / `--down` a little below the line's rates
  there is nothing to probe: nothing dropped at the policer, echo p90
  35 ms with both directions loaded.
* **Controls.** Independent paths are not held back by the common rate:
  each path's window keeps it at what its own bottleneck delivers, and the
  session's rate is their sum.

### Five relays in front of a policed downlink

The scenario the session controller was written for: five relays of
200 Mbit/s each, round trips of 150–250 ms, and a 500 Mbit/s policer in
front of the receiver (`mpbench --path delay=75ms,rate=200mbit` … `--path
delay=125ms,rate=200mbit --shared-rx 'none|police=500mbit'`, 30 s).

| | goodput | dropped at the policer | relays carried | sender's peak over 100 ms / 1 s | sender after 2 s, p50 / p90 |
|---|---|---|---|---|---|
| per-path (two runs) | 299–374 Mbit/s | 5.4–12.8% | +20–36% | 0.9–2.1 Gbit/s / 580–990 Mbit/s | 340–500 / 590–760 Mbit/s |
| **session** (four runs) | 432–442 Mbit/s | 1.2–1.5% | +6–7% | 0.9–1.1 Gbit/s / 560–610 Mbit/s | 474–488 / 499–532 Mbit/s |

Per-path controllers each find "200" and start up on their own: together
they send up to three times what the policer passes, and in the steady
state still a fifth more, because none of them sees what the others lose.
One controller starts up once, stops at the first loss that every path has,
and then stays just below the policer; what it sends above it are the
start-up's last round and a probe now and then.

### Where every path's own rate changes

The session controller is built for paths that share something. Where the
paths are independent and their *own* rates keep changing, a controller per
path follows each of them more closely than one rate with per-path limits
does (`scripts/bench-matrix.sh`, `scripts/lossy-bench.sh`, 8–15 s runs,
download / upload in Mbit/s):

| scenario | per-path | session |
|---|---|---|
| four lossy paths (20/10/5/1% loss, 50 Mbit/s each) | 146 / 153 | **169–171 / 172–173** |
| two equal paths (40 Mbit/s) | 74 / 75 | 76 / 76 |
| 10 + 100 Mbit/s | 97 / 111 | 95–96 / 100–103 |
| 5 ms and 150 ms round trips, 100 Mbit/s each | 165 / 181 | 159–166 / 166–176 |
| one path with 2% loss, one with jitter | 92 / 94 | 92–94 / 91–94 |
| two paths flapping on their own (50↔10, 20↔60 Mbit/s) | 51–59 / 54–61 | 48–53 / 54–56 |
| three paths: 1% loss and outages, jitter, 2% reordering | 97–111 / 101 | 75–98 / 86–96 |
| two good paths and one that collapses (40↔2 Mbit/s, 3% loss) | 89 | 78–79 |
| two good paths and one with outages (1 s of every 2) | 74 | 65–66 |

Steady paths, lossy or not, are about the same or better (the four lossy
paths by a sixth: the baselines keep random loss out of the rate). Paths
whose own capacity flaps, or that drop out and come back, are 10–15%
behind: a path's limit follows its relay down at once, but up again only by
tries every few rounds. With round trips as different as 5 and 150 ms the
upload is a few percent behind, and cutting the fast path stalled a
transfer for 0.5–0.75 s in three runs against 0.15 s (one round of the
session is the long path's round trip). Small transfers are unchanged
except on very lossy paths, where a 256 KB transfer took 0.3–0.6 s against
0.25–0.3 s (16 MB: 1.4–1.6 s against 1.6–1.8 s).

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
  bottleneck. See [congestion-control.md](congestion-control.md).

## CPU

With unconstrained loopback paths, a single process running both endpoints
and both relays moves ~3.7 Gbit/s download / ~4.0 Gbit/s upload (7 of 8
cores busy). Internet paths, not CPU, are the bottleneck for this use case.

### CPU at moderate rates

Each endpoint (and relay) in its own process: `mpquic remote`, one
`mpquic relay --link SPEC` per path, `mpquic local`, and `scripts/netbench`
downloading through SOCKS5 (one stream unless noted). CPU is measured over
6 s after a 3 s warm-up, 100% = one core; the relay column is the first
(emulating) relay. Average of 2 runs, Ryzen 7 5700X, all processes on one
8-core host. "before" is commit 93843ef, "after" adds the GOMAXPROCS
governor (`internal/procs`, see [threads](../design/README.md#threads)), a pass-through
fast path in the relay, and cheaper reinjection-queue and datagram-ACK
bookkeeping:

| condition | throughput before → after | local CPU | remote CPU | relay CPU |
|---|---|---|---|---|
| 20 Mbit/s, 10 ms | 19 → 19 Mbit/s | 19% → 11% | 20% → 13% | 21% → 12% |
| 100 Mbit/s, 10 ms | 95 → 95 Mbit/s | 35% → 18% | 28% → 13% | 38% → 16% |
| 300 Mbit/s, 10 ms | 286 → 286 Mbit/s | 117% → 41% | 104% → 29% | 146% → 38% |
| 100 Mbit/s upload | 94 → 92 Mbit/s | 28% → 14% | 34% → 18% | 37% → 15% |
| 2 × 50 Mbit/s (10 / 30 ms) | 95 → 94 Mbit/s | 36% → 20% | 32% → 15% | 26% → 13% |
| 100 Mbit/s, 1% loss | 96 → 93 Mbit/s | 39% → 19% | 31% → 14% | 40% → 17% |
| 100 Mbit/s, 3% loss | 93 → 94 Mbit/s | 41% → 21% | 31% → 17% | 42% → 18% |
| 50 Mbit/s 10% loss (datagram mode) + 50 Mbit/s clean | 90 → 90 Mbit/s | 43% → 24% | 40% → 20% | 28% → 17% |
| 100 Mbit/s, 150 ms RTT | 96 → 96 Mbit/s | 36% → 20% | 30% → 17% | 38% → 17% |
| 3 paths: 50 / 30 (2% loss) / 20 Mbit/s | 96 → 94 Mbit/s | 43% → 24% | 36% → 18% | 24% → 13% |
| 100 Mbit/s, 16 streams | 95 → 95 Mbit/s | 43% → 19% | 36% → 19% | 45% → 16% |
| unlimited, 1 path | 989 → 1450 Mbit/s | 150% → 124% | 140% → 109% | 174% → 193% |
| unlimited, 2 paths, 4 streams | 1242 → 1162 Mbit/s | 160% → 160% | 192% → 195% | 117% → 107% |
| 64 B round trip, p50 | 2.63 → 2.57 ms | 23% → 15% | 24% → 15% | 28% → 16% |
| new connection, p50 | 11.91 → 11.75 ms | 16% → 8% | 14% → 7% | 16% → 10% |

A plain relay (`mpquic relay` without `--link`) forwarding a 100 Mbit/s
download used 49% → 21%.

Before, at moderate rates, about half of the CPU went to the Go scheduler
rather than to packets: each packet is handed through several goroutines,
and with idle CPUs each hand-off woke another thread (futex wake, spinning,
sleep). Running on one thread halved the CPU at the same throughput and
latency. The governor keeps a process on one thread until it is busy or
goroutines queue for it, then doubles the number of threads. A single path
peaks higher with fewer threads (~1.45 Gbit/s on 2 vs ~1 Gbit/s on 8). In
the last unlimited case the whole host is saturated, and runs vary by about
±10%. What remains at moderate rates is mostly system calls: quic-go
acknowledges every second packet, so the receiver sends one ACK per two
data packets, and nothing batches until the rate is high. On this testbed
the send-side system calls also include the next hop's receive work, as
all processes share one host.

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
scripts/shared-bench.sh ./bin/mpbench      # paths behind a shared uplink, coupled vs. uncoupled
mpbench --path ... --path ... --shared 'none|rate=150mbit,queue=50ms'   # a shared bottleneck (fwd|rev)
mpbench --path delay=20ms,rate=50mbit,loss=1% --path delay=60ms,rate=80mbit -v
scripts/bench-wireguard.sh                 # SOCKS5 vs WireGuard entrypoint (Docker)
```
