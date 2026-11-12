# Congestion control (`internal/mp/rate.go`, `internal/cc`)

A session has **one** congestion controller for all its paths. It decides
one rate for the session; the paths only carry their share of it. This page
says what the controller does and, for every rule, why it is there and where
it comes from. Measurements:
[common bottlenecks](../benchmarks/multipath.md#common-bottlenecks-an-endpoints-line-is-slower-than-the-relays),
[single path](../benchmarks/congestion-control.md).

## Why one controller

Every path is a QUIC connection of its own, and the obvious design gives
each its own congestion controller. That is what mpquic did first: a BBR per
path, and later a layer in the session that capped the BBRs' estimates so
that they added up to what the session delivered.

But the paths of a session are not independent. Every one leaves through
the sending endpoint's uplink and arrives through the receiving endpoint's
downlink, and those are often slower than the relays (a home line in front
of relays in data centres). A controller per path has each path probe for
the whole line. Five paths through 200 Mbit/s relays in front of a
500 Mbit/s policer each found "200", started up on their own and sent
1.3–1.7 Gbit/s into a line that passes 0.5; in the steady state the relays
still carried a fifth more than was delivered, because a controller that
only sees its own path cannot know what the others lose.

So the session has the rate, and the scheduler decides which path carries
what (see [session.md](session.md)). There is no other congestion control:
the per-path BBR is gone.

## The design in short

The controller is BBR's design, applied to the session instead of a
connection, with BBRv2's use of loss. Two things are added that a single
connection does not need: a baseline for each path's random loss, and a
rate for a path whose relay is slower than its share.

| | rule | from |
|---|---|---|
| **rate** | *B* is the highest rate at which the session's data was delivered in one of the last ten rounds | BBR's bandwidth estimate |
| **start-up** | send twice what is acknowledged, until *B* stops growing, the round trips rise, or loss shows | slow start; BBR's "full pipe"; HyStart++; BBRv2 |
| **probe** | every eight rounds one round at 1.25·*B*, then as much less; a probe that was delivered is followed at once by a larger one | BBR's probe cycle |
| **drain** | after a start-up, a probe or a cut: send less until no more is in flight than *B* needs | BBR's drain |
| **loss** | two rounds that lose beyond the paths' baselines, on all paths: *B* comes down by the share that was lost; probes that are answered by loss become rare | BBRv2's loss bound; exponential back-off |
| **paths** | a path paces at its share and may have two round trips' worth of it in flight | BBR's window |
| **shares** | *B* is shared equally; a path whose window has held it back has a rate of its own, found by the same rules, and gets no more than that | max-min fairness |

Everything below is these rules in detail.

## Rounds, and what is measured

The session works in **rounds**. A round is as long as the longest smoothed
round trip of the active paths (but no longer than twice that path's base
RTT, so that a queue does not slow the controller down), at least 50 ms and
at most 1 s: what is sent in one round is acknowledged in the next, and 50 ms
is enough packets to measure a rate and a loss share.

Every path's controller (`cc.Meter`) counts each packet for the round it was
sent in, and each acknowledgement and loss for the round of its packet,
however much later it is known. So at the end of round *k* the session knows
exactly what round *k*−2 sent and lost (its losses took a second round to
be noticed), and what round *k*−1 lost so far. From that, and from the bytes
acknowledged during the round, it has:

* **the delivery rate** of a round: the rate at which acknowledgements came
  in during it, but no more than was sent in it or the round before (whose
  packets they acknowledge). Acknowledgements that arrive in a bunch are not
  capacity; BBR takes the longer of the send and the acknowledgement
  interval for the same reason. A round counts only if the session had the
  data to send what it could (85% of it): what an application-limited round
  delivers says nothing about the pipe. This is BBR's rule for
  application-limited samples.
* **the loss of a round**, per path, against the path's baseline (below).
* **a queue**: a path sees one if its round trips are above its base RTT by
  an eighth, at least 4 ms and at most 16 ms. These are HyStart++'s
  thresholds (RFC 9406): less is jitter. The base RTT is the lowest of the
  last 10 s (BBR's window for its minimum RTT): a connection's lifetime
  minimum would make a path whose route got longer look queued forever.

### Loss is judged against a baseline

mpquic is meant to run over lossy paths, so loss as such cannot mean
congestion. What tells the two apart is that congestion loss comes with the
rate and random loss does not. So every path has a **baseline**: the share
it loses when the pipe is clearly not full. A round shows that if the
session, and the path, sent less than 0.9 of what they put out two rounds
later (the round after a probe; a start-up's rounds while the rate still
doubles). The baseline is the loss of such rounds together, later ones
counting more, and it is known once they add up to 200 packets: one small
round says little (two lost of a hundred is as likely as none).

A path's round is **lossy** if it lost more than the baseline by 2% of what
it sent (BBRv2's loss threshold), and by more than chance explains: three
standard deviations, for losses that come in bursts of four packets, as
random loss on real links does. A round is lossy *for the session* if that
holds for the paths that carried three quarters of it, and for all of them
together. Loss on one path only is that path's own business (below).

## The session rate *B*

**Start-up.** The session sends twice the rate at which acknowledgements
are arriving (measured continuously, over the last round's length), so the
rate doubles every round trip, as slow start does with its window. It ends

* when *B* has grown by less than a quarter in three rounds: the pipe is
  full (BBR);
* or when every path has seen a queue for a round's length: the common
  bottleneck is filling its buffer (HyStart). A whole round, because the
  bottleneck was then busy all that time, so what came out in it is what it
  carries; a session's first burst alone raises the round trip for a
  moment;
* or when the last two rounds are lossy on all paths, by at least 32
  packets: BBRv2 ends its start-up on loss in the same way, and asks for
  eight loss events, so that one burst of random loss before the baselines
  are known does not end it.

The last two are checked at every tick, not only at the end of a round: a
round at twice the pipe's rate queues or loses a round's worth. *B* is then
what the pipe puts out at that moment, and the session drains. What leaves
the sender overshoots by up to a factor of two for about one round trip, as
with any slow start; what arrives is never more than the line carries.

A queue or loss ends the start-up only while the session sends at least
what was delivered. One with little to send (the acknowledgements of a
download) sees the other direction's queue and loss, not its own.

**At *B*.** After the start-up the session sends at *B*, and *B* is the
highest delivery rate of the last ten rounds that counted. A pipe delivers
no faster than its bottleneck, so the highest recent delivery rate is the
bottleneck's rate; ten rounds outlast a probe cycle, so a rate that a probe
found is confirmed or forgotten by the next. (Rounds that do not count do
not age the others: *B* is not forgotten while the application is idle.)
When the capacity drops, *B* follows after ten rounds at the latest;
meanwhile the paths' windows hold what queues to a round trip's worth.

**Probes.** Every eight rounds one round sends 1.25·*B*, and the next one
as much less, so that what the probe queued is gone again: BBR's cycle.
Three rounds later, when the probe's packets have been acknowledged and
their losses are known, the session judges it:

* If *B* has risen by at least a quarter of what the probe added, there is
  more. The next probe follows at once and asks for twice as much more
  (1.5, then 2): a capacity that is far away is found in a few steps, as in
  slow start. (Why a quarter: paths differ in delay, so a probe's packets
  reach the receiver interleaved with those of the rounds around it, and
  only part of what a delivered probe added shows in any one round, a third
  at least.)
* If not, *B* is what it was before the probe. A rate that kept every
  slightly higher sample would creep above what the pipe carries and keep a
  queue there.
* If the probe's round was lossy, loss answered it (below).

**Drains.** After a start-up, a probe or a cut the session sends less: half
of *B* after a start-up or a cut (a round's worth may be queued), and after
a probe as much less as the probe sent more. A drain lasts until no more is
in flight than *B* needs over the paths' round trips without a queue, which
is BBR's drain target, and two rounds at most: round trips also grow with
queues that are not ours, and sending less does not shorten those.

**Loss.** Two lossy rounds in a row that were paced at *B* mean that *B* is
more than the pipe takes: a policer, or a buffer that is full. What was not
lost is what it takes, so *B* comes down by the share that was lost beyond
the baselines (a policer passes its rate of whatever arrives faster, and so
does a full buffer), but not below half of *B* at once. This does not depend
on the session having sent at *B* all the time: what it sent, it paced at
*B*. Two rounds, because a policer lets a burst pass before it drops.

Probing such a ceiling costs loss every time, so each time loss answers at
about the same rate (within 15%), the wait for the next probe doubles, from
8 rounds up to 128. At another rate it starts over, and it also starts over
when *B* has fallen far below the ceiling (the capacity dropped: what was
learned there does not hold probes back here).

## The paths

A path's congestion controller (`cc.follower`) decides nothing about rates.

* It **paces** at the rate the session gives it: its share of *B* times the
  round's gain.
* It **bounds what is in flight** to what its share needs over two round
  trips without a queue (on short paths: one round trip and 25 ms, quic-go's
  maximum ACK delay; never less than quic-go's initial window of 32
  packets). This is BBR's window. A path that carries its share has one
  round trip's worth in flight, so the second is room for a queue of one
  round trip. A path that carries less than it is given (a slow relay, a
  bottleneck whose capacity dropped) queues that much and no more, and a
  path that stops delivering stops sending.

The window is for the path's share of *B* as it was measured, not for the
rate of the moment: a round that sends less to drain a queue must not find
its window shrunk below what is in flight, and before the session has
measured a rate the window is the initial window, whatever the first round
is paced at. And it is for what the path is given, not for what it
delivers: a window that follows the delivery rate shrinks when the path
delivers less because the window holds it back, and so shrinks itself.

### Shares

*B* (times the round's gain) is shared out among the usable paths
**max-min fairly**: equal shares, except that a path gets no more than its
own rate (next section), and the others share the rest. Equal shares keep
every path loaded, so a failing path leaves only its share to be taken
over, and loss recovery always has capacity on every path. Which stream's
data goes on which path is the scheduler's decision, not the rate
controller's: small transfers still go to the fastest path.

A path that is not usable (suspect) keeps a minimal share for its pings.
Before the session has a rate, every path is paced at twice the initial
window per round trip: the path's shortest one (the smoothed RTT begins with
the handshake's, which may be many times the path's), or 50 ms until there
is a sample. A path gets its share when it joins, not at the session's next
tick.

### A path's own rate

A relay may be slower than its path's share of *B*, or police. Such a path
has a **rate of its own**, and it is the same thing as *B*, for one path:
the highest rate at which the path delivered in the last ten rounds, found
and probed by the same code (`cpRate`).

* Only rounds in which the path's **window held it back** count (and rounds
  in which it delivered more than its rate): a path that delivers what it
  is given has shown no limit, and holding it to what it happened to carry
  would freeze whatever split the paths began with. The rate is known after
  three such rounds: acknowledgements that stall for a moment hold a path
  back, too.
* **Loss**: a path that is lossy for two rounds at its plain share, while
  the session is not, has its rate come down by what it lost, like *B*.
* **Probes**: every eight rounds a path that is held to its own rate gets a
  quarter more for a round and then as much less until what that queued is
  gone, with the same judgement and the same back-off as the session's
  probes (if a quarter more is still within an equal share of *B*; a path
  that gets about an equal share is probed with the session). The other paths get that much less and more, so the probe costs a
  common bottleneck nothing, and it shows what the path carries even when
  the common bottleneck is full.
* A path's rate is forgotten when an equal share of *B* has been less than
  it for ten rounds, and at the end of a start-up (what a path delivered
  while the rates doubled says little).

In the start-up the paths' rates are not applied: they are two rounds old,
and the rates double every round. A path that cannot follow is held by its
window.

## Configured rates

`up` / `down` (see [configuration](../configuration.md)) are ceilings for
*B*: an endpoint's own uplink, and what the peer is told about the downlink.
At the ceiling the session does not probe. With both set a little below the
line's rates there is no overshoot at the start, nothing is dropped at a
policer, and nothing queues.

## The constants

| constant | value | what | why this value |
|---|---|---|---|
| round | longest RTT, 50 ms–1 s | the unit of all measurements | one round trip: what is sent is acknowledged a round later |
| `cpRounds` | 10 | rounds whose highest delivery rate is *B* | BBR |
| `cpStartGain` | 2 | start-up | slow start |
| `cpStartGrowth`, `cpStartRounds` | 1.25, 3 | the pipe is full | BBR |
| `cpLossEvents` | 8 bursts | loss that ends a start-up | BBRv2 |
| `cpProbeGain`, `cpProbeEvery` | 1.25, 8 | a probe and its cycle | BBR |
| `cpMaxProbeGain` | 2 | probes that keep finding more | slow start's rate of growth |
| `cpMaxHold` | 128 rounds | the longest wait between probes | a policed line is probed every 6–30 s |
| `cpSameCeil` | 15% | "the same ceiling" | wider than a probe's error, narrower than a probe |
| `cpMaxDrain` | 2 rounds | the longest drain | a round's worth is queued at most (the windows) |
| `cpLossExcess` | 2% | loss that counts | BBRv2 |
| `cpBurst` | 4 packets | random loss comes in bursts | counted singly, every burst would be significant |
| `cpCut` | 0.5 | the most *B* comes down at once | TCP's halving |
| `cpCommon` | 75% | "all paths" | all but a small one |
| `cpBasePkts`, `cpBaseKeep` | 200, 0.75 | a baseline is known; older rounds fade | 2% of 200 is four packets, one burst |
| `cpClearly` | 0.9 | "clearly less" | beyond a round's measuring error |
| `cpBusyFrac`, `cpStartBusy` | 0.85, 0.5 | the session had the data for the round | at twice the rate, sending what was delivered is half |
| `cpOwnKnown` | 3 rounds | a path's own rate is known | longer than a stall of acknowledgements |
| queue threshold | RTT/8, 4–16 ms | a path sees a queue | HyStart++ (RFC 9406) |
| `cpBaseWindow` | 10 s | base RTT | BBR |
| window | 2 round trips, ≥ 32 packets | what a path may have in flight | BBR; quic-go's initial window |

## Limitations

* **Start-up overshoots**, like any slow start: the sender sends up to
  twice what the line carries for about one round trip before the first
  loss or queue can be seen. With `up` / `down` configured there is no
  overshoot.
* **A drop in capacity is followed late.** *B* is the highest delivery rate
  of ten rounds, so it follows a drop after up to ten rounds, unless loss
  shows first (two rounds). Meanwhile the paths' windows hold the queue to
  about a round trip's worth, which is latency for every stream. A line
  whose rate changes all the time (a radio link) is carried less evenly
  than a steady one. This is BBR's behaviour, and the price of a rate that
  does not follow every dip.
* **Probing a policer costs loss.** Each probe that hits the ceiling loses
  up to a fifth of one round. The back-off makes that rare (one probe in
  128 rounds once the ceiling is confirmed), at the price of finding more
  capacity late when the policer's rate goes up.
* **Loss that every path has and that changes** (all paths over the same
  radio link) is taken for congestion when it rises: *B* comes down by
  about the loss rate, until rounds below *B* (after the cut, after every
  probe) have put it into the baselines.
* **A queue that is not ours** in front of the acknowledgements (an upload
  filling the uplink while a download runs) lengthens the round trips. A
  path whose round trip more than doubles that way is held back by its
  window until the base RTT has caught up, after 5–10 s.
* **A relay slower than its share** keeps a queue of up to a round trip
  (what its window allows) while its rate is being found, and part of that
  afterwards: a path that is held to its own rate sends exactly what the
  relay takes, so what a probe queued is drained, but not more.

## Seeing what it does

The session's stats log line shows `rate=<B>` (and `limit=` if configured);
a path that is held to a rate of its own shows it as `own=`.
`MPQUIC_RATE_DEBUG=1` logs one line per round: the phase, what was sent and
acknowledged, the loss of the last two rounds against the baselines, *B*,
and each path's rate.
