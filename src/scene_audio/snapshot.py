from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import numpy as np


SNAPSHOT_SCHEMA_VERSION = 1


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(str(path), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    except OSError:
        pass
    finally:
        os.close(descriptor)


def save_snapshot(
    path: Path,
    metadata: dict[str, Any],
    arrays: dict[str, np.ndarray],
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        with temporary.open("wb") as handle:
            np.savez(
                handle,
                metadata=np.asarray(
                    json.dumps(
                        metadata,
                        sort_keys=True,
                        separators=(",", ":"),
                        ensure_ascii=True,
                    )
                ),
                **arrays,
            )
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    finally:
        if temporary.exists():
            try:
                temporary.unlink()
            except OSError:
                pass


def load_snapshot(path: Path) -> tuple[dict[str, Any], dict[str, np.ndarray]]:
    with np.load(path, allow_pickle=False) as data:
        if "metadata" not in data.files:
            raise ValueError("snapshot metadata is missing")
        metadata = json.loads(str(data["metadata"].item()))
        if not isinstance(metadata, dict):
            raise ValueError("snapshot metadata must be an object")
        arrays = {
            name: np.asarray(data[name]).copy()
            for name in data.files
            if name != "metadata"
        }
    return metadata, arrays


def discard_snapshot(path: Path) -> None:
    try:
        path.unlink()
    except FileNotFoundError:
        pass

