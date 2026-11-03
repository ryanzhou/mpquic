# Congestion control

Each path runs its own BBRv1 sender (vendored from Hysteria, with mpquic's
changes described in [DESIGN.md](DESIGN.md#congestion-control)). This page
measures it: **one bulk stream over one emulated path at 150 ms RTT** (75 ms
each way), produced by `scripts/cc-bench.py`. It runs `mpbench` with a single
`internal/linkem` path (the same impairment in both directions), 15 s per run.

* **Mbit/s**: steady-state goodput after the first 2 s. **eff** is goodput
  divided by the link rate. The link rate includes UDP/IP overhead, so the
  ceiling is ~96%.
* **ramp**: time until the first 0.5 s interval reaches 90% of the steady rate.
* **+latency p50/p90**: what the transfer adds to the latency of other
  streams through the tunnel: a 100-byte echo every 100 ms during the
  transfer, minus the idle echo time. This is bufferbloat as the user sees it.

`bbr` is the default. `cubic` is quic-go's built-in loss-based controller,
selectable with `--congestion cubic` for comparison. Single runs; flapping
links and high rates vary by several percent between runs.

## Headline

| link | BBR goodput | note |
|---|---|---|
| ≤ 300 Mbit/s | **92–96% of the link** | full rate after ~2–2.5 s (BBR startup takes ~10 RTTs) |
| 1 Gbit/s | **~850–900 Mbit/s** | raw quic-go+BBR without mpquic measures the same range |
| unlimited | **~1.1–1.3 Gbit/s** | bounded by the 64 MB max stream window (64 MB / 0.16 s ≈ 1.3–1.4 Gbit/s) |

BBR holds 85–96% of the link up to 5% random loss and 80% at 10%. CUBIC
collapses to 7% at only 0.1% loss. A bulk transfer adds a median ~6 ms (p90
~30–40 ms, during BBR's periodic probing) to everything else in the tunnel,
however deep the bottleneck buffer; CUBIC fills the buffer (+250–500 ms with
0.5–2 s buffers).

## Bufferbloat: before and after

Stock BBRv1 (mpquic ≤ v0.1.0) had three problems with queues, fixed in the
default profile (details in [DESIGN.md](DESIGN.md#congestion-control)):

1. **A standing queue of one BDP** on a saturated path: the 0.75 drain phase
   ended after one min RTT whether or not the queue was gone, so BBR ended up
   limited by its 2·BDP window. Fixed by holding the drain phase until bytes in
   flight are down to the BDP (`drainToTarget`).
2. **Seconds of queue after a rate drop**: the max-bandwidth filter kept the
   old rate for 10 round trips, which stretched with the queue. Fixed by
   resetting a stale estimate (persistent queue + delivery rate below ¾ of the
   estimate), then re-probing quickly so a returning rate is used again.
3. **ProbeRTT turn-taking could lock up** (no path of the session probed its
   minimum RTT again). Fixed.

Added latency, 150 ms RTT, cc-bench (`bbr`):

| link | before p50/p90 | after p50/p90 |
|---|---|---|
| 100 Mbit/s, 100 ms buffer | +29/+52 ms | +6/+38 ms |
| 100 Mbit/s, 300 ms buffer | +30/+74 ms | +6/+38 ms |
| 20 Mbit/s, 500 ms buffer | +29/+69 ms | +8/+35 ms |
| 20 Mbit/s, 2 s buffer | +35/+63 ms | +8/+42 ms |
| 100 ↔ 20 Mbit/s every 3 s, 1 s buffer | +157/+1001 ms | +55/+915 ms |
| 40 ↔ 5 Mbit/s every 3 s, 2 s buffer | +36/+1908 ms | +5…157/+1572 ms (2 runs) |

Throughput and loss tolerance are unchanged (within run-to-run noise). With
rate changes every 3 s at 150 ms RTT (20 RTTs per phase), each 8× drop still
leaves a full window at the old rate to drain, which dominates p90: what was
in flight cannot be recalled. On shorter RTTs the gain is much larger, e.g.
a 50 ms RTT, 20 Mbit/s path with a 500 ms buffer: echo under load 105 → 59 ms
(idle 52 ms). In the multipath benchmarks (docs/BENCHMARKS.md) echo under
load dropped from ~2× the best path's RTT to +4–14 ms.

### Simulator

`internal/cc/bbr/sim_test.go` drives the sender through a deterministic
bottleneck model (time-varying rate, drop-tail buffer, random loss, ACK
aggregation, outages, competing BBR, Reno and unresponsive flows; QUIC-like
loss detection: 3-packet threshold and probe timeouts). Stock BBRv1 →
mpquic, 3 runs:

| scenario | before | after |
|---|---|---|
| 20 Mbit/s, 50 ms, 500 ms buffer: median queue | 8–11 ms | 3–5 ms |
| 40 → 5 Mbit/s for 6 s, 2 s buffer: time with > 50 ms queue | 5.7–6 s | 1.2–1.5 s |
| 40 → 28 Mbit/s: time with > 25 ms queue | 1.7–6.3 s | 0.5 s |
| 40 ↔ 5 Mbit/s every 3 s, 50 ms: utilization, median queue | 97–98%, 170–260 ms | 92–94%, 16 ms |
| 40 ↔ 5 Mbit/s every 8 s, 50 ms | 85–90%, 51–54 ms | 96%, 15 ms |
| 50 ↔ 10 Mbit/s every 5 s, 30 ms | 85–89%, 19–32 ms | 95–98%, 9 ms |
| 100 ↔ 20 Mbit/s every 3 s, 150 ms | 94%, 350–365 ms | 85–87%, 150 ms |
| 40 ↔ 2 Mbit/s every 3 s, 20 ms, 3% loss | 94–100%, 75–170 ms | 95–97%, 11 ms |
| unresponsive traffic takes 60% for 7 s: time with > 100 ms queue | 2.5–2.6 s | 0.6 s |
| two BBR flows, 1 s buffer: split, median queue | 45/53 to 52/46, 55–59 ms | 38/61 to 39/60, 29–32 ms |
| vs. Reno, 50 ms buffer: BBR share | 91–92% | 33–35% |
| vs. Reno, 1–3 s buffer: BBR share | 19–35% | 13–27% |
| ACK aggregation 40 ms on a 5 ms path: utilization | 97–98% | 97% |

BBRv1 no longer crowds out loss-based flows in shallow buffers; in deep
buffers it keeps the minority share it always had (the queue is the other
flow's). The conservative profile's overestimate avoidance collapses under
ACK aggregation on low-RTT paths (17%, 8%, 2% utilization with 10–40 ms ACK
bursts on 5–10 ms paths) and is not used by default.

## Bandwidth (no loss, 100 ms drop-tail buffer, default window)

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) | cubic Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|---|
| 10 Mbit/s | 9.2 (92%, 1.5s, +21/+49ms) | 9.6 (96%, 2.0s, +76/+103ms) |
| 50 Mbit/s | 47.7 (95%, 2.0s, +6/+37ms) | 48.1 (96%, 2.0s, +45/+50ms) |
| 100 Mbit/s | 94.9 (95%, 2.5s, +6/+28ms) | 94.6 (95%, 2.0s, +25/+28ms) |
| 300 Mbit/s | 279.5 (93%, 2.5s, +6/+36ms) | 274.0 (91%, 2.5s, +38/+39ms) |
| 1000 Mbit/s | 898.9 (90%, 4.0s, +8/+89ms) | 508.3 (51%, 2.0s, +42/+50ms) |
| unlimited | 1307.0 (–, 4.0s, +4/+38ms) | 742.3 (–, 2.0s, +50/+68ms) |

## Stream window vs. reachable rate (unlimited link, no loss)

A single stream can't exceed `window / RTT`. The window auto-tunes per stream
from 8 MB up to `--window` (default 64 MB).

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|
| window 4 MB | 218.2 (–, 2.0s, +0/+0ms) |
| window 8 MB | 421.8 (–, 2.0s, +0/+0ms) |
| window 16 MB | 862.5 (–, 3.0s, +0/+1ms) |
| window 32 MB | 1053.3 (–, 2.5s, +33/+53ms) |
| window 64 MB | 1116.0 (–, 2.5s, +52/+149ms) |

Auto-tuning only grows the window when a whole window is consumed within 4
RTTs. A slow consumer (for example a slow SOCKS client) therefore stays at
8 MB, and the sender never buffers more than the peer allows.

## Random loss (100 Mbit/s)

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) | bbr-aggressive Mbit/s (eff, ramp, +latency p50/p90) | bbr-conservative Mbit/s (eff, ramp, +latency p50/p90) | cubic Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|---|---|---|
| 0% loss | 92.0 (92%, 2.0s, +6/+32ms) | 95.9 (96%, 2.5s, +32/+48ms) | 94.3 (94%, 2.5s, +5/+39ms) | 94.6 (95%, 2.0s, +42/+44ms) |
| 0.1% loss | 92.4 (92%, 2.0s, +5/+29ms) | 94.9 (95%, 2.5s, +7/+23ms) | 94.4 (94%, 2.5s, +6/+32ms) | 6.9 (7%, 1.0s, +15/+51ms) |
| 0.5% loss | 96.4 (96%, 2.5s, +3/+35ms) | 91.1 (91%, 2.0s, +17/+45ms) | 96.1 (96%, 2.5s, +3/+32ms) | 2.7 (3%, 1.0s, +50/+59ms) |
| 1% loss | 91.0 (91%, 2.0s, +2/+27ms) | 93.0 (93%, 2.0s, +7/+20ms) | 96.0 (96%, 2.5s, +3/+26ms) | 1.3 (1%, 1.0s, +52/+157ms) |
| 2% loss | 95.7 (96%, 2.5s, +2/+29ms) | 93.9 (94%, 2.0s, +2/+29ms) | 94.4 (94%, 2.5s, +2/+29ms) | 1.0 (1%, 0.5s, +51/+201ms) |
| 5% loss | 84.7 (85%, 2.0s, +2/+28ms) | 89.1 (89%, 2.0s, +2/+26ms) | 88.5 (88%, 2.5s, +2/+153ms) | 0.4 (0%, 0.5s, +52/+294ms) |
| 10% loss | 79.9 (80%, 2.5s, +0/+13ms) | 79.0 (79%, 2.0s, +4/+158ms) | 74.3 (74%, 2.5s, +4/+181ms) | 0.2 (0%, 7.5s, +203/+1133ms) |

## Bursty loss (Gilbert-Elliott, mean burst 5 packets, 100 Mbit/s)

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) | cubic Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|---|
| 0.5% loss | 92.0 (92%, 2.5s, +3/+38ms) | 5.4 (5%, 1.0s, +42/+62ms) |
| 2% loss | 93.3 (93%, 2.5s, +2/+21ms) | 2.1 (2%, 0.5s, +52/+106ms) |

## Bottleneck buffer depth (100 Mbit/s, no loss)

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) | bbr-conservative Mbit/s (eff, ramp, +latency p50/p90) | cubic Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|---|---|
| queue 5 ms | 87.9 (88%, 2.0s, +5/+6ms) | 97.3 (97%, 2.5s, +4/+6ms) | 64.0 (64%, 2.0s, +2/+3ms) |
| queue 20 ms | 89.9 (90%, 2.0s, +6/+19ms) | 92.8 (93%, 2.5s, +7/+21ms) | 77.4 (77%, 2.0s, +2/+3ms) |
| queue 100 ms | 94.5 (94%, 2.5s, +6/+38ms) | 95.4 (95%, 2.5s, +6/+34ms) | 92.7 (93%, 2.0s, +42/+44ms) |
| queue 300 ms | 92.1 (92%, 1.5s, +6/+38ms) | 92.1 (92%, 2.0s, +6/+35ms) | 97.3 (97%, 1.5s, +165/+168ms) |

## Jitter, reordering, policing (100 Mbit/s)

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) | cubic Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|---|
| jitter 10 ms | 93.3 (93%, 2.5s, +17/+50ms) | 95.4 (95%, 2.0s, +32/+35ms) |
| reorder 2% | 94.6 (95%, 2.5s, +2/+18ms) | 1.0 (1%, 0.5s, +51/+128ms) |
| policer 100 Mbit/s | 96.5 (–, 2.5s, +1/+4ms) | 77.4 (–, 2.0s, +1/+3ms) |

## Deep buffers and rate changes (no loss)

| link | bbr Mbit/s (eff, ramp, +latency p50/p90) | bbr-conservative Mbit/s (eff, ramp, +latency p50/p90) | cubic Mbit/s (eff, ramp, +latency p50/p90) |
|---|---|---|---|
| 20 Mbit/s, 500 ms buffer | 18.6 (93%, 1.5s, +8/+35ms) | 18.4 (92%, 1.5s, +9/+38ms) | 19.1 (96%, 1.5s, +247/+254ms) |
| 20 Mbit/s, 2 s buffer | 18.4 (92%, 1.5s, +8/+42ms) | 18.4 (92%, 1.5s, +8/+43ms) | 19.1 (96%, 1.5s, +502/+509ms) |
| 40 ↔ 5 Mbit/s every 3 s, 2 s buffer | 11.6 (–, 1.0s, +157/+1572ms) | 10.8 (–, 1.0s, +41/+1888ms) | 10.5 (–, 1.0s, +456/+2011ms) |
| 100 ↔ 20 Mbit/s every 3 s, 1 s buffer | 42.0 (–, 1.5s, +55/+915ms) | 43.1 (–, 1.5s, +94/+925ms) | 42.8 (–, 1.5s, +203/+1000ms) |

## Takeaways

* **BBR is the right default for relay paths.** It holds 85–96% of the link
  through random loss, bursty loss and reordering, with shallow or deep
  buffers and with policers, and keeps the queue (and so the latency of
  everything else in the tunnel) small. CUBIC is only competitive on clean
  links, and fills deep buffers.
* **Profiles hardly matter for throughput.** `bbr-aggressive` keeps the old
  standing queue (+30–45 ms here) for a slightly faster ramp.
  `bbr-conservative` ramps more gently; avoid it on Wi-Fi/cellular paths with
  low RTT (see above). `bbr` is the default.
* **Ramp-up at 150 ms RTT takes ~2–2.5 s**: BBR startup doubles roughly once
  per RTT. Multipath helps short transfers here, because the lowest-latency
  path ramps first.
* **Above ~500 Mbit/s per stream at 150 ms**, throughput is bounded by the
  stream window (default 64 MB → ~1.3 Gbit/s; raise `--window` for more). On
  a 1 Gbit/s bottleneck it's bounded by quic-go/BBRv1 dynamics at an ~19 MB
  BDP.
* **Rapid, large rate changes** (8× every few seconds) still cost up to one
  window of queue per drop, mostly at long RTTs.
* Several streams or several paths add up. These per-stream limits apply to
  one TCP connection over one path.

Reproduce: `scripts/cc-bench.py [--rtt 150] [--suite bandwidth,window,loss,burst,buffer,jitter,bloat]`,
and `go test -v -run TestSim ./internal/cc/bbr/` for the simulator.
