# Configuration

Everything can be given on the command line or in a JSON config file
(`-c FILE`); flags override the file. `mpquic MODE --help` prints the flags.

## Modes

| mode | shadowsocks equivalent | purpose |
|---|---|---|
| `mpquic local` | `ss-local` / `sslocal` | SOCKS5 server (CONNECT + UDP ASSOCIATE with `-u`), optionally a TUN interface and a WireGuard server too |
| `mpquic tunnel -L host:port` | `ss-tunnel` | fixed-target port forward: TCP, plus UDP on the same port with `-u` |
| `mpquic remote` | `ss-server` / `ssserver` | accepts paths, dials targets |
| `mpquic local --reverse` / `remote --reverse` | – | same, but the local accepts the paths and the remote dials them |
| `mpquic relay` | `socat UDP-LISTEN,fork UDP:…` | UDP forwarder, optional [link emulation](deployment.md#link-emulation) |

For example, a DNS forwarder (TCP and UDP on 127.0.0.1:5353), with UDP
carried reliably over streams:

```sh
mpquic remote -k secret -u
mpquic tunnel -s remote.example.com -k secret -u --reliable-udp -l 5353 -L 1.1.1.1:53
```

Reverse mode is described in [deployment.md](deployment.md#reverse-mode-the-local-side-has-the-public-ip).
`mpquic wg-keygen` and `mpquic wg-pubkey` make WireGuard keys
([wireguard.md](wireguard.md)); on Windows there are also `mpquic gui` and
`mpquic service` ([windows.md](windows.md)).

## Shadowsocks-compatible flags

| flag | meaning |
|---|---|
| `-c FILE` | JSON config (shadowsocks-libev or shadowsocks-rust format) |
| `-s HOST[:PORT]` | **repeatable**: one path per server address (local), or bind address (remote) |
| `-p PORT` | default server port (8388) |
| `-b ADDR` / `-l PORT` | local SOCKS5 listen address / port (127.0.0.1:1080) |
| `-k PASSWORD` | shared password |
| `-m METHOD` | accepted and ignored: QUIC (TLS 1.3) always encrypts |
| `-t SECONDS` | idle timeout of proxied connections (300) |
| `-u` / `-U` | enable UDP relay (SOCKS5 UDP ASSOCIATE; the UDP tunnel in tunnel mode); `mode: tcp_and_udp` in the config. Set it on both ends |
| `-L HOST:PORT` | tunnel target (tunnel mode) |
| `-v` | verbose logging |

`--fast-open`, `--reuse-port`, `--no-delay`, `-a`, `-f`, `-n`, `-6`,
`--mptcp` are accepted and ignored. SIP003 plugins are rejected.
`--congestion` and `--no-coupling` (and the `congestion` / `no_coupling`
keys) of earlier releases are accepted and ignored too: there is one
congestion controller, the session's.

## MPQUIC options

| flag | config key | default | |
|---|---|---|---|
| `--window` | `window` | `64MB` | maximum per-stream window (auto-tuned from 4 MB); one stream's rate ≤ window / slowest RTT |
| `--chunk` | `chunk_size` | `16KB` | scheduling unit |
| `--stats N` | `stats_interval` | off | log the session rate and per-path RTT/bandwidth/bytes every N s |
| `--reverse` | `reverse` | false | reverse mode (set on both ends): the local listens, the remote dials back |
| `--no-dup` | `no_dup` | false | disable redundant copies on idle paths |
| `--up RATE` / `--down RATE` | `up` / `down` | measured | this endpoint's uplink / downlink rate, if known (`20mbit`, `"100 mbps"`; config numbers are bit/s). The session never sends more than `up` over all paths together and tells the peer to send no more than `down`; set them a little below the real rates (95%) and the line's buffer stays empty. Without them the session measures what it delivers over all paths and probes for more; see [session rate](design/congestion.md#the-session-rate) |
| `--race-connect` | `race_connect` | false | remote only (incl. reverse), aggressive: open each outbound TCP connection by racing up to 5 attempts started at 0, 100, 300, 600 and 1000 ms (each from a new source port, rotating over the target's IPs, IPv6/IPv4 interleaved); the first to connect wins and the rest are aborted. For networks or NATs that drop connection setup, where a lost SYN otherwise stalls the connection for 1 s, 3 s or more. An attempt that fails outright starts the next at once |
| `--reliable-udp` | `reliable_udp` | false | local/tunnel only, needs `-u`: relay all UDP over streams (retransmitted, congestion-controlled, any size; in order, so a loss delays later datagrams of the same association) instead of QUIC datagrams, which drop payloads the path MTU cannot carry. Datagrams sent faster than the tunnel carries them are dropped once ~200 ms are queued. The remote needs `-u` too. See [reliable UDP](design/proxy.md#reliable-udp) |
| `--mtu BYTES` | `mtu` | discover | path MTU (IP packet size, 1228–65535) of every path: fixed QUIC packet size, no MTU discovery. Only needed if the MTU drops mid-connection (see [packet sizes](design/protocol.md#packet-sizes-mtu)) |
| `--cert/--key` | `cert`/`key` | self-signed | server TLS certificate (optional: the peers authenticate each other with the password, not the certificate) |
| `--sni` | `sni` | `mpquic` | client TLS SNI |
| `--no-socks` | `no_socks` | false | local only: do not run the SOCKS5 listener (with a TUN interface or a WireGuard listener) |
| `--wg-*` | `wireguard` | off | WireGuard listener, see [wireguard.md](wireguard.md) |
| `--tun*`, `--fwmark` | `tun`, `fwmark` | off | TUN interface and the firewall mark of the QUIC sockets, see [tun.md](tun.md) |

## Config file

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
[`examples/`](../examples) for local, remote, relay, reverse, WireGuard, TUN
and ss-rust-style configs.

## Tuning

The defaults are meant to be right; these are the options that depend on
the deployment.

* `window` (default 64 MB) is the *maximum* per-stream window. Each stream
  starts at 4 MB and doubles whenever a full window is consumed within 4× the
  slowest path's RTT, i.e. when the window is what limits the stream. The
  sender buffers no more than the peer currently allows, so a slow consumer
  keeps its window, and the memory it pins, small. A stream's rate is
  bounded by window / (slowest path RTT including queueing): 64 MB is
  ~1.3 Gbit/s at 150 ms (see
  [benchmarks/congestion-control.md](benchmarks/congestion-control.md)).
* `chunk_size` (16 KB) is the scheduling granularity.
* `no_dup`: disables duplication if the relay traffic is metered.
* `up` / `down` (e.g. `"20mbit"`): this endpoint's uplink and downlink
  rates, if known. Ceilings for what the session sends and receives over
  all paths together; the rates are measured without them. Set them a
  little below the real rates (95%) and the line's buffer stays empty. See
  [configured rates](design/congestion.md#configured-rates).
* `stats_interval` (`--stats N`): logs the session rate (`rate=`, `limit=`)
  and, per path, RTT, the rate it paces at (`own=`: its own rate, if it
  is held to one below an equal share), bytes and loss every N seconds: the first
  thing to look at when a path misbehaves.
* `GOMAXPROCS` (environment): the number of threads is adapted to the load;
  setting it pins the number (see [design/README.md](design/README.md#threads)).
