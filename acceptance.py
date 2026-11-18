"""Acceptance suite: dedup/window alignment, skew split, tenant quota
isolation, fault recovery, and report reproducibility.

Usage: python acceptance.py [--regen]
"""

import argparse
import copy
import json
import os
import subprocess
import sys
import time

BASE = os.path.dirname(os.path.abspath(__file__))
PY = sys.executable
BASE_CONFIG = os.path.join(BASE, "config.json")
ACCEPT_DIR = os.path.join(BASE, "acceptance_out")

CHECKS = []


def check(name, ok, detail=""):
    CHECKS.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name}" + (f" - {detail}" if detail else ""))


def load_base_config():
    with open(BASE_CONFIG, "r", encoding="utf-8") as fh:
        return json.load(fh)


def write_scenario_config(name, mutate=None):
    cfg = load_base_config()
    cfg["data_dir"] = os.path.join(BASE, "data")
    cfg["output_dir"] = "out"
    cfg["state_dir"] = "state"
    if mutate:
        mutate(cfg)
    path = os.path.join(ACCEPT_DIR, name)
    os.makedirs(path, exist_ok=True)
    cfg_path = os.path.join(path, "config.json")
    with open(cfg_path, "w", encoding="utf-8") as fh:
        json.dump(cfg, fh, indent=2)
    return cfg_path


def run_engine(cfg_path, extra_args=()):
    t0 = time.monotonic()
    proc = subprocess.run(
        [PY, os.path.join(BASE, "run.py"), "run", "--config", cfg_path, *extra_args],
        capture_output=True, text=True, cwd=BASE)
    elapsed = time.monotonic() - t0
    report_path = os.path.join(ACCEPT_DIR, os.path.basename(os.path.dirname(cfg_path)),
                               "out", "report.json")
    report = None
    if os.path.exists(report_path):
        with open(report_path, "r", encoding="utf-8") as fh:
            report = json.load(fh)
    return proc, elapsed, report


