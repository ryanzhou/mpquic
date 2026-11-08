#!/usr/bin/env bash
# Paths that share a bottleneck: three 100 Mbit/s relay paths behind one
# slower uplink of the sending endpoint (and controls without a shared
# bottleneck). Each scenario runs with and without shared-bottleneck
# coupling (--no-coupling) and prints one line per run: steady goodput, what
# was sent beyond the goodput (session-level copies: reinjections/rescues and
# duplicates) and the drops at the shared bottleneck, the echo latency under
# load, and the per-100 ms goodput (p10/min) and longest stall.
# Usage: [DUR=10s] scripts/shared-bench.sh [mpbench binary] [scenario...]
set -u
B=${1:-./bin/mpbench}; shift || true
P3="--path delay=10ms,rate=100mbit --path delay=20ms,rate=100mbit --path delay=30ms,rate=100mbit"
declare -A S
S[uplink150-q20]="$P3 --shared none|rate=150mbit,queue=20ms"
S[uplink150-q50]="$P3 --shared none|rate=150mbit,queue=50ms"
S[uplink150-q200]="$P3 --shared none|rate=150mbit,queue=200ms"
S[uplink150-q50-upload]="$P3 --shared rate=150mbit,queue=50ms|none --upload"
S[uplink150-q50-4streams]="$P3 --shared none|rate=150mbit,queue=50ms --streams 4"
S[uplink250-q50]="$P3 --shared none|rate=250mbit,queue=50ms"
S[uplink150-rtt150]="--path delay=40ms,rate=100mbit --path delay=60ms,rate=100mbit --path delay=75ms,rate=100mbit --shared none|rate=150mbit,queue=100ms"
S[uplink-flap150-60]="$P3 --shared none|rate=150mbit,rate2=60mbit,flap=4s,queue=100ms"
S[uplink100-lossy]="--path delay=10ms,rate=100mbit,loss=1% --path delay=20ms,rate=100mbit --shared none|rate=100mbit,queue=50ms"
S[uplink400-not-limiting]="$P3 --shared none|rate=400mbit,queue=50ms"
S[independent-3x50]="--path delay=10ms,rate=50mbit --path delay=20ms,rate=50mbit --path delay=30ms,rate=50mbit"
S[independent-identical]="--path delay=20ms,rate=50mbit --path delay=20ms,rate=50mbit"
S[independent-lossy]="--path delay=10ms,rate=50mbit --path delay=25ms,rate=30mbit,loss=2% --path delay=40ms,rate=20mbit"
names=("$@")
[ ${#names[@]} -eq 0 ] && names=(uplink150-q20 uplink150-q50 uplink150-q200 uplink150-q50-upload uplink150-q50-4streams
  uplink250-q50 uplink150-rtt150 uplink-flap150-60 uplink100-lossy uplink400-not-limiting
  independent-3x50 independent-identical independent-lossy)
for n in "${names[@]}"; do
  read -r -a args <<<"${S[$n]}"
  test=bulk
  for i in "${!args[@]}"; do
    [ "${args[$i]}" = --upload ] && test=upload && unset 'args[$i]'
  done
  for mode in coupled uncoupled; do
    extra=()
    [ $mode = uncoupled ] && extra=(--no-coupling)
    out=$($B "${args[@]}" "${extra[@]}" --tests $test --duration ${DUR:-10s} --single=false 2>/dev/null)
    steady=$(sed -nE 's/^bulk .*steady=([0-9.]+) Mbit.*/\1/p' <<<"$out")
    wire=$(sed -nE 's/^wire .*\(\+([0-9.-]+)% over goodput: reinject ([0-9.]+)% dup ([0-9.]+)%\).*/copies \2%+\3%/p' <<<"$out")
    drops=$(sed -nE 's/^wire .*shared-link drops=[0-9]+ \(([0-9.]+)%\).*/drops \1%/p' <<<"$out")
    echo_=$(sed -nE 's/^loaded .*p50=([0-9.]+)ms p90=([0-9.]+)ms.*idle_p50=([0-9.]+)ms/echo \1\/\2ms (idle \3)/p' <<<"$out")
    jit=$(sed -nE 's/^jitter .*p10=([0-9.]+) .*min=([0-9.]+) .*stall=(.*)/100ms p10 \1 min \2, stall \3/p' <<<"$out")
    printf "%-24s %-9s %6s Mbit/s  %-16s %-12s %-28s %s\n" "$n" $mode "$steady" "$wire" "$drops" "$echo_" "$jit"
  done
done
