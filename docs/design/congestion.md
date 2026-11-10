# Congestion control (`internal/cc`, `internal/mp/coupling.go`)

Every path has its own QUIC connection and its own congestion controller;
the session then makes the paths behave as one flow where they share a
bottleneck. The first part of this page is about one path, the second about
all of them together. Measurements:
[single path](../benchmarks/congestion-control.md),
[common bottlenecks](../benchmarks/multipath.md#common-bottlenecks-an-endpoints-line-is-slower-than-the-relays).

## Per-path BBR

BBR (vendored from Hysteria, MIT) runs per path. BBR is a good fit here: it
tolerates random loss, it gives an explicit bottleneck-bandwidth estimate the
scheduler can use, and it keeps queues short. One change matters for
multipath. A path that the scheduler deliberately leaves idle only carries
ACKs and pings, and stock BBR's bytes-in-flight heuristic mistakes that for a
saturated pipe. The bandwidth estimate then collapses to ~0.5 Mbit/s, cwnd to
4 packets, and BBR leaves STARTUP. The next burst then ramps up over many
seconds. The scheduler therefore tells BBR exactly when a path's sender is
idle (an app-limited hint), so samples from idle periods are ignored. This
alone doubled post-idle throughput in one scenario (45 → 76 Mbit/s).

Further changes keep queues short (bufferbloat), since every stream of
a session shares the paths' bottleneck queues: a bulk download's queue is
latency added to every interactive stream through the tunnel. See
[../benchmarks/congestion-control.md](../benchmarks/congestion-control.md)
for measurements.

* **Drain to target.** Stock BBRv1 paces at a max-filtered bandwidth estimate
  that is a little above the real rate, and its 0.75 gain phase ends after
  one min RTT whether or not the queue built by the 1.25 probe has drained.
  It ends up limited by its 2·BDP window with a standing queue of a whole
  BDP, which doubles the path RTT under load. The default profile holds the
  0.75 phase until bytes in flight are down to the BDP.
* **Stale estimate reset.** When the bottleneck rate drops (cellular, Wi-Fi,
  a relay whose uplink gets busy), BBRv1 keeps pacing at the old rate until
  the old samples leave its 10-round max filter. With a deep buffer nothing
  is lost, the queue grows to the 2·BDP window, and the rounds the filter
  counts stretch with the queue: an 8× drop kept ~0.8 s of queue for many
  seconds. BBR never holds a queue for a whole min RTT on its own, so if for
  `max(2·minRTT, 100 ms)` every RTT sample shows a queue of at least
  `max(minRTT/2, 10 ms)` *and* the delivery rate stays below ¾ of the
  estimate, the estimate is replaced by the measured delivery rate and the
  queue is drained. The long interval spans ACK-aggregation bursts (Wi-Fi,
  cellular), which otherwise look like rate drops. Delivery samples marked
  app-limited count too while the application has data: BBR marks them
  whenever bytes in flight dip below the BDP (e.g. in loss recovery), and
  after a rate drop that mark lasts a whole queue-inflated round trip. The max filter itself still
  counts real round trips: a wall-clock window made BBR starve against
  loss-based flows filling a deep buffer, where the inflated RTT really is the
  feedback loop. Resets are counted per path and shown in the
  session's stats log line as `rate_drops=N`.
* **Regrowth after a rate drop.** BBRv1 grows its estimate by at most 25%
  per gain cycle (8 min RTTs), so after a reset a rate that comes back (a
  flapping link) would take ~10 s to be used again at 150 ms RTT. For 10 s
  after a reset, while the estimate is below the old one, the gain cycle
  therefore re-probes (1.25) right after each drain, and once the estimate
  has grown by 15% since a probe started BBR re-enters STARTUP. In the
  simulator, on a link flapping between 40 and 5 Mbit/s every 3 s (50 ms
  RTT) this uses ~93% of the capacity with a median queue of ~15 ms; stock
  BBRv1 used ~97% but kept the buffer full (median queue 170–260 ms).
* **ProbeRTT turn-taking** (see [session.md](session.md#lossy-and-misbehaving-paths),
  "BBR ProbeRTT") only holds the session-wide turn while
  actually in ProbeRTT. A path whose min RTT expired on the first ACK after
  an idle period skips ProbeRTT; it used to keep the turn forever, so no other
  path of the session ever probed again.

The `bbr-conservative` profile's overestimate avoidance (Chromium's "A0"
sampler) is not used by default: under ACK aggregation on low-RTT paths (ACKs
in 10–40 ms bursts on a 5–10 ms path) it collapses the estimate to a few
percent of the link.

## Session rate: the paths are congestion-controlled as one flow

The relays are usually independent, but the paths are not: every path
leaves through the sending endpoint's uplink and arrives through the
receiving endpoint's downlink, and those are often slower than the relays (a
100/20 Mbit/s home line in front of three relays in data centres). Each
path's BBR measures its own delivery rate and keeps the highest it has seen.
Behind a common bottleneck the paths see their best rates at different
moments (one path gets more while another drains or probes its RTT), so the
estimates add up to more than the bottleneck carries. Left alone, the paths
keep its buffer full, which is latency for every stream of the session.
Where it drops instead (a shallow buffer, a policer), they lose the excess
all the time, and BBR ignores loss: behind a 50 Mbit/s policer three paths
sent 60–160 Mbit/s by their estimates and lost a quarter of it. Every lost
packet is retransmitted by QUIC, often rescued on another path as well, and
stalls the stream that waits for it. If the bottleneck is the receiver's
downlink, all of that crosses the relays first: they carried 47% more than
the session delivered.

