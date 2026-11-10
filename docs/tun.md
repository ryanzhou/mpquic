# TUN interface

With `--tun`, the local creates a **network interface**, and whatever the
system routes to it is proxied over the multipath connection exactly as for
a WireGuard peer: TCP, UDP and ping (the last two with `-u` on both ends),
IPv4 and IPv6, of the machine itself or forwarded for others. It is the
WireGuard listener without WireGuard: for traffic that starts on the machine
running the local, or that it routes, there is nothing to encrypt and
decrypt on the way to the tunnel and no UDP hop through the loopback
interface. (In an in-process benchmark the same transfer took about 40% less
CPU than through the WireGuard listener and a userspace WireGuard client.)
Use the [WireGuard listener](wireguard.md) for other devices, and for
Windows.

It needs Linux (root or `CAP_NET_ADMIN`, and `/dev/net/tun`) or macOS (root;
experimental there: built and unit-tested, not yet tried on a Mac).

## Some networks through the tunnel

```sh
# a few networks through the tunnel, the rest as before
mpquic local -s remote.example.com:8388 -s relay-a.example.net:9000 -k 'long-random-password' -u \
             --tun mpquic0 --tun-addr 10.99.0.1/24 --tun-route 10.0.0.0/8 --tun-route 192.0.2.0/24
```

mpquic creates the interface, assigns `--tun-addr`, brings it up and adds
`--tun-route` to the main routing table; when it exits, the interface and
with it the addresses and routes are gone. Anything more is ordinary
routing, done by you or by the router software that starts mpquic.

## Everything through the tunnel (Linux)

The QUIC packets of the paths
must not be routed into the interface themselves. Give them a firewall mark
and route everything *without* the mark through the interface, as `wg-quick`
does:

```sh
mpquic local ... -u --no-socks --tun mpquic0 --tun-addr 10.99.0.1/24 --fwmark 0x4d51 &
ip route add default dev mpquic0 table 51
ip rule add not fwmark 0x4d51 table 51
ip rule add table main suppress_prefixlength 0   # keep the LAN and other specific routes
# the same three with "ip -6" for IPv6
```

`--fwmark` also marks the DNS queries for the servers' names (mpquic then
resolves them itself, with the servers in `/etc/resolv.conf`). When mpquic
exits, the interface and the route in table 51 disappear and traffic uses the
normal default route again; see "Persistent interface" below if that must
not happen.

## On a router

Route the clients' traffic into the interface with a rule
of your own; replies come back out of the interface as from the internet:

```sh
sysctl -w net.ipv4.ip_forward=1
mpquic local ... -u --no-socks --tun mpquic0 &
ip route add default dev mpquic0 table 77
ip rule add iif br-lan lookup 77
```

The interface needs no address for this. With strict reverse-path filtering
(`rp_filter=1`), relax it for the interface, as for any policy-routed tunnel.

## Options

| flag | config key (`"tun": {...}`) | |
|---|---|---|
| `--tun NAME` | `name` | create the interface (Linux: any name, default `mpquic0`; macOS: `utun` or `utunN`). Any `--tun*` option, or a `tun` section, enables it |
| `--tun-addr ADDR/BITS` | `addresses` | assign an address, repeatable |
| `--tun-route ADDR/BITS` | `routes` | route a destination through the interface (main table), repeatable; `default` is `0.0.0.0/0` |
| `--tun-mtu BYTES` | `mtu` | interface MTU, 1280–65535 (default 1500). Set it here, not with `ip link`: the local's TCP uses it |
| `--fwmark MARK` | `fwmark` (top level) | Linux: firewall mark of the QUIC sockets, in any mode |
| `--no-socks` | `no_socks` (top level) | do not run the SOCKS5 listener |

See [`examples/tun-local.json`](../examples/tun-local.json).

## Notes

* **Persistent interface (Linux).** If an interface of that name already
  exists as a persistent TUN device (`ip tuntap add dev mpquic0 mode tun`),
  mpquic attaches to it instead and leaves it in place when it exits. Its
  addresses, routes and firewall rules then survive restarts, and while
  mpquic is not running the interface has no carrier and drops what is
  routed to it: nothing leaks around the tunnel.
* TCP is terminated at the interface, so the tunnel takes nothing from the
  MTU and there is no MSS to clamp. UDP is relayed datagram by datagram:
  what the paths cannot carry (about 1150 bytes of payload at a 1280-byte
  path MTU) is dropped with an ICMP "fragmentation needed" unless
  `--reliable-udp` is on. The [notes of the WireGuard listener](wireguard.md)
  about `-u`, ping and TCP resets apply here too.
* The interface uses segmentation offload where the kernel has it: the
  system hands over and receives TCP in large segments (GSO/GRO).
* In Docker: `--cap-add NET_ADMIN --device /dev/net/tun`.
* If the interface is deleted while mpquic runs, mpquic exits with an error.
* `scripts/tun-test.sh` tests all of the above end to end in network
  namespaces (as root): a router forwarding a client, and a host sending
  everything through the tunnel.

How it works: [design/packet-frontends.md](design/packet-frontends.md).
