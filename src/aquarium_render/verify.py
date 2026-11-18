"""Structured comparison of resumed output against an uninterrupted run."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .errors import RenderMismatchError
from .png import decode_png_rgba
from .scene import Scene

CHANNELS = ("R", "G", "B", "A")


def _flatten_differences(
    value: Any,
    path: str = "projection",
) -> list[tuple[str, Any]]:
    if isinstance(value, dict):
        result: list[tuple[str, Any]] = []
        for key in sorted(value):
            result.extend(_flatten_differences(value[key], f"{path}.{key}"))
        return result
    if isinstance(value, list):
        result = []
        for index, item in enumerate(value):
            result.extend(_flatten_differences(item, f"{path}[{index}]"))
        return result
    return [(path, value)]


def _compare_projection_reports(reference: dict[str, Any], actual: dict[str, Any]) -> list[dict[str, Any]]:
    ref_values: dict[str, Any] = {}
    act_values: dict[str, Any] = {}
    for section in (
        "scene_sha256",
        "particle_fingerprint",
        "renderer_revision",
        "projections",
        "layers",
        "shadow_maps",
    ):
        ref_values.update(dict(_flatten_differences(reference.get(section, {}), section)))
        act_values.update(dict(_flatten_differences(actual.get(section, {}), section)))
    diffs: list[dict[str, Any]] = []
    for path in sorted(set(ref_values) | set(act_values)):
        if ref_values.get(path) != act_values.get(path):
            diffs.append(
                {
                    "scope": "projection",
                    "angle_index": None,
                    "block_id": None,
                    "pixel": None,
                    "channel": path,
                    "reference": ref_values.get(path),
                    "actual": act_values.get(path),
                }
            )
    return diffs


def _compare_array(
    reference: np.ndarray,
    actual: np.ndarray,
    angle_index: int,
    scene: Scene,
    channel_names: tuple[str, ...],
    scope: str,
    limit: int = 200,
) -> tuple[list[dict[str, Any]], int]:
    if reference.shape != actual.shape:
        return (
            [
                {
                    "scope": scope,
                    "angle_index": angle_index,
                    "block_id": None,
                    "pixel": None,
                    "channel": "shape",
                    "reference": list(reference.shape),
                    "actual": list(actual.shape),
                }
            ],
            1,
        )
    mismatched = np.any(reference != actual, axis=2) if reference.ndim == 3 else reference != actual
    locations = np.argwhere(mismatched)
    total = int(locations.shape[0])
    diffs: list[dict[str, Any]] = []
    render = scene.render
    for y, x in locations[:limit].tolist():
        if reference.ndim == 3:
            for channel_index, channel in enumerate(channel_names):
                if reference[y, x, channel_index] == actual[y, x, channel_index]:
                    continue
                diffs.append(
                    {
                        "scope": scope,
                        "angle_index": angle_index,
                        "block_id": (y // render.block_height) * render.blocks_x
                        + (x // render.block_width),
                        "pixel": [int(x), int(y)],
                        "channel": channel,
                        "reference": int(reference[y, x, channel_index]),
                        "actual": int(actual[y, x, channel_index]),
                    }
                )
                if len(diffs) >= limit:
                    break
        else:
            diffs.append(
                {
                    "scope": scope,
                    "angle_index": angle_index,
                    "block_id": (y // render.block_height) * render.blocks_x
                    + (x // render.block_width),
                    "pixel": [int(x), int(y)],
                    "channel": "ID",
                    "reference": int(reference[y, x]),
                    "actual": int(actual[y, x]),
                }
            )
    return diffs, total


def compare_run_directories(
    reference_dir: str | Path,
    actual_dir: str | Path,
    scene: Scene,
    limit: int = 200,
) -> list[dict[str, Any]]:
    reference = Path(reference_dir)
    actual = Path(actual_dir)
    diffs: list[dict[str, Any]] = []
    for angle_index in range(len(scene.camera.angles())):
        for kind in ("rgba", "ownership", "preview"):
            filename = f"angle_{angle_index:04d}_{kind}.png"
            ref_path = reference / filename
            act_path = actual / filename
            if not ref_path.exists() or not act_path.exists():
                diffs.append(
                    {
                        "scope": kind,
                        "angle_index": angle_index,
                        "block_id": None,
                        "pixel": None,
                        "channel": "file",
                        "reference": str(ref_path) if ref_path.exists() else "missing",
                        "actual": str(act_path) if act_path.exists() else "missing",
                    }
                )
                continue
            ref_image = decode_png_rgba(ref_path.read_bytes())
            act_image = decode_png_rgba(act_path.read_bytes())
            if kind in {"rgba", "preview"}:
                image_diffs, _ = _compare_array(
                    ref_image,
                    act_image,
                    angle_index,
                    scene,
                    CHANNELS,
                    kind,
                    limit=max(limit - len(diffs), 1),
                )
            else:
                image_diffs, _ = _compare_array(
                    ref_image,
                    act_image,
                    angle_index,
                    scene,
                    ("R", "G", "B", "A"),
                    "ownership",
                    limit=max(limit - len(diffs), 1),
                )
            diffs.extend(image_diffs)
            if len(diffs) >= limit:
                break
        if len(diffs) >= limit:
            break

    ref_report_path = reference / "report.json"
    act_report_path = actual / "report.json"
    if ref_report_path.exists() and act_report_path.exists():
        try:
            ref_report = json.loads(ref_report_path.read_text(encoding="utf-8"))
            act_report = json.loads(act_report_path.read_text(encoding="utf-8"))
            diffs.extend(_compare_projection_reports(ref_report, act_report))
        except (OSError, json.JSONDecodeError, TypeError, ValueError) as exc:
            diffs.append(
                {
                    "scope": "projection",
                    "angle_index": None,
                    "block_id": None,
                    "pixel": None,
                    "channel": "report_parse",
                    "reference": str(exc),
                    "actual": None,
                }
            )
    elif ref_report_path.exists() != act_report_path.exists():
        diffs.append(
            {
                "scope": "projection",
                "angle_index": None,
                "block_id": None,
                "pixel": None,
                "channel": "report_file",
                "reference": ref_report_path.exists(),
                "actual": act_report_path.exists(),
            }
        )
    return diffs


def enforce_reference(
    reference_dir: str | Path,
    actual_dir: str | Path,
    scene: Scene,
) -> None:
    diffs = compare_run_directories(reference_dir, actual_dir, scene)
    if diffs:
        raise RenderMismatchError(diffs)

