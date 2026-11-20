#!/usr/bin/env python3
"""Verify stored demo artifacts and optionally rerun full determinism checks."""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
from pathlib import Path
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender.injection import INJECTIONS


TRACE_FILES = ("trace.jsonl", "render.png", "error_heatmap.png", "snapshot.json")
EXPECTED_INJECTIONS = {
    "dim_mismatch": ("E_VOLUME_INVALID", "DIM_LENGTH_MISMATCH"),
    "window_nonfinite": ("E_VOLUME_INVALID", "WINDOW_NON_FINITE"),
    "tf_out_of_range": ("E_TF_INVALID", "CONTROL_POINT_OUT_OF_RANGE"),
    "truncated_voxels": ("E_VOLUME_INVALID", "DATA_TRUNCATED"),
    "budget_overflow": ("E_BUDGET_EXCEEDED", "DEGRADATION_EXHAUSTED"),
}


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def read_trace(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line]


def verify_tier(root: Path, tier: str) -> dict:
    sample = root / "samples" / tier
    demo = root / f"demo_{tier}"
    baseline = json.loads((sample / "baseline_hash.json").read_text(encoding="utf-8"))
    assert sha256(sample / "volume.svol") == baseline["volume_sha256"]
    rows = read_trace(demo / "trace.jsonl")
    render_rows = [row for row in rows if row["event"] == "render"]
    repeat_rows = [row for row in rows if row["event"] == "repeat_check"]
    snapshot_rows = [row for row in rows if row["event"] == "snapshot"]
    assert len(render_rows) == 4
    assert render_rows[0]["content_hash"] == baseline["baseline_content_hash"]
    assert render_rows[-1]["degradation_stages"] == [
        "increase_step",
        "lower_early_term",
        "reduce_batch",
    ]
    assert repeat_rows == [{"event": "repeat_check", "identical": True}]
    snapshot = json.loads((demo / "snapshot.json").read_text(encoding="utf-8"))
    assert snapshot["content_hash"] == render_rows[-1]["content_hash"]
    assert snapshot_rows[0]["content_hash"] == snapshot["content_hash"]
    assert snapshot_rows[0]["stale_fields"] == ["view", "lut", "budget"]
    for name in TRACE_FILES:
        assert (demo / name).is_file()
    return {
        "baseline_content_hash": baseline["baseline_content_hash"],
        "baseline_image": baseline["baseline_image"],
        "degradation_stages": render_rows[-1]["degradation_stages"],
        "demo_file_sha256": {name: sha256(demo / name) for name in TRACE_FILES},
        "dims": baseline["dims"],
        "final_content_hash": render_rows[-1]["content_hash"],
        "final_error": render_rows[-1]["error"],
        "final_memory_used_bytes": render_rows[-1]["budget"]["memory_used_bytes"],
        "final_samples_used": render_rows[-1]["budget"]["samples_used"],
        "repeat_identical": True,
        "snapshot_stale_fields": snapshot_rows[0]["stale_fields"],
        "step_size": render_rows[-1]["step_size"],
        "volume_sha256": baseline["volume_sha256"],
    }


def verify_injections(root: Path) -> dict:
    result: dict[str, dict] = {}
    for injection in INJECTIONS:
        directory = root / f"demo_inj_{injection}"
        assert [path.name for path in directory.iterdir() if path.is_file()] == ["trace.jsonl"]
        rows = read_trace(directory / "trace.jsonl")
        error = [row for row in rows if row["event"] == "error"][-1]
        code, detail = EXPECTED_INJECTIONS[injection]
        assert error["code"] == code
        assert error["detail"] == detail
        assert error["inject"] == injection
        assert error["injection_point"].endswith(f":{injection}")
        result[injection] = {
            "code": code,
            "detail": detail,
            "field": error.get("field"),
            "offset": error.get("offset"),
            "only_trace_written": True,
        }
    return result


def rerun_determinism(root: Path, tier: str) -> dict:
    from volrender.demo import render_budget_trajectory

    volume = root / "samples" / tier / "volume.svol"
    with tempfile.TemporaryDirectory(prefix=f"volrender-determinism-{tier}-") as temporary:
        temporary_path = Path(temporary)
        render_budget_trajectory(
            volume_path=volume,
            output_dir=temporary_path / "a",
            tier=tier,
        )
        render_budget_trajectory(
            volume_path=volume,
            output_dir=temporary_path / "b",
            tier=tier,
        )
        comparisons = {}
        for name in TRACE_FILES:
            first = sha256(temporary_path / "a" / name)
            second = sha256(temporary_path / "b" / name)
            current = sha256(root / f"demo_{tier}" / name)
            assert first == second == current
            comparisons[name] = first
        return {"byte_identical": True, "sha256": comparisons}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path, default=ROOT / "artifacts")
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "reports" / "validation.json",
    )
    parser.add_argument(
        "--rerun-determinism",
        action="store_true",
        help="rerun both tiers twice and compare every artifact byte-for-byte",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    packages = {}
    for name in ("numpy", "PySide6", "PyOpenGL", "pytest"):
        try:
            packages[name] = importlib.metadata.version(name)
        except importlib.metadata.PackageNotFoundError:
            packages[name] = None
    pytest_report = args.artifacts / "reports" / "pytest.txt"
    report = {
        "environment": {
            "packages": packages,
            "python": sys.version.split()[0],
            "runtime": "local",
        },
        "pytest": (
            pytest_report.read_text(encoding="utf-8").splitlines()[-1]
            if pytest_report.exists()
            else None
        ),
        "tiers": {
            "small": verify_tier(args.artifacts, "small"),
            "large": verify_tier(args.artifacts, "large"),
        },
        "injections": verify_injections(args.artifacts),
    }
    if args.rerun_determinism:
        report["determinism"] = {
            "small": rerun_determinism(args.artifacts, "small"),
            "large": rerun_determinism(args.artifacts, "large"),
        }
    elif args.output.exists():
        previous = json.loads(args.output.read_text(encoding="utf-8"))
        if (
            previous.get("determinism")
            and previous.get("tiers") == report["tiers"]
            and previous.get("injections") == report["injections"]
        ):
            report["determinism"] = previous["determinism"]
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(report, sort_keys=True, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(json.dumps({"verified": True, "report": str(args.output)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

