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

The control stream then carries length-prefixed frames:

| frame | contents | use |
|---|---|---|
| PING/PONG | seq | path liveness, sent every 200 ms while streams are active (1 s when idle) |
| ACK | per stream: cumulative offset, window, ≤32 SACK ranges | data-level acknowledgement + flow control |
| RESET | stream id, code | abort (dial failure, idle timeout, …) |
| DGRAM | raw datagram | UDP payloads too large for a QUIC DATAGRAM frame |

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
and sent on the **two** lowest-latency paths, so a single lossy or dead path
can't delay acknowledgements. Each ACK carries the cumulative point, the
window and up to 32 SACK ranges.

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
   `rate = max(BBR bottleneck bandwidth, cwnd / (2·minRTT))`; the second term
   keeps the estimate sane while BBR's own estimate is still
   application-limited. `stall_q` charges a path whose writer has been blocked
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
   de-duplicates. The receiver truncates its SACK list to 32 ranges, so chunks
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

### Path failure detection

Each path is pinged every 200 ms while there are active streams. A path is
**suspect** when its oldest ping has gone unanswered for
`max(600 ms, 3·SRTT + 4·RTTVAR)`. A suspect path gets no new data, and every
chunk whose only transmissions were on suspect or dead paths is queued for
reinjection. Within a bulk transfer, overdue reinjection usually recovers
even before suspicion: in the benchmarks a download stalled at most ~100–150
ms when the best path was cut. A path that answers again becomes active. A
path that stays suspect for 6 s is closed, and the local re-dials it with
exponential backoff (0.5 → 5 s) using a new UDP socket, so a stale relay or
NAT mapping is replaced. Relay restarts without loss of state are handled by
QUIC connection migration.

## Differences from MPTCP

| | MPTCP (Linux default) | MPQUIC |
|---|---|---|
| scheduler | min-RTT with free cwnd | N-path ECF water-filling per stream (latency for small flows, aggregation for bulk) |
| HOL / tail | reinject + halve the slow subflow's cwnd | duplicate onto idle faster paths; no penalisation |
| small flows on lossy best path | wait for retransmission | eager redundant copy on 2nd path |
| path failure | RTO-driven, seconds | ping-based, ~0.6 s, plus overdue reinjection (~2 RTT) |
| per-connection HOL | all data on a subflow is one byte stream | one QUIC stream per (path, connection) |
| CC | coupled (LIA/OLIA) Reno | uncoupled BBR per path (relays are assumed to be independent bottlenecks) |
| middleboxes | must pass TCP options | plain UDP; relays are stateless forwarders |

The congestion controllers aren't coupled because the paths are, by
assumption, disjoint routes through different relays. If several paths share
a bottleneck, they will together take more than one flow's fair share there.

## Tuning

* `window` (default 16 MB) must exceed (aggregate rate) × (slowest path RTT,
  including its queue). With 8 MB on a 5 ms + 150 ms × 100 Mbit/s pair,
  throughput oscillated. 16 MB and above reached ~95% of capacity.
* `chunk_size` (16 KB) is the scheduling granularity.
* `congestion`: `bbr` (default), `bbr-aggressive` (faster startup),
  `bbr-conservative` (less queueing).
* `no_dup`: disables duplication if the relay traffic is metered.

## Limitations / future work

* No congestion coupling between paths that share a bottleneck.
* BBRv1 builds about one BDP of queue on a saturated path, so interactive
  latency under full load roughly doubles the path's base RTT. That's similar
  to a single TCP flow.
* A path that recovers from a long blackout waits for QUIC's exponentially
  backed-off probe timer. That takes ~1–2 s after the path heals, during
  which the other paths carry the traffic.
* UDP datagrams are sent on the best path only, with no redundancy.
* SIP003 plugins and SOCKS username/password authentication aren't
  supported.
