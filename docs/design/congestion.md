# Congestion control (`internal/mp/rate.go`, `internal/cc`)

A session has **one** congestion controller for all its paths. It decides
one rate for the session; the paths only carry their share of it. This page
describes that controller, then the per-path BBR that is still there as an
alternative (`no_coupling`). Measurements:
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
only sees its own path cannot know what the others lose. Correcting five
independent estimators from above took more rules than a single estimator
needs.

So the session has the rate, and the scheduler decides which path carries
what (see [session.md](session.md)). MPTCP's coupled congestion control and
Hysteria's configured rates are the two existing answers to the same
problem; this is the first taken to its end (one controller instead of
coupled ones), with the second as an option (`up` / `down`).

## The paths follow

A path's congestion controller (`cc.follower`) has no start-up or probing of
its own, and its rate is not its decision. It does two things:

* it **paces** at the rate the session assigns (`Meter.SetRate`), and
* it bounds what is **in flight** to what the path itself delivers in two
  round trips (at least one round trip plus 25 ms, and 32 packets). The
  delivery rate is the highest of the last four measuring windows (a round
  trip each) in which the path had data to send; the round trip is the
  session's base RTT for the path (below).

The window is BBR's, and it is what protects a path from its share: a relay
that is given twice what it carries queues one round trip's worth and is
then clocked by its own acknowledgements, long before the session has
worked out a limit for it. A path that stops delivering stops sending. And
in the start-up, where the session doubles the rate every round, a path can
only send twice what it delivers per round trip, so the window does for one
path what the session does for all.

That window is for a path that delivers *less* than it is given. A path
that delivers its rate (at least 0.9 of it) may have its rate times its
smoothed round trip in flight on top: round trips also grow with queues
that are not the path's or not this direction's (a download's
acknowledgements behind an upload's queue on the same line), and a window
of two base round trips would then hold a healthy path below its rate.

Hysteria's Brutal paces at a configured rate in the same way. The path also
counts, for the session: every packet is booked on the session round it was
sent in, and its acknowledgement or loss is counted for that round however
much later it is known (`Meter.RoundStats`).

## What the session measures

The session works in **rounds** of the longest smoothed round-trip time of
its paths (50 ms to 1 s, at most twice a path's base RTT). At the end of
round *k* it knows:

* **What rounds *k*-1 and *k*-2 delivered and lost**, exactly: the bytes
  sent in each, and of those the bytes acknowledged and declared lost. For
  *k*-2 that is final; for *k*-1 it is nearly complete. A round's delivery
  *rate* is what was acknowledged over the round's length, or over the time
  its acknowledgements took to come in if that was longer: then the packets
  queued, and that is the rate at which the queue let them through.
* **What the pipe puts out now**: the bytes acknowledged during round *k*
  on all paths, whatever round they were sent in. After two rounds at the
  same rate this is what the pipe takes.
* **Queues**: a path saw a queue if even its lowest RTT sample of the round
  was above its base RTT (the lowest of the last 15–30 s) by
  `max(3 ms, shortest base RTT / 10)`.
* **Whether it had data**: a round counts as *busy* if the paths sent at
  least 85% of their shares, or could not because their windows were full.
  Rounds in which the application had less to send say nothing about the
  pipe and change nothing: not *B*, and not the phase. That matters most
  for the direction that only carries acknowledgements. Its round trips
  grow with the *other* direction's queue, and it loses packets like any
  other traffic; if it took either for its own congestion, it would end
  its start-up at the trickle it happens to send, and acknowledgements and
  pings would then be paced at that.

**Loss is judged against a baseline.** Every path has a baseline loss: the
share it loses whatever the rate (a radio link, a bad cable). A round shows
it if the pipe was clearly not full: the round sent less than 0.9 of what
the pipe was putting out before it (the round after a probe), or less than
0.8 of what it put out afterwards (a start-up's rounds while the rate still
grows). Other rounds can only lower it. A round merely below *B* is not
enough: after the capacity has dropped, three quarters of *B* may still be
twice what the pipe takes. A round is *lossy* if the paths
lost clearly more than their baselines (by 2% of what they sent, and by
more than chance explains), and if that is so for the paths that carried
three quarters of the round. Loss on one path only is that path's own
business (below); random loss, however heavy, is in the baseline.

## The session rate

*B* is the rate the session sends at, over all paths together.

* **Start-up.** The rate is twice the rate at which acknowledgements are
  arriving (measured continuously, over the last round), so it doubles
  about every round trip, like TCP's slow start. It ends
  * when a round loses more than the paths' baselines on every path (the
    first rounds have shown the baselines by then, so random loss does not
    end it): the pipe takes no more;
  * or when a tenth of a round did not come out, or *B* has not grown by a
    quarter for two rounds while the paths see a queue. (Without a queue,
    slow growth is not the pipe's doing: the paths' windows open with what
    they deliver, a round trip behind.)

  Loss and queues are also checked at every tick within a round: as soon
  as the last two rounds show loss beyond the baselines on every path, or
  every path's round-trip time is clearly above its base (by a quarter of
  the base RTT, at least 3 ms), the session sends no more than is being
  delivered for the rest of the round. What reaches the receiver is never
  more than the line carries; what leaves the sender overshoots by up to a
  factor of two for about one round trip, as with any slow start. *B* is
  then what the pipe puts out, and one round at half of it drains what the
  overshoot queued. None of this applies to a session that sends less than
  half of what it may (see "whether it had data" above).
