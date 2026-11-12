# Congestion control (`internal/cc`, `internal/mp/coupling.go`)

Congestion control has two layers, each for one job:

* **BBR version 3 on every path** finds what that path carries and obeys
  the loss on it.
* **The session rate** makes the paths' estimates add up to what the
  session delivers over all of them together.

This page says why it takes both, what each does, and what the result
does badly. Measurements:
[single path](../benchmarks/congestion-control.md),
[several paths](../benchmarks/multipath.md).

## Why two layers

What multipath congestion control should do has a well-understood answer
(network utility maximisation, Kelly and Voice; the coupled controllers of
MPTCP: LIA in RFC 6356, OLIA, BALIA): every path has a controller that
answers to the congestion signal of its own path, and the controllers are
coupled so that paths through one bottleneck behave there like one flow.

Each layer was tried without the other, and each fails in one case:

* **Only a session layer over a controller that ignores loss** (BBR
  version 1, the earlier design). Behind a policer or a shallow buffer
  every path keeps sending what it once measured, and the paths together
  lose the excess all the time. The session layer can cap the estimates
  but only finds a policer's rate by what it lets through.
* **Only BBRv3 on every path.** BBR's estimate of a path is the highest
  delivery rate of its last probes. Flows through one bottleneck take
  their best moments at different times (one gets more while another
  drains or probes its round trip), so their estimates add up to more
  than it carries. Where it drops, BBRv3 sees that. Where it has a deep
  buffer nothing is lost, and the paths keep a standing queue of one to
  two round trips there: several BBR flows always do, whoever they belong
  to.

Five relays of 200 Mbit/s at 150 to 250 ms round trip, in front of a
receiver's line of 500 Mbit/s; goodput of a download, and what a small
request takes while it runs (152 ms when idle):

| the receiver's line | BBRv1 + session rate | BBRv3 only | **BBRv3 + session rate** |
|---|---|---|---|
| a policer | 296 Mbit/s, 12.7% dropped | 381–396 Mbit/s, 3.5% dropped | **398–412 Mbit/s, 1.6–2.7% dropped** |
| 100 ms of buffer | 383 Mbit/s, 164 ms | 388 Mbit/s, 225 ms | **436 Mbit/s, 176 ms** |
| 500 ms of buffer | 441 Mbit/s, 166 ms | 414 Mbit/s, 400 ms | **449 Mbit/s, 205 ms** |

(Runs of 30 s: three with the policer for the last two columns, single runs otherwise.) On a 20 Mbit/s
uplink with a 300 ms buffer in front of three relays, a request during an
upload took 31 ms with the session rate and 143 ms with BBRv3 alone.

## BBRv3 on a path (`internal/cc/bbr/bbr3.go`)

