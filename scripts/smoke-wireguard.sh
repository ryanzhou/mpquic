#!/usr/bin/env bash
# End-to-end test of the WireGuard listener with a real WireGuard client
# (wireguard-go + kernel routing in a container):
#
#   client --WireGuard--> mpquic local --2 paths--> mpquic remote --> target / internet
#
# Checks TCP (HTTP, bulk download), UDP (DNS, incl. a fragmented response) and
# ping. The remote runs as an unprivileged user (Docker allows unprivileged
# ICMP sockets by default). The local serves SOCKS5 too, unless
# WG_ONLY=1 (--no-socks). Needs Docker; internet access for the DNS/public
# checks (skip them with OFFLINE=1).
set -u
ROOT=$(realpath "$(dirname "$0")/..")
W=${W:-/tmp/mpquic-wg-smoke}
NET=mpq-wg-smoke
mkdir -p "$W"
cleanup() {
	docker rm -f mpq-wg-target mpq-wg-udpecho mpq-wg-remote mpq-wg-local mpq-wg-client >/dev/null 2>&1
	docker network rm "$NET" >/dev/null 2>&1
}
trap cleanup EXIT
cleanup

echo "== build"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$W/mpquic" ./cmd/mpquic) || exit 1
WGMOD=$(cd "$ROOT" && go list -m -f '{{.Dir}}' golang.zx2c4.com/wireguard)
(cd "$WGMOD" && CGO_ENABLED=0 go build -o "$W/wireguard-go" .) || exit 1
cat > "$W/Dockerfile" <<'EOD'
FROM alpine:3.20
RUN apk add --no-cache wireguard-tools-wg curl bind-tools iputils iproute2 socat
COPY wireguard-go /usr/local/bin/wireguard-go
EOD
docker build -q -t mpq-wg-client "$W" >/dev/null || exit 1

SKEY=$("$W/mpquic" wg-keygen | sed -n 's/^PrivateKey = //p')
SPUB=$(echo "$SKEY" | "$W/mpquic" wg-pubkey)
CKEY=$("$W/mpquic" wg-keygen | sed -n 's/^PrivateKey = //p')
CPUB=$(echo "$CKEY" | "$W/mpquic" wg-pubkey)

docker network create --ipv6 --subnet fd00:6d70:7771::/64 "$NET" >/dev/null || exit 1
run() { docker run -d --network "$NET" -v "$W/mpquic:/mpquic:ro" "$@" >/dev/null; }
docker run -d --network "$NET" --name mpq-wg-target traefik/whoami >/dev/null
docker run -d --network "$NET" --name mpq-wg-udpecho mpq-wg-client socat -b 65536 UDP6-LISTEN:9999,fork,ipv6only=0 PIPE >/dev/null
run --name mpq-wg-remote --user 1000:1000 alpine:3.20 /mpquic remote -p 8388 -k pw -u
SOCKS="-b 0.0.0.0 -l 1080"
[ -n "${WG_ONLY:-}" ] && SOCKS=--no-socks
run --name mpq-wg-local alpine:3.20 /mpquic local -s mpq-wg-remote:8388 -s mpq-wg-remote:8388 -k pw -u \
	$SOCKS --wg-listen 51820 --wg-private-key "$SKEY" --wg-peer "$CPUB"
TARGET=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" mpq-wg-target)
REMOTE=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" mpq-wg-remote)
UDPECHO=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" mpq-wg-udpecho)
TARGET6=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").GlobalIPv6Address}}" mpq-wg-target)
UDPECHO6=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").GlobalIPv6Address}}" mpq-wg-udpecho)
LOCAL=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" mpq-wg-local)

# The client routes the target and two public resolvers through WireGuard
# (not everything: the WireGuard endpoint itself must stay reachable).
docker run -d --network "$NET" --name mpq-wg-client --cap-add NET_ADMIN --device /dev/net/tun \
	-e WG_PROCESS_FOREGROUND=1 mpq-wg-client wireguard-go wg0 >/dev/null
