#!/usr/bin/env python3
"""Compare 'multipath' (good paths + bad path) against the 'subset' baseline
(good paths only) for every bench-results/harm/*.txt; flag regressions."""
import glob, os, re, sys

def ms(v):
    m = re.match(r"([\d.]+)(ms|s|µs)$", v)
    if not m:
        return None
    x = float(m.group(1))
    return {"s": x * 1000, "ms": x, "µs": x / 1000}[m.group(2)]

def parse(path):
    runs, cur = {}, None
    for line in open(path):
        m = re.match(r"===== (\S+) ", line)
        if m:
            cur = runs.setdefault("full" if m.group(1) == "multipath" else "base", {})
            continue
        if cur is None:
            continue
        g = lambda rx: (re.search(rx, line) or [None, None])[1]
        if line.startswith("latency  echo 100B"):
            cur["echo p50"], cur["echo p99"] = ms(g(r"p50=(\S+)")), ms(g(r"p99=(\S+)"))
        elif line.startswith("latency  echo 32KB"):
            cur["echo32K p50"], cur["echo32K p90"] = ms(g(r"p50=(\S+)")), ms(g(r"p90=(\S+)"))
        elif line.startswith("ttfb"):
            cur["new conn p50"] = ms(g(r"p50=(\S+)"))
        elif line.startswith("bulk     download"):
            cur["bulk Mbit/s"] = float(g(r"steady=([\d.]+)"))
        elif line.startswith("jitter   download"):
            cur["bulk p10 Mbit/s"] = float(g(r"p10=([\d.]+)"))
            cur["stall"] = ms(g(r"stall=(\S+)"))
        elif re.match(r"fct\s+2048KB", line):
            cur["2MB fct"] = ms(g(r"p50=(\S+)"))
        elif re.match(r"fct\s+16384KB", line):
            cur["16MB fct"] = ms(g(r"p50=(\S+)"))
        elif line.startswith("mixed"):
            cur["load echo p50"], cur["load echo p99"] = ms(g(r"p50=(\S+)")), ms(g(r"p99=(\S+)"))
    return runs

# metric -> (higher_is_better, relative tolerance, absolute tolerance)
TOL = {"echo p50": (False, .10, 3), "echo p99": (False, .25, 10), "new conn p50": (False, .10, 3),
       "echo32K p50": (False, .10, 3), "echo32K p90": (False, .25, 10),
       "bulk Mbit/s": (True, .05, 0), "bulk p10 Mbit/s": (True, .20, 0), "stall": (False, .25, 50),
       "2MB fct": (False, .10, 10), "16MB fct": (False, .10, 50),
       "load echo p50": (False, .15, 5), "load echo p99": (False, .30, 20)}

worse_total = 0
for f in sorted(glob.glob(os.path.join(sys.argv[1] if len(sys.argv) > 1 else "bench-results/harm", "*.txt"))):
    r = parse(f)
    if "full" not in r or "base" not in r:
        continue
    name = os.path.basename(f)[:-4]
    cells, worse = [], []
    for k, (hib, rel, ab) in TOL.items():
        a, b = r["full"].get(k), r["base"].get(k)
        if a is None or b is None:
            continue
        bad = (a < b * (1 - rel) - ab) if hib else (a > b * (1 + rel) + ab)
        cells.append(f"{k}: {a:.1f} vs {b:.1f}{' ✗' if bad else ''}")
        if bad:
            worse.append(k)
    worse_total += len(worse)
    print(f"{name:15s} {'OK ' if not worse else 'WORSE: ' + ', '.join(worse)}")
    print("    " + " | ".join(cells))
print(f"\nregressions: {worse_total}")
