# WireGuard listener

The local can also be a **userspace WireGuard server**, in addition to (or
instead of) SOCKS5. Any standard WireGuard client (phones, routers, `wg-quick`)
connects to it, and the traffic it routes into the tunnel is proxied
transparently over the multipath connection, as if the remote were the
WireGuard server:

* **TCP** to any address, relayed like a SOCKS5 CONNECT;
* **UDP** (with `-u` on both ends), including DNS and HTTP/3: relayed like
  SOCKS5 UDP ASSOCIATE, full-cone per client socket;
* **ping** (ICMP echo, IPv4 and IPv6, with `-u` on both ends): the remote
  sends the pings and relays the replies.

## Setup

No root, TUN device or kernel module is needed on the local. Generate key
pairs for the server and for each client:

```sh
mpquic wg-keygen       # PrivateKey = ... / PublicKey = ...
```

Then run the local with the server's private key and the clients' public keys:

```sh
# WireGuard on UDP 51820 plus SOCKS5 on 127.0.0.1:1080
mpquic local -s remote.example.com:8388 -s relay-a.example.net:9000 -k 'long-random-password' -u \
             --wg-listen 51820 --wg-private-key 'SERVER_PRIVATE_KEY' --wg-peer 'CLIENT_PUBLIC_KEY'
# WireGuard only
mpquic local ... --no-socks --wg-listen 51820 --wg-private-key ... --wg-peer ...
```

Client config (`wg-quick` format). `Address` can be any private address,
since the local forwards everything and never routes between clients; set
`DNS` to a public resolver, which is then reached through the tunnel:

```ini
[Interface]
PrivateKey = CLIENT_PRIVATE_KEY
Address = 10.66.0.2/32, fd66::2/128
DNS = 1.1.1.1
MTU = 1420

[Peer]
PublicKey = SERVER_PUBLIC_KEY   # also logged by the local at startup
Endpoint = local-host.example.net:51820
AllowedIPs = 0.0.0.0/0, ::/0
```

## Options

| flag | config key (`"wireguard": {...}`) | |
|---|---|---|
| `--wg-listen [HOST:]PORT` | `listen` | UDP listen address (default port 51820, all interfaces). Any `--wg-*` option enables the listener |
| `--wg-private-key KEY` | `private_key` | the server's private key (base64, as `wg genkey`) |
| `--wg-peer PUBKEY[,IP...]` | `peers: [{"public_key", "allowed_ips"}]` | a client, repeatable. Its allowed IPs (tunnel addresses) default to any; with several clients, give each one its own |
| `--wg-psk KEY` | `peers[].preshared_key` | optional preshared key (the flag applies to every peer) |
| `--wg-mtu BYTES` | `mtu` | interface MTU, 1280–9000 (default 1420) |
| `--no-socks` | `no_socks` (top level) | do not run the SOCKS5 listener |

See [`examples/wireguard-local.json`](../examples/wireguard-local.json). Keys given on the command line are
visible to other local users (`ps`); prefer the config file on shared hosts.
`mpquic wg-pubkey` prints the public key of a private key read on stdin.
The Windows configurator keeps a `wireguard` section it finds in the config
but does not edit it (edit the JSON file), and the service installer opens
the WireGuard UDP port in the firewall.
`scripts/smoke-wireguard.sh` tests the listener end to end against a real
WireGuard client in Docker.

## Notes

* Without `-u`, the local answers UDP with ICMP port unreachable and drops
  pings (and logs a warning at startup).
* For pings the remote opens an
  unprivileged ICMP socket (Linux: allowed by `net.ipv4.ping_group_range`,
  which Docker enables in containers; macOS: always), or else a raw socket
  (root / `CAP_NET_RAW`; Windows: Administrator). Otherwise it logs a
  warning and pings time out. Only echo is relayed: ICMP errors from the
  internet (for example for `traceroute`) are not.
* As with SOCKS5 UDP, a datagram that the paths cannot carry is dropped (see
  [UDP payload sizes](design/proxy.md#udp-payload-sizes)); the local then sends the client an ICMP
  "fragmentation needed"/"packet too big" with the tunnel's limit. With
  `--reliable-udp`, UDP goes over streams and has no size limit.
* TCP handshakes complete in the local right away (as SOCKS5 replies
  immediately): a connection the remote cannot make is reset after
  connecting instead of being refused.

For traffic of the machine that runs the local, or that it routes, the
[TUN interface](tun.md) does the same without WireGuard in between. How the
listener works: [design/packet-frontends.md](design/packet-frontends.md).
