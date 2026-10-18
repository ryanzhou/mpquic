# Congestion control: single-stream throughput

These measurements use **one bulk stream over one path at 150 ms RTT** (75 ms
each way), produced by `scripts/cc-bench.py`. It runs `mpbench` with a single
emulated path (`internal/linkem`, the same impairment in both directions), 15
s per run. "Mbit/s" is steady-state goodput, measured after the first 2 s.
"eff" is goodput divided by the link rate. The link rate includes UDP/IP
overhead, so the ceiling is ~96%. "ramp" is the time until the first 0.5 s
interval reaches 90% of the steady rate.

`bbr` is the default (BBRv1 from Hysteria, with mpquic's app-limited hint).
`cubic` is quic-go's built-in loss-based controller, selectable with
`--congestion cubic` for comparison.

## Headline: reachable throughput at 150 ms RTT

| link | BBR goodput | note |
|---|---|---|
| ≤ 300 Mbit/s | **90–95% of the link** | full rate after ~2–3 s (BBR startup takes ~10 RTTs) |
| 1 Gbit/s | **~750–880 Mbit/s** (75–88%) | high run-to-run variance; raw quic-go+BBR without mpquic measures the same range |
| unconstrained | **~1.3 Gbit/s** | bounded by the 64 MB max stream window (64 MB / 0.16 s ≈ 1.3–1.4 Gbit/s) |

BBR keeps this up to **2% random loss at 93%** of the link, 5% at 90% and
10% at 80%. Loss-based CUBIC collapses to 9% at only 0.1% loss.

## Bandwidth (no loss, 100 ms drop-tail buffer, default window)

| link | bbr Mbit/s (eff, ramp) | cubic Mbit/s (eff, ramp) |
|---|---|---|
| 10 Mbit/s | 9.3 (93%, 2.0s) | 9.6 (96%, 2.0s) |
| 50 Mbit/s | 46.2 (92%, 2.0s) | 47.9 (96%, 2.0s) |
| 100 Mbit/s | 94.9 (95%, 2.5s) | 96.6 (97%, 2.0s) |
| 300 Mbit/s | 271.5 (90%, 2.0s) | 287.3 (96%, 2.5s) |
| 1000 Mbit/s | 777.2 (78%, 3.0s) | 747.6 (75%, 3.0s) |
| unlimited | 1289.1 (–, 3.0s) | 728.6 (–, 2.0s) |

## Stream window vs. reachable rate (unlimited link, no loss)

A single stream can't exceed `window / RTT`. The window auto-tunes per stream
from 8 MB up to `--window` (default 64 MB).

| --window (max) | bbr Mbit/s (ramp) |
|---|---|
| 4 MB | 167 (2.0s) |
| 8 MB | 333 (2.0s) |
| 16 MB | 602 (2.5s) |
| 32 MB | 967 (3.0s) |
| 64 MB (default) | 1347 (3.5s) |

Before auto-tuning (a fixed 16 MB window), one stream topped out at ~650
Mbit/s on an unconstrained 150 ms path, and at ~450 Mbit/s on a 1 Gbit/s link
with a 100 ms buffer. Auto-tuning only grows the window when a whole window
is consumed within 4 RTTs. A slow consumer (for example a slow SOCKS client)
therefore stays at 8 MB, and the sender never buffers more than the peer
allows.

## Random loss (100 Mbit/s)

| link | bbr | bbr-aggressive | bbr-conservative | cubic |
|---|---|---|---|---|
| 0% loss | 94.8 (95%, 2.5s) | 92.4 (92%, 2.0s) | 92.3 (92%, 2.0s) | 95.7 (96%, 2.0s) |
| 0.1% loss | 92.9 (93%, 3.0s) | 93.4 (93%, 1.5s) | 90.3 (90%, 3.0s) | 9.0 (9%, 1.0s) |
| 0.5% loss | 91.7 (92%, 3.0s) | 92.7 (93%, 2.5s) | 91.9 (92%, 3.0s) | 1.9 (2%, 1.0s) |
| 1% loss | 92.3 (92%, 3.0s) | 92.3 (92%, 3.0s) | 94.3 (94%, 3.0s) | 1.3 (1%, 1.0s) |
| 2% loss | 93.4 (93%, 3.0s) | 90.8 (91%, 2.5s) | 92.6 (93%, 3.5s) | 1.0 (1%, 0.5s) |
| 5% loss | 89.5 (90%, 3.0s) | 89.0 (89%, 3.0s) | 89.6 (90%, 3.0s) | 0.6 (1%, 1.0s) |
| 10% loss | 80.0 (80%, 3.0s) | 77.5 (78%, 3.5s) | 77.7 (78%, 3.5s) | 0.3 (0%, 0.5s) |

## Bursty loss (Gilbert-Elliott, mean burst 5 packets, 100 Mbit/s)

| link | bbr | cubic |
|---|---|---|
| 0.5% loss | 93.2 (93%, 2.0s) | 3.6 (4%, 1.0s) |
| 2% loss | 93.9 (94%, 3.0s) | 1.9 (2%, 0.5s) |

## Bottleneck buffer depth (100 Mbit/s, no loss)

| buffer | bbr | bbr-conservative | cubic |
|---|---|---|---|
| 5 ms | 90.6 (91%, 2.0s) | 91.1 (91%, 2.5s) | 66.3 (66%, 2.0s) |
| 20 ms | 92.2 (92%, 2.5s) | 92.1 (92%, 2.5s) | 80.5 (80%, 1.5s) |
| 100 ms | 94.9 (95%, 2.5s) | 95.5 (96%, 2.0s) | 97.1 (97%, 2.5s) |
| 300 ms | 92.4 (92%, 2.0s) | 92.3 (92%, 2.0s) | 97.7 (98%, 1.5s) |

## Jitter, reordering, policing (100 Mbit/s)

| link | bbr | cubic |
|---|---|---|
| jitter 10 ms | 93.3 (93%, 2.5s) | 96.2 (96%, 2.0s) |
| reorder 2% (+20 ms) | 91.9 (92%, 2.5s) | 0.9 (1%, 0.5s) |
| token-bucket policer 100 Mbit/s | 95.6 (2.0s) | 72.7 (2.0s) |

(The burst, buffer and jitter tables were measured with the previous fixed
16 MB window. At 100 Mbit/s × 150 ms the BDP is ~1.9 MB, so the window
change doesn't affect them.)

## Takeaways

* **BBR is the right default for relay paths.** It holds 90%+ of the link
  through random loss, bursty loss and reordering, with shallow or deep
  buffers and with policers. CUBIC is only competitive on clean links with
  deep buffers. On a long-RTT internet path with any loss, it collapses.
* **Profiles hardly matter.** `bbr-aggressive` ramps slightly faster.
  `bbr-conservative` builds less queue, at the cost of a few percent at very
  high loss. `bbr` is a good default.
* **Ramp-up at 150 ms RTT takes ~2–3 s**: BBR startup doubles roughly once
  per RTT. Multipath helps short transfers here, because the
  lowest-latency path ramps first.
* **Above ~500 Mbit/s per stream at 150 ms**, throughput is bounded by the
  stream window (default 64 MB → ~1.3 Gbit/s; raise `--window` for more). On
  a 1 Gbit/s bottleneck it's bounded by quic-go/BBRv1 dynamics at an ~19 MB
  BDP. Raw quic-go measured the same 750–880 Mbit/s range as mpquic, and
  neither raising BBR's cwnd cap nor its pacing burst changed it.
* Several streams or several paths add up. These per-stream limits apply to
  one TCP connection over one path.

Reproduce: `scripts/cc-bench.py [--rtt 150] [--suite bandwidth,window,loss,burst,buffer,jitter]`.
