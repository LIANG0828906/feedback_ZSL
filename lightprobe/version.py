from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from dataclasses import replace
from types import MappingProxyType
from typing import Any

import numpy as np

from .errors import interp_failed, version_stale
from .record import ProbeRecord
from .state import BakeState
from .sh import evaluate_irradiance


@dataclass(frozen=True)
class PublishedVersion:
    version: int
    content_hash: str
    state: BakeState
    published_at: float

    def summary(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "published_at": self.published_at,
            **self.state.to_summary(),
        }


class VersionedStore:
    """Copy-on-publish version store; readers never observe cross-version probes."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._current: PublishedVersion | None = None

    @property
    def current(self) -> PublishedVersion | None:
        with self._lock:
            return self._current

    def publish(self, state: BakeState, *, version: int | None = None) -> PublishedVersion:
        content_hash = state.content_hash()
        for probe_id, record in state.records.items():
            if not record.verify_content_hash():
                raise version_stale(
                    "refusing to publish a probe with an invalid content hash",
                    probe_id=probe_id,
                )
        frozen_state = _freeze_state(state)
        if frozen_state.content_hash() != content_hash:
            raise version_stale("state changed while it was being published")
        with self._lock:
            if version is None:
                version = 1 if self._current is None else self._current.version + 1
            if version < 1:
                raise version_stale("published version must be positive")
            if self._current is not None and version <= self._current.version:
                version = self._current.version + 1
            published = PublishedVersion(
                version=version,
                content_hash=content_hash,
                state=frozen_state,
                published_at=time.time(),
            )
            self._current = published
            return published

    def validate_expected(
        self,
        expected_version: int | None,
        expected_content_hash: str | None,
    ) -> PublishedVersion:
        with self._lock:
            current = self._current
        if current is None:
            raise version_stale("no irradiance version has been published")
        if expected_version is not None and expected_version != current.version:
            raise version_stale(
                "requested version is no longer current",
                expected_version=expected_version,
                current_version=current.version,
            )
        if (
            expected_content_hash is not None
            and expected_content_hash != current.content_hash
        ):
            raise version_stale(
                "requested content hash does not match the current version",
                expected_content_hash=expected_content_hash,
                current_content_hash=current.content_hash,
            )
        actual_hash = current.state.content_hash()
        if actual_hash != current.content_hash:
            raise version_stale(
                "published version number and content hash no longer agree",
                version=current.version,
                expected_content_hash=current.content_hash,
                actual_content_hash=actual_hash,
            )
        return current

    def query(
        self,
        position: np.ndarray | list[float] | tuple[float, float, float],
        normal: np.ndarray | list[float] | tuple[float, float, float] | None = None,
        *,
        expected_version: int | None = None,
        expected_content_hash: str | None = None,
    ) -> dict[str, Any]:
        published = self.validate_expected(expected_version, expected_content_hash)
        state = published.state
        position_array = np.asarray(position, dtype=np.float64)
        if position_array.shape != (3,):
            raise interp_failed("query position must have shape (3,)", [])
        normal_array = (
            np.asarray((0.0, 1.0, 0.0), dtype=np.float64)
            if normal is None
            else np.asarray(normal, dtype=np.float64)
        )
        if normal_array.shape != (3,) or np.linalg.norm(normal_array) <= 1.0e-12:
            raise interp_failed("query normal must be a non-zero 3D vector", [])
        normal_array = normal_array / np.linalg.norm(normal_array)

        base, fraction = state.grid.cell_for_position(position_array)
        fx, fy, fz = fraction
        weights = (
            (1.0 - fx) * (1.0 - fy) * (1.0 - fz),
            fx * (1.0 - fy) * (1.0 - fz),
            (1.0 - fx) * fy * (1.0 - fz),
            fx * fy * (1.0 - fz),
            (1.0 - fx) * (1.0 - fy) * fz,
            fx * (1.0 - fy) * fz,
            (1.0 - fx) * fy * fz,
            fx * fy * fz,
        )
        indices, _ = state.grid.trilinear_indices(base)
        probe_ids = [state.grid.probe_ids[index] for index in indices]
        missing = [probe_id for probe_id in probe_ids if probe_id not in state.records]
        if missing:
            raise interp_failed(
                "incomplete interpolation neighborhood in the published version", missing
            )
        coefficient_array = np.zeros(
            ((state.config.sh_order + 1) ** 2, 3), dtype=np.float64
        )
        for weight, probe_id in zip(weights, probe_ids):
            coefficient_array += weight * state.records[probe_id].coefficients
        irradiance = evaluate_irradiance(
            coefficient_array, normal_array[None, :], state.config.sh_order
        )[0]
        return {
            "ok": True,
            "version": published.version,
            "content_hash": published.content_hash,
            "position": [float(item) for item in position_array],
            "normal": [float(item) for item in normal_array],
            "weights": [float(item) for item in weights],
            "irradiance": [float(item) for item in irradiance],
            "probes": [
                {
                    "probe_id": probe_id,
                    "version": published.version,
                    "content_hash": state.records[probe_id].content_hash,
                    "weight": float(weight),
                }
                for probe_id, weight in zip(probe_ids, weights)
            ],
        }

    def read_all(
        self,
        *,
        expected_version: int | None = None,
        expected_content_hash: str | None = None,
    ) -> PublishedVersion:
        return self.validate_expected(expected_version, expected_content_hash)


def _freeze_record(record: ProbeRecord) -> ProbeRecord:
    raw = np.array(record.raw_radiance_coefficients, dtype=np.float64, copy=True)
    coefficients = np.array(record.coefficients, dtype=np.float64, copy=True)
    raw.setflags(write=False)
    coefficients.setflags(write=False)
    return replace(record, raw_radiance_coefficients=raw, coefficients=coefficients)


def _freeze_state(state: BakeState) -> BakeState:
    records = {
        probe_id: _freeze_record(record) for probe_id, record in state.records.items()
    }
    return BakeState(
        scene=state.scene,
        grid=state.grid,
        directions=state.directions,
        audit_directions=state.audit_directions,
        config=state.config,
        strategy=state.strategy,
        interpolation=state.interpolation,
        records=MappingProxyType(records),
        factor_versions=MappingProxyType(dict(state.factor_versions)),
    )

