#!/usr/bin/env bash
# Quick end-to-end smoke test: remote + 2 emulated relays + local, curl via SOCKS5.
set -u
B=${B:-$(realpath "$(dirname "$0")/../bin/mpquic")}
W=${W:-/tmp/mpquic-smoke}
mkdir -p "$W"; cd "$W"
PIDS=()
trap 'kill "${PIDS[@]}" 2>/dev/null' EXIT
[ -f big.bin ] || head -c 50000000 /dev/urandom > big.bin
python3 -m http.server 18080 >/dev/null 2>&1 & PIDS+=($!)
$B remote -s 127.0.0.1 -p 18388 -k secret --stats 5 > remote.log 2>&1 & PIDS+=($!)
$B relay --listen 127.0.0.1:19001 --target 127.0.0.1:18388 --link "${L1:-delay=20ms,rate=100mbit}" > relay1.log 2>&1 & PIDS+=($!)
$B relay --listen 127.0.0.1:19002 --target 127.0.0.1:18388 --link "${L2:-delay=60ms,rate=100mbit}" > relay2.log 2>&1 & PIDS+=($!)
sleep 0.5
$B local -s 127.0.0.1:19001 -s 127.0.0.1:19002 -l 11080 -k secret --stats 2 > local.log 2>&1 & PIDS+=($!)
sleep 1.5
echo "== bulk 50MB"
curl -s -o out.bin -w "%{time_total}s %{speed_download}B/s\n" --socks5-hostname 127.0.0.1:11080 http://127.0.0.1:18080/big.bin
cmp big.bin out.bin && echo "content OK"
echo "== small requests"
for i in 1 2 3 4 5; do curl -s -o /dev/null -w "%{time_total}\n" --socks5-hostname 127.0.0.1:11080 http://127.0.0.1:18080/; done
