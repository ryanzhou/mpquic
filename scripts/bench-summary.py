#!/usr/bin/env python3
"""Summarise bench-results/*.txt (from bench-matrix.sh) as a markdown table."""
import re, sys, glob, os

def parse(path):
    runs, cur = {}, None
    for line in open(path):
        m = re.match(r"===== (\S+) ", line)
        if m:
            cur = runs.setdefault(m.group(1), {})
            continue
        if cur is None:
            continue
        def grab(key, rx):
            mm = re.search(rx, line)
            if mm:
                cur[key] = mm.group(1)
        if line.startswith("latency  echo 100B"):
            grab("echo_p50", r"p50=(\S+)"); grab("echo_p99", r"p99=(\S+)")
        elif line.startswith("ttfb"):
            grab("ttfb", r"p50=(\S+)")
        elif line.startswith("bulk     download"):
            grab("down", r"steady=([\d.]+)")
        elif line.startswith("bulk     upload"):
            grab("up", r"steady=([\d.]+)")
        elif re.match(r"fct\s+2048KB", line):
            grab("fct2m", r"p50=(\S+)")
        elif re.match(r"fct\s+16384KB", line):
            grab("fct16m", r"p50=(\S+)")
        elif line.startswith("mixed"):
            grab("mixed", r"p50=(\S+)")
        elif line.startswith("failover"):
            grab("stall", r"stall=(\S+),"); grab("fecho", r"echo RTT=(\S+)")
    return runs

def ms(v):
    if v is None: return None
    m = re.match(r"([\d.]+)(ms|s|µs)", v)
    if not m: return None
    x = float(m.group(1))
    return x*1000 if m.group(2) == "s" else (x/1000 if m.group(2) == "µs" else x)

cols = [("echo_p50","echo p50"),("echo_p99","echo p99"),("ttfb","new conn"),("down","download Mbit/s"),
        ("up","upload Mbit/s"),("fct2m","2 MB FCT"),("fct16m","16 MB FCT"),("mixed","echo under load"),("stall","failover stall")]
d = sys.argv[1] if len(sys.argv) > 1 else "bench-results"
order = ["equal","hetero","lossy-direct","three-paths","asym-rtt","flapping"]
files = sorted(glob.glob(os.path.join(d, "*.txt")), key=lambda f: order.index(os.path.basename(f)[:-4]) if os.path.basename(f)[:-4] in order else 99)
for f in files:
    name = os.path.basename(f)[:-4]
    runs = parse(f)
    print(f"\n#### {name}\n")
    print("| run | " + " | ".join(c[1] for c in cols) + " |")
    print("|---" * (len(cols)+1) + "|")
    for run, v in runs.items():
        print(f"| {run} | " + " | ".join(v.get(k, "–") for k, _ in cols) + " |")
