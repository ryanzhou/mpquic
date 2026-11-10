# Packet front-ends (`internal/ipstack`, `internal/wg`, `internal/tundev`)

SOCKS5 hands the local connections and datagrams. The WireGuard listener and
the TUN interface hand it IP packets instead: of WireGuard peers, or of
whatever the operating system routes to an interface. Both feed the same
userspace network stack, which turns the packets into the streams, UDP
associations and pings of [proxy.md](proxy.md). User guides:
[../wireguard.md](../wireguard.md), [../tun.md](../tun.md).

```
 WireGuard peers ─UDP─> wireguard-go ─┐                  ┌─> TCP: forwarder ─> stream
                                      ├─> ipstack.Stack ─┼─> UDP: one association per client socket
 system routes ─> TUN device (fd) ────┘   (gVisor)       └─> ICMP echo: datagram to the remote
```

## The stack (`internal/ipstack`)

A front-end injects its clients' packets (`Stack.Inject`) and sends back
what the stack emits (`Stack.Out`). Each front-end has a stack of its own.

The stack is a gVisor network stack in promiscuous mode with spoofing, so it
accepts TCP connections to any address and replies as that address. TCP SYNs
go to a forwarder that opens a stream (address first) and then completes the
handshake. UDP and ICMP bypass gVisor's sockets: UDP is taken from the stack
after IP reassembly, one association per client socket; echo requests are
intercepted before the stack (which would otherwise answer them itself);
replies are built as raw IP packets, fragmented to the interface MTU.

* TCP uses SACK with classic loss recovery (RFC 6675) instead of gVisor's
  default RACK: after a retransmission timeout RACK re-entered recovery on
  the first ACK and sent one or two segments per timeout, which stalled a
  connection for seconds after a burst loss.
* A UDP flow is released after 2 minutes without traffic, or after 15 s if
  it only ever sent to port 53; an echo flow after 30 s.
* A datagram the paths cannot carry is answered with ICMP "fragmentation
  needed" / "packet too big" carrying the tunnel's limit; with UDP relaying
  off, UDP is answered with ICMP port unreachable.
* Echo flows are numbered per session. Each stack gets a range of 4096 flow
  ids, by which the replies from the remote find their front-end, so several
  front-ends can run on one session.

## WireGuard listener (`internal/wg`)

wireguard-go runs with the stack as its "TUN device": what it decrypts from
the peers is injected, what the stack emits is encrypted to the peer whose
allowed IPs contain the destination. Nothing of the operating system is
involved, so it needs no privileges. With a listen address, the UDP socket
is bound to it; otherwise wireguard-go's default sockets are used.

## TUN interface (`internal/tundev`)

The same data path minus WireGuard: a read loop injects what the system
routes to the interface, a write loop returns the stack's packets in
batches. On Linux the device is opened with a virtio-net header
(wireguard-go's `tun` package), so the kernel hands over TCP in segments of
up to 64 KB, which are split for the stack, and consecutive segments of a
flow in a batch are coalesced on the way back (GSO/GRO): one system call
moves many packets.

mpquic configures only what goes away with the interface (addresses, link
state, routes through it). An existing persistent device of the same name is
attached to instead and left in place. If the device disappears, the read
loop fails and the process exits.

Keeping the paths' own QUIC packets out of the interface is routing policy
and left to the caller: `fwmark` marks the path sockets (`SO_MARK`, through
`mp.Options.Control`) and the DNS lookups of the servers' names, which then
use Go's resolver, for a rule such as wg-quick's `not fwmark … table …`.
