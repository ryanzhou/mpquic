# Building, testing and benchmarking

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

`go test ./...` needs no privileges. CI runs it with the race detector on
Linux, and without on Windows.

## End-to-end tests

| script | what it runs |
|---|---|
| `scripts/smoke.sh` | remote, two emulated relays and a local on one machine; `curl` through SOCKS5 |
| `scripts/smoke-reverse.sh` | the same in reverse mode |
| `scripts/smoke-wireguard.sh` | the WireGuard listener against a real WireGuard client (Docker) |
| `scripts/tun-test.sh` | the TUN interface in network namespaces (root): a router forwarding a client, and a host sending everything through a persistent interface with `--fwmark`; also run in CI |
| `scripts/windows-smoke.ps1` | the Windows service, CLI and configurator (CI) |

`deploy/docker-compose.yml` is a self-contained testbed (a remote, three
emulated relay paths, a local SOCKS5 endpoint);
`deploy/docker-compose.reverse.yml` is the same in reverse mode. See
[deployment.md](deployment.md#docker).

## mpbench

`mpbench` builds a complete in-process testbed: a target server, a remote,
one emulated relay per `--path`, and a local. It then measures echo latency,
connection setup, bulk download/upload, flow completion times, latency under
load, and failover (best path cut mid-transfer). It also runs every path alone
as a baseline:

```sh
mpbench --path delay=10ms,rate=40mbit --path delay=30ms,rate=40mbit,loss=1%
scripts/bench-matrix.sh        # the scenarios in docs/benchmarks/multipath.md
MPQUIC_TRACE=1 mpbench ...     # log every scheduling decision
```

The links are `internal/linkem` links, with the same impairment keys as
`mpquic relay` ([deployment.md](deployment.md#link-emulation)); `--shared` /
`--shared-rx` put a common bottleneck in front of or behind all paths (an
endpoint's uplink or downlink), and `--up` / `--down` set the configured
rates. The scripts behind each table are listed at the end of
[benchmarks/multipath.md](benchmarks/multipath.md#reproduce) and at the top
of [benchmarks/congestion-control.md](benchmarks/congestion-control.md).

## Debugging

| environment variable | effect |
|---|---|
| `MPQUIC_TRACE=1` | log every scheduling decision |
| `MPQUIC_RATE_DEBUG=1` | log every round of the session rate |
| `MPQUIC_DGRAM_LOSS=FRACTION` | the path loss from which stream data travels as datagrams (default 0.08) |
| `GOMAXPROCS=N` | pin the number of threads |

`--stats N` logs the session's and every path's state every N seconds; `-v`
turns on debug logging (every connection).
