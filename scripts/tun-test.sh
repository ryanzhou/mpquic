#!/usr/bin/env bash
# End-to-end test of the TUN interface (Linux, needs root, iproute2, curl,
# ping and python3). Three network namespaces:
#
#   lan (a client) -- rt (a router: mpquic local --tun) -- wan (mpquic remote
#                                                          and the targets)
#
# 1. router: the router forwards what the client sends into the TUN
#    interface with a routing rule, as a router system would. The targets are
#    not routable from the router, so whatever arrives went through the tunnel.
# 2. host: the router sends its own traffic through the TUN interface
#    (default route in a table of its own); the QUIC sockets carry a firewall
#    mark that exempts them. The server, and the DNS server that knows its
#    name, are only reachable over the default route, so without the mark the
#    tunnel would loop into itself. The interface is a persistent one, made
#    and routed to before mpquic starts: it must stay when mpquic exits, and
#    then carry nothing.
#
# Each case checks TCP, UDP (small and fragmented), ping and a bulk transfer
# each way, over IPv4 and, where the kernel has it, IPv6.
# Usage: scripts/tun-test.sh [mpquic binary]
set -u
B=$(realpath "${1:-./bin/mpquic}")
[ "$(id -u)" = 0 ] || { echo "needs root" >&2; exit 1; }
T=$(mktemp -d)
N=mpqt$$
LAN=$N-lan RT=$N-rt WAN=$N-wan
pids=()
fail=0
V6=; [ -e /proc/sys/net/ipv6 ] && V6=1
v6() { [ -z "$V6" ] || "$@"; }

cleanup() {
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  for n in $LAN $RT $WAN; do ip netns del $n 2>/dev/null; done
  rm -rf "$T" /etc/netns/$RT
}
trap cleanup EXIT

bg() { ip netns exec "$@" & pids+=($!); } # namespace command...
in_() { local n=$1; shift; ip netns exec "$n" "$@"; }
check() { # name command...
  local name=$1; shift
  if out=$("$@" 2>&1); then echo "ok    $name${out:+  ($out)}"; else echo "FAIL  $name: $out"; fail=1; fi
}

for n in $LAN $RT $WAN; do ip netns add $n; in_ $n ip link set lo up; done
ip link add l0 netns $LAN type veth peer name lan0 netns $RT
ip link add w0 netns $RT type veth peer name wan0 netns $WAN
in_ $LAN ip addr add 10.77.0.2/24 dev l0;  v6 in_ $LAN ip -6 addr add fd77::2/64 dev l0 nodad
in_ $RT ip addr add 10.77.0.1/24 dev lan0; v6 in_ $RT ip -6 addr add fd77::1/64 dev lan0 nodad
in_ $RT ip addr add 10.78.0.2/24 dev w0;   in_ $WAN ip addr add 10.78.0.1/24 dev wan0
for x in "$LAN l0" "$RT lan0" "$RT w0" "$WAN wan0"; do in_ ${x% *} ip link set ${x#* } up; done
in_ $LAN ip route add default via 10.77.0.1; v6 in_ $LAN ip -6 route add default via fd77::1
in_ $RT ip route add default via 10.78.0.1
in_ $WAN ip route add 10.77.0.0/24 via 10.78.0.2 # never used: the tunnel hides the client
# the server (10.89.0.1) and the targets (10.88.0.1, fd88::1) live in wan
in_ $WAN ip addr add 10.89.0.1/32 dev lo; in_ $WAN ip addr add 10.88.0.1/32 dev lo
v6 in_ $WAN ip -6 addr add fd88::1/128 dev lo nodad
in_ $RT sysctl -qw net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
v6 in_ $RT sysctl -qw net.ipv6.conf.all.forwarding=1
in_ $WAN sysctl -qw net.ipv4.ping_group_range="0 2147483647"

head -c $((64 << 20)) /dev/urandom >"$T/blob"
SUM=$(sha256sum <"$T/blob" | cut -d' ' -f1)
cat >"$T/udpecho.py" <<'PY'
import socket, sys
fam = socket.AF_INET6 if ":" in sys.argv[1] else socket.AF_INET
s = socket.socket(fam, socket.SOCK_DGRAM); s.bind((sys.argv[1], 7000))
while True:
    d, a = s.recvfrom(65535); s.sendto(d, a)
PY
cat >"$T/dns.py" <<'PY'
# answers every A query with 10.89.0.1 (the server), anything else with no records
import socket, struct
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("10.89.0.1", 53))
while True:
    q, a = s.recvfrom(512)
    end = q.index(b"\0", 12) + 5
    qtype = struct.unpack("!H", q[end - 4:end - 2])[0]
    ans = b"\xc0\x0c\0\1\0\1\0\0\0\x3c\0\4" + socket.inet_aton("10.89.0.1") if qtype == 1 else b""
    s.sendto(q[:2] + b"\x81\x80\0\1" + struct.pack("!H", 1 if ans else 0) + b"\0\0\0\0" + q[12:end] + ans, a)
