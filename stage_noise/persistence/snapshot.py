"""Chunked rendering with durable, content-addressed progress snapshots."""

from __future__ import annotations

import hashlib
import json
import os
import shutil

import numpy as np

from ..errors import BusinessCode, StageNoiseError

SNAPSHOT_NAME = "snapshot.json"


def canonical_digest(obj) -> str:
    payload = json.dumps(obj, sort_keys=True, separators=(",", ":"),
                         ensure_ascii=False)
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


class SnapshotStore:
    def __init__(self, state_dir: str):
        self.state_dir = state_dir
        self.parts_dir = os.path.join(state_dir, "parts")
        self.path = os.path.join(state_dir, SNAPSHOT_NAME)

    def reset(self) -> None:
        if os.path.isdir(self.state_dir):
            shutil.rmtree(self.state_dir)

    def ensure_dirs(self) -> None:
        os.makedirs(self.parts_dir, exist_ok=True)

    def write_part(self, chunk_index: int, stereo: np.ndarray) -> str:
        self.ensure_dirs()
        part_path = os.path.join(self.parts_dir,
                                 f"chunk_{chunk_index:06d}.npy")
        tmp_path = os.path.join(self.parts_dir,
                                f"chunk_{chunk_index:06d}.tmp")
        with open(tmp_path, "wb") as handle:
            np.save(handle, stereo)
        os.replace(tmp_path, part_path)
        with open(part_path, "rb") as handle:
            return hashlib.sha256(handle.read()).hexdigest()

    def read_part(self, chunk_index: int) -> np.ndarray:
        part_path = os.path.join(self.parts_dir, f"chunk_{chunk_index:06d}.npy")
        return np.load(part_path)

    def part_hash(self, chunk_index: int) -> str:
        part_path = os.path.join(self.parts_dir, f"chunk_{chunk_index:06d}.npy")
        with open(part_path, "rb") as handle:
            return hashlib.sha256(handle.read()).hexdigest()

    def commit(self, payload: dict) -> None:
        tmp_path = self.path + ".tmp"
        with open(tmp_path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, sort_keys=False, indent=2)
            handle.write("\n")
        os.replace(tmp_path, self.path)

    def load(self) -> dict | None:
        if not os.path.isfile(self.path):
            return None
        try:
            with open(self.path, "r", encoding="utf-8") as handle:
                payload = json.load(handle)
        except (json.JSONDecodeError, OSError):
            return None
        if not isinstance(payload, dict):
            return None
        return payload

    def exists(self) -> bool:
        return os.path.isfile(self.path)

    def verify_parts(self, expected: list) -> None:
        for index, expected_hash in enumerate(expected):
            part_path = os.path.join(self.parts_dir,
                                     f"chunk_{index:06d}.npy")
            if not os.path.isfile(part_path):
                raise StageNoiseError(
                    BusinessCode.SNAPSHOT_CORRUPT,
                    f"missing rendered part for chunk {index}",
                    details={"chunk_index": index})
            if self.part_hash(index) != expected_hash:
                raise StageNoiseError(
                    BusinessCode.SNAPSHOT_CORRUPT,
                    f"part hash mismatch for chunk {index}",
                    details={"chunk_index": index})

    def cleanup(self) -> None:
        if os.path.isdir(self.state_dir):
            shutil.rmtree(self.state_dir)
