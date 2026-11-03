# MPQUIC

**Multipath QUIC tunnel proxy, shadowsocks-style.** `mpquic local` is a SOCKS5
server (like `ss-local`); `mpquic remote` forwards to the targets (like
`ss-server`). The local instance reaches the remote over **several paths at
once**: the direct address plus any number of plain UDP relays (`socat`, or
the built-in `mpquic relay`). Every path is a QUIC connection, and all of them
land on the same remote instance. A multipath scheduler built for this
setting spreads proxied connections across them:

* **bulk transfers** get close to the *sum* of all path capacities;
* **small/interactive traffic** keeps the latency of the *best* path, and is
  protected against loss and outages on individual paths (redundant copies on
  idle paths, sub-second failover);
* paths come and go transparently (relay restarts, NAT rebinding, blackouts).

The focus is userspace multipath performance, not obfuscation. Traffic is
QUIC/TLS 1.3, and peers authenticate each other with the shared password.

```
 browser ──SOCKS5──> mpquic local ═══ direct UDP ═══════════════╗
                                  ═══ UDP ─> relay A (socat) ═══╬══> mpquic remote ──> internet
                                  ═══ UDP ─> relay B (socat) ═══╝
```

Read [docs/DESIGN.md](docs/DESIGN.md) for the protocol and the scheduling
algorithm, [docs/BENCHMARKS.md](docs/BENCHMARKS.md) for multipath measurements,
and [docs/CONGESTION.md](docs/CONGESTION.md) for single-stream / congestion
control results (for example, reachable throughput at 150 ms RTT).

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

### Reverse mode (the local side has the public IP)

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
`examples/reverse-*.json`. Keepalive pings hold the remote's NAT mappings
open. Until the remote has connected, the local refuses SOCKS requests
immediately. If the remote restarts, its new session immediately replaces
the old one. `deploy/docker-compose.reverse.yml` is a reverse-mode testbed,
and `mpbench --reverse` benchmarks this setup (performance matches normal
mode).

### Docker

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

### Windows (service and configurator)

The Windows release (`mpquic-windows-amd64.exe`, `mpquic-windows-arm64.exe`)
is a single executable with three roles:

* **Command line:** `mpquic.exe local|tunnel|remote|relay ...` works exactly
  as on Linux (Ctrl-C to stop).
* **Configurator:** double-click the exe (or run `mpquic.exe gui`) to open a
  GUI that edits the settings, installs and updates the Windows service,
  starts, stops and restarts it, and shows its log. It asks for administrator
  rights (UAC) when it starts.
* **Windows service:** each instance runs one role (`local`, `tunnel`,
  `remote` or `relay`) from a config file, starts at boot, and is restarted
  automatically if it fails.

From an elevated prompt, the same can be scripted. `service install` takes
the role and the same options as the foreground command:

```bat
mpquic.exe service install remote -p 8388 -k long-random-password -u
mpquic.exe service install local -s remote.example.com:8388 -s relay-a.example.net:9000 -k long-random-password -u
mpquic.exe service install --name relay relay --listen :9000 --target remote.example.com:8388
:: or from an existing JSON config:
mpquic.exe service install local -c C:\path\to\local.json
mpquic.exe service status | start | stop | restart  [--name NAME]
mpquic.exe service uninstall [--name NAME] [--purge]
```

| | |
|---|---|
| binary | `%ProgramFiles%\MPQUIC\mpquic.exe`. `install` copies the running exe there. To upgrade, run the new exe's `service install` (or *Save and apply* in the GUI) |
| config | `%ProgramData%\MPQUIC\NAME.json` (the usual JSON format plus `"role"`). Readable only by Administrators, SYSTEM and the service |
| log | `%ProgramData%\MPQUIC\logs\NAME.log`, rotated at 10 MB. Start failures also go to the Application event log |
| account | `NT AUTHORITY\LocalService`. TLS `cert`/`key` files must be readable by it (for example, put them in `%ProgramData%\MPQUIC`) |
| firewall | `install` adds an inbound Windows Firewall rule for the QUIC/relay UDP ports. For SOCKS5/tunnel listeners, only when bound to a non-loopback address |

`NAME` defaults to `MPQUIC`. Use `--name` to run several instances, for
example a relay next to a local. The Windows build is console-mode. On
Windows 11 24H2 and later, double-clicking opens no console window. On
earlier versions, a console window flashes briefly before the configurator
appears.

## Build

Go ≥ 1.26:

```sh
go build -o mpquic ./cmd/mpquic      # the deployable binary (static with CGO_ENABLED=0)
go build -o mpbench ./cmd/mpbench    # in-process benchmark/testbed
go test ./...
make dist                            # all release binaries, including Windows (dist/)
```

The Windows resources (manifest, icon, version info) live in
`cmd/mpquic/winres` and are compiled into the committed
`cmd/mpquic/rsrc_windows_*.syso` files by `make winres`.

## Modes and options

| mode | shadowsocks equivalent | purpose |
|---|---|---|
| `mpquic local` | `ss-local` / `sslocal` | SOCKS5 server (CONNECT + UDP ASSOCIATE with `-u`) |
| `mpquic tunnel -L host:port` | `ss-tunnel` | fixed-target TCP port forward |
| `mpquic remote` | `ss-server` / `ssserver` | accepts paths, dials targets |
| `mpquic local --reverse` / `remote --reverse` | – | same, but the local accepts the paths and the remote dials them |
| `mpquic relay` | `socat UDP-LISTEN,fork UDP:…` | UDP forwarder, optional link emulation |

Shadowsocks-compatible flags:

