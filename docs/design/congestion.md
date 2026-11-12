# Congestion control (`internal/cc`, `internal/mp/ratelimit.go`)

Every path has its own QUIC connection and its own congestion controller:
BBR version 3, ported from the IETF draft. The session adds three small
things around the controllers and no rate control of its own. This page
says why it is built that way, what the controller does, where the port
differs from the draft, and what it does badly. Measurements:
[single path](../benchmarks/congestion-control.md),
[several paths](../benchmarks/multipath.md).

## A controller per path

What multipath congestion control should do has a well-understood answer.
The theory (network utility maximisation, Kelly and Voice; the coupled
controllers of MPTCP: LIA in RFC 6356, OLIA, BALIA) and the practice
(MPTCP, MP-QUIC) agree on the structure:

* **each path has a controller that answers to the congestion signal of
  its own path**, and
* the controllers may be *coupled in how fast they grow*, so that a
  session with several paths through one bottleneck takes no more of it
  than a single flow would. That is a fairness policy, not something the
  session needs in order to work.

Nothing above the paths has to find out which paths share a bottleneck. A
bottleneck that several paths pass (an endpoint's own line, a policer in
front of it) signals on each of them: every path's controller sees the
loss or the queue that it causes there, and gives way. The session's send
rate is the sum of what the paths' controllers allow.

mpquic follows that structure without the coupling: the paths of a
session behind one bottleneck compete there like so many separate flows
(see [limitations](#limitations)).

The earlier design did not fit it. Its per-path controller was BBR version
1, which ignores packet loss; behind a policer or a shallow buffer, every
path kept sending what it had once measured, and the paths together lost
the excess all the time. A layer in the session measured what all paths
delivered together and capped the paths' estimates to that. Working from
the top down, that layer had to infer which constraint was binding where
(the common line, or one relay), and each case it got wrong needed another
rule. The cause was the controller, so the controller was replaced and the
layer removed.

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

What makes it right for this job is the loss response: a path stays under
a policer or a small buffer by itself, and only probes above it for a few
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
* **The application-limited mark comes from the session** (below); quic-go
  does not tell its congestion controller whether there was data to send.

## What the session adds

1. **Whether the application has data.** A path that the scheduler leaves
   idle carries only ACKs and pings. BBR must not take the low delivery
   rate of those for the path's capacity, so the scheduler tells every
   path's controller when it has nothing queued for it
   (`AppLimitedHint`): those samples do not lower the estimate.
2. **ProbeRTT one path at a time** (`ProbeRTTGate`). A path in ProbeRTT
   carries half its rate or less for a quarter of a second. If the paths of a
   session did that together, every stream would stall; one at a time, the
   others carry the traffic. A path whose turn it is not waits and enters
   when the gate is free.
3. **A configured rate** (`up` / `down`, `ratelimit.go`). If the rate of
   an endpoint's line is known, the session never fills it to find out.
   The limit (ours for sending; the peer announces what it wants to
   receive) is shared out max-min fairly as bounds on the paths' pacing
   rates. How much a path can use is known exactly: outside its start-up,
   BBR never sends faster than 1.25 times its estimate. A path that can
   use less than an equal share is bound to that, the others share the
   rest equally, and as a path's estimate grows so does its bound, up to
   an equal share. The bounds never add up to more than the limit, in any
   state of the controllers.

## Loss tolerance

BBRv3 takes loss above 2 % for congestion. It cannot tell otherwise, and
on a path that drops packets at random (a bad radio link, an overloaded
relay that drops rather than queues) that is wrong: the controller halves
and halves again, and the path is left with a few percent of its rate.

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

Two things to know:

* **The tolerance is what a policer or a full buffer may also take.** A
  path with a tolerance of 10 % that is limited by a policer loses up to
  10 % there before its controller gives way. Set it only on paths that
  need it, and not much above their loss rate.
* **It helps up to about 20 % loss.** BBR finds a rate by sending 1.25
  times its estimate and seeing more delivered. With more than 20 % loss
  even the probe delivers less than the estimate, and the estimate decays
  whatever the tolerance.

## Limitations

* **Random loss above 2 % without a tolerance** collapses a path's rate
  (to a fifth at 5 % loss, a few percent at 10 %, in the simulator and in
  the [benchmarks](../benchmarks/congestion-control.md)). BBRv1 did not
  care; that is the price of obeying policers. Use `loss_tolerance`.
* **No coupling.** N paths behind one bottleneck take the share of N BBR
  flows from other traffic there. Among themselves they share it unevenly
  at times (BBR flows do), which the scheduler evens out for the streams.
  Their probes are not synchronised: at a policer, one path or another is
  probing most of the time, and a few percent of the packets are lost
  there for good.
* **Start-up overshoot.** Every path doubles its rate each round trip
  until it sees the limit, so for a few round trips a new session sends up
  to twice what a common bottleneck carries. With `up` / `down` set it
  does not.
* **A rate drop on a deep buffer** is only seen when the bandwidth filter
  has forgotten the old rate: two probing cycles, 5 to 10 s, longer if the
  drop is large (the queue makes round trips long). Until then the path
  keeps up to two BDPs of the old rate in flight. With a shallow buffer
  the loss shows it within a round trip or two. The earlier controller had
  a rule of its own for this case (a queue that stays while less is
  delivered than estimated resets the estimate within a second); BBRv3 has
  none.
* **Below a loss bound, 15 % is left unused.** Where a path's rate is set
  by a policer or a small buffer, BBRv3 cruises at 85 % of the volume at
  which it last saw loss, to leave room for others, and its probes fill
  the rest only briefly.
* **ProbeRTT costs a little throughput**: a path at half a BDP for 200 ms
  every 5 s, unless its round trip keeps finding new lows by itself.
* `congestion: cubic` keeps quic-go's built-in CUBIC on every path, for
  comparison; none of the above applies to it.
