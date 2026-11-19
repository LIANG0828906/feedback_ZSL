from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from pathlib import Path
from typing import Any, Iterable

import numpy as np


ROOT = Path(__file__).resolve().parents[1]


def load_json(path: str | os.PathLike[str]) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def resolve_path(value: str | os.PathLike[str], base: Path = ROOT) -> Path:
    path = Path(value)
    return path if path.is_absolute() else base / path


def load_config(config_path: str | os.PathLike[str] = "config.json") -> dict[str, Any]:
    config = load_json(resolve_path(config_path))
    config["_config_path"] = str(resolve_path(config_path))
    config["_root"] = str(ROOT)
    return config


def load_manifest_for_config(config: dict[str, Any]) -> dict[str, Any]:
    return load_json(resolve_path(config["model_manifest"]))


def atomic_write_json(path: str | os.PathLike[str], value: Any) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{destination.name}.", suffix=".tmp", dir=str(destination.parent)
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(value, handle, ensure_ascii=False, indent=2, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def parameter_names(manifest: dict[str, Any]) -> list[str]:
    model = manifest["model"]
    rows = int(model["feature_dim"])
    classes = int(model["num_classes"])
    layout = model["parameter_layout"]
    names: list[str] = []
    for row in range(rows):
        for class_index in range(classes):
            names.append(
                layout["weight"].format(row=row, class_index=class_index)
            )
    for class_index in range(classes):
        names.append(layout["bias"].format(class_index=class_index))
    return names


def parameter_shape(manifest: dict[str, Any]) -> tuple[int, int]:
    model = manifest["model"]
    return int(model["feature_dim"]), int(model["num_classes"])


def parameters_from_vector(
    vector: np.ndarray, manifest: dict[str, Any]
) -> dict[str, float]:
    names = parameter_names(manifest)
    flat = np.asarray(vector, dtype=np.float64).reshape(-1)
    if flat.size != len(names):
        raise ValueError(f"expected {len(names)} parameters, got {flat.size}")
    return {name: float(flat[index]) for index, name in enumerate(names)}


def vector_from_parameters(
    parameters: dict[str, float], manifest: dict[str, Any]
) -> np.ndarray:
    names = parameter_names(manifest)
    missing = [name for name in names if name not in parameters]
    if missing:
        raise KeyError(f"missing parameters: {missing[:3]}")
    return np.asarray([parameters[name] for name in names], dtype=np.float64)


def shard_for_parameter(name: str, shard_ids: Iterable[str]) -> str:
    ordered = sorted(shard_ids)
    if not ordered:
        raise ValueError("at least one parameter shard is required")
    digest = hashlib.sha256(name.encode("utf-8")).digest()
    slot = int.from_bytes(digest[:8], byteorder="big", signed=False) % len(ordered)
    return ordered[slot]


def expected_shard_assignments(
    manifest: dict[str, Any], shard_ids: Iterable[str]
) -> dict[str, list[str]]:
    ordered = sorted(shard_ids)
    result = {shard_id: [] for shard_id in ordered}
    for name in parameter_names(manifest):
        result[shard_for_parameter(name, ordered)].append(name)
    return result


def load_data_metadata(config: dict[str, Any]) -> dict[str, Any]:
    metadata_path = resolve_path(config["data"]["metadata"])
    return load_json(metadata_path)


def shard_data_paths(
    config: dict[str, Any], metadata: dict[str, Any], shard_index: int
) -> tuple[Path, Path]:
    data_dir = resolve_path(config["data"]["directory"])
    shard = metadata["shards"][shard_index]
    return data_dir / shard["features_file"], data_dir / shard["labels_file"]


def open_shard(
    config: dict[str, Any], metadata: dict[str, Any], shard_index: int
) -> tuple[np.memmap, np.memmap]:
    features_path, labels_path = shard_data_paths(config, metadata, shard_index)
    features = np.load(features_path, mmap_mode="r")
    labels = np.load(labels_path, mmap_mode="r")
    return features, labels


def step_batch_slice(
    step: int, samples_per_worker: int, shard_sample_count: int
) -> slice:
    start = (int(step) - 1) * int(samples_per_worker)
    stop = start + int(samples_per_worker)
    if start < 0 or stop > int(shard_sample_count):
        raise IndexError(
            f"step {step} batch [{start}:{stop}] exceeds shard size "
            f"{shard_sample_count}"
        )
    return slice(start, stop)


def relative_error(actual: float, expected: float) -> float:
    denominator = max(abs(expected), 1e-12)
    return abs(actual - expected) / denominator


def finite_or_none(value: float) -> float | None:
    return float(value) if math.isfinite(float(value)) else None
