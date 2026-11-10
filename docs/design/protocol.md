# Wire protocol (`internal/mp`)

What the two ends say to each other on a path: how a path is set up and
authenticated, the control frames, how stream data is framed, and how large
the packets are. What the streams and datagrams carry for the proxy is in
[proxy.md](proxy.md).

## Path setup and authentication

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

## Control frames

The handshake also carries the protocol version (currently **2**; both ends
must run the same version). Control frames are length-prefixed and travel as
QUIC DATAGRAMs (falling back to the reliable control stream when a control
frame is too large or datagrams are unavailable). A reliable stream would put the
liveness and acknowledgement signals behind every retransmission on a lossy
path, which used to produce false "path suspect" events and late SACKs:

| frame | contents | use |
|---|---|---|
| PING/PONG | seq | path liveness and ping-loss estimate; every 100 ms while streams are active (1 s when idle) |
| ACK | per stream: cumulative offset, window, ≤200 SACK ranges (delta-encoded) | data-level acknowledgement + flow control; sent on the two best paths four times per RTT of the fastest path (every 2–25 ms), refreshed every 30 ms while data is arriving |
| LIMIT | rate | the most the sender of the frame wants to receive over all paths together (its configured `down`); on the control stream |
| PATHACK | path index, largest datagram seq, 256-bit bitmap | per-path receipt of data datagrams (loss detection on lossy paths) |
| RESET | stream id, code | abort (dial failure, idle timeout, …) |

Every DATAGRAM payload starts with a type byte: 0 = proxied UDP, 1 = control
frames, 2 = stream data (`dseq id off len|fin data`, see
[session.md](session.md#lossy-and-misbehaving-paths)). Proxied UDP is described in
[proxy.md](proxy.md#udp).

The version only changes when the two ends could no longer understand each
other; [../deployment.md](../deployment.md#upgrading) lists what changed and
what older peers ignore.

## Stream data

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

## Packet sizes (MTU)

QUIC packets are sent with the IP don't-fragment bit and never fragmented.
Each path starts at 1200-byte packets (UDP payload; the minimum QUIC allows,
so any path that carries 1228-byte IPv4 / 1248-byte IPv6 packets connects,
including 1280-byte tunnels such as Tailscale, ZeroTier or IPv6-in-IPv4).
quic-go's path MTU discovery then raises it, up to 1452 bytes (a 1500-byte
IPv6 or 1480-byte IPv4 packet); it converges within ~8 s, ending up to 20
bytes below the real limit. The packet size barely matters for throughput:
per-packet overhead is ~4% at 1452 bytes and ~5% at 1252.

Discovery only ever raises the size (it restarts when a relay's address
changes). If a path's MTU drops mid-connection, e.g. a route change onto a
smaller tunnel, full-size packets vanish while small pings still pass. For
paths like that, `mtu` fixes the packet size: MTU − 28 for IPv4 paths, − 48
for IPv6, between 1200 and 1452, with discovery off.

TCP is not encapsulated: it ends at the local and is re-chunked into QUIC
packets, so the client's MSS is irrelevant and nothing needs MSS clamping;
the remote's connections to targets use the remote host's own MSS.

Proxied UDP has to fit into one packet to travel as a datagram; the sizes
are in [proxy.md](proxy.md#udp-payload-sizes).

## Limitations

* A path MTU that drops mid-connection is not detected (set `mtu`).
