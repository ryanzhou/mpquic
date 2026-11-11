# Proxying (`internal/proxy`)

What the multipath streams and datagrams carry: proxied TCP connections,
UDP associations and pings. The local side is fed by the SOCKS5 and tunnel
listeners and by the [packet front-ends](packet-frontends.md); the remote
side dials the targets.

## TCP

As in shadowsocks, the first bytes of every stream are the target address in
SOCKS5 format. That means there's no separate "open" message. The local
replies to the SOCKS client immediately, and the request bytes follow the
address with zero extra round trips. A target the remote cannot reach resets
the stream.

TCP is terminated at both ends, not encapsulated: the remote's connections
to the targets are ordinary connections of the remote host. With
`race_connect` the remote opens each of them by racing up to five attempts
(see [../configuration.md](../configuration.md#mpquic-options)).

## UDP

UDP (SOCKS5 UDP ASSOCIATE, the UDP tunnel, WireGuard, TUN) travels as QUIC
DATAGRAM frames (`assoc_id ‖ socks_addr ‖ payload`) on the lowest-latency
path that can carry them (see "UDP payload sizes" for the limits), or over
streams with reliable UDP (below). On the remote each association owns one
UDP socket, so replies from any peer are relayed back (full-cone); an
association without traffic for two minutes is closed.

Relaying a datagram never waits. It is queued for the best path that takes
its size and has room (64 datagrams per path, besides the connection's own
32), and dropped otherwise: the sender is the reader of the TUN device or
of a socket, with everybody's TCP behind it, and the remote's receive loop
also carries the session's pings and acknowledgements. For the same reason
the remote resolves targets given by name in a goroutine of the
association, with the datagrams that wait for the answer, and keeps the
answer for a minute (a failure for 5 s).

The remote keeps at most 8192 associations (sockets), and 1024 sockets for
echo flows; beyond that, and when the system refuses another socket, the
ones idle for longest are closed.

Two more datagram forms share that format:

* `assoc_id` alone releases an association (the local sends it when a SOCKS
  control connection closes or a WireGuard or TUN UDP flow idles out), so the
  remote closes its socket before its 2-minute idle timer. Older remotes
  fail to parse the address and ignore it.
* association id 0 carries ICMP echo for the WireGuard listener and the
  TUN interface:
  `0 ‖ socks_addr(ip, port 0) ‖ flow:u16 ‖ seq:u16 ‖ data`. The address is
  the target in requests and the replying host in replies; `flow` names an
  echo flow of the local (one per client, target and echo identifier). The
  remote pings from one unprivileged ICMP socket per flow, or from a shared
  raw socket with an identifier per flow. An older remote would send it as
  UDP to port 0, which the OS rejects.

## UDP payload sizes

Proxied UDP has to fit in one QUIC packet to travel as a DATAGRAM (see
[protocol.md](protocol.md#packet-sizes-mtu) for the packet sizes). quic-go
allows packet size − 37 bytes, minus the type byte, the association id (1–2
bytes) and the SOCKS address (7 bytes IPv4, 19 IPv6, 4 + name for domains).
For an IPv4 target (and a 1-byte id) that leaves 1200 − 37 − 1 − 8 = 1154
bytes of UDP payload at the start, ~1390–1406 on a 1500-byte path after
discovery, and `mtu − 74` with `mtu` set (1206 at 1280). A datagram that no
path can carry is **dropped**, like a network with a smaller MTU drops it,
including while discovery may still raise the limit. Protocols that discover
the MTU themselves (QUIC inside the tunnel, i.e. HTTP/3) settle at a size
that fits. Drops are logged (at most every 30 s) and counted in the stats
(`udp_too_large`); set the client's MTU ≤ the logged maximum + 28, or use
reliable UDP. There is no reliable fallback for large datagrams (e.g. big
DNS responses, which an IP network would fragment).

## Reliable UDP

With `reliable_udp` (`--reliable-udp`, local and tunnel roles) every UDP
association (SOCKS5 UDP ASSOCIATE, UDP tunnel client, WireGuard or TUN UDP
flow) is one multipath stream instead, opened on its first datagram:

```
stream = 0x55 frame*          (0x55: not a SOCKS5 address type)
frame  = len:varint socks_addr payload
```

The address is the target towards the remote and the replying peer back
towards the local; the remote relays through one UDP socket per stream
(full-cone, as for datagrams). The local closes its side (FIN) when the
association ends; the remote then closes the socket and its side. A stream
that fails (idle timeout, lost session) is reopened on the next datagram. A
remote without UDP enabled (or older than reliable UDP) resets the stream,
which the local logs as a warning.

Datagrams then get retransmission, multipath scheduling, reinjection and
congestion control, and have no size limit (65,535-byte payloads), at the
cost of head-of-line blocking and in-order delivery within an association.
Only the local's setting matters: a remote with `-u` serves both kinds.

UDP senders don't slow down when the tunnel is full, and a stream would
otherwise buffer up to its window (seconds). Each writer (local and remote)
therefore measures how fast its stream's unsent backlog drains (an average
over 50 ms samples taken while backlogged) and drops a new datagram when the
backlog exceeds 64 KB and would take more than 200 ms to drain: tail drop at
the tunnel entrance, like a router queue. Drops are logged at most every
30 s. In a test with 20 MB/s offered to two 8 Mbit/s paths, the last
datagram arrived ~0.5 s after the sender stopped instead of ~2.9 s.

## Limitations

* UDP datagrams are sent on the best path that can carry them, with no
  redundancy (unless reliable UDP is on).
* Only ICMP echo is relayed: ICMP errors from the internet (for example for
  `traceroute`) are not.
* SIP003 plugins and SOCKS username/password authentication aren't
  supported.
