"""Structured comparison of two completed render output directories."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np

from .io_utils import atomic_write_json


def _load_json(path: Path) -> dict[str, Any]:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def compare_outputs(
    reference_dir: str | Path,
    candidate_dir: str | Path,
    *,
    output_path: str | Path | None = None,
    max_entries: int = 64,
) -> dict[str, Any]:
    reference = Path(reference_dir)
    candidate = Path(candidate_dir)
    problems: list[dict[str, Any]] = []
    reference_report_path = reference / "report.json"
    candidate_report_path = candidate / "report.json"
    chunk_size = 32
    chunks_x = 1
    try:
        reference_report = _load_json(reference_report_path)
        candidate_report = _load_json(candidate_report_path)
        chunk_size = int(reference_report["image"]["chunk_size"])
        chunks_x = int(reference_report["image"]["chunks_x"])
    except Exception as exc:
        return {
            "ok": False,
            "code": "RW_E_COMPARE_INPUT",
            "message": f"cannot read report metadata: {exc}",
            "differences": [],
        }

    differences: list[dict[str, Any]] = []
    mismatch_count = 0
    for filename, labels in (
        ("rgb.npy", ("R", "G", "B")),
        ("ownership.npy", ("owner",)),
    ):
        reference_array = np.load(reference / filename, allow_pickle=False)
        candidate_array = np.load(candidate / filename, allow_pickle=False)
        if reference_array.shape != candidate_array.shape:
            differences.append(
                {
                    "file": filename,
                    "chunk_id": None,
                    "pixel": None,
                    "channels": ["shape"],
                    "reference_shape": list(reference_array.shape),
                    "candidate_shape": list(candidate_array.shape),
                }
            )
            mismatch_count += 1
            continue
        unequal = reference_array != candidate_array
        if reference_array.ndim == 3:
            pixel_mask = np.any(unequal, axis=2)
        else:
            pixel_mask = unequal
        mismatch_count += int(pixel_mask.sum())
        coordinates = np.argwhere(pixel_mask)
        for y, x in coordinates[: max(0, max_entries - len(differences))]:
            channels: list[str] = []
            if reference_array.ndim == 3:
                for channel_index, label in enumerate(labels):
                    if reference_array[y, x, channel_index] != candidate_array[y, x, channel_index]:
                        channels.append(label)
                reference_value: Any = [
                    int(value) for value in reference_array[y, x].tolist()
                ]
                candidate_value: Any = [
                    int(value) for value in candidate_array[y, x].tolist()
                ]
            else:
                channels = ["owner"]
                reference_value = int(reference_array[y, x])
                candidate_value = int(candidate_array[y, x])
            chunk_id = int(y // chunk_size) * chunks_x + int(x // chunk_size)
            differences.append(
                {
                    "file": filename,
                    "chunk_id": chunk_id,
                    "pixel": [int(x), int(y)],
                    "channels": channels,
                    "reference": reference_value,
                    "candidate": candidate_value,
                }
            )

    for filename in ("shadow_projections.json", "ownership_map.json"):
        reference_bytes = (reference / filename).read_bytes()
        candidate_bytes = (candidate / filename).read_bytes()
        if reference_bytes != candidate_bytes:
            differences.append(
                {
                    "file": filename,
                    "chunk_id": None,
                    "pixel": None,
                    "channels": ["document"],
                    "reference_sha256": hashlib.sha256(reference_bytes).hexdigest(),
                    "candidate_sha256": hashlib.sha256(candidate_bytes).hexdigest(),
                }
            )
            mismatch_count += 1

    if reference_report_path.read_bytes() != candidate_report_path.read_bytes():
        mismatch_count += 1
        differences.append(
            {
                "file": "report.json",
                "chunk_id": None,
                "pixel": None,
                "channels": ["report"],
                "reference_sha256": hashlib.sha256(
                    reference_report_path.read_bytes()
                ).hexdigest(),
                "candidate_sha256": hashlib.sha256(
                    candidate_report_path.read_bytes()
                ).hexdigest(),
            }
        )

    result = {
        "ok": mismatch_count == 0,
        "code": "RW_E_RESUME_MISMATCH" if mismatch_count else "RW_OK",
        "mismatch_count": mismatch_count,
        "differences": differences,
        "compared": [
            "rgb.npy",
            "ownership.npy",
            "shadow_projections.json",
            "ownership_map.json",
            "report.json",
        ],
    }
    if output_path is not None:
        atomic_write_json(output_path, result)
    return result
