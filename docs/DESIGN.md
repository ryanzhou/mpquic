# MPQUIC design

MPQUIC is a shadowsocks-style proxy (a SOCKS5 `local` and a forwarding
`remote`) whose transport is a *multipath* set of QUIC connections. A local
instance opens one QUIC connection ("path") per configured server address.
Typically that's the remote's own address (the direct path) plus any number of
plain UDP relays (`socat UDP-LISTEN:…,fork UDP:remote:port` or
`mpquic relay`). All paths terminate at the same remote instance, which groups
them into a session. Proxied TCP connections are spread across the paths by a
scheduler with two goals:

* **Bulk**: get close to the *sum* of the path capacities.
* **Interactive and small transfers**: keep the latency of the *best* path, and
  hide the loss and outages of individual paths.

```
 app ──SOCKS5──> mpquic local ══ path 0: direct UDP ══════════════════╗
                    │          ══ path 1: UDP ─> relay A (socat) ════╣══> mpquic remote ──TCP──> target
                    │          ══ path 2: UDP ─> relay B (socat) ════╝
                 one session, N QUIC connections, M multipath streams
```

## Roles and reverse mode

Two roles are independent of each other:

| | QUIC role | proxy role |
|---|---|---|
| normal `local` | dialer (`-s` = paths) | SOCKS5, opens streams |
| normal `remote` | listener (`-s` = bind) | accepts streams, dials targets |
| `local --reverse` | listener (`-s` = bind) | SOCKS5, opens streams |
| `remote --reverse` | dialer (`-s` = paths: the local's address + relays) | accepts streams, dials targets |

The session layer is symmetric. Scheduling, ACKs, redundancy and failure
detection don't depend on the role, and a session accepts incoming streams
iff it has a stream handler. Only the following depend on the QUIC role:

* The **dialer** generates the session id. It re-dials failed paths and
  checks the listener's session nonce: a changed nonce means the listener
  lost its state, so the dialer resets its streams.
* The **listener** groups paths by session id and expires sessions that
  have had no paths for a while. In reverse mode (`SingleSession`), a new
  session from the peer immediately closes older sessions without paths.
  This way SOCKS connections that were in flight when the remote restarted
  fail fast instead of hanging. New streams always open on the session
  with the most active paths.

## Why one QUIC connection per path (and not one multipath-QUIC connection)

Each path has its own QUIC connection, with its own TLS handshake, packet
number space, loss recovery, pacing and congestion controller. This is the
MPTCP "subflow" model, and it has practical advantages:

* It reuses a mature, high-performance QUIC stack (quic-go) unchanged, apart
  from the pluggable congestion controller from the apernet fork. Per-path
  loss recovery, pacing, PMTU discovery and connection migration (relay
  restarts, NAT rebinding) all come for free.
* The relays are completely dumb UDP forwarders, and the remote needs no
  knowledge of the topology.
* Per-path state is isolated: a broken path can't corrupt the others, and it
  can be torn down and re-dialed at any time.

Everything multipath lives in a session layer above the connections. That
layer has full control over scheduling, acknowledgements and redundancy,
which is where it improves on MPTCP.

## Wire protocol

Path setup (client → server, on the first bidirectional stream of a new
connection):

```
HELLO    = "MPQ1" ver:u8 session_id[16] path_index:varint tag[32]
HELLO_OK = "MPQ1" 0:u8  server_nonce[8] tag[32]
tag      = HMAC-SHA256(HKDF(password), role ‖ TLS-exporter ‖ …)
```

Both sides prove they know the password with an HMAC over the **TLS exporter
secret** of *this* connection. A man in the middle that terminates TLS on both
sides sees two different exporters and can't forge either tag, so
certificates are not verified (the server uses an ephemeral self-signed
certificate by default). `server_nonce` identifies the server-side session
incarnation. If it changes (server restart, or the session expired), the
client resets its streams instead of waiting for data that no longer exists.

The handshake also carries the protocol version (currently **2**; both ends
must run the same version). Control frames are length-prefixed and travel as
QUIC DATAGRAMs (falling back to the reliable control stream when a frame is
too large or datagrams are unavailable). A reliable stream would put the
liveness and acknowledgement signals behind every retransmission on a lossy
path, which used to produce false "path suspect" events and late SACKs:

| frame | contents | use |
|---|---|---|
| PING/PONG | seq | path liveness and ping-loss estimate; every 100 ms while streams are active (1 s when idle) |
| ACK | per stream: cumulative offset, window, ≤200 SACK ranges (delta-encoded) | data-level acknowledgement + flow control; sent on the two best paths, re-sent every 30 ms while data is arriving |
| PATHACK | path index, largest datagram seq, 256-bit bitmap | per-path receipt of data datagrams (loss detection on lossy paths) |
| RESET | stream id, code | abort (dial failure, idle timeout, …) |
| DGRAM | raw datagram | UDP payloads too large for a QUIC DATAGRAM frame |

Every DATAGRAM payload starts with a type byte: 0 = proxied UDP, 1 = control
frames, 2 = stream data (`dseq id off len|fin data`, see "Lossy paths").

Stream data is carried in **unidirectional QUIC streams, one per (path,
multipath-stream, direction)**:

```
uni   = mstream_id chunk*
chunk = offset:varint (len<<1|fin):varint data
```

Offsets are data sequence numbers in the multipath stream's byte space, like
MPTCP's DSN. FIN takes one virtual byte. A chunk is immutable once it has been
sent, so a chunk reinjected on another path is byte-identical, and the
receiver de-duplicates by offset. Separate QUIC streams per (path, mstream)
mean a loss on one path only blocks that stream's data on that path. It never
blocks other proxied connections that share the path.

As in shadowsocks, the first bytes of every stream are the target address in
SOCKS5 format. That means there's no separate "open" message. The local
replies to the SOCKS client immediately, and the request bytes follow the
address with zero extra round trips. UDP ASSOCIATE payloads travel as QUIC
DATAGRAM frames (`assoc_id ‖ socks_addr ‖ payload`) on the lowest-latency
path.

## Session layer

### Send side

Each stream keeps an ordered list of chunks (up to 16 KB by default; small
application writes are coalesced while a chunk is unsent). A chunk stays
until it is cumulatively acknowledged. Every chunk records each transmission
as `(path, time)`. The receiver's ACK advertises `window = consumed +
Window`, and the application's `Write` blocks once `Window` bytes are
outstanding. This gives end-to-end backpressure to the TCP socket.

### Receive side

Chunks are inserted into a per-stream reorder buffer, de-duplicated by
offset, and delivered in order. ACKs are coalesced for up to `AckDelay` (2 ms)
and sent as datagrams on the **two** lowest-latency paths, so a single lossy
or dead path can't delay acknowledgements; they are refreshed every 30 ms
while data is arriving, since a datagram can be lost. Each ACK carries the
cumulative point, the window and up to 200 SACK ranges.

### Pull-based scheduling

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
   `rate = max(BBR bottleneck bandwidth, cwnd / (2·minRTT))` while BBR is in
   STARTUP or has no estimate; the second term keeps the estimate sane while
   BBR's own estimate is still application-limited (capped at twice the
   measured bandwidth once the path has carried a burst; see "Lossy paths"). `stall_q` charges a path whose writer has been blocked
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

   **Eager redundancy.** Small chunks (≤ 8 KB: requests, TLS handshakes,
   keystrokes) are additionally duplicated onto an idle second path whenever
   that copy would arrive before a loss recovery on the first path could.
   This uses a token bucket of 512 KB/s, so it never applies to bulk data. It
   cut the p99 latency of small round trips over a 2%-loss best path from 71
   ms to 39 ms in the benchmarks.

   Duplicates and SACKs only ever *add* information; the receiver
   de-duplicates. The receiver truncates its SACK list to 200 ranges, so chunks
   beyond the last reported range are treated as "unknown" rather than lost.
   This matters with high reordering, for example a 10 ms path plus a 300 ms
   path.

### Congestion control

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
docs/CONGESTION.md for measurements.

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
  cellular), which otherwise look like rate drops. The max filter itself still
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
* **ProbeRTT turn-taking** (see below) only holds the session-wide turn while
  actually in ProbeRTT. A path whose min RTT expired on the first ACK after
  an idle period skips ProbeRTT; it used to keep the turn forever, so no other
  path of the session ever probed again.

The `bbr-conservative` profile's overestimate avoidance (Chromium's "A0"
sampler) is not used by default: under ACK aggregation on low-RTT paths (ACKs
in 10–40 ms bursts on a 5–10 ms path) it collapses the estimate to a few
percent of the link.

### Lossy and misbehaving paths

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
  loss and resend on the best path; paths in BBR STARTUP (whose queue is
  still growing) get one more RTT. The front is carried by the path with the
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
* **Honest rate of thin paths.** A path that only ever carried small
  messages stays in BBR STARTUP with its initial window; once it has carried
  a burst, it is rated at most twice its measured bandwidth instead of its
  initial-window rate, so a 1 Mbit/s relay isn't mistaken for a fast one.
* **BBR ProbeRTT.** BBR cuts a path's window to 4 packets for ~200 ms every
  ~10 s. While another path can carry the traffic, a path in ProbeRTT takes
  no new data and the chunk stuck in its sender is resent elsewhere. The ProbeRTT interval is jittered per path
  (7.5–12.5 s), and the paths of a session take turns: a path doesn't enter
  ProbeRTT while another one is in it.

### Path failure detection

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

## Differences from MPTCP

| | MPTCP (Linux default) | MPQUIC |
|---|---|---|
| scheduler | min-RTT with free cwnd | N-path ECF water-filling per stream (latency for small flows, aggregation for bulk) |
| HOL / tail | reinject + halve the slow subflow's cwnd | duplicate onto idle faster paths; no penalisation |
| small flows on lossy best path | wait for retransmission | eager redundant copy on 2nd path |
| lossy / slow subflow | penalise, still head-of-line blocks | look-ahead placement, per-chunk streams or datagrams, deadline rescue |
| path failure | RTO-driven, seconds | datagram pings, ~0.6 s, plus deadline rescue (~1 RTT) |
| per-connection HOL | all data on a subflow is one byte stream | one QUIC stream per (path, connection) |
| CC | coupled (LIA/OLIA) Reno | uncoupled BBR per path (relays are assumed to be independent bottlenecks) |
| middleboxes | must pass TCP options | plain UDP; relays are stateless forwarders |

The congestion controllers aren't coupled because the paths are, by
assumption, disjoint routes through different relays. If several paths share
a bottleneck, they will together take more than one flow's fair share there.

## Tuning

* `window` (default 64 MB) is the *maximum* per-stream window. Each stream
  starts at 4 MB and doubles whenever a full window is consumed within 4× the
  slowest path's RTT, i.e. when the window is what limits the stream. The
  sender buffers no more than the peer currently allows, so a slow consumer
  keeps its window, and the memory it pins, small. A stream's rate is
  bounded by window / (slowest path RTT including queueing): 64 MB is
  ~1.3 Gbit/s at 150 ms (see docs/CONGESTION.md).
* `chunk_size` (16 KB) is the scheduling granularity.
* `congestion`: `bbr` (default), `bbr-aggressive` (faster startup),
  `bbr-conservative` (gentler startup; avoid on Wi-Fi/cellular paths with
  low RTT, see above), `cubic` (quic-go's built-in loss-based controller, for
  comparison only: it collapses under random loss and fills deep buffers).
* `no_dup`: disables duplication if the relay traffic is metered.

## Limitations / future work

* No congestion coupling between paths that share a bottleneck.
* A sudden rate drop still costs one window's worth of queue: what is already
  in flight (up to 2·BDP at the old rate) has to drain at the new rate before
  the queue is gone (~1 s for an 8× drop at 50 ms RTT), and smaller drops
  (< 25%) are left to BBR's 10-round filter.
* Against a loss-based flow filling a deep buffer, BBRv1 gets a minority
  share (~20% against Reno in the simulator), as before.
* A path that recovers from a long blackout waits for QUIC's exponentially
  backed-off probe timer. That takes ~1–2 s after the path heals, during
  which the other paths carry the traffic.
* UDP datagrams are sent on the best path only, with no redundancy.
* SIP003 plugins and SOCKS username/password authentication aren't
  supported.
