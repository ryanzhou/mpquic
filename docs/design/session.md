# Session layer (`internal/mp`)

A session is the set of paths between a local and a remote, and the
multipath streams that run over them. This page describes how a stream's
data is cut up, which path sends what, how it is acknowledged, and what
happens when a path loses packets or dies. The paths' rates come from
[congestion control](congestion.md); the frames are in
[protocol.md](protocol.md).

## Send side

Each stream keeps an ordered list of chunks (up to 16 KB by default, and on
a slow path no more than it sends in 5 ms: a path's sender is busy with a
chunk for that long, and a keystroke or a rescue that is due next waits
behind it, 20 ms for 16 KB at 6.5 Mbit/s; small application writes are
coalesced while a chunk is unsent). A chunk stays
until it is cumulatively acknowledged. Every chunk records each transmission
as `(path, time)`. The receiver's ACK advertises `window = consumed +
Window`, and the application's `Write` blocks once `Window` bytes are
outstanding. This gives end-to-end backpressure to the TCP socket.

## Receive side

Chunks are inserted into a per-stream reorder buffer, de-duplicated by
offset, and delivered in order. ACKs are sent as datagrams on the **two**
lowest-latency paths, so a single lossy or dead path can't delay
acknowledgements; they are refreshed every 30 ms while data is arriving,
since a datagram can be lost. Each ACK carries the cumulative point, the
window and up to 200 SACK ranges (as many as fit in a datagram: about 110
with 16 KB chunks; from 48 ranges on, the sender takes the list as complete
only up to its last range).

A window update is such an ACK too, sent once. A sender that is blocked by
the window with nothing in flight therefore asks for it every 500 ms
(WINDOW): otherwise one lost datagram would leave both ends waiting for
each other until the idle timeout.

ACKs travel against the data, through the receiver's uplink, which is often
the slowest link of all (and may be busy with an upload):

* **Cadence.** The first ACK after a pause goes out after `AckDelay` (2 ms).
  While data keeps arriving, ACKs follow four times per round trip of the
  fastest path: every 5 ms at 20 ms RTT, at most every 25 ms. The sender
  allows for the same interval before it calls an acknowledgement late.
  (One ACK every 2 ms on two paths, each repeating the stream's whole SACK
  state, was megabits of acknowledgements for a fast download.)
* **Only the latest state waits.** ACKs are cumulative, so a path keeps one
  pending ACK state per stream (and per path, for datagram receipts), and
  builds the datagram when its connection can send. While the connection is
  busy or slow, newer state replaces older instead of queueing behind it.
  With a plain queue, a receiver whose uplink was busy with an upload once
  had 187 ACK datagrams waiting, each telling the sender about data it had
  long since resent: over half of a download was sent twice.

## Pull-based scheduling

Every path has one sender goroutine. quic-go's `Write` blocks while the
path's congestion window or pacer is exhausted, so a path asking for its next
chunk means "this path has capacity *now*". At that moment the session
decides what this path should send, in priority order:

1. **Reinjection queue.** Chunks whose transmissions are all on failed or
   suspect paths, or which are long overdue (no ACK after 2·SRTT + 4·RTTVAR on
   every path that carried them), are resent first. They only go to a path
   that hasn't carried them, and only if no better idle path exists.

2. **New data, gated by Earliest-Completion-First generalised to N paths.**
   For a candidate chunk of stream S, the session collects the paths that
   would deliver it strictly sooner than path p. Its estimate for a path is
   `est(q) = owd_q + stall_q + size / rate_q`. It then works out when those
   faster paths could finish S's remaining backlog `k` by water-filling:

   ```
   find t such that  Σ_q rate_q · max(0, t − owd_q) = k
   p may take the chunk only if  est(p) ≤ t
   ```

   For a small request, `k` is tiny, so only the best path sends: the
   *latency of the shortest path* is preserved. For a bulk transfer, `k` is
   large and t grows until every path is worth using: the *aggregate
   throughput*. Near the end of a transfer the slow paths stop first, which
   avoids the classic MPTCP tail where the last segments crawl over the
   slowest subflow. `k` is computed per stream as the bytes round-robin will
   send before S's backlog is drained, so an interactive stream sharing the
   session with a bulk download still gets fast-path treatment.

   *Estimates.* `owd = min(SRTT, max(latest RTT, min RTT)) / 2`: it reacts
   immediately when a queue drains, and smoothly when one builds.
   `rate` is the path's share of the session rate (see
   [congestion.md](congestion.md#shares)). `stall_q` charges a path whose writer has been blocked
   much longer than its rate implies, which happens with a silently dead path.

3. **Opportunistic duplication (only on otherwise idle capacity).** An idle
   path looks at the head of each stream's unacknowledged data. It re-sends a
   chunk if it would deliver that chunk earlier than the chunk's current
   transmission is expected to arrive. A transmission whose ACK is overdue
   counts as lost. This removes head-of-line blocking at the end of
   transfers, when a slow path still holds data, and when a packet is lost on
   one path. That's the problem MPTCP's "opportunistic retransmission +
   penalisation" tries to solve. Here it happens proactively and without
   shrinking anybody's congestion window.

   **Eager redundancy.** Small chunks (requests, TLS handshakes,
   keystrokes: all that the stream had to send was ≤ 8 KB; a slow path
   carves small chunks from bulk data too, which does not count) are
   additionally duplicated onto an idle second path whenever
   that copy would arrive before a loss recovery on the first path could.
   This uses a token bucket of 512 KB/s, so it never applies to bulk data. It
   cut the p99 latency of small round trips over a 2%-loss best path from 71
   ms to 39 ms in the benchmarks.

   Duplicates and SACKs only ever *add* information; the receiver
   de-duplicates. The receiver truncates its SACK list to 200 ranges, so chunks
   beyond the last reported range are treated as "unknown" rather than lost.
   This matters with high reordering, for example a 10 ms path plus a 300 ms
   path.

## Lossy and misbehaving paths

The rule is: **adding a path must never make the session worse than leaving
it out** by more than a transient: like MPTCP after a link change, the
session may take a moment to adapt, but it then settles at a state that is
at least as good as without the path. No path is excluded by rule; a bad
path is simply given only the work it can do without hurting the others.
The mechanisms, roughly in the order data meets them:

* **Loss estimate per path.** `loss = max(packet loss EWMA, ping loss /
  2)`, where packet loss is counted by the congestion-controller wrapper
  (quic-go only counts losses itself in its built-in controller). It drives chunk sizing (a chunk on a lossy path is small enough that
  P(any of its packets lost) ≤ 25%), the expected loss-recovery delay that is
  part of every delivery estimate (`lossDelay`), and the transport choice
  below.
* **No head-of-line blocking inside a path.** On paths with > 0.3% loss every
  chunk gets its own QUIC stream, so one lost packet only delays that chunk.
  At ≥ 8% loss (`MPQUIC_DGRAM_LOSS`) data is sent as unreliable DATAGRAMs
  instead: one packet per chunk, no retransmission on the lossy path at all.
  The receiver reports receipt per path (PATHACK); a datagram that is 3
  sequence numbers and a reorder window behind a received one is lost and its
  chunk is resent on the best *other* path straight away.
* **Look-ahead placement.** A path slower than the fastest one doesn't take
  data from the stream's front. It takes data from as far ahead as the other
  paths will have delivered by the time its chunk arrives, pessimistically:
  `ahead = R_total · (safe_p − est_front)`. `safe` is a path's delivery
  estimate plus a margin: jitter, and on lossy paths the time to notice a
  loss and resend on the best path. The front is carried by the path with the
  lowest `safe`, so a lossy path with a short RTT takes data from ahead too,
  instead of leaving holes right at the front. Estimates include a decaying
  record of recent queueing-delay spikes (`late`), so a relay whose
  bandwidth collapses now and then is planned for pessimistically. The
  inputs are smoothed so placement doesn't jitter with every RTT sample.
  In-order delivery therefore doesn't wait for the slow path, even when one of
  its chunks has to be rescued.
* **Deadline rescue and RACK.** Every transmission records a pessimistic
  arrival bound (re-evaluated with the path's current RTT, and allowing one
  QUIC recovery round on stream sends). A chunk that the stream needs (a hole
  at the receiver, or the front has been sent past it) and is past its bound
  plus an ACK trip is resent on the best path that hasn't carried it. A chunk sent on a path, while a *later* chunk on the
  same path has been acknowledged by more than a reorder window, is presumed
  lost (RACK) when another path would beat QUIC's own retransmission.
  Rescues are ranked by expected delivery plus a risk penalty, so a chunk
  lost on one lossy path isn't sent over another lossy one when a clean path
  is available. Rescues of chunks far ahead of the front wait until the
  stream's unsent front has gone out (earliest deadline first).
* **Insurance copies for small transfers.** A small chunk of a short or
  interactive transfer placed on a lossy path is also queued for a copy on
  the best other path (same token bucket as eager redundancy). Its latency is
  then bounded by what the clean path alone would deliver.
* **Honest rate of thin paths.** A path whose relay is slower than its
  share of the session rate is held to what it delivers (see
  [congestion.md](congestion.md#a-paths-own-rate)), so a 1 Mbit/s relay
  isn't mistaken for a fast one.

## Path failure detection

Pings travel as unreliable datagrams, every 100 ms while streams are active.
A path is **suspect** when no ping has been answered for
`max(600 ms, 3·SRTT + 4·RTTVAR)`, stretched by the measured ping loss so that
a healthy lossy path is falsely suspected with probability < 10⁻⁴. A suspect
path gets no new data, and every chunk whose only transmissions were on
suspect or dead paths is queued for reinjection. Within a bulk transfer,
deadline rescue usually recovers even before suspicion: in the benchmarks a
download stalled at most ~100–150 ms when the best path was cut. A path that
answers again becomes active. A path that stays
suspect for 6 s is closed, and the local re-dials it with exponential backoff
(0.5 → 5 s) using a new UDP socket, so a stale relay or NAT mapping is
replaced. Relay restarts without loss of state are handled by QUIC connection
migration.

## Limitations

* A path that recovers from a long blackout waits for QUIC's exponentially
  backed-off probe timer. That takes ~1–2 s after the path heals, during
  which the other paths carry the traffic.
