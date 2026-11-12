# Design overview

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

The traffic enters the local through one of several front-ends and leaves
the remote as ordinary TCP, UDP and ICMP:

```
 SOCKS5 / tunnel ───────────────┐
 WireGuard peers ──┐            ├─> proxy ─> session ─> paths (QUIC) ══> remote: session ─> proxy ─> targets
 TUN interface ────┴─> ipstack ─┘  streams,   scheduler,  each paced at
                                   UDP assoc. ACKs,       its share of the
                                              rescue      session rate
```

## Modules

Each page describes one layer; the packages are under `internal/`.

| package | what it does | page |
|---|---|---|
| `mp` (`proto.go`, `transport.go`, `endpoints.go`) | path setup, authentication, frames, packet sizes | [protocol.md](protocol.md) |
| `mp` (`session.go`, `sched.go`, `path.go`, `stream.go`, `sendqueue.go`) | streams, scheduling, acknowledgements, rescue, path failure | [session.md](session.md) |
| `mp/rate.go`, `cc` | the session rate (one congestion controller for all paths) and the paths' pacing and windows | [congestion.md](congestion.md) |
| `proxy` | SOCKS5, tunnel, remote dialer, UDP associations, reliable UDP, ICMP echo | [proxy.md](proxy.md) |
| `ipstack`, `wg`, `tundev` | the packet front-ends: a userspace network stack behind WireGuard or a TUN interface | [packet-frontends.md](packet-frontends.md) |
| `linkem` | UDP relay with link emulation (`mpquic relay`, and the testbed's links) | [../deployment.md](../deployment.md#relays), [../development.md](../development.md) |
| `app`, `config`, `procs` | runs a role from a shadowsocks-compatible config; adapts the number of threads to the load | this page, [../configuration.md](../configuration.md) |
| `winsvc`, `gui`, `logfile` | Windows service, configurator and log file | [../windows.md](../windows.md) |

`cmd/mpquic` is the command line, `cmd/mpbench` the in-process testbed
([../development.md](../development.md)). Measurements are in
[../benchmarks/](../benchmarks/).

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

## Differences from MPTCP

| | MPTCP (Linux default) | MPQUIC |
|---|---|---|
| scheduler | min-RTT with free cwnd | N-path ECF water-filling per stream (latency for small flows, aggregation for bulk) |
| HOL / tail | reinject + halve the slow subflow's cwnd | duplicate onto idle faster paths; no penalisation |
| small flows on lossy best path | wait for retransmission | eager redundant copy on 2nd path |
| lossy / slow subflow | penalise, still head-of-line blocks | look-ahead placement, per-chunk streams or datagrams, deadline rescue |
| path failure | RTO-driven, seconds | datagram pings, ~0.6 s, plus deadline rescue (~1 RTT) |
| per-connection HOL | all data on a subflow is one byte stream | one QUIC stream per (path, connection) |
| CC | coupled (LIA/OLIA) Reno, one controller per subflow | one rate controller for the session (BBR's design; loss counts beyond the paths' baselines), the paths pace at their shares; optional configured up/down rates |
| middleboxes | must pass TCP options | plain UDP; relays are stateless forwarders |

There is one congestion controller per session, not per path (see
[congestion.md](congestion.md)): behind a common bottleneck the paths
together take what one flow would, and a path with a slower bottleneck of
its own is held to what that carries.

## Threads

By default the local, remote and relay adapt the number of Go scheduler
threads to the load (`internal/procs`). Each packet passes through several
goroutines (socket reader, QUIC connection loop, send queue, stream reader,
proxied connection). With idle CPUs around, each hand-off wakes another OS
thread, which spins and sleeps again, and the goroutines bounce between
CPUs. At moderate rates that costs as much as the packet processing itself.
So a process starts on one thread and doubles the number, up to all CPUs or
the container's limit, when they are busy (CPU usage near the number of
threads) or goroutines queue for them (mean scheduling latency ≥ 100 µs).
It halves it again when the load fits in half. Setting `GOMAXPROCS` in the
environment pins the number and turns this off. See
[../benchmarks/multipath.md](../benchmarks/multipath.md#cpu).

## Limitations

Each page ends with the limitations of its layer:
[protocol](protocol.md#limitations), [session](session.md#limitations),
[congestion control](congestion.md#limitations) and
[proxying](proxy.md#limitations).