* **Cruise.** The session sends at *B*. A round at *B* that comes out
  (95% of *B* is put out) confirms *B*; if less comes out for ten rounds,
  *B* is the most that did.
* **Probes.** Every few rounds one round is sent at 1.25·*B*, and the next
  one at 0.75·*B* while the acknowledgements come in, so what the probe
  queued is gone again by the time its result is known.
  * If at least half of what the probe added was delivered, *B* is what
    the probe round delivered, and the next probe follows at once. If the
    probe did not queue either (no path saw a queue during it or after),
    there is clearly room: the next probe is at 1.5·*B*, and from the
    second such success in a row at 2·*B*. If it queued, the pipe is
    nearly full, and the next probe stays at 1.25: a larger one would
    mostly add to the queue.
  * Otherwise *B* stays. A probe's delivery cannot be measured exactly
    (the paths' parts of the pulse reach a common queue at different
    times, which makes it look a few percent better than it was), so less
    than half is not evidence. The next probe asks for half as much more
    (1.125, down to 1.06), where a little spare capacity does show as half
    of the probe, and it comes sooner, because a small probe queues
    little: every 8 rounds at 1.25, every 2 at 1.06. Every third probe at
    1.06 that finds nothing is followed by a full one: where delivery is
    uneven (other traffic in the same queue), a small probe proves nothing
    either way.
* **Loss: a ceiling.** Two lossy rounds in a row at *B* mean that *B* is
  more than the pipe takes: a policer, or a buffer that is full. *B* becomes
  98% of what the pipe puts out by then. The first lossy round is not used
  for that: a policer lets a burst pass before it drops, so only the second
  shows its rate. After that:
  * a round below *B* follows, so the baselines are current (if the loss
    was random after all, it is in the baselines from then on and does not
    count again);
  * probes are **held**: the next one comes after 16 rounds, and each time
    loss stops *B* at about the same rate again (within 15%), the hold
    doubles, up to 128 rounds. Probing a policer costs a round of loss
    every time, so it should be rare where the ceiling has been confirmed.
    The hold ends when *B* is no longer near that ceiling (the capacity
    dropped);
  * a held probe lasts **two rounds**. If the second is lossy and no path
    saw a queue (a policer), 96% of what it delivered is the ceiling, and
    *B* is raised to that if it was lower. If the probe finds more without
    loss and four rounds at the higher *B* stay clean, the hold is over.
  A probe that loses while the paths see a queue has overflowed a buffer.
  That is no ceiling to remember: it counts as a probe that found nothing,
  and the next one is smaller.
* **Queues.** When every path that carried data saw a queue for a whole
  round, the bottleneck they all pass is congested.
  * If the pipe then puts out clearly less than *B* (below 0.9·*B*), the
    capacity dropped, and *B* follows at once. This is also checked at
    every tick within a round, with the acknowledgements of the last
    round's length: a whole round at the old rate can be more than the
    bottleneck's buffer holds. A queue alone changes nothing: it may not
    be ours.
  * Over three queued rounds, *B* is what was put out on average if that
    is less than 0.98·*B*.
  * A queue that stays for two rounds is drained: rounds at 0.75·*B* until
    some path sees no queue any more (at most 8 rounds, and at most every
    8 rounds). After *B* came down for a capacity drop the drain starts at
    once: what queued while the session was still sending at the old rate
    can be a whole buffer's worth, and it is latency for every stream and
    for the other direction's acknowledgements until it is gone. A full
    probe follows soon after, in case the capacity is back.
  * If the first drain round does not shrink the queue although it sent
    clearly less than the pipe was putting out, the queue is not the
    session's (the other direction's, or other people's traffic), and it
    becomes part of the paths' base RTT.
* **Known rates** (`up` / `down`). If an endpoint's uplink or downlink rate
  is configured, the session never sends more than `up` over all paths
  together, and it announces `down` to the peer (a control frame), which
  then sends no more than that. The limit is a ceiling for *B* from the
  first packet, start-up included, and is never probed. Below it everything
  above still applies, so a line that is slower than configured is handled
  as without the option. Set them a little below the real rates (95%) and
  the bottleneck's queue stays empty and nothing is dropped.

