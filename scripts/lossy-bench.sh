#!/usr/bin/env bash
# Lossy multipath scenario (production-like): 20% / 10% / 5% / 1% loss paths.
# Usage: scripts/lossy-bench.sh [mpbench binary] [extra mpbench args]
B=${1:-./bin/mpbench}; shift
LOG=$(mktemp)
$B --single=false --duration 15s --tests latency,ttfb,bulk,upload,fct,mixed \
  --path delay=20ms,rate=50mbit,loss=20% \
  --path delay=30ms,rate=50mbit,loss=10% \
  --path delay=40ms,rate=50mbit,loss=5%,jitter=5ms \
  --path delay=60ms,rate=50mbit,loss=1% \
  -v "$@" 2>"$LOG" | grep -vE "^path [0-9]|timeline"
echo "suspect flaps: $(grep -c 'path suspect' "$LOG")"
