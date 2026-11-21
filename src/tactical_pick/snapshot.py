"""Immutable, atomically published scene snapshots and leased handles."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any

from .bvh import BVH
from .errors import ToolError, fail
from .scene import SceneData


@dataclass(frozen=True)
class SceneSnapshot:
    scene: SceneData
    unit_bvh: BVH
    cover_bvh: BVH
    content_hash: str

    @classmethod
    def from_scene(cls, scene: SceneData) -> "SceneSnapshot":
        unit_bounds = [unit.bounds() for unit in scene.units]
        cover_bounds = [triangle.bounds() for triangle in scene.cover_triangles]
        return cls(
            scene=scene,
            unit_bvh=BVH.build(unit_bounds),
            cover_bvh=BVH.build(cover_bounds),
            content_hash=scene.compute_hash(),
        )

    @property
    def version(self) -> int:
        return self.scene.version

    def verify_seal(self) -> bool:
        return self.scene.version == self.version and self.scene.compute_hash() == self.content_hash


class SnapshotHandle:
    """A lease that remains valid until explicitly released."""

    __slots__ = ("_snapshot", "_expected_version", "_expected_hash", "_released")

    def __init__(self, snapshot: SceneSnapshot) -> None:
        self._snapshot = snapshot
        self._expected_version = snapshot.version
        self._expected_hash = snapshot.content_hash
        self._released = False

    @property
    def released(self) -> bool:
        return self._released

    @property
    def version(self) -> int:
        return self._expected_version

    @property
    def content_hash(self) -> str:
        return self._expected_hash

    def snapshot(self) -> SceneSnapshot:
        if self._released:
            fail(
                "E_STALE_HANDLE",
                "snapshot handle has already been released",
                "snapshot.handle.released",
                first_conflict={"version": self._expected_version},
            )
        snapshot = self._snapshot
        if snapshot is None:
            fail("E_STALE_HANDLE", "snapshot handle was discarded", "snapshot.handle")
        if snapshot.version != self._expected_version or snapshot.content_hash != self._expected_hash:
            fail(
                "E_SNAPSHOT_CORRUPT",
                "snapshot seal does not match the leased version",
                "snapshot.content_hash",
                first_conflict={
                    "expected_version": self._expected_version,
                    "actual_version": snapshot.version,
                    "expected_hash": self._expected_hash,
                    "actual_hash": snapshot.content_hash,
                },
            )
        return snapshot

    def release(self) -> None:
        self._released = True
        self._snapshot = None  # type: ignore[assignment]

    def __enter__(self) -> "SnapshotHandle":
        self.snapshot()
        return self

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        self.release()


class SceneManager:
    """Publishes whole immutable snapshots without holding a reader lock."""

    def __init__(self, scene: SceneData, *, verify_on_acquire: bool = False) -> None:
        snapshot = SceneSnapshot.from_scene(scene)
        self._current = snapshot
        self._archive: dict[int, SceneSnapshot] = {snapshot.version: snapshot}
        self._publish_lock = threading.Lock()
        self._edit_lock = threading.Lock()
        self._verify_on_acquire = verify_on_acquire
        self._publish_count = 0

    @property
    def edit_lock(self) -> threading.Lock:
        return self._edit_lock

    @property
    def current(self) -> SceneSnapshot:
        # A single reference load is atomic under CPython and always sees a complete snapshot.
        return self._current

    def current_metadata(self) -> dict[str, Any]:
        snapshot = self._current
        return {
            "version": snapshot.version,
            "content_hash": snapshot.content_hash,
        }

    def acquire(self, requested_version: int | None = None) -> SnapshotHandle:
        snapshot = self._current
        if requested_version is not None and snapshot.version != requested_version:
            fail(
                "E_VERSION_STALE",
                "requested version is not the currently published version",
                "snapshot.requested_version",
                first_conflict={
                    "requested_version": requested_version,
                    "current_version": snapshot.version,
                    "current_hash": snapshot.content_hash,
                },
            )
        if self._verify_on_acquire and not snapshot.verify_seal():
            fail(
                "E_SNAPSHOT_CORRUPT",
                "published snapshot failed its content seal",
                "snapshot.content_hash",
                first_conflict={"version": snapshot.version, "content_hash": snapshot.content_hash},
            )
        handle = SnapshotHandle(snapshot)

        # A requested-version query may race with publication. It must not silently
        # bind to a version that ceased to be current while the handle was acquired.
        if requested_version is not None and self._current is not snapshot:
            handle.release()
            fail(
                "E_VERSION_STALE",
                "a newer version was published while the query was being bound",
                "snapshot.requested_version",
                first_conflict={
                    "requested_version": requested_version,
                    "current_version": self._current.version,
                    "current_hash": self._current.content_hash,
                },
            )
        return handle

    def acquire_archived(self, version: int) -> SnapshotHandle:
        snapshot = self._archive.get(version)
        if snapshot is None:
            fail(
                "E_VERSION_STALE",
                "requested version is not in the local archive",
                "snapshot.archive",
                first_conflict={"requested_version": version, "archived": sorted(self._archive)},
            )
        if self._verify_on_acquire and not snapshot.verify_seal():
            fail(
                "E_SNAPSHOT_CORRUPT",
                "archived snapshot failed its content seal",
                "snapshot.content_hash",
                first_conflict={"version": version, "content_hash": snapshot.content_hash},
            )
        return SnapshotHandle(snapshot)

    def publish(self, snapshot: SceneSnapshot) -> None:
        with self._publish_lock:
            current_version = self._current.version
            if snapshot.version <= current_version:
                fail(
                    "E_VERSION_STALE",
                    "published versions must be strictly increasing",
                    "snapshot.publish.version",
                    first_conflict={"new_version": snapshot.version, "current_version": current_version},
                )
            if snapshot.version in self._archive:
                fail(
                    "E_VERSION_STALE",
                    "version already exists in archive",
                    "snapshot.publish.version",
                    first_conflict={"version": snapshot.version},
                )
            self._archive[snapshot.version] = snapshot
            self._current = snapshot
            self._publish_count += 1

    def publish_scene(self, scene: SceneData) -> SceneSnapshot:
        snapshot = SceneSnapshot.from_scene(scene)
        self.publish(snapshot)
        return snapshot

    def archive_metadata(self) -> list[dict[str, Any]]:
        return [
            {"version": version, "content_hash": snap.content_hash}
            for version, snap in sorted(self._archive.items())
        ]

    def verify_all(self) -> bool:
        """Full archive integrity check for offline diagnostics."""
        return all(snapshot.verify_seal() for snapshot in self._archive.values())


def corrupt_snapshot_for_test(manager: SceneManager) -> ToolError:
    """Input-entry test hook; changes only the published seal, never query code."""
    snapshot = manager.current
    object.__setattr__(snapshot, "content_hash", "corrupt-" + "0" * 56)
    return ToolError(
        "E_SNAPSHOT_CORRUPT",
        "injected snapshot corruption",
        "snapshot.content_hash",
        {"version": snapshot.version},
    )