## Shares

*B* (times the round's gain) is shared out among the usable paths
**max-min fairly**: equal shares, except that a path gets no more than its
own limit, and the others share the rest.

A path has a **limit of its own** when it alone has trouble: for two rounds
its queue is clearly longer than what all paths see, or it loses beyond its
baseline while the others do not (a relay slower than its share). The limit
is 98% of what the path put out then, if that is less than it was given (a
path that puts out its share is not in trouble, whatever its queue looks
like), and the path sends half of it for one round, so its queue is gone.
This is only learned while the session sends steadily and the path gets its
share; after a start-up or while the rate changes, a path's output says
little about what it can carry. Every 8
rounds (doubling when it fails, up to 64) the limit is raised by a quarter
for three rounds, and stays raised if the path then puts out more.

Equal shares keep every path loaded: a failing path leaves only its share
to be taken over, and loss recovery has capacity everywhere. Which stream's
data goes on which path is the scheduler's decision, not the rate
controller's: small transfers still go to the fastest path.

A path that is not usable (suspect, benched) keeps a minimal share for its
pings. Before the session has a rate, every path has a window of 32 packets
and is paced at twice that per round trip.

The session's stats log line shows `rate=<B>` (and `limit=` if configured);
a path with a limit of its own shows it as `own=`. `MPQUIC_RATE_DEBUG=1`
logs every round.

## Without the session rate: BBR per path

With `no_coupling`, every path runs a BBR of its own (vendored from
Hysteria, MIT; `internal/cc/bbr`) and the session rate is off. Configured
limits still apply, split equally among the paths. This is the earlier
design's per-path controller, kept for comparison and as a fallback; it has
the problems described at the top wherever the paths share a bottleneck.
Its local changes to BBRv1:

* **App-limited hint.** A path the scheduler leaves idle only carries ACKs
  and pings, and stock BBR's bytes-in-flight heuristic mistakes that for a
  saturated pipe. The scheduler tells BBR when a path's sender is idle, so
  samples from idle periods are ignored.
* **Drain to target.** The 0.75 gain phase lasts until bytes in flight are
  down to the BDP, instead of one min RTT whether or not the queue built by
  the 1.25 probe has drained.
* **Stale estimate reset.** If for `max(2·minRTT, 100 ms)` every RTT sample
  shows a queue of at least `max(minRTT/2, 10 ms)` and the delivery rate
  stays below ¾ of the estimate, the estimate is replaced by the measured
  delivery rate and the queue is drained (shown as `rate_drops=N` in the
  stats line). For 10 s afterwards the gain cycle re-probes right after
  each drain, so a rate that comes back is used again quickly.
* **ProbeRTT turn-taking**: only one path of a session cuts its window for
  ProbeRTT at a time (see
  [session.md](session.md#lossy-and-misbehaving-paths)).

`congestion` selects the BBR profile for this mode (and `cubic`, quic-go's
built-in controller, which also bypasses the session rate).

## Limitations

* **Start-up overshoots**, like any slow start: the sender sends up to
  twice what the line carries for about one round trip before the first
  loss or queue can be seen. With `up` / `down` configured there is no
  overshoot.
* **Probing a policer costs loss.** Each probe that hits the ceiling loses
  up to a fifth of one or two rounds. The hold makes that rare (one probe
  in 128 rounds once the ceiling is confirmed), at the price of finding
  more capacity late when the policer's rate goes up: up to 128 rounds.
  With `up` / `down` set below the policed rate nothing is dropped and
  nothing is probed.
* **A policer with a large burst allowance** lets probes pass for as long
  as the burst lasts; the loss comes later, and *B* is cut then. The hold
  still doubles each time, so this settles too, but the first minutes see
  more loss than at a policer with a small burst.
* **Loss that every path has and that changes** (all paths over the same
  radio link) is taken for a ceiling once when it rises: *B* is cut by
  about the loss rate, and the round after that puts the loss into the
  baselines. Probes recover *B*, after the hold.
* **A probe adds queue** at a bottleneck that is full: a quarter of a
  round at first, a sixteenth once the probes have shrunk. With `up` /
  `down` set there are no probes.
* **Spare capacity of less than about 3%** is not found: the smallest
  probe asks for 6% more and needs half of that delivered.
* **Queues are judged by round-trip times**, which also grow with a queue
  in the ACK direction (an upload filling the receiver's uplink while it
  downloads). The session rate therefore never comes down for a queue
  alone, and a queue that draining does not shrink is taken into the base
  RTT; until then (a few rounds) the pipe may look congested when it is
  not.
* **Fairness to other traffic** is BBR-like, not TCP-like: the session
  does not back off for loss it takes to be random, and against a
  loss-based flow filling a deep buffer it gets what its probes find.