PY
cat >"$T/udpping.py" <<'PY'
import os, socket, sys
host, size = sys.argv[1], int(sys.argv[2])
fam = socket.AF_INET6 if ":" in host else socket.AF_INET
s = socket.socket(fam, socket.SOCK_DGRAM); s.settimeout(3)
for i in range(3):
    d = os.urandom(size); s.sendto(d, (host, 7000))
    try:
        r, a = s.recvfrom(65535)
    except socket.timeout:
        continue
    sys.exit(0 if r == d and a[0] == host else "bad reply from %s" % (a,))
sys.exit("no reply")
PY
bg $WAN python3 -m http.server -d "$T" -b ${V6:+::}${V6:-0.0.0.0} 8080 >/dev/null 2>&1
bg $WAN python3 "$T/udpecho.py" 10.88.0.1
bg $WAN python3 "$T/dns.py"
mkdir -p /etc/netns/$RT; echo "nameserver 10.89.0.1" >/etc/netns/$RT/resolv.conf
v6 bg $WAN python3 "$T/udpecho.py" fd88::1
bg $WAN "$B" remote -s 10.89.0.1 -p 8388 -k test -u --log-level warn
for _ in $(seq 100); do in_ $WAN curl -s --noproxy '*' -m 1 -o /dev/null http://10.88.0.1:8080/ && break; sleep 0.1; done

