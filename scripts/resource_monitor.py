#!/usr/bin/env python3
import argparse
import json
import os
import time
import urllib.request


def process_rss_bytes(needles):
    total = 0
    seen = set()
    for name in os.listdir("/proc"):
        if not name.isdigit():
            continue
        pid = int(name)
        try:
            with open(f"/proc/{pid}/cmdline", "rb") as f:
                cmd = f.read().replace(b"\x00", b" ").decode("utf-8", "replace")
        except OSError:
            continue
        if not any(n in cmd for n in needles):
            continue
        try:
            with open(f"/proc/{pid}/status") as f:
                for line in f:
                    if line.startswith("VmRSS:"):
                        total += int(line.split()[1]) * 1024
                        seen.add(pid)
                        break
        except OSError:
            continue
    return total, len(seen)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--addr", required=True)
    ap.add_argument("--duration", type=float, default=120)
    ap.add_argument("--out", required=True)
    ap.add_argument("--stop-file", default="")
    ap.add_argument("--needle", action="append", default=[])
    args = ap.parse_args()
    needles = args.needle or ["archive-server", "redis-server"]
    deadline = time.time() + args.duration
    max_active = 0
    max_rss = 0
    max_proc = 0
    samples = 0
    start = time.time()
    while time.time() < deadline:
        if args.stop_file and os.path.exists(args.stop_file):
            break
        try:
            with urllib.request.urlopen(f"{args.addr}/v1/status?queue=1", timeout=2) as resp:
                status = json.load(resp)
            max_active = max(max_active, int(status.get("repairs", {}).get("in_progress", 0)))
        except Exception:
            pass
        rss, procs = process_rss_bytes(needles)
        max_rss = max(max_rss, rss)
        max_proc = max(max_proc, procs)
        samples += 1
        time.sleep(0.05)
    result = {
        "duration_sec": round(time.time() - start, 3),
        "samples": samples,
        "max_repairs_in_progress": max_active,
        "max_archive_and_redis_rss_bytes": max_rss,
        "max_archive_and_redis_rss_mib": round(max_rss / (1024 * 1024), 3),
        "max_matched_processes": max_proc,
        "rss_scope": "all archive-server and redis-server processes in this WSL2 instance",
    }
    with open(args.out, "w") as f:
        json.dump(result, f, indent=2)
        f.write("\n")


if __name__ == "__main__":
    main()
