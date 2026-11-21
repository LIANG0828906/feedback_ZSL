"""Content-addressed caches for markers, delays, fits and compensated segments."""

from __future__ import annotations

import hashlib
import json
import threading
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np

from .models import (
    DriftModel,
    MarkerChannelResult,
    MarkerHit,
    SegmentDelay,
    SyncConfig,
    canonical_json,
    dataclass_to_dict,
)
from .wavio import write_json_atomic, write_npz_atomic


@dataclass
class CacheStats:
    marker_hits: int = 0
    marker_misses: int = 0
    delay_hits: int = 0
    delay_misses: int = 0
    fit_hits: int = 0
    fit_misses: int = 0
    segment_hits: int = 0
    segment_misses: int = 0
    bytes_reused: int = 0

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)


def _key(*values: object) -> str:
    return hashlib.sha256(canonical_json(list(values)).encode("utf-8")).hexdigest()


def _marker_result_from_dict(payload: dict) -> MarkerChannelResult:
    markers = tuple(MarkerHit(**marker) for marker in payload["markers"])
    return MarkerChannelResult(
        channel=int(payload["channel"]),
        markers=markers,
        missing_indices=tuple(int(value) for value in payload["missing_indices"]),
        peak_to_noise=float(payload["peak_to_noise"]),
    )


def _segment_from_dict(payload: dict) -> SegmentDelay:
    keys = {
        "segment_index",
        "start_sample",
        "end_sample",
        "center_sample",
        "delay_samples",
        "peak_correlation",
        "ci_low_samples",
        "ci_high_samples",
        "valid",
        "method",
    }
    return SegmentDelay(**{key: payload[key] for key in keys})


def _drift_from_dict(payload: dict) -> DriftModel:
    copied = dict(payload)
    copied["evidence_segments"] = tuple(copied.get("evidence_segments", ()))
    copied["candidates"] = tuple(copied.get("candidates", ()))
    for key in ("offset_ci", "drift_ppm_ci", "step_ci"):
        if copied.get(key) is not None:
            copied[key] = tuple(copied[key])
    return DriftModel(**copied)


class CacheBackend:
    """Thread-safe file cache. Every entry is immutable and atomically renamed."""

    def __init__(self, root: str | Path):
        self.root = Path(root).expanduser().resolve()
        self.marker_dir = self.root / "markers"
        self.delay_dir = self.root / "delays"
        self.fit_dir = self.root / "fits"
        self.segment_dir = self.root / "segments"
        for directory in (self.marker_dir, self.delay_dir, self.fit_dir, self.segment_dir):
            directory.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()

    def marker_key(
        self,
        source_sha: str,
        channel: int,
        marker_signature: str,
        min_snr: float,
    ) -> str:
        return _key(
            "markers-v2",
            source_sha,
            int(channel),
            marker_signature,
            float(min_snr),
        )

    def delay_key(
        self,
        source_sha: str,
        channel: int,
        reference_channel: int,
        strategy: str,
        start: int,
        stop: int,
        max_lag_seconds: float,
        min_correlation: float,
        method: str,
    ) -> str:
        return _key(
            "delay-v1",
            source_sha,
            int(channel),
            int(reference_channel),
            strategy,
            int(start),
            int(stop),
            float(max_lag_seconds),
            float(min_correlation),
            method,
        )

    def fit_key(
        self,
        delays: tuple[SegmentDelay, ...],
        sample_rate: int,
        config: SyncConfig,
    ) -> str:
        fit_parameters = {
            "linear_min_abs_rate_ppm": config.linear_min_abs_rate_ppm,
            "step_min_samples": config.step_min_samples,
            "step_bic_gain": config.step_bic_gain,
            "fit_min_points": config.fit_min_points,
            "tolerance_samples": config.tolerance_samples,
            "reference_channel": config.reference_channel,
        }
        delay_payload = [item.to_dict() for item in delays]
        return _key("fit-v1", delay_payload, int(sample_rate), fit_parameters)

    def segment_key(
        self,
        source_sha: str,
        channel: int,
        start: int,
        stop: int,
        model: DriftModel,
        interpolation: str,
        sinc_half_taps: int,
    ) -> str:
        return _key(
            "segment-v2",
            source_sha,
            int(channel),
            int(start),
            int(stop),
            model.signature(),
            interpolation,
            int(sinc_half_taps),
        )

    def get_markers(self, key: str, stats: CacheStats) -> MarkerChannelResult | None:
        path = self.marker_dir / f"{key}.json"
        with self._lock:
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))
                result = _marker_result_from_dict(payload)
            except (OSError, ValueError, KeyError, TypeError):
                stats.marker_misses += 1
                return None
            stats.marker_hits += 1
            return result

    def put_markers(self, key: str, result: MarkerChannelResult) -> None:
        write_json_atomic(self.marker_dir / f"{key}.json", result.to_dict())

    def get_delays(self, key: str, stats: CacheStats) -> tuple[SegmentDelay, ...] | None:
        path = self.delay_dir / f"{key}.json"
        with self._lock:
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))
                result = tuple(_segment_from_dict(item) for item in payload["segments"])
            except (OSError, ValueError, KeyError, TypeError):
                stats.delay_misses += 1
                return None
            stats.delay_hits += 1
            return result

    def put_delays(self, key: str, delays: tuple[SegmentDelay, ...]) -> None:
        write_json_atomic(
            self.delay_dir / f"{key}.json",
            {"segments": [item.to_dict() for item in delays]},
        )

    def get_fit(self, key: str, stats: CacheStats) -> DriftModel | None:
        path = self.fit_dir / f"{key}.json"
        with self._lock:
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))
                result = _drift_from_dict(payload)
            except (OSError, ValueError, KeyError, TypeError):
                stats.fit_misses += 1
                return None
            stats.fit_hits += 1
            return result

    def put_fit(self, key: str, model: DriftModel) -> None:
        write_json_atomic(self.fit_dir / f"{key}.json", model.to_dict())

    def get_segment(
        self,
        key: str,
        stats: CacheStats,
    ) -> tuple[np.ndarray, str] | None:
        path = self.segment_dir / f"{key}.npz"
        with self._lock:
            try:
                with np.load(path, allow_pickle=False) as archive:
                    data = np.asarray(archive["data"], dtype=np.int16)
                    stored_hash = str(archive["hash"].item())
            except (OSError, ValueError, KeyError, TypeError):
                stats.segment_misses += 1
                return None
            actual_hash = hashlib.sha256(data.tobytes(order="C")).hexdigest()
            if actual_hash != stored_hash:
                stats.segment_misses += 1
                return None
            stats.segment_hits += 1
            stats.bytes_reused += data.nbytes
            return data, stored_hash

    def put_segment(self, key: str, data: np.ndarray) -> str:
        data = np.ascontiguousarray(data, dtype=np.int16)
        digest = hashlib.sha256(data.tobytes(order="C")).hexdigest()
        write_npz_atomic(
            self.segment_dir / f"{key}.npz",
            data=data,
            hash=np.asarray(digest),
        )
        return digest

    def discard_segment(self, key: str) -> None:
        with self._lock:
            (self.segment_dir / f"{key}.npz").unlink(missing_ok=True)
