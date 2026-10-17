#!/usr/bin/env bash
# Reverse-mode smoke test: the local listens for QUIC on :18488 (its "public"
# address) and serves SOCKS5 on :11082; two relays forward to the local; the
# remote dials the local directly and via both relays.
set -u
B=${B:-$(realpath "$(dirname "$0")/../bin/mpquic")}
W=${W:-/tmp/mpquic-smoke}
mkdir -p "$W"; cd "$W"
PIDS=()
trap 'kill "${PIDS[@]}" 2>/dev/null' EXIT
[ -f big.bin ] || head -c 50000000 /dev/urandom > big.bin
python3 -m http.server 18081 >/dev/null 2>&1 & PIDS+=($!)
$B local --reverse -s 127.0.0.1 -p 18488 -l 11082 -k secret -u --stats 5 > rlocal.log 2>&1 & PIDS+=($!)
$B relay --listen 127.0.0.1:19101 --target 127.0.0.1:18488 --link "${L1:-delay=20ms,rate=100mbit}" > rrelay1.log 2>&1 & PIDS+=($!)
socat UDP4-LISTEN:19102,fork,reuseaddr UDP4:127.0.0.1:18488 & PIDS+=($!)
sleep 0.5
$B remote --reverse -s 127.0.0.1:18488 -s 127.0.0.1:19101 -s 127.0.0.1:19102 -k secret -u > rremote.log 2>&1 & PIDS+=($!)
sleep 1.5
echo "== bulk 50MB"
curl -s -o out.bin -w "%{time_total}s %{speed_download}B/s\n" --socks5-hostname 127.0.0.1:11082 http://127.0.0.1:18081/big.bin
cmp big.bin out.bin && echo "content OK"
echo "== small requests"
for i in 1 2 3; do curl -s -o /dev/null -w "%{time_total}\n" --socks5-hostname 127.0.0.1:11082 http://127.0.0.1:18081/; done
grep -h "path up" rlocal.log rremote.log | cut -c1-160