sleep 1
cx() { docker exec mpq-wg-client "$@"; }
echo "$CKEY" > "$W/client.key"
docker cp "$W/client.key" mpq-wg-client:/client.key >/dev/null
cx wg set wg0 private-key /client.key peer "$SPUB" endpoint "$LOCAL:51820" allowed-ips 0.0.0.0/0,::/0 || exit 1
cx ip addr add 10.66.0.2/32 dev wg0
cx ip -6 addr add fd66::2/128 dev wg0
cx ip link set wg0 mtu 1420 up
for r in "$TARGET/32" "$UDPECHO/32" 1.1.1.1/32 8.8.8.8/32 "$TARGET6/128" "$UDPECHO6/128" 192.0.2.1/32; do cx ip route add "$r" dev wg0; done

FAIL=0
check() { # name, command...
	local name=$1; shift
	if out=$("$@" 2>&1); then echo "ok   $name: $(echo "$out" | tail -1)"; else echo "FAIL $name: $out"; FAIL=1; fi
}
echo "== tests (target $TARGET, remote $REMOTE)"
check "http via tunnel (seen from the remote)" sh -c "docker exec mpq-wg-client curl -sS -m 10 http://$TARGET/ | grep -x 'RemoteAddr: $REMOTE:[0-9]*'"
check "bulk download 50MB" docker exec mpq-wg-client sh -c \
	"curl -sS -m 60 -o /tmp/d -w '%{speed_download} B/s' 'http://$TARGET/data?size=50000000' && test \$(wc -c < /tmp/d) -eq 50000000"
check "ping target" docker exec mpq-wg-client ping -c 3 -W 2 "$TARGET"
check "no reply from a nonexistent host (pings are real)" sh -c "! docker exec mpq-wg-client ping -c 2 -W 1 192.0.2.1"
for size in 100 1300 4000 20000; do # >1392: IP fragments in both directions
	check "udp echo $size bytes" docker exec mpq-wg-client sh -c \
		"head -c $size /dev/urandom > /tmp/u; socat -b 65536 -T 3 - UDP4:$UDPECHO:9999 < /tmp/u > /tmp/v; cmp /tmp/u /tmp/v && echo same"
done
check "http over IPv6" sh -c "docker exec mpq-wg-client curl -sS -m 10 'http://[$TARGET6]/' | grep -c '^RemoteAddr: \[fd00:6d70:7771::'"
check "ping6 target" docker exec mpq-wg-client ping -6 -c 3 -W 2 "$TARGET6"
for size in 100 4000; do
	check "udp6 echo $size bytes" docker exec mpq-wg-client sh -c \
		"head -c $size /dev/urandom > /tmp/u; socat -b 65536 -T 3 - UDP6:[$UDPECHO6]:9999 < /tmp/u > /tmp/v; cmp /tmp/u /tmp/v && echo same"
done
if [ -z "${OFFLINE:-}" ]; then
	check "dns (udp) via 1.1.1.1" docker exec mpq-wg-client sh -c "dig +short +time=3 +tries=2 @1.1.1.1 example.com | grep -E '^[0-9.]+$'"
	check "https to 1.1.1.1" docker exec mpq-wg-client curl -sS -m 10 -o /dev/null -w '%{http_code}' https://1.1.1.1/
	check "ping 1.1.1.1" docker exec mpq-wg-client ping -c 3 -W 2 1.1.1.1
fi
if [ -z "${WG_ONLY:-}" ]; then
	check "socks5 alongside WireGuard" sh -c "docker exec mpq-wg-client curl -sS -m 10 --socks5 $LOCAL:1080 http://$TARGET/ | grep -x 'RemoteAddr: $REMOTE:[0-9]*'"
else
	check "no socks5 with --no-socks" sh -c "! docker exec mpq-wg-client curl -sS -m 3 --socks5 $LOCAL:1080 http://$TARGET/"
fi
check "no traffic outside the tunnel" sh -c "docker exec mpq-wg-client curl -sS -m 3 http://$REMOTE:1/ && false || true"
if [ $FAIL -ne 0 ]; then
	echo "== local log"; docker logs mpq-wg-local 2>&1 | tail -20
	echo "== remote log"; docker logs mpq-wg-remote 2>&1 | tail -20
	exit 1
fi
echo "all passed"
