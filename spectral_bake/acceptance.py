"""Local acceptance checks."""
from __future__ import annotations

import json
import shutil
import subprocess
import sys
from dataclasses import replace
from pathlib import Path

from .batch import load_latest_valid
from .config import load_config
from .engine import run_bake
from .errors import BudgetExceeded, RecoveryInvalid
from .samplegen import generate_samples
from .spectrum import load_materials


def _fresh_samples(workdir: Path, name: str) -> Path:
    out = workdir / name
    if out.exists():
        shutil.rmtree(out)
    generate_samples(out)
    return out


def _load(workdir_samples: Path):
    config = load_config(workdir_samples / "config.json")
    materials = load_materials(workdir_samples / "materials.json", config.grid)
    return config, materials


def check_parallelism_independence(workdir: Path) -> dict:
    samples = _fresh_samples(workdir, "parallel")
    config, materials = _load(samples)
    manifests = {}
    for par in (1, 2, 4, 8):
        cfg = replace(config, parallelism=par)
        manifests[par] = run_bake(cfg, materials, batch_id=f"par{par}")
    ref = manifests[1]
    details = []
    ok = True
    for par, manifest in manifests.items():
        same_codes = manifest["entries"] == ref["entries"]
        # chain tips differ only because batch_id enters the genesis link;
        # per-entry hashes and codes must be byte-identical.
        same_hashes = [e["content_hash"] for e in manifest["entries"]] == [
            e["content_hash"] for e in ref["entries"]
        ]
        details.append({"parallelism": par, "codes_equal": same_codes, "hashes_equal": same_hashes})
        ok = ok and same_codes and same_hashes
    return {"ok": ok, "details": details}


def check_ordered_degradation(workdir: Path) -> dict:
    samples = _fresh_samples(workdir, "degrade")
    config, materials = _load(samples)
    # Baseline publish so we can prove it is retained on budget failure.
    baseline = run_bake(config, materials, batch_id="base")

    # Tight per-task quota: forces the full declared sequence stage by stage.
    tight = replace(config, per_task_memory_bytes=70_000)
    degraded = run_bake(tight, materials, batch_id="degraded")
    stages = [d["stage"] for d in degraded["degradation"]]
    declared = list(config.degradation_sequence)
    ordered = stages == declared[: len(stages)] and len(stages) > 0

    # Impossible quota: sequence exhausts -> E_BUDGET_EXCEEDED, baseline kept.
    impossible = replace(config, per_task_memory_bytes=1_000)
    rejected = False
    try:
        run_bake(impossible, materials, batch_id="impossible")
    except BudgetExceeded:
        rejected = True
    latest = load_latest_valid(samples / "snapshots")
    retained = latest is not None and latest["batch_id"] == "degraded"
    baseline_intact = latest["entries"] == degraded["entries"]
    ok = ordered and rejected and retained and baseline_intact
    return {
        "ok": ok,
        "stages_applied": stages,
        "declared_sequence": declared,
        "budget_exceeded_raised": rejected,
        "previous_batch_retained": retained,
        "baseline_chain_tip": baseline["hash_chain"][-1],
    }


def check_kill_recovery(workdir: Path) -> dict:
    samples = _fresh_samples(workdir, "recovery")
    config, materials = _load(samples)
    # Reference: uninterrupted run.
    reference = run_bake(config, materials, batch_id="ref")

    samples2 = _fresh_samples(workdir, "recovery_killed")
    # Killed run in a subprocess (SIGKILL after a few sphere commits).
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "spectral_bake.cli",
            "bake",
            "--config",
            str(samples2 / "config.json"),
            "--materials",
            str(samples2 / "materials.json"),
            "--batch-id",
            "crash",
            "--simulate-kill-after",
            "3",
        ],
        capture_output=True,
        text=True,
        cwd=str(Path(__file__).resolve().parent.parent),
    )
    killed = proc.returncode != 0
    journal = samples2 / "snapshots" / "batch-crash.journal.jsonl"
    journal_lines = len(journal.read_text().strip().splitlines()) if journal.exists() else 0

    # Recovery run: same batch id, must reuse journal and bake the rest.
    config2, materials2 = _load(samples2)
    recovered = run_bake(config2, materials2, batch_id="crash")
    same_as_reference = recovered["entries"] == reference["entries"]

    # Corrupt the manifest -> E_RECOVERY_INVALID.
    manifest_path = samples2 / "snapshots" / "batch-crash.json"
    manifest = json.loads(manifest_path.read_text())
    manifest["entries"][0]["codes"]["srgb"][0] += 1  # break the hash chain
    manifest_path.write_text(json.dumps(manifest))
    invalid_detected = False
    try:
        load_latest_valid(samples2 / "snapshots")
    except RecoveryInvalid:
        invalid_detected = True

    # In-flight worker threads may commit extra spheres before SIGKILL lands,
    # so the journal holds at least 3 entries; recovery must still reproduce
    # the reference manifest exactly.
    ok = killed and journal_lines >= 3 and same_as_reference and invalid_detected
    return {
        "ok": ok,
        "killed_process_returncode": proc.returncode,
        "journal_entries_before_recovery": journal_lines,
        "recovered_equals_reference": same_as_reference,
        "broken_chain_detected": invalid_detected,
    }


def run_acceptance(workdir) -> dict:
    workdir = Path(workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    return {
        "parallelism_independence": check_parallelism_independence(workdir),
        "ordered_degradation": check_ordered_degradation(workdir),
        "kill_recovery": check_kill_recovery(workdir),
    }
