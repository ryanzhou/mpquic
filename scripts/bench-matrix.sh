#!/usr/bin/env bash
# Runs mpbench over a set of emulated network scenarios and writes raw
# results to bench-results/<scenario>.txt. Usage: scripts/bench-matrix.sh [scenario...]
set -u
cd "$(dirname "$0")/.."
go build -o bin/mpbench ./cmd/mpbench || exit 1
mkdir -p bench-results
declare -A S
S[equal]="--path delay=10ms,rate=40mbit --path delay=30ms,rate=40mbit"
S[hetero]="--path delay=5ms,rate=10mbit --path delay=50ms,rate=100mbit"
S[lossy-direct]="--path delay=10ms,rate=50mbit,loss=2% --path delay=25ms,rate=50mbit,jitter=3ms"
S[three-paths]="--path delay=8ms,rate=30mbit,loss=1%,outage=6s/1500ms --path delay=20ms,rate=40mbit,jitter=5ms --path delay=60ms,rate=60mbit,reorder=2%"
S[asym-rtt]="--path delay=5ms,rate=100mbit --path delay=150ms,rate=100mbit"
S[flapping]="--path delay=10ms,rate=50mbit,rate2=10mbit,flap=3s --path delay=30ms,rate=20mbit,rate2=60mbit,flap=4s"
names=("$@")
[ ${#names[@]} -eq 0 ] && names=(equal hetero lossy-direct three-paths asym-rtt flapping)
for n in "${names[@]}"; do
  echo "### $n: ${S[$n]}"
  ./bin/mpbench ${S[$n]} --duration 8s --tests latency,ttfb,bulk,upload,fct,mixed,failover 2>/dev/null | tee "bench-results/$n.txt"
done
