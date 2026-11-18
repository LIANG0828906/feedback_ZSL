"""Byte-level baseline comparison with structured difference reporting."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .scene import canonical_json

_CHANNELS = ("R", "G", "B", "A")


def _block_id(scene: dict[str, Any], camera_index: int, x: int, y: int) -> str:
    camera = scene["cameras"][camera_index]
    block_w, block_h = (int(v) for v in scene["render"]["block_size"])
    blocks_per_row = (int(camera["width"]) + block_w - 1) // block_w
    block_index = (y // block_h) * blocks_per_row + x // block_w
    return f"{camera['id']}/b{block_index:04d}"


def _read_npy(path: Path) -> np.ndarray:
    return np.load(path, allow_pickle=False)


def _landing_pixel(scene: dict[str, Any], point: list[float]) -> tuple[str, list[int]]:
    camera = scene["cameras"][0]
    position = np.asarray(camera["position"], dtype=np.float64)
    target = np.asarray(camera["target"], dtype=np.float64)
    up = np.asarray(camera["up"], dtype=np.float64)
    forward = target - position
    forward /= np.linalg.norm(forward)
    right = np.cross(forward, up)
    right /= np.linalg.norm(right)
    true_up = np.cross(right, forward)
    relative = np.asarray(point, dtype=np.float64) - position
    depth = float(np.dot(relative, forward))
    if depth <= 1.0e-12:
        return f"{camera['id']}/b0000", [0, 0]
    half_height = np.tan(np.radians(float(camera["fov_y_deg"])) * 0.5)
    aspect = float(camera["width"]) / float(camera["height"])
    ndc_x = float(np.dot(relative, right)) / (depth * half_height * aspect)
    ndc_y = float(np.dot(relative, true_up)) / (depth * half_height)
    x = int(round((ndc_x + 1.0) * 0.5 * float(camera["width"]) - 0.5))
    y = int(round((1.0 - ndc_y) * 0.5 * float(camera["height"]) - 0.5))
    x = min(max(x, 0), int(camera["width"]) - 1)
    y = min(max(y, 0), int(camera["height"]) - 1)
    return _block_id(scene, 0, x, y), [x, y]


def compare_baseline(
    current_dir: str | Path,
    baseline_dir: str | Path,
    scene: dict[str, Any],
    current_images: dict[str, dict[str, np.ndarray]],
    current_report: dict[str, Any],
    limit: int = 4000,
) -> dict[str, Any]:
    current = Path(current_dir)
    baseline = Path(baseline_dir)
    differences: list[dict[str, Any]] = []
    total = 0

    def add(record: dict[str, Any]) -> None:
        nonlocal total
        total += 1
        if len(differences) < limit:
            differences.append(record)

    for camera_index, camera in enumerate(scene["cameras"]):
        camera_id = str(camera["id"])
        for key, channel_names in [
            ("rgba", _CHANNELS),
            ("face_id", ("face_id",)),
        ]:
            current_array = current_images[camera_id][key]
            paths = {
                "rgba": "C{index:02d}.rgba.npy",
                "face_id": "C{index:02d}.face_id.npy",
            }
            filename = paths[key].format(index=camera_index)
            baseline_path = baseline / filename
            if not baseline_path.exists():
                add(
                    {
                        "kind": "missing_baseline_file",
                        "camera_id": camera_id,
                        "file": filename,
                        "block_id": _block_id(scene, camera_index, 0, 0),
                        "channel": "file",
                        "pixel": [0, 0],
                    }
                )
                continue
            baseline_array = _read_npy(baseline_path)
            if baseline_array.shape != current_array.shape:
                add(
                    {
                        "kind": "shape",
                        "camera_id": camera_id,
                        "block_id": _block_id(scene, camera_index, 0, 0),
                        "pixel": [0, 0],
                        "channel": "shape",
                        "baseline": list(baseline_array.shape),
                        "current": list(current_array.shape),
                    }
                )
                continue
            if np.array_equal(current_array, baseline_array):
                continue
            if key == "rgba":
                mismatch = current_array != baseline_array
                for y, x, channel in np.argwhere(mismatch):
                    add(
                        {
                            "kind": "image_pixel",
                            "camera_id": camera_id,
                            "block_id": _block_id(scene, camera_index, int(x), int(y)),
                            "pixel": [int(x), int(y)],
                            "channel": channel_names[int(channel)],
                            "baseline": int(baseline_array[y, x, channel]),
                            "current": int(current_array[y, x, channel]),
                        }
                    )
            else:
                for y, x in np.argwhere(current_array != baseline_array):
                    add(
                        {
                            "kind": "image_pixel",
                            "camera_id": camera_id,
                            "block_id": _block_id(scene, camera_index, int(x), int(y)),
                            "pixel": [int(x), int(y)],
                            "channel": "face_id",
                            "baseline": int(baseline_array[y, x]),
                            "current": int(current_array[y, x]),
                        }
                    )

    baseline_report_path = baseline / "report.json"
    if not baseline_report_path.exists():
        add(
            {
                "kind": "missing_baseline_report",
                "block_id": f"{scene['cameras'][0]['id']}/b0000",
                "channel": "report",
                "pixel": [0, 0],
            }
        )
    else:
        with baseline_report_path.open("r", encoding="utf-8") as handle:
            baseline_report = json.load(handle)
        current_landings = current_report.get("caustics", {}).get("lights", [])
        baseline_landings = baseline_report.get("caustics", {}).get("lights", [])
        current_sources = {item.get("source_id"): item for item in current_landings}
        baseline_sources = {item.get("source_id"): item for item in baseline_landings}
        for source_id in sorted(set(current_sources) | set(baseline_sources)):
            current_source = current_sources.get(source_id)
            baseline_source = baseline_sources.get(source_id)
            if current_source is None or baseline_source is None:
                add(
                    {
                        "kind": "caustic_source",
                        "source_id": source_id,
                        "block_id": f"{scene['cameras'][0]['id']}/b0000",
                        "channel": "source_presence",
                        "pixel": [0, 0],
                    }
                )
                continue
            current_samples = current_source.get("samples", [])
            baseline_samples = baseline_source.get("samples", [])
            if len(current_samples) != len(baseline_samples):
                add(
                    {
                        "kind": "caustic_landing_count",
                        "source_id": source_id,
                        "block_id": f"{scene['cameras'][0]['id']}/b0000",
                        "channel": "sample_count",
                        "baseline": len(baseline_samples),
                        "current": len(current_samples),
                        "pixel": [0, 0],
                    }
                )
            for index in range(min(len(current_samples), len(baseline_samples))):
                current_sample = current_samples[index]
                baseline_sample = baseline_samples[index]
                if canonical_json(current_sample) == canonical_json(baseline_sample):
                    continue
                differing_fields = sorted(
                    key
                    for key in set(current_sample) | set(baseline_sample)
                    if current_sample.get(key) != baseline_sample.get(key)
                )
                block_id, pixel = _landing_pixel(
                    scene,
                    current_sample.get("landing_point", [0.0, 0.0, 0.0]),
                )
                add(
                    {
                        "kind": "caustic_landing",
                        "source_id": source_id,
                        "sample_index": int(
                            current_sample.get("sample_index", index)
                        ),
                        "face_id": current_sample.get("face_id"),
                        "channel": ",".join(differing_fields),
                        "fields": differing_fields,
                        "block_id": block_id,
                        "baseline": {
                            key: baseline_sample.get(key) for key in differing_fields
                        },
                        "current": {
                            key: current_sample.get(key) for key in differing_fields
                        },
                        "pixel": pixel,
                    }
                )
        if canonical_json(current_report) != canonical_json(baseline_report):
            if total == 0:
                add(
                    {
                        "kind": "report_mismatch",
                        "block_id": f"{scene['cameras'][0]['id']}/b0000",
                        "channel": "report",
                        "pixel": [0, 0],
                    }
                )

    return {
        "schema": "elytron-difference-report/v1",
        "status": "different" if total else "identical",
        "difference_count": total,
        "truncated": total > len(differences),
        "limit": limit,
        "differences": differences,
    }