The controller is a port of
[draft-ietf-ccwg-bbr](https://datatracker.ietf.org/doc/draft-ietf-ccwg-bbr/)
(version 05), function by function and with the draft's names, on the
delivery rate sampler that was already vendored (from Hysteria, MIT; a port
of Chromium's). In short:

| | what | why |
|---|---|---|
| `max_bw` | the highest delivery rate of the last two probing cycles | what the path carries |
| `min_rtt` | the lowest round trip of the last 10 s | the path without a queue; with `max_bw` it gives the BDP, the data that fits in the path |
| `inflight_longterm` | the volume in flight at which a probe last lost more than 2 % | a bound that the next probes approach carefully: the buffer, or other flows' share, ends there |
| `bw_shortterm`, `inflight_shortterm` | after a round trip with loss between probes: what was just delivered, at least 0.7 of the bound before | the path carries less than it did (a rate drop, a new competitor); reset at the next probe |
| Startup | 2.77 times the estimate, until the rate stops growing by a quarter for three rounds, or a round loses more than 2 % | finds the rate in about log2 of it round trips |
| ProbeBW | cycles of: **down** (0.9) until the queue is gone and 15 % of the bound is left free, **cruise** (1.0) for 2–3 s, **refill** (1.0, one round), **up** (1.25) until the rate stops growing or the loss says stop | sends at the rate, probes for more now and then, drains what the probe queued |
| ProbeRTT | every 5 s without a new lowest round trip: half a BDP in flight for 200 ms | sees the round trip without its own queue |

What it adds to version 1 is the loss response: a path stays under a
policer or a small buffer by itself, and only probes above it for a few
round trips every few seconds. A bound found this way is not a rate cut
by a factor for every loss, as in Reno or CUBIC, so random loss of up to
about 1 % costs little.

### Where the port differs from the draft's pseudocode

Each of these is marked in the code.

* **Losses are judged per acknowledgement.** The draft goes through lost
  packets one by one; quic-go reports the losses of an ACK frame together.
  The loss rate is taken over the round trip of the packet just
  acknowledged, as in Chromium's BBRv2.
* **The probe's feedback phase ends, and the bandwidth filter advances,
  once per cycle.** The draft's text says so (5.5.6) but its pseudocode
  does not end the phase; Linux does. And where the round after a probe
  cannot advance the filter because its sample was application-limited (a
  ProbeRTT cut the probe short), the start of the next probe does, as in
  Chromium. Without this an estimate could outlive the rate it was measured
  at for good: after an 8-fold rate drop on a deep buffer, round trips
  become so long that every probe is cut short.
* **Start-up ends on loss counted in ACK frames** that reported loss (six
  in a round), where the draft counts lost sequence ranges.
* **Not there**: ECN (QUIC paths through UDP relays rarely carry it), the
  undo of a loss response found to be spurious (quic-go does not report
  that), and the window cut on a retransmission timeout (quic-go sends its
  probe packets regardless of the window).

### What the session tells a path's controller

* **Whether the application has data** (`AppLimitedHint`). A path that the
  scheduler leaves idle carries only ACKs and pings, and one that it feeds
  with gaps delivers less than it could. BBR must not take either for the
  path's capacity, and quic-go does not tell its congestion controller
  whether there was data to send. Every moment in which the scheduler has
  nothing for a path counts, however short, as a TCP sender notes the
  moment its send buffer runs empty.
* **When it may enter ProbeRTT** (`ProbeRTTGate`): one path of a session
  at a time, so that the others carry the traffic meanwhile. A path whose
  turn it is not enters when the gate is free.
* **Its share of the session rate** (`RateCap`, `RateGain`; next section).
  The share bounds the path's estimate, and what it sends in any state.
  While it does, the path does not run its own probing cycle: it paces at
  the session's gain.
* **Its loss tolerance** (`LossTolerance`; below).

## Session rate: the paths' estimates add up to what is delivered

The relays are usually independent, but the paths are not: every path
leaves through the sending endpoint's uplink and arrives through the
receiving endpoint's downlink, and those are often slower than the relays
(a 100/20 Mbit/s home line in front of three relays in data centres).

So the session measures what it delivers over all paths together, and
shares that out to the paths (`internal/mp/coupling.go`). Like BBR, it
works with delivery rates. Delay only supports what the rates say, because
a round-trip time also grows with queues that are not the session's (the
other direction's: a download's ACKs behind an upload on the same uplink;
or other people's traffic). Loss is each path's controller's business;
the session only sees what it leaves of the delivery rate. And congestion,
for the session, is only what *all* paths show at once: one path's own
queue or loss is that path's business.

* **Rounds and delivery rate.** The session works in rounds of the largest
  smoothed RTT of its paths (50 ms to 1 s, at most twice a path's base RTT).
  The delivery rate of a round is how fast the bytes *sent in it* came out
  at the other end: per path, what was acknowledged between the round's
  start and end, each shifted by that path's RTT and timed from ACK to ACK
  (`cc.Meter.Mark`), and never more than was sent. It is known one round
  later.
* **Session rate.** *B* is the highest delivery rate seen. It stands while
  what is sent is delivered: that only shows a lower bound (as do rounds in
  which the paths had no data to send), and counting such rounds would let
  *B* sink whenever the paths happen to send a little less than their
  shares. Rounds in which *every* path delivered clearly less than it sent
  (below 0.98, less its loss tolerance: a path's known loss is no sign of
  a bottleneck) show what a bottleneck that all of them pass carries, and
  *B* is the maximum of the last ten of those.
* **Shares.** *B* is shared out max-min fairly as a cap on each path's
  estimate: a path whose own estimate is below an equal share keeps it,
  since its own bottleneck limits it, and the others get equal shares of
  the rest. So the estimates add up to what the session measurably
  delivers. Equal shares keep every path loaded: a path in ProbeRTT or
  failing leaves only its share to take over, and rescues have capacity
  everywhere. (The scheduler still puts small transfers on the fastest
  path.)
* **When.** *B* is shared out for 32 rounds after the pipe was last seen
  full: every path delivers less than it sends, or every path sees a queue
  (below), while delivery has stopped growing (the session's by less than
  25% per round, and no path in start-up is growing with the session's
  total growing along). A probe doesn't count, as it fills every path's
  own bottleneck too. Paths behind a common bottleneck keep showing this,
  so nothing has to be detected or remembered. Independent paths stop
  showing it and are left to their own controllers again. A path in
  start-up gets a share when the pipe is full, which ends its start-up;
  otherwise it is left to find its rate.
* **Probing.** A capped path can't find more capacity alone (that would
  only move its share), so the session probes on all capped paths at
  once: one round at 1.25·*B*, then one at 0.75·*B* while the ACKs come
  in, so that whatever the probe queued is gone again by the time its
  result is known. If the probe was delivered (*B* grew by half of what
  it added), the session probes again right away, from the second success
  in a row at 1.5 and then 2 (one success may be a policer's burst
  allowance). Otherwise it waits twice as long before the next probe: 2,
  4, then every 8 rounds. No path is driven harder than 1.25 times its
  own estimate: its share may be all that its own bottleneck carries.
* **Queues.** When every path that carried data saw a queue for a whole
  round (its lowest RTT sample at least `max(3 ms, shortest base RTT / 10)`
  above its base RTT, the lowest of the last 15–30 s), the bottleneck they
  all pass is congested.
  * If such rounds also delivered clearly less than *B* (their average, of
    up to three, is below 0.9·*B*), the capacity dropped, and *B* follows
    at once. A queue with all of *B* delivered changes nothing: it may not
    be ours, and following the delivery rate down for it would never end.
  * A maximum of noisy samples is a few percent high. So while *B* limits
    every path and all of them see a queue, *B* is trimmed: if the session
    delivered clearly less on average over 10 such rounds (below 0.98·*B*,
    loss aside), *B* is that average.
  * A queue that stays for two rounds is drained by a round at 0.75·*B*, at
    most every 8 rounds. If the queue doesn't shrink, it isn't the
    session's, and it becomes part of the paths' base RTT.
* **Known rates** (`up` / `down`). If an endpoint's uplink or downlink
  rate is configured, the session never sends more than `up` over all
  paths together, and it announces `down` to the peer (a control frame;
  older peers ignore it), which then sends no more than that. The limit is
  a ceiling for *B*, kept from the first packet (also in start-up, and
  with a single path) and never probed. Below it everything above still
  applies, so a line that is slower than configured is handled as without
  the option. Set them a little below the real rates (95%) and the
  bottleneck's queue stays empty.
* **Path failure.** When a path fails (suspect or closed), the others
  start up again to take over what it carried.

With a single path and no configured limit, the session rate is only
measured, and the path's controller is on its own.

The session's stats log line shows `rate=<B>` (and `limit=`); a path whose
estimate is capped shows its own estimate as `own=`.

## Loss tolerance

BBRv3 takes loss above 2 % for congestion. It cannot tell otherwise, and
on a path that drops packets at random (a bad radio link, an overloaded
relay that drops rather than queues) that is wrong: the controller cuts
its bounds again and again, and the path is left with a few percent of
its rate. A path that reorders packets is in the same position: quic-go
declares a packet lost when three later ones have arrived, and does not
tell the controller when it turns up after all.

For paths known to be like that, `loss_tolerance` (a share, e.g. `10%`)
says how much loss is *not* congestion:

* while probing, it replaces the 2 % threshold (if higher);
* between probes, where the draft answers to any round with loss, a round
  only counts if it lost more than the tolerance.

Nothing else changes: the path's rate is still what BBR measures that the
path delivers. That is the difference to Hysteria's Brutal, which sends at
a configured rate whatever happens; a tolerance of `100%` ignores loss as
Brutal does, but still finds the rate itself.

It is set per endpoint (`loss_tolerance`, `--loss-tolerance`) or per path
(in a `servers` entry of the dialing side). Each end announces its value
for a path to the other, and a path uses the larger of the two, so one
entry covers both directions.

Three things to know:

* **Set it to about twice the path's loss rate.** Loss is judged over one
  round trip, a few hundred packets, and what is lost of those varies
  around the path's average. A tolerance just above the average is
  exceeded now and then, and each time costs rate. Four paths of 50 Mbit/s with 20, 10, 5 and 1 % loss carried
  44 Mbit/s without tolerances, 70 to 105 with 25, 15 and 10 %, and 140 to
  170 with 40, 30 and 20 % (the earlier controller, which ignored loss:
  145 to 153).
* **The tolerance is what a policer or a full buffer may also take.** A
  path with a tolerance of 10 % that is limited by a policer loses up to
  10 % there before its controller gives way, and the session rate does
  not count that loss either. Set it only on paths that need it. With 100 % on all five paths
  of the policer example above: 288 Mbit/s with 11.6 % dropped, instead
  of about 400 with 2 %.
* **It helps up to about 20 % loss.** BBR finds a rate by sending 1.25
  times its estimate and seeing more delivered. With more than 20 % loss
  even the probe delivers less than the estimate, and the estimate decays
  whatever the tolerance.

## Limitations

* **Random loss above 2 %, or reordering, without a tolerance** collapses
  a path's rate (one path of 100 Mbit/s at 150 ms: 41 Mbit/s at 2 % loss, 4 at 5 %, 1 at
  10 %, and 20 with 2 % of the packets reordered). BBRv1 did not care; that is the price of
  obeying policers. Use `loss_tolerance`.
* **The session rate treats all paths of a session alike.** If only some
  of them share a bottleneck while another has spare capacity of its own,
  the pipe is rarely "full" for all of them at once, and the paths behind
  the common bottleneck are left to their own controllers: they keep a
  queue there if its buffer is deep.
* **A single path on a short round trip** (a few tens of milliseconds)
  keeps a queue of up to one round trip in a deep buffer between its
  probes: the highest of many delivery rate samples is a few percent
  above the path's rate when acknowledgements arrive in batches that are
  large against a round trip's data, and BBR paces only 1 % below its
  estimate. On long round trips the samples are finer and there is no
  queue.
* **Start-up overshoot.** Every path doubles its rate each round trip
  until it sees the limit, so for a few round trips a new session sends up
  to twice what a common bottleneck carries. With `up` / `down` set it
  does not.
* **A rate drop on one path with a deep buffer** is only seen when the
  bandwidth filter has forgotten the old rate: two probing cycles, 5 to
  10 s, longer if the drop is large. With a shallow buffer the loss shows
  it within a round trip or two, and a drop of a line that all paths
  pass is seen by the session rate at once.
* **A session probe** adds a quarter of a round of queue at a bottleneck
  that is already full, for two rounds in ten: tens of milliseconds for
  the other streams. With `up` / `down` set there are no probes.
* **Queues are judged by round-trip times**, which also grow with a queue
  in the ACK direction (an upload filling the receiver's uplink while it
  downloads). The session rate therefore never comes down for a queue
  alone, and a queue that draining doesn't shrink is taken into the base
  RTT; until then (a few rounds) the pipe may look full when it isn't.
* `congestion: cubic` keeps quic-go's built-in CUBIC on every path, for
  comparison; none of the above applies to it, and the session rate does
  not act on it.
