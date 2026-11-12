#!/usr/bin/env bash
# Paths behind a common bottleneck: relays that are not the limit (or less
# so) behind the sending endpoint's uplink (--shared) or in front of the
# receiving endpoint's downlink (--shared-rx), and controls without a common
# bottleneck. Each scenario prints one line:
#   steady goodput
#   relay: what the relays carried beyond the goodput, and the packets dropped
#          at the uplink / at the relays / at the downlink
#   cp:    bytes the session sent again (rescues + duplicates), of the goodput
#   est:   what the sender's paths pace at, added up (p50/p90/max, Mbit/s)
#   echo:  p50/p90 of a 100-byte round trip during the transfer (idle)
#   p10:   goodput per 100 ms (10th percentile, minimum) and the longest stall
# Usage: [DUR=12s] scripts/shared-bench.sh [mpbench binary] [scenario...]
set -u
B=${1:-./bin/mpbench}; shift || true
P3="--path delay=10ms --path delay=25ms --path delay=40ms"
P3J="--path delay=10ms,jitter=2ms --path delay=25ms,jitter=4ms --path delay=40ms,jitter=3ms,loss=0.1%"
P4R="--path delay=8ms,rate=300mbit --path delay=20ms,rate=200mbit --path delay=35ms,rate=200mbit --path delay=60ms,rate=100mbit"
P3R="--path delay=10ms,rate=100mbit --path delay=20ms,rate=100mbit --path delay=30ms,rate=100mbit"
declare -A S
# the local's uplink (upload)
S[up20-deep]="$P3 --shared rate=20mbit,queue=300ms|none --tests upload"
S[up20-shallow]="$P3 --shared rate=20mbit,queue=15ms|none --tests upload"
S[up50-jitter]="$P3J --shared rate=50mbit,queue=100ms|none --tests upload"
# the local's downlink, behind the relays (download)
S[down100-q50]="$P3 --shared-rx none|rate=100mbit,queue=50ms --tests bulk"
S[down100-q10]="$P3 --shared-rx none|rate=100mbit,queue=10ms --tests bulk"
S[down100-q300]="$P3 --shared-rx none|rate=100mbit,queue=300ms --tests bulk"
S[down50-policer]="$P3 --shared-rx none|police=50mbit --tests bulk"
S[down100-jitter]="$P3J --shared-rx none|rate=100mbit,queue=50ms --tests bulk"
S[down100-4relays]="$P4R --shared-rx none|rate=100mbit,queue=50ms --tests bulk"
S[down100-4streams]="$P3 --shared-rx none|rate=100mbit,queue=50ms --tests bulk --streams 4"
S[down-flap100-40]="$P3 --shared-rx none|rate=100mbit,rate2=40mbit,flap=4s,queue=100ms --tests bulk"
# a 100/20 Mbit/s line: a download while an upload runs
S[home-bidir]="$P3 --shared rate=20mbit,queue=200ms|none --shared-rx none|rate=100mbit,queue=50ms --tests bidir"
# the same line with its rates configured (--up/--down at 95%)
S[home-bidir-limits]="$P3 --shared rate=20mbit,queue=200ms|none --shared-rx none|rate=100mbit,queue=50ms --tests bidir --up 19mbit --down 95mbit"
S[down50-policer-limit]="$P3 --shared-rx none|police=50mbit --tests bulk --down 47mbit"
# the remote's uplink (download), relays of 100 Mbit/s
S[srvup150-q20]="$P3R --shared none|rate=150mbit,queue=20ms --tests bulk"
S[srvup150-q50]="$P3R --shared none|rate=150mbit,queue=50ms --tests bulk"
S[srvup-flap150-60]="$P3R --shared none|rate=150mbit,rate2=60mbit,flap=4s,queue=100ms --tests bulk"
S[srvup100-lossy]="--path delay=10ms,rate=100mbit,loss=1% --path delay=20ms,rate=100mbit --shared none|rate=100mbit,queue=50ms --tests bulk"
# controls: no common bottleneck, or one that only limits some paths
S[srvup400-not-limiting]="$P3R --shared none|rate=400mbit,queue=50ms --tests bulk"
S[independent-3x50]="--path delay=10ms,rate=50mbit --path delay=20ms,rate=50mbit --path delay=30ms,rate=50mbit --tests bulk"
S[independent-identical]="--path delay=20ms,rate=50mbit --path delay=20ms,rate=50mbit --tests bulk"
S[independent-lossy]="--path delay=10ms,rate=50mbit --path delay=25ms,rate=30mbit,loss=2% --path delay=40ms,rate=20mbit --tests bulk"
S[one-slow-relay]="--path delay=10ms,rate=20mbit --path delay=25ms --path delay=40ms --shared-rx none|rate=100mbit,queue=50ms --tests bulk"
order=(up20-deep up20-shallow up50-jitter down100-q50 down100-q10 down100-q300 down50-policer down100-jitter
  down100-4relays down100-4streams down-flap100-40 home-bidir home-bidir-limits down50-policer-limit
  srvup150-q20 srvup150-q50 srvup-flap150-60 srvup100-lossy
  srvup400-not-limiting independent-3x50 independent-identical independent-lossy one-slow-relay)
names=("$@")
[ ${#names[@]} -eq 0 ] && names=("${order[@]}")
for n in "${names[@]}"; do
  [ -n "${S[$n]:-}" ] || { echo "unknown scenario $n (have: ${order[*]})" >&2; exit 1; }
  read -r -a args <<<"${S[$n]}"
  for mode in session; do
    out=$($B "${args[@]}" --duration "${DUR:-12s}" --single=false 2>/dev/null)
    steady=$(sed -nE 's/^bulk .*steady=([0-9.]+) Mbit.*/\1/p' <<<"$out" | tail -1)
    relay=$(sed -nE 's/^relays .*goodput \(\+([0-9.-]+)%\); dropped: uplink ([0-9.]+)% relays ([0-9.]+)% downlink ([0-9.]+)%/relay +\1% drop \2\/\3\/\4%/p' <<<"$out" | tail -1)
    wire=$(sed -nE 's/^wire .*reinject ([0-9.]+)% dup ([0-9.]+)%\).*/cp \1+\2%/p' <<<"$out" | tail -1)
    est=$(sed -nE 's/^estimate .*p50=([0-9.]+) p90=([0-9.]+) max=([0-9.]+) .*/est \1\/\2\/\3/p' <<<"$out" | tail -1)
    echo_=$(sed -nE 's/^loaded .*p50=([0-9.]+)ms p90=([0-9.]+)ms.*idle_p50=([0-9.]+)ms/echo \1\/\2 (idle \3)/p' <<<"$out" | tail -1)
    jit=$(sed -nE 's/^jitter .*p10=([0-9.]+) .*min=([0-9.]+) .*stall=(.*)/p10 \1 min \2 stall \3/p' <<<"$out" | tail -1)
    printf "%-22s %-9s %6s Mbit/s | %-34s | %-13s | %-22s | %-26s | %s\n" "$n" "$mode" "$steady" "$relay" "$wire" "$est" "$echo_" "$jit"
  done
done
