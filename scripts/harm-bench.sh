#!/usr/bin/env bash
# "Do no harm" benchmark: two good paths alone vs. the same two plus one bad
# path. Adding a bad path must not make any metric worse than excluding it.
# Usage: scripts/harm-bench.sh [mpbench binary] [scenario...]
set -u
B=${1:-./bin/mpbench}; shift || true
GOOD="--path delay=15ms,rate=40mbit --path delay=35ms,rate=40mbit,jitter=2ms"
declare -A BAD
BAD[loss20]="delay=25ms,rate=40mbit,loss=20%"
BAD[loss10-rtt300]="delay=150ms,rate=40mbit,loss=10%"
BAD[jitter80]="delay=20ms,rate=40mbit,jitter=80ms"
BAD[thin1mbit]="delay=10ms,rate=1mbit"
BAD[flapping]="delay=10ms,rate=40mbit,outage=2s/1s"
BAD[collapse]="delay=10ms,rate=40mbit,rate2=2mbit,flap=3s,loss=3%"
names=("$@")
[ ${#names[@]} -eq 0 ] && names=(loss20 loss10-rtt300 jitter80 thin1mbit flapping collapse)
mkdir -p bench-results/harm
for n in "${names[@]}"; do
  echo "### $n: bad path = ${BAD[$n]}"
  $B $GOOD --path "${BAD[$n]}" --subset 0,1 --single=false --duration 12s \
     --tests latency,ttfb,bulk,fct,mixed 2>/dev/null | grep -vE "^path [0-9]|timeline|fct +(16|256)KB" \
     | tee "bench-results/harm/$n.txt"
done
