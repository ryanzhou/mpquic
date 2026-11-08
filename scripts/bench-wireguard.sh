#!/usr/bin/env bash
# Benchmark the SOCKS5 and WireGuard entrypoints of mpquic local against each
# other, in Docker:
#
#   client --SOCKS5 (TCP)-------> mpquic local ==paths==> mpquic remote --> target
#   client --WireGuard (kernel)-> (same local, same multipath session)
#
# Scenarios (SCENARIOS="lan wan"):
#   lan  one direct path, no emulation: CPU-bound ceiling and overhead
#   wan  two emulated paths through mpquic relays (WAN_A / WAN_B specs)
# A "direct" baseline (client -> target, no tunnel) runs in lan.
# The client uses the kernel WireGuard implementation. All containers share
# the host's CPUs. Results: $W/results.txt and a summary table.
set -u
ROOT=$(realpath "$(dirname "$0")/..")
W=${W:-/tmp/mpquic-wg-bench}
NET=mpq-wg-bench
REPS=${REPS:-3}
DUR=${DUR:-10s}
SCENARIOS=${SCENARIOS:-lan wan}
WAN_A=${WAN_A:-delay=10ms,rate=100mbit,queue=50ms}
WAN_B=${WAN_B:-delay=25ms,rate=100mbit,queue=50ms}
C="mpqb-target mpqb-remote mpqb-relay-a mpqb-relay-b mpqb-local mpqb-client mpqb-direct"
mkdir -p "$W"
cleanup() {
	docker rm -f $C >/dev/null 2>&1
	docker network rm "$NET" >/dev/null 2>&1
}
trap cleanup EXIT
cleanup

echo "== build" >&2
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$W/mpquic" ./cmd/mpquic && CGO_ENABLED=0 go build -o "$W/netbench" ./scripts/netbench) || exit 1
cat > "$W/Dockerfile" <<'EOD'
FROM alpine:3.20
RUN apk add --no-cache wireguard-tools-wg iproute2
EOD
docker build -q -t mpq-wg-bench "$W" >/dev/null || exit 1

SKEY=$("$W/mpquic" wg-keygen | sed -n 's/^PrivateKey = //p')
SPUB=$(echo "$SKEY" | "$W/mpquic" wg-pubkey)
CKEY=$("$W/mpquic" wg-keygen | sed -n 's/^PrivateKey = //p')
CPUB=$(echo "$CKEY" | "$W/mpquic" wg-pubkey)
echo "$CKEY" > "$W/client.key"

docker network create "$NET" >/dev/null || exit 1
run() { docker run -d --network "$NET" -v "$W:/b:ro" "$@" >/dev/null || exit 1; }
ip_of() { docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$1"; }
run --name mpqb-target mpq-wg-bench /b/netbench server
run --name mpqb-remote mpq-wg-bench /b/mpquic remote -p 8388 -k pw -u
run --name mpqb-client --cap-add NET_ADMIN mpq-wg-bench sleep infinity
run --name mpqb-direct mpq-wg-bench sleep infinity # baseline client, no WireGuard route
TARGET=$(ip_of mpqb-target)
cx() { docker exec mpqb-client "$@"; }
cpu() { docker exec "$1" sh -c 'sed -n "s/^usage_usec //p" /sys/fs/cgroup/cpu.stat'; }

RES="$W/results.txt"
: > "$RES"

start_local() { # paths...
	docker rm -f mpqb-local >/dev/null 2>&1
	local paths=()
	for p in "$@"; do paths+=(-s "$p"); done
	run --name mpqb-local mpq-wg-bench /b/mpquic local "${paths[@]}" -k pw -u -b 0.0.0.0 -l 1080 \
		--wg-listen 51820 --wg-private-key "$SKEY" --wg-peer "$CPUB"
	LOCAL=$(ip_of mpqb-local)
	cx ip link del wg0 2>/dev/null
	cx ip link add wg0 type wireguard || exit 1
	docker cp "$W/client.key" mpqb-client:/client.key >/dev/null
	cx wg set wg0 private-key /client.key peer "$SPUB" endpoint "$LOCAL:51820" allowed-ips 0.0.0.0/0
	cx ip addr add 10.66.0.2/32 dev wg0
	cx ip link set wg0 mtu 1420 up
	cx ip route add "$TARGET/32" dev wg0
	sleep 2
	# wait until both entrypoints work
	for i in $(seq 50); do
		cx /b/netbench client -target "$TARGET" -socks "$LOCAL:1080" -mode connect -n 1 >/dev/null 2>&1 &&
			cx /b/netbench client -target "$TARGET" -mode connect -n 1 >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	echo "entrypoints not ready" >&2; docker logs mpqb-local >&2; exit 1
}

# bench SCENARIO ENTRY TEST ARGS...: runs one measurement, appends a result line.
bench() {
	local sc=$1 entry=$2 test=$3; shift 3
	local via=()
	case $entry in
	socks) via=(-socks "$LOCAL:1080") ;;
	wireguard) ;; # routed through wg0
	direct) ;;    # client -> target over the bridge
	esac
	local l0 r0 t0 l1 r1 t1 out cl=mpqb-client
	[ "$entry" = direct ] && cl=mpqb-direct
	l0=$(cpu mpqb-local); r0=$(cpu mpqb-remote); t0=$(date +%s%N)
	out=$(docker exec $cl /b/netbench client -target "$TARGET" "${via[@]}" "$@" 2>&1)
	l1=$(cpu mpqb-local); r1=$(cpu mpqb-remote); t1=$(date +%s%N)
	local wall=$(( (t1 - t0) / 1000 ))
	local line="scenario=$sc entry=$entry test=$test $out local_cpu=$(( (l1 - l0) * 100 / wall )) remote_cpu=$(( (r1 - r0) * 100 / wall ))"
	echo "$line" | tee -a "$RES" >&2
}

