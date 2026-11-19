"""One-command local verification of every business objective.

Runs on the reduced sample by default; pass --full for the 120k/128-batch
dataset and --keep to leave generated data and state on disk.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

from window_engine.config import JobConfig
from window_engine.datagen import GenSpec, generate
from window_engine.report import business_fingerprint
from window_engine.util import read_json


def run(cmd: list[str], *, expect: int = 0) -> subprocess.CompletedProcess:
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != expect:
        raise SystemExit(
            f"command failed ({proc.returncode}): {' '.join(cmd)}\n{proc.stderr}"
        )
    return proc


def write_config(path: str, root: str, **overrides) -> str:
    cfg = {
        "data_dir": os.path.join(root, "data"),
        "state_dir": os.path.join(root, "state", overrides.pop("state_tag", "job")),
        "result_dir": os.path.join(root, "results"),
        "spill_dir": os.path.join(root, "spill", overrides.pop("spill_tag", "job")),
        "allowed_lateness_ms": 120_000,
        "partitions": 4,
        "workers": 2,
        "state_memory_bytes": 2 * 1024 * 1024,
        "mode": "mixed",
    }
    cfg.update(overrides)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(cfg, fh)
    return path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--full", action="store_true")
    parser.add_argument("--keep", action="store_true")
    args = parser.parse_args()

    root = tempfile.mkdtemp(prefix="we-verify-")
    python = sys.executable
    try:
        spec = (
            GenSpec()
            if args.full
            else GenSpec(
                history_rows=6000,
                incremental_rows=2000,
                batches=16,
                history_hours=4,
            )
        )
        print(f"[1/7] generating data into {root}/data ...")
        manifest = generate(os.path.join(root, "data"), spec)
        print(
            f"      history={manifest['history_row_count']} "
            f"stream={manifest['stream_row_count']} "
            f"duplicates={manifest['duplicate_rows']}"
        )

        batch_cfg = write_config(
            os.path.join(root, "batch.json"), root, mode="batch"
        )
        mixed_cfg = write_config(os.path.join(root, "mixed.json"), root)

        print("[2/7] pure batch vs batch/stream mixed ...")
        run([python, "-m", "window_engine.cli", "run", "--config", batch_cfg,
             "--report-name", "batch"])
        run([python, "-m", "window_engine.cli", "run", "--config", mixed_cfg,
             "--report-name", "mixed"])
        batch = read_json(os.path.join(root, "results", "batch.json"))
        mixed = read_json(os.path.join(root, "results", "mixed.json"))
        assert business_fingerprint(batch) == business_fingerprint(mixed)
        print(f"      OK: {mixed['window_count']} windows, "
              f"dedup={mixed['dedup_dropped']}, late={mixed['late_events']}")

        print("[3/7] kill after a batch + resume ...")
        fault_path = os.path.join(root, "fault-kill.json")
        with open(fault_path, "w", encoding="utf-8") as fh:
            json.dump({"rules": [{"fault": "kill_after_commit", "batch": 5}]}, fh)
        kill_cfg = write_config(
            os.path.join(root, "kill.json"), root,
            state_tag="kill", spill_tag="kill", fault_config=fault_path,
        )
        run([python, "-m", "window_engine.cli", "run", "--config", kill_cfg,
             "--report-name", "killed"])
        run([python, "-m", "window_engine.cli", "run", "--config", kill_cfg,
             "--report-name", "resumed"])
        resumed = read_json(os.path.join(root, "results", "resumed.json"))
        assert resumed["windows"] == mixed["windows"]
        assert resumed["recovery"]["recoveries"] == 1
        run([python, "-m", "window_engine.cli", "run", "--config", kill_cfg,
             "--report-name", "resumed2"])
        resumed2 = read_json(os.path.join(root, "results", "resumed2.json"))
        assert resumed2["windows"] == resumed["windows"]
        print("      OK: no duplicate output, repeat resume idempotent")

        print("[4/7] fault error codes E_STATE / E_INPUT ...")
        for fault, exit_code in (
            ("state_locked", 71),
            ("state_corrupt", 71),
            ("batch_read_failure", 72),
        ):
            fpath = os.path.join(root, f"fault-{fault}.json")
            with open(fpath, "w", encoding="utf-8") as fh:
                json.dump({"rules": [{"fault": fault, "batch": 4}]}, fh)
            cpath = write_config(
                os.path.join(root, f"{fault}.json"), root,
                state_tag=fault, spill_tag=fault, fault_config=fpath,
            )
            proc = subprocess.run(
                [python, "-m", "window_engine.cli", "run", "--config", cpath,
                 "--report-name", fault],
                capture_output=True, text=True,
            )
            assert proc.returncode == exit_code, (fault, proc.stderr)
            assert f'"{ "E_STATE" if exit_code == 71 else "E_INPUT" }"' in proc.stderr
        print("      OK: E_STATE for locked/corrupt state, E_INPUT for read failure")

        print("[5/7] partition invariance and skew split ...")
        fingerprints = []
        for partitions in (1, 2, 3, 5):
            cpath = write_config(
                os.path.join(root, f"p{partitions}.json"),
                root,
                state_tag=f"p{partitions}",
                spill_tag=f"p{partitions}",
                partitions=partitions,
            )
            run([python, "-m", "window_engine.cli", "run", "--config", cpath,
                 "--report-name", f"p{partitions}"])
            report = read_json(
                os.path.join(root, "results", f"p{partitions}.json")
            )
            fingerprints.append(
                {
                    "windows": report["windows"],
                    "dedup_dropped": report["dedup_dropped"],
                    "late_events": report["late_events"],
                    "window_count": report["window_count"],
                }
            )
        assert all(fp == fingerprints[0] for fp in fingerprints)
        print("      OK: identical output for every partition count")

        print("[6/7] watermark tolerance semantics ...")
        relaxed_cfg = write_config(
            os.path.join(root, "relaxed.json"), root,
            state_tag="relaxed", spill_tag="relaxed",
            allowed_lateness_ms=600_000,
        )
        strict_cfg = write_config(
            os.path.join(root, "strict.json"), root,
            state_tag="strict", spill_tag="strict",
            allowed_lateness_ms=0,
        )
        run([python, "-m", "window_engine.cli", "run", "--config", relaxed_cfg,
             "--report-name", "relaxed"])
        run([python, "-m", "window_engine.cli", "run", "--config", strict_cfg,
             "--report-name", "strict"])
        relaxed = read_json(os.path.join(root, "results", "relaxed.json"))
        strict = read_json(os.path.join(root, "results", "strict.json"))
        assert strict["late_events"] >= relaxed["late_events"] or (
            sum(w["event_count"] for w in strict["windows"])
            <= sum(w["event_count"] for w in relaxed["windows"])
        )
        print(
            f"      OK: strict late={strict['late_events']} "
            f"relaxed late={relaxed['late_events']}"
        )

        print("[7/7] resource budget ...")
        started = time.time()
        budget_cfg = write_config(
            os.path.join(root, "budget.json"), root,
            state_tag="budget", spill_tag="budget",
            state_memory_bytes=512 * 1024,
        )
        run([python, "-m", "window_engine.cli", "run", "--config", budget_cfg,
             "--report-name", "budget"])
        budget = read_json(os.path.join(root, "results", "budget.json"))
        elapsed = time.time() - started
        assert budget["windows"] == mixed["windows"]
        assert budget["peak_rss_bytes"] < 1024**3
        assert elapsed < 360
        print(
            f"      OK: elapsed={elapsed:.1f}s "
            f"peak_rss={budget['peak_rss_bytes'] // 1024 // 1024}MiB "
            f"spill={budget['spill_bytes']} bytes"
        )

        print("\nALL CHECKS PASSED")
        return 0
    finally:
        if args.keep:
            print(f"artifacts kept in {root}")
        else:
            shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
