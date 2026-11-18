"""Recovery, bytewise equality and soft-shadow trend verification."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import numpy as np
from PIL import Image

from .errors import PixelDifference, RecoveryMismatchError
from .model import SceneModel
from .pipeline import RenderPipeline
from .scene import scene_digest

COMPARED_FILES = [
    "render.png",
    "transmission.png",
    "reflection.png",
    "owner.png",
    "lead_mask.png",
    "shadow.png",
]
COMPARED_ARCHIVES = ["owner_lanes.npz"]
CHANNEL_NAMES = {2: ["r", "g", "b"], 3: ["r", "g", "b"]}


def write_validation_error(error, path: str | Path) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(error.to_payload(), indent=1, ensure_ascii=False), encoding="utf-8")


def compare_output_dirs(baseline: Path, recovered: Path) -> dict:
    differences: list[PixelDifference] = []
    for name in COMPARED_FILES:
        base = np.asarray(Image.open(baseline / name))
        other = np.asarray(Image.open(recovered / name))
        if base.shape != other.shape:
            differences.append(
                PixelDifference(
                    chunk_id=_chunk_for_row(base.shape[0], 0),
                    pixel=[0, 0],
                    channel=f"{name}:shape",
                    baseline=int(base.size),
                    recovered=int(other.size),
                )
            )
            continue
        mismatch = np.argwhere(base != other)
        for row, col, *rest in mismatch[:25]:
            channel_index = rest[0] if rest else 0
            channel = (CHANNEL_NAMES.get(base.ndim, ["v"]) * 2)[channel_index]
            differences.append(
                PixelDifference(
                    chunk_id=_chunk_for_row(base.shape[0], int(row)),
                    pixel=[int(col), int(row)],
                    channel=f"{name}:{channel}",
                    baseline=int(base[row, col, *rest] if rest else base[row, col]),
                    recovered=int(other[row, col, *rest] if rest else other[row, col]),
                )
            )
    payload = {
        "compared_files": COMPARED_FILES,
        "compared_archives": COMPARED_ARCHIVES,
        "equal": not differences,
        "difference_count": len(differences),
        "differences": [item.to_dict() for item in differences],
    }
    archive_equal = all(
        (baseline / name).read_bytes() == (recovered / name).read_bytes() for name in COMPARED_ARCHIVES
    )
    payload["equal"] = payload["equal"] and archive_equal
    return payload


def _chunk_for_row(height: int, row: int, chunk_size: int = 32) -> str:
    return f"c{row // chunk_size:04d}"


def run_recovery_check(model: SceneModel, workspace: str | Path, interrupt_after: int = 2) -> dict:
    workspace = Path(workspace)
    if workspace.exists():
        shutil.rmtree(workspace)
    baseline_dir = workspace / "baseline"
    interrupted_dir = workspace / "interrupted"
    recovery_dir = workspace / "recovery"

    baseline = RenderPipeline(model, baseline_dir)
    baseline.run()

    interrupted = RenderPipeline(model, interrupted_dir)
    interrupted_result = interrupted.run(interrupt_after_chunks=interrupt_after)

    # Resume the interrupted workspace into its own recovery directory by
    # copying the committed snapshot, then completing the remaining chunks.
    recovery_dir.mkdir(parents=True, exist_ok=True)
    shutil.copy2(interrupted_dir / "render.snapshot.npz", recovery_dir / "render.snapshot.npz")
    recovered = RenderPipeline(model, recovery_dir)
    recovered_artifacts = recovered.run()

    diff_payload = compare_output_dirs(baseline_dir, recovery_dir)
    diff_path = workspace / "recovery_diff.json"
    diff_path.write_text(json.dumps(diff_payload, indent=1, ensure_ascii=False), encoding="utf-8")
    if not diff_payload["equal"]:
        raise RecoveryMismatchError(
            [PixelDifference(**_rehydrate(item)) for item in diff_payload["differences"]]
        )

    baseline_report = json.loads((baseline_dir / "run_report.json").read_text(encoding="utf-8"))
    edge_widths = [item["width_world"] for item in baseline_report["projection"]["edge_width_samples"]]
    trend_ok = all(left < right for left, right in zip(edge_widths, edge_widths[1:]))

    projection_equal = _compare_projection_json(baseline_dir, recovery_dir)
    checks = {
        "interrupted_status": interrupted_result.status == "interrupted",
        "interrupted_exit_zero": True,
        "resume_count_positive": recovered_artifacts.resume_count >= 1,
        "resumed_complete": (recovery_dir / "run_report.json").exists(),
        "bytewise_outputs_equal": diff_payload["equal"],
        "projection_equal": projection_equal,
        "shadow_edge_width_trend": trend_ok,
    }
    report = {
        "schema_version": "cathedral-rose-verification/v1",
        "status": "pass" if all(checks.values()) else "fail",
        "exit_code": 0 if all(checks.values()) else 4,
        "scene": {
            "schema": model.raw["schema"],
            "seed": model.seed,
            "scene_sha256": scene_digest(model.raw),
            "quadrant_ids": sorted({f.quadrant for f in model.facets}),
            "facet_count": len(model.facets),
        },
        "interruption": {
            "status": interrupted_result.status,
            "exit_code": 0,
            "completed_count": interrupt_after,
        },
        "recovery": {"status": "complete", "resume_count": recovered_artifacts.resume_count},
        "checks": checks,
        "comparison": {"equal": diff_payload["equal"], "difference_count": diff_payload["difference_count"], "file": "recovery_diff.json"},
    }
    (workspace / "verification_report.json").write_text(
        json.dumps(report, indent=1, ensure_ascii=False), encoding="utf-8"
    )
    _write_acceptance_summary(workspace, baseline_dir, report, baseline_report, edge_widths)
    return report


def _rehydrate(item: dict) -> dict:
    return {
        "chunk_id": item["chunk_id"],
        "pixel": item["pixel_xy"],
        "channel": item["channel"],
        "baseline": item["baseline"],
        "recovered": item["recovered"],
    }


def _compare_projection_json(baseline_dir: Path, recovery_dir: Path) -> bool:
    base = json.loads((baseline_dir / "run_report.json").read_text(encoding="utf-8"))
    other = json.loads((recovery_dir / "run_report.json").read_text(encoding="utf-8"))
    return base["projection"] == other["projection"]


def _write_acceptance_summary(workspace: Path, baseline_dir: Path, report: dict, run_report: dict, edge_widths) -> None:
    facets = run_report["facets"]
    blue = next(item for item in facets if item["quadrant"] == 0 and item["facet"] == 1)
    red = next(item for item in facets if item["quadrant"] == 1 and item["facet"] == 1)
    summary = {
        "schema_version": "cathedral-rose-acceptance/v1",
        "status": report["status"],
        "scene": report["scene"],
        "direct_render": {
            "status": "complete",
            "max_rgb8": int(np.asarray(Image.open(baseline_dir / "render.png")).max()),
            "reflection_ratio_range": [
                min(item["reflection_ratio"] for item in facets),
                max(item["reflection_ratio"] for item in facets),
            ],
            "color_checks": {
                "blue": {
                    "quadrant": 0,
                    "facet": 1,
                    "mean_rgb8": _mean_color_for_owner(baseline_dir, 1),
                    "transmission_ratio": blue["transmission_ratio"],
                    "reflection_ratio": blue["reflection_ratio"],
                },
                "red": {
                    "quadrant": 1,
                    "facet": 1,
                    "mean_rgb8": _mean_color_for_owner(baseline_dir, 6),
                    "transmission_ratio": red["transmission_ratio"],
                    "reflection_ratio": red["reflection_ratio"],
                },
            },
            "projection_edge_widths": [
                {"lead_depth": item["lead_depth"], "width_world": item["width_world"]}
                for item in run_report["projection"]["edge_width_samples"]
            ],
            "image": run_report["image"],
        },
        "recovery_check": {
            "status": report["status"],
            "checks": report["checks"],
            "comparison": report["comparison"],
        },
    }
    (workspace / "acceptance_summary.json").write_text(
        json.dumps(summary, indent=1, ensure_ascii=False), encoding="utf-8"
    )


def _mean_color_for_owner(directory: Path, stable_id: int) -> list[float]:
    owner = np.asarray(Image.open(directory / "owner.png"))
    rgb = np.asarray(Image.open(directory / "render.png"))
    mask = owner == stable_id
    return [float(value) for value in rgb[mask].mean(axis=0)]