| flag | meaning |
|---|---|
| `-c FILE` | JSON config (shadowsocks-libev or shadowsocks-rust format) |
| `-s HOST[:PORT]` | **repeatable**: one path per server address (local), or bind address (remote) |
| `-p PORT` | default server port (8388) |
| `-b ADDR` / `-l PORT` | local SOCKS5 listen address / port (127.0.0.1:1080) |
| `-k PASSWORD` | shared password |
| `-m METHOD` | accepted and ignored: QUIC (TLS 1.3) always encrypts |
| `-t SECONDS` | idle timeout of proxied connections (300) |
| `-u` / `-U` | enable UDP relay; `mode: tcp_and_udp` in the config. Set it on both ends |
| `-L HOST:PORT` | tunnel target (tunnel mode) |
| `-v` | verbose logging |

`--fast-open`, `--reuse-port`, `--no-delay`, `-a`, `-f`, `-n`, `-6`,
`--mptcp` are accepted and ignored. SIP003 plugins are rejected.

MPQUIC-specific flags (and config keys):

| flag | config key | default | |
|---|---|---|---|
| `--congestion` | `congestion` | `bbr` | `bbr`, `bbr-aggressive`, `bbr-conservative`, `cubic` (quic-go built-in, for comparison) |
| `--window` | `window` | `64MB` | maximum per-stream window (auto-tuned from 4 MB); one stream's rate ≤ window / slowest RTT |
| `--chunk` | `chunk_size` | `16KB` | scheduling unit |
| `--stats N` | `stats_interval` | off | log per-path RTT/bandwidth/bytes every N s |
| `--reverse` | `reverse` | false | reverse mode (set on both ends): the local listens, the remote dials back |
| `--no-dup` | `no_dup` | false | disable redundant copies on idle paths |
| `--race-connect` | `race_connect` | false | remote only (incl. reverse), aggressive: open each outbound TCP connection by racing up to 5 attempts started at 0, 100, 300, 600 and 1000 ms (each from a new source port, rotating over the target's IPs, IPv6/IPv4 interleaved); the first to connect wins and the rest are aborted. For networks or NATs that drop connection setup, where a lost SYN otherwise stalls the connection for 1 s, 3 s or more. An attempt that fails outright starts the next at once |
| `--mtu BYTES` | `mtu` | discover | path MTU (IP packet size, 1228–65535) of every path: fixed QUIC packet size, no MTU discovery. Only needed if the MTU drops mid-connection (see docs/DESIGN.md, "Packet sizes") |
| `--cert/--key` | `cert`/`key` | self-signed | server TLS certificate (optional, see security notes) |
| `--sni` | `sni` | `mpquic` | client TLS SNI |

### Config file

Shadowsocks configs work unchanged; `server` may now be a list (one entry per
path), and entries without a port use `server_port`:

```json
{
  "server": ["203.0.113.10", "relay-a.example.net:9000", "relay-b.example.net:9000"],
  "server_port": 8388,
  "local_address": "127.0.0.1",
  "local_port": 1080,
  "password": "change-me",
  "method": "chacha20-ietf-poly1305",
  "mode": "tcp_and_udp",
  "timeout": 300
}
```

shadowsocks-rust's `"servers": [{"server": …, "server_port": …, "password": …}]`
is accepted too; in MPQUIC all entries are paths to the *same* remote. See
`examples/` for local, remote, relay and ss-rust-style configs.

## Relays and the link emulator

A relay is any UDP forwarder from a public port to the remote's UDP port.
Each local→relay flow needs its own upstream socket, which socat's `fork` and
`mpquic relay` both provide. Relays hold no keys and see only QUIC packets.
If a relay restarts, QUIC migrates the connection to the new source port. If
it stays down, the path is declared suspect within ~0.6 s, its in-flight data
is reinjected on the other paths, and the path is re-dialed until the relay
comes back.

`mpquic relay` can also **emulate** network impairments per direction, which
is useful to test deployments:

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

## Benchmarking

`mpbench` builds a complete in-process testbed: a target server, a remote,
one emulated relay per `--path`, and a local. It then measures echo latency,
connection setup, bulk download/upload, flow completion times, latency under
load, and failover (best path cut mid-transfer). It also runs every path alone
as a baseline:

```sh
mpbench --path delay=10ms,rate=40mbit --path delay=30ms,rate=40mbit,loss=1%
scripts/bench-matrix.sh        # the scenarios in docs/BENCHMARKS.md
MPQUIC_TRACE=1 mpbench ...     # log every scheduling decision
```

## Upgrading

The wire protocol has a version number that both ends check during the
handshake. Version 2 sends the control plane (pings, ACKs) as QUIC datagrams
and adds datagram-carried data on very lossy paths. Upgrade the local and the
remote together: a version-1 peer is rejected.

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

## Project layout

```
cmd/mpquic        CLI (local / tunnel / remote / relay), shadowsocks-compatible flags
cmd/mpbench       in-process testbed & benchmarks
internal/app      runs a role from a config (shared by the CLI, service and GUI)
internal/winsvc   Windows service: install/update, run, firewall, ACLs
internal/gui      Windows configurator (tailscale/walk)
internal/logfile  rotating log file
internal/mp       multipath session: paths, streams, scheduler, ACKs, auth
internal/cc       congestion-control glue + vendored BBR (from Hysteria, MIT)
internal/proxy    SOCKS5 / tunnel / remote dialer / UDP associate
internal/linkem   UDP relay with link emulation
internal/config   shadowsocks-compatible JSON config
deploy/           docker-compose testbed
```