traffic() { # namespace label
  local n=$1 l=$2
  check "$l tcp ipv4" in_ $n curl -sS --noproxy '*' -m 10 -o /dev/null -w '%{http_code}' http://10.88.0.1:8080/
  v6 check "$l tcp ipv6" in_ $n curl -sS --noproxy '*' -m 10 -o /dev/null -w '%{http_code}' 'http://[fd88::1]:8080/'
  check "$l tcp refused" bash -c "! ip netns exec $n curl -sS --noproxy '*' -m 10 -o /dev/null http://10.88.0.1:9/ 2>/dev/null"
  check "$l udp 100 bytes" in_ $n python3 "$T/udpping.py" 10.88.0.1 100
  v6 check "$l udp 1000 bytes ipv6" in_ $n python3 "$T/udpping.py" fd88::1 1000
  check "$l ping" bash -c "ip netns exec $n ping -n -c 3 -i 0.2 -W 2 10.88.0.1 | tail -1"
  v6 check "$l ping ipv6" bash -c "ip netns exec $n ping -n -c 3 -i 0.2 -W 2 fd88::1 | tail -1"
  check "$l download 64 MB" bash -c "
    s=\$(ip netns exec $n curl -sS --noproxy '*' -m 60 -o '$T/got' -w '%{speed_download}' http://10.88.0.1:8080/blob) || exit 1
    [ \"\$(sha256sum <'$T/got' | cut -d' ' -f1)\" = $SUM ] || { echo corrupted; exit 1; }
    echo \"\$((\${s%.*} * 8 / 1000000)) Mbit/s\""
  check "$l upload 16 MB" bash -c "head -c $((16 << 20)) '$T/blob' | ip netns exec $n curl -sS --noproxy '*' -m 60 -o /dev/null -w '%{http_code}' -T - http://10.88.0.1:8080/up | grep -q 501"
}

wait_tun() { # the interface exists and a path is up
  for _ in $(seq 100); do
    in_ $RT ip link show mpqt0 >/dev/null 2>&1 && grep -q 'path up' "$T/local.log" && return 0
    sleep 0.1
  done
  echo "FAIL  mpquic local did not come up"; cat "$T/local.log"; exit 1
}

echo "== router: forwarded traffic of a LAN client"
bg $RT "$B" local -s 10.89.0.1 -p 8388 -k test -u --no-socks --tun mpqt0 --log-level info >"$T/local.log" 2>&1
lp=$!
wait_tun
in_ $RT ip rule add iif lan0 lookup 77;    in_ $RT ip route add default dev mpqt0 table 77
v6 in_ $RT ip -6 rule add iif lan0 lookup 77; v6 in_ $RT ip -6 route add default dev mpqt0 table 77
traffic $LAN router
check "router udp 1400 bytes (reliable UDP off: too big for a datagram, dropped)" bash -c "! ip netns exec $LAN python3 '$T/udpping.py' 10.88.0.1 1400 2>/dev/null"
kill $lp; wait $lp 2>/dev/null
check "router: the interface is gone after exit" bash -c "! ip netns exec $RT ip link show mpqt0 2>/dev/null"
in_ $RT ip rule del iif lan0 lookup 77; v6 in_ $RT ip -6 rule del iif lan0 lookup 77

echo "== interface deleted under a running mpquic"
bg $RT "$B" local -s 10.89.0.1 -p 8388 -k test --no-socks --tun mpqt0 --log-level info >"$T/local.log" 2>&1
lp=$!
wait_tun
in_ $RT ip link del mpqt0
check "mpquic exits with an error" bash -c "for _ in \$(seq 50); do kill -0 $lp 2>/dev/null || break; sleep 0.1; done; ! kill -0 $lp 2>/dev/null && grep -q 'TUN interface mpqt0' '$T/local.log'"
kill $lp 2>/dev/null; wait $lp 2>/dev/null

echo "== host: the machine's own traffic, default route through a persistent interface"
in_ $RT ip tuntap add dev mpqt0 mode tun
in_ $RT ip link set mpqt0 up
# as wg-quick: everything without the mark uses table 51, except what the
# main table knows a specific route for
for f in -4 ${V6:+-6}; do
  in_ $RT ip $f route add default dev mpqt0 table 51
  in_ $RT ip $f rule add not fwmark 0x4d51 table 51
  in_ $RT ip $f rule add table main suppress_prefixlength 0
done
check "host: nothing gets out while mpquic is not running" bash -c "! ip netns exec $RT curl -sS --noproxy '*' -m 2 -o /dev/null http://10.88.0.1:8080/ 2>/dev/null"
: >"$T/local.log"
bg $RT "$B" local -s server.test -p 8388 -k test -u --reliable-udp --no-socks --tun mpqt0 \
  --tun-addr 10.99.0.1/24 ${V6:+--tun-addr fd99::1/64} --fwmark 0x4d51 --log-level info >"$T/local.log" 2>&1
lp=$!
wait_tun
traffic $RT host
check "host udp 4000 bytes (reliable UDP, fragmented)" in_ $RT python3 "$T/udpping.py" 10.88.0.1 4000
check "host: the server is reached directly, not through the interface" bash -c "ip netns exec $RT ip route get 10.89.0.1 mark 0x4d51 | grep -q 'dev w0' && ip netns exec $RT ip route get 10.89.0.1 | grep -q 'dev mpqt0'"
kill $lp; wait $lp 2>/dev/null
check "host: the persistent interface stays, with its address and route" bash -c "ip netns exec $RT ip addr show mpqt0 | grep -q 10.99.0.1 && ip netns exec $RT ip route show table 51 | grep -q mpqt0"
check "host: nothing gets out after mpquic exits" bash -c "! ip netns exec $RT curl -sS --noproxy '*' -m 2 -o /dev/null http://10.88.0.1:8080/ 2>/dev/null"
exit $fail