def strip_durations(node):
    if isinstance(node, dict):
        return {k: strip_durations(v) for k, v in node.items() if k != "duration_sec"}
    if isinstance(node, list):
        return [strip_durations(v) for v in node]
    return node


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--regen", action="store_true")
    args = parser.parse_args()

    os.makedirs(ACCEPT_DIR, exist_ok=True)

    if args.regen or not os.path.exists(os.path.join(BASE, "data", "manifest.json")):
        proc = subprocess.run([PY, os.path.join(BASE, "run.py"), "generate",
                               "--config", BASE_CONFIG],
                              capture_output=True, text=True, cwd=BASE)
        check("generate dataset", proc.returncode == 0, proc.stdout.strip())
    else:
        check("generate dataset", True, "reused existing data/")

    # ---- scenario 1: normal two-tenant run -------------------------------
    cfg1 = write_scenario_config("s1_normal")
    proc, elapsed, report = run_engine(cfg1)
    ok = proc.returncode == 0 and report is not None
    check("s1: both tenants succeed", ok and all(
        t["status"] == "SUCCEEDED" for t in report["tenants"].values()))
    if report:
        tenants = report["tenants"]
        check("s1: end-to-end < 300s per tenant", elapsed < 300,
              f"{elapsed:.1f}s")
        for tid, t in tenants.items():
            check(f"s1[{tid}]: batch/stream consistency",
                  t["consistency"]["status"] == "OK")
            check(f"s1[{tid}]: dedup aligns effective records",
                  t["effective_records"]["batch"] == t["effective_records"]["stream"],
                  f"batch={t['effective_records']['batch']} "
                  f"stream={t['effective_records']['stream']}")
            check(f"s1[{tid}]: batch dedup dropped > 0",
                  t["dedup"]["batch_dropped"] > 0,
                  f"{t['dedup']['batch_dropped']}")
            check(f"s1[{tid}]: late records counted",
                  t["stream"]["late_records"] > 0,
                  f"{t['stream']['late_records']}")
            check(f"s1[{tid}]: skew keys split",
                  t["skew"]["split_keys"] > 0,
                  f"{t['skew']}")
            check(f"s1[{tid}]: heaviest unit <= 25% of rows",
                  t["heaviest_unit_row_share"] <= 0.25,
                  f"{t['heaviest_unit_row_share']:.4f}")
            peak = max(u["peak_mem_bytes"] for u in t["units"])
            quota = next(tn["memory_quota_bytes"] for tn in load_base_config()
                         ["tenants"] if tn["tenant_id"] == tid)
            check(f"s1[{tid}]: unit peak mem within quota",
                  peak <= quota, f"peak={peak} quota={quota}")
        check("s1: results written for both tenants", all(
            os.path.exists(os.path.join(ACCEPT_DIR, "s1_normal", "out", tid,
                                        "results.json"))
            for tid in tenants))

    # ---- scenario 1b: split results equal unsplit reference --------------
    sys.path.insert(0, BASE)
    from telemetry_engine import core, ingest, manifest as manifest_mod
    manifest = manifest_mod.load_manifest(os.path.join(BASE, "data"))
    window_ms = 3600_000
    for tid, fleet in (("fleet_a", "A"), ("fleet_b", "B")):
        recs, _ = ingest.read_batch(os.path.join(BASE, "data"), manifest, fleet)
        kept, _ = core.dedup(recs)
        reference = {}
        for rec in kept:
            ws = core.window_start(rec[1], window_ms)
            slot = reference.setdefault((rec[0], ws), [0, 0, 0, 0])
            slot[0] += 1
            slot[1] += rec[2]
            slot[2] += rec[3]
            slot[3] += rec[4]
        res_path = os.path.join(ACCEPT_DIR, "s1_normal", "out", tid,
                                "results.json")
        with open(res_path, "r", encoding="utf-8") as fh:
            results = json.load(fh)["results"]
        mismatch = 0
        for row in results:
            key = (row["vehicle_id"], row["window_start"])
            ref = reference.get(key)
            if ref is None or ref[0] != row["record_count"]:
                mismatch += 1
            elif (round(ref[1] / ref[0] / 10, 3) != row["avg_speed"]
                    or ref[2] != row["total_fuel_ml"]
                    or ref[3] != row["total_distance_m"]):
                mismatch += 1
        check(f"s1b[{tid}]: skew-split results match unsplit reference",
              mismatch == 0 and len(results) == len(reference),
              f"windows={len(results)} mismatches={mismatch}")

    # ---- scenario 2: report reproducibility ------------------------------
    cfg2 = write_scenario_config("s2_repro")
    proc2, _, report2 = run_engine(cfg2)
    same = (report is not None and report2 is not None
            and strip_durations(report) == strip_durations(report2))
    check("s2: reports reproducible (except durations)", same)

    # ---- scenario 3: worker crash + incremental recovery -----------------
    def mutate_crash(cfg):
        cfg["fault_injection"] = {
            "crash": [{"tenant": "fleet_a", "task_index": 3, "times": 1},
                      {"tenant": "fleet_a", "task_index": 120, "times": 1}],
            "always_fail": [],
        }
    cfg3 = write_scenario_config("s3_crash", mutate_crash)
    proc3, _, report3 = run_engine(cfg3)
    t3 = report3["tenants"]["fleet_a"] if report3 else None
    check("s3: tenant recovers from worker crashes",
          t3 is not None and t3["status"] == "SUCCEEDED"
          and t3["faults"]["task_retries"] >= 1,
          f"retries={t3['faults']['task_retries'] if t3 else 'n/a'}")
    if t3 and report:
        t1 = report["tenants"]["fleet_a"]
        check("s3: results identical to no-fault run",
              t3["consistency"] == t1["consistency"]
              and t3["effective_records"] == t1["effective_records"])
        check("s3: committed tasks not recomputed",
              t3["tasks"]["executed"] <= t3["tasks"]["planned"])

    # ---- scenario 4: retry exhaustion fails only that tenant -------------
    def mutate_always(cfg):
        cfg["fault_injection"] = {
            "crash": [],
            "always_fail": [{"tenant": "fleet_b", "task_index": 5}],
        }
    cfg4 = write_scenario_config("s4_exhaust", mutate_always)
    proc4, _, report4 = run_engine(cfg4)
    if report4:
        ta, tb = report4["tenants"]["fleet_a"], report4["tenants"]["fleet_b"]
        check("s4: failing tenant gets E_TASK_RETRY_EXHAUSTED",
              tb["status"] == "FAILED"
              and tb["error_code"] == "E_TASK_RETRY_EXHAUSTED",
              f"error={tb['error_code']}")
        check("s4: completed vehicle list emitted on failure",
              bool(tb.get("completed_vehicles")),
              f"{len(tb.get('completed_vehicles', []))} vehicles")
        check("s4: other tenant unaffected",
              ta["status"] == "SUCCEEDED"
              and ta["consistency"]["status"] == "OK")
        check("s4: no partial results for failed tenant",
              not os.path.exists(os.path.join(
                  ACCEPT_DIR, "s4_exhaust", "out", "fleet_b", "results.json")))

    # ---- scenario 5: resume skips committed tasks -------------------------
    cfg5 = write_scenario_config("s5_resume", mutate_always)
    proc5, _, report5 = run_engine(cfg5)  # fails fleet_b like s4
    cfg5b = write_scenario_config("s5_resume")  # same dirs, no faults
    proc5b, _, report5b = run_engine(cfg5b, extra_args=("--resume",))
    if report5b:
        tb = report5b["tenants"]["fleet_b"]
        check("s5: resume completes previously failed tenant",
              tb["status"] == "SUCCEEDED"
              and tb["consistency"]["status"] == "OK")
        check("s5: committed tasks skipped on resume",
              tb["tasks"]["resumed_skipped"] > 0
              and tb["tasks"]["executed"] + tb["tasks"]["resumed_skipped"]
              == tb["tasks"]["planned"],
              f"resumed={tb['tasks']['resumed_skipped']} "
              f"executed={tb['tasks']['executed']}")

    # ---- scenario 6: memory quota isolation -------------------------------
    def mutate_quota(cfg):
        for tenant in cfg["tenants"]:
            if tenant["tenant_id"] == "fleet_b":
                tenant["memory_quota_bytes"] = 100_000
    cfg6 = write_scenario_config("s6_quota", mutate_quota)
    proc6, _, report6 = run_engine(cfg6)
    if report6:
        ta, tb = report6["tenants"]["fleet_a"], report6["tenants"]["fleet_b"]
        check("s6: over-quota tenant aborted with E_MEMORY_QUOTA_EXCEEDED",
              tb["status"] == "FAILED"
              and tb["error_code"] == "E_MEMORY_QUOTA_EXCEEDED",
              f"error={tb['error_code']}")
        check("s6: in-quota tenant completes normally",
              ta["status"] == "SUCCEEDED"
              and ta["consistency"]["status"] == "OK")

    failed = [c for c in CHECKS if not c[1]]
    print(f"\n{len(CHECKS) - len(failed)}/{len(CHECKS)} checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
