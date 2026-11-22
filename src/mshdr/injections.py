"""Real local input/parameter anomaly injection helpers."""

from __future__ import annotations

import shutil
from pathlib import Path
from typing import Any

from .dataset import DatasetIndex
from .io_utils import read_json, value_version, write_json


ANOMALY_KINDS = ("missing_band", "exposure_order", "bit_depth_change")


def create_injected_dataset(
    source_dataset: Path,
    destination: Path,
    *,
    kind: str,
    scene_id: str | None = None,
    band_index: int = 0,
) -> dict[str, Any]:
    """Copy a generated dataset and mutate one real input file or metadata field."""

    if kind not in ANOMALY_KINDS:
        raise ValueError(f"Unsupported anomaly kind: {kind}")
    source_dataset = Path(source_dataset)
    destination = Path(destination)
    if destination.exists():
        shutil.rmtree(destination)
    shutil.copytree(source_dataset.parent, destination)
    injected_dataset_path = destination / source_dataset.name
    index = DatasetIndex.load(injected_dataset_path)
    record = index.record(scene_id) if scene_id is not None else index.scene_records[0]
    selected_scene_id = str(record["scene_id"])
    scene_root = destination / str(record["directory"])
    metadata_path = scene_root / "metadata.json"
    truth_path = scene_root / "truth.json"
    metadata = read_json(metadata_path, field="metadata")
    truth = read_json(truth_path, field="truth")

    if kind == "missing_band":
        if not (0 <= band_index < len(metadata["bands"])):
            raise IndexError("band_index is out of range")
        band_file = scene_root / str(metadata["bands"][band_index]["file"])
        band_file.unlink()
        description = f"removed band payload file for band {band_index}"
    elif kind == "exposure_order":
        times = [float(frame["exposure_time"]) for frame in metadata["exposures"]["frames"]]
        reversed_times = list(reversed(times))
        for frame, exposure_time in zip(metadata["exposures"]["frames"], reversed_times, strict=True):
            frame["exposure_time"] = exposure_time
        metadata.pop("metadata_version", None)
        new_version = value_version(metadata)
        metadata["metadata_version"] = new_version
        truth["metadata_version"] = new_version
        record["metadata_version"] = new_version
        write_json(metadata_path, metadata)
        write_json(truth_path, truth)
        write_json(injected_dataset_path, index.data)
        description = "reversed exposure times while keeping frame order and files"
    else:
        if not (0 <= band_index < len(metadata["bands"])):
            raise IndexError("band_index is out of range")
        original = int(metadata["bands"][band_index]["bit_depth"])
        metadata["bands"][band_index]["bit_depth"] = original + 1
        write_json(metadata_path, metadata)
        description = f"changed band {band_index} bit depth from {original} to {original + 1} without version bump"

    return {
        "dataset": str(injected_dataset_path),
        "scene_id": selected_scene_id,
        "kind": kind,
        "description": description,
    }

