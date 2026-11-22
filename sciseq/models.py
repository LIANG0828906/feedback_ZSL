"""Domain data structures shared by loaders, processors, and the GUI."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SciSeqError


def array_sha256(array: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(array)
    digest = hashlib.sha256()
    digest.update(str(contiguous.dtype).encode("ascii"))
    digest.update(json.dumps(list(contiguous.shape)).encode("ascii"))
    digest.update(contiguous.tobytes(order="C"))
    return digest.hexdigest()


@dataclass(frozen=True)
class FeatureSpec:
    feature_id: str
    kind: str
    center: tuple[float, float]
    sigma: float
    amplitude: float
    endpoints: tuple[tuple[float, float], tuple[float, float]] | None = None


@dataclass(frozen=True)
class SequenceTruth:
    reference_frame: int
    shifts: np.ndarray
    noise_sigmas: np.ndarray
    features: tuple[FeatureSpec, ...]
    frame_features: tuple[dict[str, tuple[float, float]], ...]

    def shift_at(self, frame_index: int) -> tuple[float, float]:
        shift = self.shifts[frame_index]
        return float(shift[0]), float(shift[1])


@dataclass(frozen=True)
class NoiseEstimate:
    sigma: float
    block_size: int
    block_sigmas: np.ndarray
    background: float


@dataclass(frozen=True)
class ExtractedFeature:
    frame: int
    feature_id: str
    kind: str
    center: tuple[float, float]
    area: int
    peak: float
    bbox: tuple[int, int, int, int]
    orientation_deg: float
    elongation: float
    mean_intensity: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame": self.frame,
            "feature_id": self.feature_id,
            "kind": self.kind,
            "center": [float(self.center[0]), float(self.center[1])],
            "area": int(self.area),
            "peak": float(self.peak),
            "bbox": [int(v) for v in self.bbox],
            "orientation_deg": float(self.orientation_deg),
            "elongation": float(self.elongation),
            "mean_intensity": float(self.mean_intensity),
        }


@dataclass
class FrameResult:
    index: int
    offset: np.ndarray
    denoised: np.ndarray
    noise: NoiseEstimate
    background: float
    features: tuple[ExtractedFeature, ...]
    stats: dict[str, Any]
    content_sha256: str
    denoised_sha256: str


@dataclass
class SequenceResult:
    sequence_id: str
    shape: tuple[int, int]
    frame_count: int
    offsets: np.ndarray
    denoised: np.ndarray
    noise_sigmas: np.ndarray
    backgrounds: np.ndarray
    features: tuple[tuple[ExtractedFeature, ...], ...]
    stats: dict[str, Any]
    mode: str
    downsample_factor: int
    denoised_mask: np.ndarray
    decision: dict[str, Any]
    parameter_version: str
    algorithm_version: str
    preview: bool = False

    def summarize(self) -> dict[str, Any]:
        return {
            "sequence_id": self.sequence_id,
            "shape": [int(self.shape[0]), int(self.shape[1])],
            "frame_count": int(self.frame_count),
            "mode": self.mode,
            "downsample_factor": int(self.downsample_factor),
            "preview": bool(self.preview),
            "parameter_version": self.parameter_version,
            "algorithm_version": self.algorithm_version,
            "decision": self.decision,
            "stats": self.stats,
            "offsets": self.offsets.astype(float).tolist(),
            "noise_sigmas": self.noise_sigmas.astype(float).tolist(),
            "feature_counts": [len(items) for items in self.features],
        }


@dataclass(frozen=True)
class Sequence:
    sequence_id: str
    root: Path
    shape: tuple[int, int]
    frame_count: int
    dtype: str
    reference_frame: int
    frame_paths: tuple[Path, ...]
    frame_hashes: tuple[str, ...]
    truth: SequenceTruth
    clean_base_path: Path
    clean_base_hash: str
    manifest: dict[str, Any] = field(repr=False)

    def frame(self, index: int) -> np.ndarray:
        if not 0 <= index < self.frame_count:
            raise SciSeqError(
                "E_META",
                "frame index outside sequence",
                frame=index,
                field="frame_index",
                details={"frame_count": self.frame_count},
            )
        try:
            image = np.load(self.frame_paths[index], allow_pickle=False)
        except (OSError, ValueError) as exc:
            raise SciSeqError(
                "E_DECODE",
                "failed to decode frame",
                frame=index,
                field="frame_file",
                details={"path": str(self.frame_paths[index]), "reason": str(exc)},
            ) from exc
        if image.shape != self.shape:
            raise SciSeqError(
                "E_META",
                "frame shape does not match manifest",
                frame=index,
                field="shape",
                details={"expected": list(self.shape), "actual": list(image.shape)},
            )
        return np.ascontiguousarray(image, dtype=np.float32)

    def clean_base(self) -> np.ndarray:
        try:
            image = np.load(self.clean_base_path, allow_pickle=False)
        except (OSError, ValueError) as exc:
            raise SciSeqError(
                "E_DECODE",
                "failed to decode clean base image",
                frame=None,
                field="clean_base_file",
                details={"path": str(self.clean_base_path), "reason": str(exc)},
            ) from exc
        if image.shape != self.shape:
            raise SciSeqError(
                "E_META",
                "clean base shape does not match manifest",
                frame=None,
                field="clean_base_shape",
                details={"expected": list(self.shape), "actual": list(image.shape)},
            )
        return np.ascontiguousarray(image, dtype=np.float32)

    def fingerprint(self) -> str:
        payload = {
            "sequence_id": self.sequence_id,
            "shape": list(self.shape),
            "frame_count": self.frame_count,
            "reference_frame": self.reference_frame,
            "frame_hashes": list(self.frame_hashes),
            "clean_base_hash": self.clean_base_hash,
            "shifts": self.truth.shifts.tolist(),
        }
        encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), allow_nan=False)
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()

