"""Run a persistent end-to-end acceptance experiment."""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from loudness_diag.blocks import (
    analyze_asset,
    compare_slicing,
    make_block_evaluator,
    slice_blocks,
)
from loudness_diag.gate import GateParameters, apply_gate
from loudness_diag.generator import generate_test_set
from loudness_diag.jsonutil import (
    atomic_write_json,
    canonical_bytes,
    load_json,
    sha256_json,
)
from loudness_diag.scheduler import SchedulerConfig
from loudness_diag.snapshot import SnapshotStore, snapshot_parameter_version
from loudness_diag.version_store import VersionedStore
from loudness_diag.wav import read_wav


def _cli(*arguments: str) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        [sys.executable, "-m", "loudness_diag", *arguments],
        cwd=ROOT,
        capture_output=True,
        check=False,
    )


def _require_success(result: subprocess.CompletedProcess[bytes], label: str) -> None:
    if result.returncode != 0:
        raise RuntimeError(
            f"{label} failed ({result.returncode}): {result.stderr.decode('utf-8')}"
        )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--output", default=str(ROOT / "artifacts" / "acceptance")
    )
    args = parser.parse_args()
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    for name in (
        "samples",
        "diagnose-a",
        "diagnose-b",
        "batch-a",
        "batch-b",
        "versions",
        "snapshots",
    ):
        path = output / name
        if path.is_dir():
            shutil.rmtree(path)
    for name in (
        "config.json",
        "params.json",
        "reslice.json",
        "acceptance_summary.json",
    ):
        path = output / name
        if path.exists():
            path.unlink()

    manifest = generate_test_set(output / "samples", seed=20261122, seconds=2.0)
    wav = read_wav(output / "samples" / "program_51_24bit.wav")
    unit = 9600 * wav.info.channels * 16
    config = SchedulerConfig(
        parallelism=3,
        memory_budget_bytes=unit * 2,
        block_memory_bytes=unit,
    )
    atomic_write_json(output / "config.json", config.as_dict())
    params = {
        "combinations": [
            {
                "absolute_lufs": -70.0,
                "relative_offset_lu": -10.0,
                "true_peak_limit_dbtp": -1.0,
            },
            {
                "absolute_lufs": -50.0,
                "relative_offset_lu": -4.0,
                "true_peak_limit_dbtp": -12.0,
            },
        ]
    }
    atomic_write_json(output / "params.json", params)

    common = [
        "--wav",
        str(output / "samples" / "program_51_24bit.wav"),
        "--asset-id",
        "acceptance",
        "--block-samples",
        "9600",
        "--config",
        str(output / "config.json"),
    ]
    for name in ("diagnose-a", "diagnose-b"):
        result = _cli("diagnose", "--output", str(output / name), *common)
        _require_success(result, name)
    report_a = (output / "diagnose-a" / "report.json").read_bytes()
    report_b = (output / "diagnose-b" / "report.json").read_bytes()
    trace_a = (output / "diagnose-a" / "trace.log").read_bytes()
    trace_b = (output / "diagnose-b" / "trace.log").read_bytes()
    byte_identical = report_a == report_b and trace_a == trace_b

    serial_config = SchedulerConfig(
        parallelism=1,
        memory_budget_bytes=unit,
        block_memory_bytes=unit,
    )
    serial_report, _ = analyze_asset(
        wav,
        block_samples=9600,
        asset_id="acceptance",
        scheduler_config=serial_config,
    )
    parallel_report = load_json(output / "diagnose-a" / "report.json")
    parallel_equal = serial_report["blocks"] == parallel_report["blocks"]

    for name in ("batch-a", "batch-b"):
        result = _cli(
            "batch",
            "--output",
            str(output / name),
            "--params",
            str(output / "params.json"),
            *common,
        )
        _require_success(result, name)
    batch_a = (output / "batch-a" / "batch_report.json").read_bytes()
    batch_b = (output / "batch-b" / "batch_report.json").read_bytes()
    batch_identical = batch_a == batch_b

    comparison = compare_slicing(
        wav,
        old_block_samples=9600,
        new_block_samples=19200,
        old_oversample=2,
        new_oversample=4,
        asset_id="acceptance",
        scheduler_config=serial_config,
    )
    atomic_write_json(output / "reslice.json", comparison)

    versions_root = output / "versions"
    version_store = VersionedStore(versions_root)
    version1 = version_store.publish(parallel_report)
    alternate, _ = analyze_asset(
        wav,
        block_samples=9600,
        asset_id="acceptance",
        gate_parameters=GateParameters(-50.0, -4.0, -12.0),
        scheduler_config=serial_config,
    )
    version2 = version_store.publish(alternate)
    old_report, old_hash = version_store.query_report(version1)
    current_report, current_hash = version_store.query_report()
    version_isolation = (
        old_hash == version1
        and current_hash == version2
        and old_report["report_hash"] == parallel_report["report_hash"]
        and current_report["report_hash"] == alternate["report_hash"]
    )

    specs = slice_blocks(wav, 9600, asset_id="acceptance")
    evaluator = make_block_evaluator(
        wav, specs, oversample=4, block_samples=9600
    )
    snapshot_parameters = snapshot_parameter_version(
        block_samples=9600,
        oversample=4,
        gate_parameters=GateParameters().as_dict(),
    )
    snapshot = SnapshotStore(
        output / "snapshots",
        asset_id="acceptance",
        parameters=snapshot_parameters,
        source_content_hash=wav.content_hash,
        parameter_version="gate-v1",
        total_blocks=len(specs),
    )
    for index in (0, 1):
        snapshot.commit_block(evaluator(index))
    committed_before = snapshot.committed_count
    reopened = SnapshotStore(
        output / "snapshots",
        asset_id="acceptance",
        parameters=snapshot_parameters,
        source_content_hash=wav.content_hash,
        parameter_version="gate-v1",
        total_blocks=len(specs),
    )
    resumed_evaluator = make_block_evaluator(
        wav, specs, oversample=4, block_samples=9600
    )
    computed: list[int] = []

    def compute(index: int) -> dict:
        computed.append(index)
        return resumed_evaluator(index)

    rows, gate_result, _ = reopened.resume(
        total_blocks=len(specs),
        compute_block=compute,
        build_gate=lambda block_rows: apply_gate(block_rows, GateParameters()),
    )
    recovery_only_missing = computed == list(range(committed_before, len(specs)))
    recovery_complete = (
        len(rows) == len(specs)
        and gate_result is not None
        and reopened.committed_count == len(specs)
    )

    batch_report = load_json(output / "batch-a" / "batch_report.json")
    summary = {
        "schema_version": 1,
        "ok": all(
            [
                byte_identical,
                parallel_equal,
                batch_identical,
                version_isolation,
                recovery_only_missing,
                recovery_complete,
            ]
        ),
        "seed": manifest["seed"],
        "sample_assets": len(manifest["assets"]),
        "block_samples": 9600,
        "block_count": len(specs),
        "diagnose_byte_identical": byte_identical,
        "parallel_serial_blocks_equal": parallel_equal,
        "batch_byte_identical": batch_identical,
        "version_isolation": version_isolation,
        "snapshot_committed_before": committed_before,
        "snapshot_computed_indices": computed,
        "recovery_only_missing": recovery_only_missing,
        "recovery_complete": recovery_complete,
        "report_hash": parallel_report["report_hash"],
        "version1": version1,
        "version2": version2,
        "batch_report_hash": batch_report["report_hash"],
        "reslice_block_count_delta": comparison["block_count_delta"],
        "flipped_block_ids": batch_report["flipped_block_ids"],
        "summary_hash": None,
    }
    summary["summary_hash"] = sha256_json(
        {key: value for key, value in summary.items() if key != "summary_hash"}
    )
    path = output / "acceptance_summary.json"
    path.write_bytes(canonical_bytes(summary))
    sys.stdout.buffer.write(canonical_bytes(summary))
    return 0 if summary["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
