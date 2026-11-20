"""Immutable published irradiance snapshots and pinned runtime queries."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .errors import ErrorCode, ProbeError
from .grid import ProbeGrid
from .interpolation import trilinear_query
from .sh import irradiance_for_normals


def _immutable(array: np.ndarray) -> np.ndarray:
    result = np.array(array, copy=True, order="C")
    result.setflags(write=False)
    return result


@dataclass(frozen=True)
class SnapshotQuery:
    version: int
    content_hash: str
    coefficients: np.ndarray
    irradiance: np.ndarray
    probe_ids: np.ndarray
    weights: np.ndarray

    @property
    def heat(self) -> float:
        return float(np.mean(self.irradiance))

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "coefficients": self.coefficients.tolist(),
            "irradiance_rgb": self.irradiance.tolist(),
            "heat": self.heat,
            "probe_ids": self.probe_ids.tolist(),
            "weights": self.weights.tolist(),
        }


@dataclass(frozen=True)
class IrradianceSnapshot:
    version: int
    content_hash: str
    scene_id: str
    grid: ProbeGrid
    coefficients: np.ndarray
    directions: np.ndarray
    sh_order: int
    strategy: str
    visibility_mean: np.ndarray
    sampled_mask: np.ndarray
    factor_versions: tuple[str, ...]
    probe_hashes: tuple[str, ...]

    @classmethod
    def from_state(cls, state: Any, version: int) -> "IrradianceSnapshot":
        return cls(
            version=int(version),
            content_hash=state.artifacts.content_hash,
            scene_id=state.scene.scene_id,
            grid=state.grid,
            coefficients=_immutable(state.artifacts.coefficients),
            directions=_immutable(state.config.resolved_directions()),
            sh_order=int(state.config.sh_order),
            strategy=state.decision.strategy,
            visibility_mean=_immutable(state.artifacts.visibility_mean),
            sampled_mask=_immutable(state.artifacts.sampled_mask),
            factor_versions=tuple(state.artifacts.factor_versions),
            probe_hashes=tuple(state.artifacts.probe_hashes),
        )

    def validate_integrity(self) -> None:
        if self.version <= 0:
            raise ProbeError(ErrorCode.VERSION_STALE, "published snapshot version must be positive")
        if self.coefficients.shape[0] != self.grid.count:
            raise ProbeError(ErrorCode.VERSION_STALE, "snapshot probe count and grid disagree")
        if len(self.factor_versions) != self.grid.count or len(self.probe_hashes) != self.grid.count:
            raise ProbeError(ErrorCode.VERSION_STALE, "snapshot metadata has the wrong probe count")
        if not np.isfinite(self.coefficients).all():
            raise ProbeError(ErrorCode.VERSION_STALE, "snapshot contains non-finite coefficients")

    def _pin(
        self, version: int | None, expected_hash: str | None
    ) -> None:
        if version is not None and int(version) != self.version:
            raise ProbeError(
                ErrorCode.VERSION_STALE,
                "requested snapshot version is no longer current",
                details={"requested": int(version), "current": self.version},
            )
        if expected_hash is not None and expected_hash != self.content_hash:
            raise ProbeError(
                ErrorCode.VERSION_STALE,
                "snapshot content hash does not match",
                details={"expected": expected_hash, "actual": self.content_hash},
            )

    def query_at(
        self,
        position: np.ndarray | tuple[float, float, float],
        normal: np.ndarray | tuple[float, float, float] = (0.0, 1.0, 0.0),
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> SnapshotQuery:
        self._pin(version, expected_hash)
        interpolation = trilinear_query(self.grid, self.coefficients, position)
        irradiance = irradiance_for_normals(
            interpolation.coefficients[None, :, :],
            self.directions,
            np.asarray([normal], dtype=np.float64),
        )[0, 0]
        return SnapshotQuery(
            version=self.version,
            content_hash=self.content_hash,
            coefficients=interpolation.coefficients,
            irradiance=np.asarray(irradiance),
            probe_ids=interpolation.probe_ids,
            weights=interpolation.weights,
        )

    def query_probe(
        self,
        probe_id: int,
        normal: np.ndarray | tuple[float, float, float] = (0.0, 1.0, 0.0),
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> SnapshotQuery:
        self._pin(version, expected_hash)
        index = int(probe_id)
        if index < 0 or index >= self.grid.count:
            raise ProbeError(
                ErrorCode.INTERP_FAILED,
                "probe id is outside the published grid",
                probe_id=index,
            )
        coefficient = self.coefficients[index]
        irradiance = irradiance_for_normals(
            coefficient[None, :, :],
            self.directions,
            np.asarray([normal], dtype=np.float64),
        )[0, 0]
        weights = np.zeros(1, dtype=np.float64)
        weights[0] = 1.0
        ids = np.asarray([index], dtype=np.int64)
        ids.setflags(write=False)
        weights.setflags(write=False)
        return SnapshotQuery(
            version=self.version,
            content_hash=self.content_hash,
            coefficients=coefficient.copy(),
            irradiance=np.asarray(irradiance),
            probe_ids=ids,
            weights=weights,
        )

    def query_probes(
        self,
        probe_ids: Iterable[int] | None = None,
        normal: np.ndarray | tuple[float, float, float] = (0.0, 1.0, 0.0),
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> dict[str, Any]:
        self._pin(version, expected_hash)
        ids = (
            np.arange(self.grid.count, dtype=np.int64)
            if probe_ids is None
            else np.asarray(list(probe_ids), dtype=np.int64)
        )
        if ids.ndim != 1 or np.any(ids < 0) or np.any(ids >= self.grid.count):
            bad = int(ids[(ids < 0) | (ids >= self.grid.count)][0])
            raise ProbeError(
                ErrorCode.INTERP_FAILED,
                "query probe id is outside the published grid",
                probe_id=bad,
            )
        irradiance = irradiance_for_normals(
            self.coefficients[ids],
            self.directions,
            np.asarray([normal], dtype=np.float64),
        )[:, 0, :]
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "probe_ids": ids.tolist(),
            "irradiance_rgb": irradiance,
            "heat": np.mean(irradiance, axis=1),
        }


class PublishedStore:
    """A thread-safe, atomically swapped set of immutable snapshots."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._current: IrradianceSnapshot | None = None
        self._history: list[IrradianceSnapshot] = []

    @property
    def next_version(self) -> int:
        with self._lock:
            return 1 if self._current is None else self._current.version + 1

    def publish(
        self,
        state: Any,
        *,
        version: int | None = None,
        expected_content_hash: str | None = None,
    ) -> IrradianceSnapshot:
        if expected_content_hash is not None and expected_content_hash != state.content_hash:
            raise ProbeError(
                ErrorCode.VERSION_STALE,
                "bake state content hash does not match the expected hash",
                details={"expected": expected_content_hash, "actual": state.content_hash},
            )
        with self._lock:
            target_version = self.next_version if version is None else int(version)
            if target_version <= 0:
                raise ProbeError(ErrorCode.VERSION_STALE, "version must be positive")
            if self._current is not None and target_version <= self._current.version:
                raise ProbeError(
                    ErrorCode.VERSION_STALE,
                    "publish version must increase monotonically",
                    details={"requested": target_version, "current": self._current.version},
                )
            snapshot = IrradianceSnapshot.from_state(state, target_version)
            snapshot.validate_integrity()
            self._current = snapshot
            self._history.append(snapshot)
            return snapshot

    def pin(
        self,
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> IrradianceSnapshot:
        with self._lock:
            snapshot = self._current
            if snapshot is None:
                raise ProbeError(ErrorCode.VERSION_STALE, "no snapshot has been published")
            if version is not None and int(version) != snapshot.version:
                raise ProbeError(
                    ErrorCode.VERSION_STALE,
                    "requested version is not current",
                    details={"requested": int(version), "current": snapshot.version},
                )
            if expected_hash is not None and expected_hash != snapshot.content_hash:
                raise ProbeError(
                    ErrorCode.VERSION_STALE,
                    "expected content hash does not match the current snapshot",
                    details={
                        "expected": expected_hash,
                        "current": snapshot.content_hash,
                    },
                )
            return snapshot

    def query_at(
        self,
        position: np.ndarray | tuple[float, float, float],
        normal: np.ndarray | tuple[float, float, float] = (0.0, 1.0, 0.0),
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> SnapshotQuery:
        snapshot = self.pin(version=version, expected_hash=expected_hash)
        return snapshot.query_at(position, normal)

    def query_probe(
        self,
        probe_id: int,
        normal: np.ndarray | tuple[float, float, float] = (0.0, 1.0, 0.0),
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> SnapshotQuery:
        snapshot = self.pin(version=version, expected_hash=expected_hash)
        return snapshot.query_probe(probe_id, normal)

    def query_probes(
        self,
        probe_ids: Iterable[int] | None = None,
        normal: np.ndarray | tuple[float, float, float] = (0.0, 1.0, 0.0),
        *,
        version: int | None = None,
        expected_hash: str | None = None,
    ) -> dict[str, Any]:
        snapshot = self.pin(version=version, expected_hash=expected_hash)
        return snapshot.query_probes(probe_ids, normal)

    def history_versions(self) -> tuple[int, ...]:
        with self._lock:
            return tuple(snapshot.version for snapshot in self._history)

    def snapshot_for_version(self, version: int) -> IrradianceSnapshot:
        with self._lock:
            for snapshot in reversed(self._history):
                if snapshot.version == int(version):
                    return snapshot
        raise ProbeError(
            ErrorCode.VERSION_STALE,
            "snapshot version is unavailable",
            details={"version": int(version)},
        )

