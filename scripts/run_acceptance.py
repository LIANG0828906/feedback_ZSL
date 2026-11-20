"""Reproduce the budget, recovery, and snapshot-integrity acceptance run."""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from glyph_atlas.errors import (
    E_BUDGET_EXCEEDED,
    E_SNAPSHOT_MISMATCH,
    GlyphAtlasError,
)
from glyph_atlas.generator import _mixed_nonzero_font
from glyph_atlas.pipeline import run_pipeline
from glyph_atlas.snapshot import atomic_write_bytes, atomic_write_json, canonical_json_bytes


def run_acceptance(root: Path) -> dict[str, Any]:
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True)
    atomic_write_json(root / "font.json", _mixed_nonzero_font(1947))

    tight = _config(
        root,
        name="config_tight.json",
        output_dir="work_tight/output",
        snapshot_dir="work/snapshots",
        temp_dir="work/tmp",
        max_total_sampling_points=1600,
    )
    resume = _config(
        root,
        name="config_resume.json",
        output_dir="work_resume/output",
        snapshot_dir="work/snapshots",
        temp_dir="work/tmp",
        max_total_sampling_points=10_000_000,
    )
    clean = _config(
        root,
        name="config_clean.json",
        output_dir="work_clean/output",
        snapshot_dir="work_clean/snapshots",
        temp_dir="work/tmp",
        max_total_sampling_points=10_000_000,
    )

    try:
        run_pipeline(tight)
        raise AssertionError("tight budget unexpectedly completed")
    except GlyphAtlasError as exc:
        if exc.code != E_BUDGET_EXCEEDED:
            raise
        budget_error = _selected_error(exc)
    budget_trace = root / "work_tight/output/ui18/trace.jsonl"
    atomic_write_bytes(root / "budget_trace.jsonl", budget_trace.read_bytes())

    resumed = run_pipeline(resume, resume=True)
    resumed_glyph = resumed.glyphs_for("ui18")["O"]
    atomic_write_bytes(
        root / "resume_trace.jsonl",
        (root / "work_resume/output/ui18/trace.jsonl").read_bytes(),
    )
    clean_report = run_pipeline(clean)
    clean_glyph = clean_report.glyphs_for("ui18")["O"]
    atomic_write_bytes(
        root / "clean_trace.jsonl",
        (root / "work_clean/output/ui18/trace.jsonl").read_bytes(),
    )

    snapshot_path = root / "work/snapshots/ui18.0001.O.snapshot.json"
    snapshot_original = snapshot_path.read_bytes()
    version_document = json.loads(snapshot_original)
    version_document["version"] = 77
    atomic_write_json(snapshot_path, version_document)
    try:
        run_pipeline(resume, resume=True)
        raise AssertionError("version-tampered snapshot unexpectedly loaded")
    except GlyphAtlasError as exc:
        if exc.code != E_SNAPSHOT_MISMATCH:
            raise
        version_error = _selected_error(exc)
    snapshot_path.write_bytes(snapshot_original)

    chain_document = json.loads(snapshot_original)
    payload = bytearray(base64.b64decode(chain_document["tiles"][0]["coverage_b64"]))
    payload[0] ^= 0x01
    chain_document["tiles"][0]["coverage_b64"] = base64.b64encode(bytes(payload)).decode("ascii")
    atomic_write_json(snapshot_path, chain_document)
    try:
        run_pipeline(resume, resume=True)
        raise AssertionError("hash-chain-tampered snapshot unexpectedly loaded")
    except GlyphAtlasError as exc:
        if exc.code != E_SNAPSHOT_MISMATCH:
            raise
        chain_error = _selected_error(exc)
    snapshot_path.write_bytes(snapshot_original)

    result = {
        "format": "glyph-atlas-acceptance/1",
        "font_sha256": resumed.font.canonical_hash(),
        "budget": {
            "code": budget_error["code"],
            "task": budget_error["details"]["task"],
            "coord": budget_error["details"]["coord"],
            "used_sampling_points": budget_error["details"]["used_sampling_points"],
            "limit_value": budget_error["details"]["limit_value"],
            "completed_indices": budget_error["details"]["completed_indices"],
            "trace_sha256": _sha(root / "budget_trace.jsonl"),
        },
        "resume": {
            "success": True,
            "tile_count": len(resumed_glyph.tiles),
            "coverage_sha256": resumed_glyph.coverage_hash,
            "whole_max_difference": resumed_glyph.whole_reference["max_difference"],
            "trace_sha256": _sha(root / "resume_trace.jsonl"),
        },
        "clean": {
            "coverage_sha256": clean_glyph.coverage_hash,
            "whole_max_difference": clean_glyph.whole_reference["max_difference"],
            "trace_sha256": _sha(root / "clean_trace.jsonl"),
        },
        "coverage_equal_after_resume": resumed_glyph.coverage_hash == clean_glyph.coverage_hash,
        "snapshot_version_check": {
            "rejected": True,
            "code": version_error["code"],
        },
        "snapshot_hash_chain_check": {
            "rejected": True,
            "code": chain_error["code"],
        },
    }
    atomic_write_json(root / "results.json", result)
    return result


def _config(
    root: Path,
    *,
    name: str,
    output_dir: str,
    snapshot_dir: str,
    temp_dir: str,
    max_total_sampling_points: int,
) -> Path:
    document = {
        "format": "glyph-atlas-config/1",
        "font": "font.json",
        "glyphs": ["O"],
        "targets": [
            {
                "name": "ui18",
                "pixel_size": 18,
                "subpixel_x": 0.25,
                "subpixel_y": 0.125,
            }
        ],
        "tile": {"width": 10, "height": 10},
        "parallelism": 1,
        "samples": {"x": 4, "y": 4},
        "quantization_step": 1,
        "fill_rule": "nonzero",
        "budget": {
            "max_task_sampling_points": 5000,
            "max_task_memory_bytes": 10_000_000,
            "max_total_sampling_points": max_total_sampling_points,
            "max_total_memory_bytes": 100_000_000,
        },
        "atlas": {"width": 128, "height": 128, "padding": 1},
        "coverage_tolerance": 0,
        "output_dir": output_dir,
        "snapshot_dir": snapshot_dir,
        "temp_dir": temp_dir,
    }
    path = root / name
    atomic_write_json(path, document)
    return path


def _selected_error(error: GlyphAtlasError) -> dict[str, Any]:
    details = dict(error.details)
    details.pop("snapshot", None)
    details.pop("trace_file", None)
    details.pop("expected", None)
    details.pop("actual", None)
    return {"code": error.code, "message": error.message, "details": details}


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default="samples/acceptance")
    args = parser.parse_args()
    result = run_acceptance(Path(args.output))
    print(canonical_json_bytes(result).decode("utf-8"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