MPTCP solves this with coupled congestion control: at a common bottleneck
the subflows together behave like one flow. Hysteria solves it by being told
the rates of the two ends. The session does both
(`internal/mp/coupling.go`). Like BBR, it works with delivery rates. Delay
only supports what the rates say, because a round-trip time also grows with
queues that are not the session's (the other direction's: a download's ACKs
behind an upload on the same uplink; or other people's traffic), and loss
is not judged at all, only what it leaves of the delivery rate. And
congestion is only what *all* paths show at once: one path's own queue or
loss is that path's business.

* **Rounds and delivery rate.** The session works in rounds of the largest
  smoothed RTT of its paths (50 ms to 1 s, at most twice a path's base RTT).
  The delivery rate of a round is how fast the bytes *sent in it* came out
  at the other end: per path, what was acknowledged between the round's
  start and end, each shifted by that path's RTT and timed from ACK to ACK
  (`cc.Meter.Mark`), and never more than was sent. It is known one round
  later. (Counting the ACKs that happen to arrive during a round instead
  mixes several rounds' sends when the paths' RTTs differ; a 25% probe then
  showed as 6%.)
* **Session rate.** *B* is the highest delivery rate seen. It stands while
  what is sent is delivered: that only shows a lower bound (as do rounds in
  which the paths had no data to send), and counting such rounds would let
  *B* sink whenever the paths happen to send a little less than their
  shares. Rounds in which *every* path delivered clearly less than it sent
  (below 0.98) show what a bottleneck that all of them pass carries, and
  *B* is the maximum of the last ten of those. That is all there is to
  loss, as in BBR: a policer drops the same share from every path, so its
  rate is found this way, and loss that all paths have costs nothing as
  long as the probes (1.25) more than make up for it. One path's own loss,
  however heavy, changes nothing.
* **Shares.** *B* is shared out max-min fairly as a cap on each path's BBR
  estimate (`bbr.Sender.RateCap`; pacing, cwnd and STARTUP's exit use the
  capped estimate): a path whose own estimate is below an equal share keeps
  it, since its own bottleneck limits it, and the others get equal shares of
  the rest. So the estimates add up to what the session measurably
  delivers. Equal shares keep every path loaded: a path in ProbeRTT or
  failing leaves only its share to take over, and rescues have capacity
  everywhere. (The scheduler still puts small transfers on the fastest
  path.)
* **When.** *B* is shared out for 32 rounds after the pipe was last seen
  full: every path delivers less than it sends, or every path sees a queue
  (below), while delivery has stopped growing (the session's by less than
  25% per round, and no path in STARTUP is growing with the session's
  total growing along: at the start all paths overshoot at the same
  moment, and a slow one may still be ramping). A probe doesn't count, as
  it fills every path's own bottleneck too. Paths behind a common
  bottleneck keep showing this, so nothing has to be detected or
  remembered. Independent paths stop showing it and are left alone again;
  they must be, because BBR's estimate for a path with random loss is what
  it *sends*, so the estimates of lossy paths never add up to what is
  delivered, and capping them would take from the clean paths (four paths
  with 20/10/5/1% loss lost a tenth of their throughput that way). Before
  the first time, *B* only shows what has been sent so far, and it lags a
  fast path that ramps up within one round of a slow one. A path in
  STARTUP gets a share when the pipe is full, which ends its STARTUP;
  otherwise it is left to find its rate.
* **Probing.** A capped path can't find more capacity alone, and doesn't
  try: its own 1.25 gain phases are off, its drain phases remain. Instead
  the session probes on all capped paths at once (`bbr.Sender.RateGain`):
  one round at 1.25·*B*, then one at 0.75·*B* while the ACKs come in, so
  that whatever the probe queued is gone again by the time its result is
  known. If the probe was delivered (*B* grew by half of what it added),
  the session probes again right away, from the second success in a row at
  1.5 and then 2 (one success may be a policer's burst allowance).
  Otherwise it waits twice as long before the next probe: 2, 4, then every
  8 rounds, which is as often as BBR itself probes. No path is driven
  harder than its own probes would drive it (1.25 times its own estimate):
  its share may be all that its own bottleneck carries.
* **Queues.** When every path that carried data saw a queue for a whole
  round (its lowest RTT sample at least `max(3 ms, shortest base RTT / 10)`
  above its base RTT, the lowest of the last 15–30 s), the bottleneck they
  all pass is congested.
  * If such rounds also delivered clearly less than *B* (their average, of
    up to three, is below 0.9·*B*), the capacity dropped, and *B* follows
    at once. A queue with all of *B* delivered changes nothing: it may not
    be ours, and following the delivery rate down for it would never end.
  * A maximum of noisy samples is a few percent high, and the paths' own
    drain phases then keep taking back what queues. So while *B* limits
    every path and all of them see a queue, *B* is trimmed: if the session
    delivered clearly less on average over 10 such rounds (below 0.98·*B*,
    loss aside), *B* is that average.
  * A queue that stays for two rounds is drained by a round at 0.75·*B*, at
    most every 8 rounds. If the queue doesn't shrink, it isn't the
    session's, and it becomes part of the paths' base RTT.
* **Known rates** (`up` / `down`, the Hysteria part). If an endpoint's
  uplink or downlink rate is configured, the session never sends more than
  `up` over all paths together, and it announces `down` to the peer (a
  control frame; older peers ignore it), which then sends no more than
  that. The limit is a ceiling for *B*, obeyed from the first packet (also
  in STARTUP, and with a single path) and never probed. Below it everything
  above still applies, so a line that is slower than configured is handled
  as without the option. Set them a little below the real rates (95%) and
  the bottleneck's queue stays empty.
* **Path failure.** When a path fails (suspect or closed), the others
  restart STARTUP to take over what it carried.

With a single path and no configured limit, the session rate is only
measured, and the path's BBR is on its own.

The session's stats log line shows `rate=<B>` (and `limit=`); a path whose
estimate is capped shows its own estimate as `own=`. `no_coupling` turns the
session rate off (configured limits still apply). Measurements: see
[../benchmarks/multipath.md](../benchmarks/multipath.md#common-bottlenecks-an-endpoints-line-is-slower-than-the-relays).

## Limitations

* The session rate treats all paths of a session alike. If only some of
  them share a bottleneck while another has spare capacity of its own, the
  pipe is rarely "full" for all of them at once, and the paths behind the
  common bottleneck behave as uncoupled paths.
* Independent paths with heavy random loss or outages look full together
  now and then, and their estimates are then capped for 32 rounds although
  nothing is shared: about 5–8% less bulk throughput than with
  `no_coupling` in the benchmarks. If the paths are
  known to be independent, `no_coupling` avoids that.
* A policer is followed the way BBR follows one: by the highest of the
  delivery rates it lets through, which is a few percent above its rate,
  and by probes every 8 rounds. About 9% of the packets were dropped at a
  50 Mbit/s policer (24% before the session rate; a variant that judged
  loss directly got to 4%, but took a tenth of the throughput from paths
  with random loss). A buffer of a few milliseconds behaves alike. With
  `down` (or `up`) set below the policed rate nothing is dropped.
* A session probe adds a quarter of a round of queue at a bottleneck that
  is already full, for two rounds in ten: tens of milliseconds for the
  other streams, which the paths' own probes cost before as well. With
  `up` / `down` set there are no probes.
* Queues are judged by round-trip times, which also grow with a queue in
  the ACK direction (an upload filling the receiver's uplink while it
  downloads). The session rate therefore never comes down for a queue
  alone, and a queue that draining doesn't shrink is taken into the base
  RTT; until then (a few rounds) the pipe may look full when it isn't.
* A sudden rate drop still costs one window's worth of queue: what is already
  in flight (up to 2·BDP at the old rate) has to drain at the new rate before
  the queue is gone (~1 s for an 8× drop at 50 ms RTT), and smaller drops
  (< 25%) are left to BBR's 10-round filter.
* Against a loss-based flow filling a deep buffer, BBRv1 gets a minority
  share (~20% against Reno in the simulator), as before.
