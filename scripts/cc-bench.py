#!/usr/bin/env python3
"""Congestion-control benchmark: one bulk stream over a single emulated path.

Runs mpbench for each (link, congestion controller) combination and prints a
markdown table with steady-state goodput, efficiency (goodput / link rate) and
ramp-up time (first 0.5 s bucket reaching 90% of the steady rate).
  scripts/cc-bench.py [--rtt 150] [--duration 15] [--quick]
"""
import argparse, re, subprocess, sys, os

ap = argparse.ArgumentParser()
ap.add_argument("--rtt", type=int, default=150, help="round-trip time in ms")
ap.add_argument("--duration", type=int, default=15)
ap.add_argument("--bin", default=os.path.join(os.path.dirname(__file__), "..", "bin", "mpbench"))
ap.add_argument("--suite", default="all", help="bandwidth,window,loss,burst,buffer,jitter or all")
args = ap.parse_args()
d = args.rtt // 2

def run(link, cc="bbr", window=None, direction="S"):
    cmd = [args.bin, "--path", link, "--tests", "bulk" if direction == "S" else "upload", "--single=false",
           "--duration", f"{args.duration}s", "--cc", cc, "-v"]
    if window:
        cmd += ["--window", str(window)]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=args.duration + 60).stdout
    m = re.search(r"steady=([\d.]+)", out)
    tl = re.search(r"timeline: ([\d ]+)", out)
    steady = float(m.group(1)) if m else 0.0
    samples = [float(x) for x in tl.group(1).split()] if tl else []
    ramp = next((i * 0.5 + 0.5 for i, x in enumerate(samples) if x >= 0.9 * steady), None) if steady > 0 else None
    return steady, ramp

def rate_of(link):
    m = re.search(r"rate=([\d.]+)mbit", link)
    return float(m.group(1)) if m else None

def table(title, rows, ccs):
    print(f"\n### {title}\n")
    print("| link | " + " | ".join(f"{c} Mbit/s (eff, ramp)" for c in ccs) + " |")
    print("|---" * (len(ccs) + 1) + "|")
    for label, link, kw in rows:
        cells = []
        for c in ccs:
            st, ramp = run(link, c, **kw)
            r = rate_of(link)
            eff = f"{st / r * 100:.0f}%" if r else "–"
            cells.append(f"{st:.1f} ({eff}, {ramp:.1f}s)" if ramp is not None else f"{st:.1f} ({eff}, –)")
        print(f"| {label} | " + " | ".join(cells) + " |", flush=True)

suites = args.suite.split(",") if args.suite != "all" else ["bandwidth", "window", "loss", "burst", "buffer", "jitter"]
B = f"delay={d}ms"
print(f"Single stream, single path, RTT {args.rtt} ms, {args.duration}s per run. "
      f"Link rates include UDP/IP overhead (goodput ceiling ~96%).")
if "bandwidth" in suites:
    table("Bandwidth (no loss, 100 ms drop-tail buffer, default auto-tuned window up to 64 MB)",
          [(f"{r} Mbit/s", f"{B},rate={r}mbit", {}) for r in (10, 50, 100, 300, 1000)] +
          [("unlimited", B, {})], ["bbr", "cubic"])
if "window" in suites:
    table("Maximum stream window (--window) vs. reachable rate (unlimited link, no loss)",
          [(f"window {w >> 20} MB", B, {"window": w}) for w in (4 << 20, 8 << 20, 16 << 20, 32 << 20, 64 << 20)],
          ["bbr"])
if "loss" in suites:
    table("Random loss (100 Mbit/s)",
          [(f"{l}% loss", f"{B},rate=100mbit,loss={l}%", {}) for l in (0, 0.1, 0.5, 1, 2, 5, 10)],
          ["bbr", "bbr-aggressive", "bbr-conservative", "cubic"])
if "burst" in suites:
    table("Bursty loss (Gilbert-Elliott, mean burst 5 packets, 100 Mbit/s)",
          [(f"{l}% loss", f"{B},rate=100mbit,loss={l}%,burst=5", {}) for l in (0.5, 2)],
          ["bbr", "cubic"])
if "buffer" in suites:
    table("Bottleneck buffer depth (100 Mbit/s, no loss)",
          [(f"queue {q} ms", f"{B},rate=100mbit,queue={q}ms", {}) for q in (5, 20, 100, 300)],
          ["bbr", "bbr-conservative", "cubic"])
if "jitter" in suites:
    table("Jitter / reordering (100 Mbit/s)",
          [("jitter 10 ms", f"{B},rate=100mbit,jitter=10ms", {}),
           ("reorder 2%", f"{B},rate=100mbit,reorder=2%,reorder_delay=20ms", {}),
           ("policer 100 Mbit/s", f"{B},police=100mbit", {})],
          ["bbr", "cubic"])
