from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any

import numpy as np


@dataclass(frozen=True)
class MeshSnapshot:
    version: int
    vertices: np.ndarray
    faces: np.ndarray
    face_block_ids: np.ndarray
    metadata: dict[str, Any]

    def __post_init__(self) -> None:
        object.__setattr__(self, "vertices", _readonly_copy(self.vertices, np.float64))
        object.__setattr__(self, "faces", _readonly_copy(self.faces, np.int64))
        object.__setattr__(
            self, "face_block_ids", _readonly_copy(self.face_block_ids, np.int64)
        )
        object.__setattr__(self, "metadata", dict(self.metadata))

    @property
    def face_count(self) -> int:
        return int(self.faces.shape[0])

    @property
    def vertex_count(self) -> int:
        return int(self.vertices.shape[0])


def _readonly_copy(array: np.ndarray, dtype: Any) -> np.ndarray:
    result = np.array(array, dtype=dtype, copy=True)
    result.flags.writeable = False
    return result


class SnapshotStore:
    """A single-reference latest snapshot store.

    Readers take one immutable object reference. Writers construct the complete
    replacement first and then swap the pointer, so no partial mesh is visible.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._snapshot: MeshSnapshot | None = None
        self._next_version = 1
        self.publish_count = 0

    def publish(
        self,
        vertices: np.ndarray,
        faces: np.ndarray,
        *,
        face_block_ids: np.ndarray | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> MeshSnapshot:
        faces_array = np.asarray(faces, dtype=np.int64)
        if face_block_ids is None:
            block_ids = np.zeros(faces_array.shape[0], dtype=np.int64)
        else:
            block_ids = np.asarray(face_block_ids, dtype=np.int64)
        if block_ids.shape[0] != faces_array.shape[0]:
            raise ValueError("face_block_ids length must match face count")
        snapshot = MeshSnapshot(
            version=self._next_version,
            vertices=vertices,
            faces=faces_array,
            face_block_ids=block_ids,
            metadata=metadata or {},
        )
        with self._lock:
            # A constructed snapshot is immutable, so assignment is the commit.
            self._snapshot = snapshot
            self._next_version += 1
            self.publish_count += 1
        return snapshot

    def latest(self) -> MeshSnapshot | None:
        with self._lock:
            return self._snapshot

    def wait_for_version(
        self, minimum_version: int, timeout: float = 10.0
    ) -> MeshSnapshot | None:
        import time

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            snapshot = self.latest()
            if snapshot is not None and snapshot.version >= minimum_version:
                return snapshot
            time.sleep(0.01)
        return self.latest()