suite() { # scenario entries...
	local sc=$1; shift
	for rep in $(seq "$REPS"); do
		for e in "$@"; do
			bench "$sc" "$e" down1 -mode down -P 1 -t "$DUR"
			bench "$sc" "$e" up1 -mode up -P 1 -t "$DUR"
			bench "$sc" "$e" down4 -mode down -P 4 -t "$DUR"
			bench "$sc" "$e" up4 -mode up -P 4 -t "$DUR"
		done
	done
	for e in "$@"; do
		bench "$sc" "$e" rr64 -mode rr -size 64 -n "${RR_N:-1000}"
		bench "$sc" "$e" connect -mode connect -n "${CONN_N:-200}"
		bench "$sc" "$e" udp64 -mode udprr -size 64 -n "${RR_N:-1000}"
		bench "$sc" "$e" udp1200 -mode udprr -size 1200 -n "${RR_N:-1000}"
	done
}

for sc in $SCENARIOS; do
	echo "== scenario $sc" >&2
	case $sc in
	lan)
		start_local mpqb-remote:8388
		suite lan ${LAN_ENTRIES:-direct socks wireguard}
		;;
	wan)
		docker rm -f mpqb-relay-a mpqb-relay-b >/dev/null 2>&1
		run --name mpqb-relay-a mpq-wg-bench /b/mpquic relay --listen :9000 --target mpqb-remote:8388 --link "$WAN_A"
		run --name mpqb-relay-b mpq-wg-bench /b/mpquic relay --listen :9000 --target mpqb-remote:8388 --link "$WAN_B"
		start_local mpqb-relay-a:9000 mpqb-relay-b:9000
		suite wan socks wireguard
		;;
	esac
done

python3 - "$RES" <<'EOPY'
import sys, statistics, collections
rows = [dict(kv.split("=", 1) for kv in l.split() if "=" in kv) for l in open(sys.argv[1]) if "mode=" in l]
groups = collections.defaultdict(list)
for r in rows:
    groups[(r["scenario"], r["test"], r["entry"])].append(r)
order = ["direct", "socks", "wireguard"]
for sc in dict.fromkeys(r["scenario"] for r in rows):
    print(f"\n### {sc}\n")
    print("| test | entry | result | local CPU % | remote CPU % |")
    print("|---|---|---|---|---|")
    for test in dict.fromkeys(r["test"] for r in rows if r["scenario"] == sc):
        for e in order:
            g = groups.get((sc, test, e))
            if not g:
                continue
            med = lambda k: statistics.median(float(x[k]) for x in g)
            if "mbps" in g[0]:
                vals = sorted(float(x["mbps"]) for x in g)
                res = f"**{med('mbps'):.0f} Mbit/s** ({' / '.join(f'{v:.0f}' for v in vals)})"
            else:
                res = f"p50 **{med('p50_ms'):.3f}** / p90 {med('p90_ms'):.3f} / p99 {med('p99_ms'):.3f} ms"
                lost = sum(int(x.get("lost", 0)) for x in g)
                if lost:
                    res += f", {lost} lost"
            print(f"| {test} | {e} | {res} | {med('local_cpu'):.0f} | {med('remote_cpu'):.0f} |")
EOPY
