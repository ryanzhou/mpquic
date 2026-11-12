# MPQUIC

**Multipath QUIC tunnel proxy, shadowsocks-style.** `mpquic local` takes
traffic in (as a SOCKS5 proxy, a network interface or a WireGuard server) and
`mpquic remote` sends it on to the internet. Between the two it travels over
**several paths at once**: the direct address plus any number of plain UDP
relays (`socat`, or the built-in `mpquic relay`). A scheduler built for this
setting spreads the connections across the paths:

* **bulk transfers** get close to the *sum* of all path capacities;
* **small and interactive traffic** keeps the latency of the *best* path, and
  is protected against loss and outages on individual paths (redundant
  copies on idle paths, sub-second failover);
* paths come and go transparently (relay restarts, NAT rebinding, blackouts).

```
 browser ──SOCKS5──> mpquic local ═══ direct UDP ═══════════════╗
                                  ═══ UDP ─> relay A (socat) ═══╬══> mpquic remote ──> internet
                                  ═══ UDP ─> relay B (socat) ═══╝
```

The focus is multipath performance, not obfuscation: the traffic is plain
QUIC (TLS 1.3), and the two ends authenticate each other with a shared
password.

## Install

Binaries for Linux (amd64, arm64, arm), macOS (arm64) and Windows (amd64,
arm64) are on the [releases page](https://github.com/ryanzhou/mpquic/releases).
Or build it (Go ≥ 1.26), or use Docker:

```sh
go build -o mpquic ./cmd/mpquic
docker build -t mpquic .
```

## Quick start

On the remote host:

```sh
mpquic remote -p 8388 -k 'long-random-password' -u
```

On each relay host (any UDP forwarder works):

```sh
socat UDP-LISTEN:9000,fork,reuseaddr UDP:remote.example.com:8388
# or: mpquic relay --listen :9000 --target remote.example.com:8388
```

On the local host, list every path with `-s` (the direct one and each relay):

```sh
mpquic local -s remote.example.com:8388 -s relay-a.example.net:9000 -s relay-b.example.net:9000 \
             -l 1080 -k 'long-random-password' -u
curl --socks5-hostname 127.0.0.1:1080 https://example.com/
```

All paths leave through the local's uplink and come back through its
downlink, and a home or office line is usually slower than the relays.
mpquic finds out what the line carries, but if you know its rates, say so (a
little below the real ones) and it doesn't have to find them by filling the
line:

```sh
mpquic local ... --up 19mbit --down 95mbit     # a 100/20 Mbit/s line
```

## Ways in

The local can take traffic in several ways, all at once if you like. Each
carries TCP, and with `-u` on both ends also UDP (and ping, where it applies).

| | for | enable with | guide |
|---|---|---|---|
| **SOCKS5 proxy** | programs that can use a proxy | on by default (`-b`, `-l`) | [configuration](docs/configuration.md) |
| **Port forward** | one fixed target | `mpquic tunnel -L host:port` | [configuration](docs/configuration.md#modes) |
| **Network interface** (TUN) | everything this machine, or a router, routes to it; Linux and macOS, needs root | `--tun mpquic0` | [tun](docs/tun.md) |
| **WireGuard server** | other devices with any WireGuard client: phones, laptops, routers | `--wg-listen 51820` | [wireguard](docs/wireguard.md) |

```sh
# an interface: route what you like into it
mpquic local -s remote.example.com:8388 -k 'long-random-password' -u \
             --tun mpquic0 --tun-addr 10.99.0.1/24 --tun-route 10.0.0.0/8

# a WireGuard server for your devices (keys: mpquic wg-keygen)
mpquic local -s remote.example.com:8388 -k 'long-random-password' -u \
             --wg-listen 51820 --wg-private-key 'SERVER_PRIVATE_KEY' --wg-peer 'CLIENT_PUBLIC_KEY'

# a DNS forwarder on 127.0.0.1:5353
mpquic tunnel -s remote.example.com:8388 -k 'long-random-password' -u -l 5353 -L 1.1.1.1:53
```

## Options you are likely to need

Flags and config files follow shadowsocks (`ss-local`, `ss-server`,
`ss-tunnel`), so existing configs work; `server` may be a list, one entry
per path.

| flag | |
|---|---|
| `-s HOST[:PORT]` | a path: the remote's address or a relay's (repeatable). On the remote: the address to listen on |
| `-p PORT` | default port (8388) |
| `-k PASSWORD` | the shared password |
| `-u` | relay UDP (and ping) too; set it on both ends |
| `-b ADDR`, `-l PORT` | where the SOCKS5 proxy listens (127.0.0.1:1080) |
| `-c FILE` | JSON config file instead of flags; see [`examples/`](examples) |
| `--up RATE`, `--down RATE` | the rates of this end's line, if known |
| `--loss-tolerance PCT` | for paths known to lose packets at random: how much loss is not congestion (default: 2%) |
| `--reliable-udp` | carry UDP reliably, for datagrams too large for the paths and for lossy paths |
| `--reverse` | the local has the public address and the remote connects to it |
| `--stats N` | log the state of the session and of every path every N seconds |

Everything else, with defaults and tuning advice, is in
[docs/configuration.md](docs/configuration.md).

## More setups

* **Relays** hold no keys and see only QUIC packets; any UDP forwarder will
  do. `mpquic relay` can also emulate a bad link (delay, loss, rate limits,
  outages) to try a setup before relying on it. See
  [deployment](docs/deployment.md#relays).
* **Reverse mode**: when the local side is the reachable one and the remote
  sits behind NAT. See
  [deployment](docs/deployment.md#reverse-mode-the-local-side-has-the-public-ip).
* **Docker**: the image runs every mode; `deploy/` has compose testbeds. See
  [deployment](docs/deployment.md#docker).
* **Windows**: one executable that is the command line, a configurator and a
  Windows service. See [windows](docs/windows.md).
* **Upgrading**: upgrade both ends together when the protocol version
  changes. See [deployment](docs/deployment.md#upgrading).

## Security notes

* The transport is QUIC with TLS 1.3. Both peers prove knowledge of the
  password with an HMAC bound to the TLS exporter of the connection, so a MITM
  can't impersonate either side. Certificates are not verified (the server
  generates an ephemeral one).
* An active MITM *can* capture an HMAC to mount an offline dictionary attack
  on the password: use a long random password.
* MPQUIC does not try to look like anything other than QUIC. It's not an
  anti-censorship tool.
* Per-path keys come from separate TLS handshakes; relays see only
  ciphertext.

## Documentation

[docs/](docs/README.md) has the full index.

* **Using it**: [configuration](docs/configuration.md),
  [deployment](docs/deployment.md), [TUN interface](docs/tun.md),
  [WireGuard listener](docs/wireguard.md), [Windows](docs/windows.md),
  [building and testing](docs/development.md).
* **How it works**: the [design overview](docs/design/README.md), then one
  page per layer: [wire protocol](docs/design/protocol.md),
  [session and scheduler](docs/design/session.md),
  [congestion control](docs/design/congestion.md),
  [proxying](docs/design/proxy.md),
  [packet front-ends](docs/design/packet-frontends.md).
* **Measurements**: [multipath benchmarks](docs/benchmarks/multipath.md),
  [congestion control](docs/benchmarks/congestion-control.md).
