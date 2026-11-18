"""Independent full rerender verification and report consistency checks."""

from __future__ import annotations

import json
import tempfile
from pathlib import Path
from typing import Any

from .diff import compare_arrays, load_raw_output
from .pipeline import render_to_directory
from .scene import Scene


def _report_recomputation_mismatches(
    expected: dict[str, Any], recomputed: dict[str, Any]
) -> list[dict[str, Any]]:
    mismatches: list[dict[str, Any]] = []
    for field in (
        "scene_sha256",
        "layers",
        "penumbra_probes",
        "blocks",
    ):
        if expected.get(field) != recomputed.get(field):
            mismatches.append({"field": field, "reason": "value differs"})
    expected_summary = expected.get("summary", {})
    recomputed_summary = recomputed.get("summary", {})
    for field in (
        "resolution",
        "block_grid",
        "block_size",
        "samples_per_pixel",
        "block_count",
        "completed_blocks",
        "image_encoding",
        "image_rgba_sha256",
        "straight_rgba_sha256",
        "optical_depth_sha256",
    ):
        if expected_summary.get(field) != recomputed_summary.get(field):
            mismatches.append({"field": f"summary.{field}", "reason": "value differs"})
    return mismatches


def verify_output(scene: Scene, output_directory: str | Path) -> dict[str, Any]:
    output_directory = Path(output_directory)
    expected_rgba, expected_depth = load_raw_output(
        output_directory, scene.camera.width, scene.camera.height
    )
    report_path = output_directory / "report.json"
    with open(report_path, "r", encoding="utf-8") as handle:
        expected_report = json.load(handle)
    with tempfile.TemporaryDirectory(prefix="shadowbox-verify-") as temporary:
        baseline_directory = Path(temporary)
        result = render_to_directory(scene, baseline_directory, fresh=True)
        if result.exit_code != 0:
            raise RuntimeError("baseline render failed")
        baseline_rgba, baseline_depth = load_raw_output(
            baseline_directory, scene.camera.width, scene.camera.height
        )
        with open(
            baseline_directory / "report.json", "r", encoding="utf-8"
        ) as handle:
            baseline_report = json.load(handle)
    comparison = compare_arrays(
        baseline_rgba,
        expected_rgba,
        baseline_depth,
        expected_depth,
        scene,
    )
    report_mismatches = _report_recomputation_mismatches(
        expected_report, baseline_report
    )
    return {
        "format": "shadowbox-verify-v1",
        "ok": comparison["equal"] and not report_mismatches,
        "pixel_comparison": comparison,
        "report_mismatches": report_mismatches,
    }
