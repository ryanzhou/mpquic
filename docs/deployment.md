# Deployment

Relays, reverse mode, Docker and upgrades. The options themselves are in
[configuration.md](configuration.md).

## Relays

A relay is any UDP forwarder from a public port to the remote's UDP port.
Each local→relay flow needs its own upstream socket, which socat's `fork` and
`mpquic relay` both provide. Relays hold no keys and see only QUIC packets.
If a relay restarts, QUIC migrates the connection to the new source port. If
it stays down, the path is declared suspect within ~0.6 s, its in-flight data
is reinjected on the other paths, and the path is re-dialed until the relay
comes back.

All paths leave through the local's uplink and come back through its
downlink. If that line is slower than the relays, as a home or office line
usually is, tell mpquic its rates with `--up` / `--down`
(see [configuration.md](configuration.md#tuning)).

### Link emulation

`mpquic relay` can also **emulate** network impairments per direction, to
test a deployment before relying on it:

```sh
mpquic relay --listen :9000 --target 127.0.0.1:8388 \
  --link delay=40ms,jitter=5ms,loss=1%,burst=3,rate=20mbit,queue=80ms,reorder=1%,outage=30s/2s
# --fwd SPEC / --rev SPEC set client->server and server->client separately
```

| key | meaning |
|---|---|
| `delay`, `jitter` | fixed one-way delay, plus uniform random extra delay |
| `loss`, `burst` | random loss probability; `burst>1` gives Gilbert-Elliott bursty loss with that mean burst length |
| `rate`, `queue` | shaping rate with a tail-drop queue (max queueing delay, default 100 ms) |
| `police`, `police_burst` | token-bucket policer (drops instead of queueing) |
| `reorder`, `reorder_delay` | probability that a packet is held back, and by how much |
| `dup` | duplication probability |
| `outage=EVERY/FOR` | periodic blackouts |
| `rate2`, `flap` | alternate between `rate` and `rate2` every `flap` |
| `mtu` | path MTU: drop packets larger than this (IPv4 size, UDP payload + 28) |

## Reverse mode (the local side has the public IP)

Sometimes the *local* host is the reachable one, and the remote (the egress
side) sits behind NAT or a firewall. Set `--reverse` on **both** ends. The
local then listens for QUIC on its public address, in addition to serving
SOCKS5. The relays forward to the local, and the remote connects back to the
local directly and through every relay. Everything else stays the same:
SOCKS clients still connect to the local, and the remote still dials the
targets.

```sh
# local (public IP 198.51.100.7): QUIC listener on :8388 + SOCKS5 on 127.0.0.1:1080
mpquic local --reverse -s 0.0.0.0 -p 8388 -l 1080 -k 'long-random-password' -u

# relays: forward to the *local*
socat UDP-LISTEN:9000,fork,reuseaddr UDP:198.51.100.7:8388

# remote (behind NAT): dial the local directly and via each relay
mpquic remote --reverse -s 198.51.100.7:8388 -s relay-a.example.net:9000 -s relay-b.example.net:9000 \
              -k 'long-random-password' -u
```

In reverse mode `-s`/`-p` (`server`/`server_port`) are the local's QUIC bind
address(es) on the local, and the list of paths on the remote; see
[`examples/reverse-*.json`](../examples). Keepalive pings hold the remote's NAT mappings
open. Until the remote has connected, the local refuses SOCKS requests
immediately. If the remote restarts, its new session immediately replaces
the old one. `deploy/docker-compose.reverse.yml` is a reverse-mode testbed,
and `mpbench --reverse` benchmarks this setup (performance matches normal
mode).

## Docker

```sh
docker build -t mpquic .
docker run -d --name mpq-remote -p 8388:8388/udp mpquic remote -p 8388 -k secret -u
docker run -d --name mpq-local  -p 127.0.0.1:1080:1080 mpquic local -b 0.0.0.0 -l 1080 \
       -s remote.example.com:8388 -s relay.example.net:9000 -k secret -u
docker run -d --name mpq-relay  -p 9000:9000/udp mpquic relay --listen :9000 --target remote.example.com:8388
```

`deploy/docker-compose.yml` is a self-contained testbed: a remote, three
emulated relay paths (one of them a flaky "direct" path with periodic
outages) and a local SOCKS5 endpoint on 127.0.0.1:1080.

```sh
docker compose -f deploy/docker-compose.yml up -d --build
curl --socks5-hostname 127.0.0.1:1080 http://whoami/
```

The image also provides the names `ss-local`, `ss-server`, `ss-tunnel` and
`mpquic-relay`. The mode is chosen from argv[0], just like the shadowsocks
binaries.

For the TUN interface add `--cap-add NET_ADMIN --device /dev/net/tun`
(see [tun.md](tun.md)).

## Upgrading

The wire protocol has a version number that both ends check during the
handshake. Version 2 sends the control plane (pings, ACKs) as QUIC datagrams
and adds datagram-carried data on very lossy paths. Upgrade the local and the
remote together: a version-1 peer is rejected.

Within version 2, newer releases add optional datagram messages that older
peers ignore: releasing a UDP association early, and ICMP echo for the
WireGuard listener and the TUN interface (which needs a remote that knows
it). Reliable UDP needs a
remote that knows it; an older one refuses the streams (the local warns).
Oversized UDP datagrams are no longer sent reliably on the control stream:
they are dropped, unless reliable UDP is on. The WINDOW control frame (a
stream blocked by flow control asks for the window again) is ignored by
older peers, which then behave as before.
