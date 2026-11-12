# Documentation

## Using mpquic

| | |
|---|---|
| [configuration.md](configuration.md) | modes, every flag and config key, the config file, tuning |
| [deployment.md](deployment.md) | relays and link emulation, reverse mode, Docker, upgrading |
| [tun.md](tun.md) | the TUN interface: the machine's or a router's traffic through the tunnel |
| [wireguard.md](wireguard.md) | the WireGuard listener: other devices through the tunnel |
| [windows.md](windows.md) | the Windows service and configurator |
| [development.md](development.md) | building, tests, the `mpbench` testbed, debugging |

## How it works

One page per layer; [design/README.md](design/README.md) is the overview and
maps the packages to the pages.

| | |
|---|---|
| [design/README.md](design/README.md) | architecture, roles and reverse mode, why one QUIC connection per path, comparison with MPTCP |
| [design/protocol.md](design/protocol.md) | path setup and authentication, frames, packet sizes |
| [design/session.md](design/session.md) | streams, scheduling, acknowledgements, lossy paths, path failure |
| [design/congestion.md](design/congestion.md) | BBRv3 on every path, configured rates, loss tolerance |
| [design/proxy.md](design/proxy.md) | TCP, UDP associations, reliable UDP, ping |
| [design/packet-frontends.md](design/packet-frontends.md) | the userspace network stack behind WireGuard and the TUN interface |

## Measurements

| | |
|---|---|
| [benchmarks/multipath.md](benchmarks/multipath.md) | multipath scenarios, bad paths, common bottlenecks, CPU, SOCKS5 vs WireGuard |
| [benchmarks/congestion-control.md](benchmarks/congestion-control.md) | one stream on one path: throughput, ramp-up and added latency per controller |
