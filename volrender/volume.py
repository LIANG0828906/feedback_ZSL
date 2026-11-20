"""Volume loading, scalar normalization, window mapping, and cache provenance."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

import numpy as np

from .errors import E_INPUT_INVALID, VolRenderError
from .format import VolumeFile, read_volume_file


@dataclass(frozen=True)
class NormalizationReference:
    """The exact scalar reference used to define the [0, 1] domain."""

    raw_min: float
    raw_max: float
    truth_source: str
    raw_dtype: str
    window_width: float
    window_level: float
    spacing: tuple[float, float, float]

    @property
    def span(self) -> float:
        return self.raw_max - self.raw_min

    def as_dict(self) -> dict[str, object]:
        return {
            "raw_min": self.raw_min,
            "raw_max": self.raw_max,
            "truth_source": self.truth_source,
            "raw_dtype": self.raw_dtype,
            "window_width": self.window_width,
            "window_level": self.window_level,
            "spacing": list(self.spacing),
        }


@dataclass(frozen=True)
class MappingChange:
    """Auditable result of a non-silent mapping rebuild."""

    reason: str
    old_revision: int
    new_revision: int
    max_abs_diff: float
    mean_abs_diff: float
    changed_voxels: int
    old_digest: str
    new_digest: str

    def as_dict(self) -> dict[str, object]:
        return {
            "reason": self.reason,
            "old_revision": self.old_revision,
            "new_revision": self.new_revision,
            "max_abs_diff": self.max_abs_diff,
            "mean_abs_diff": self.mean_abs_diff,
            "changed_voxels": self.changed_voxels,
            "old_digest": self.old_digest,
            "new_digest": self.new_digest,
        }


def _validate_spacing(spacing: Iterable[float]) -> tuple[float, float, float]:
    values = tuple(float(v) for v in spacing)
    if len(values) != 3 or any(not np.isfinite(v) or v <= 0.0 for v in values):
        raise VolRenderError(
            E_INPUT_INVALID,
            "spacing must contain three finite positive values",
        )
    return values


class VolumeData:
    """A normalized scalar volume with explicit mapping provenance.

    The numerical chain is:

    ``raw -> normalized(raw_min, raw_max) -> window(width, level) -> [0, 1]``.

    Both normalized and window-mapped arrays are kept so callers can audit the
    full chain rather than only its final result.
    """

    def __init__(
        self,
        raw: np.ndarray,
        *,
        spacing: tuple[float, float, float],
        window_level: float,
        window_width: float,
        truth_source: str = "volume_minmax",
        source_path: str | None = None,
        file_sha256: str | None = None,
    ) -> None:
        array = np.asarray(raw)
        if array.ndim != 3:
            raise VolRenderError(E_INPUT_INVALID, "raw volume must be three-dimensional")
        if array.size == 0:
            raise VolRenderError(E_INPUT_INVALID, "raw volume must not be empty")
        if not (
            np.issubdtype(array.dtype, np.integer)
            or np.issubdtype(array.dtype, np.floating)
        ):
            raise VolRenderError(
                E_INPUT_INVALID,
                f"unsupported volume scalar type: {array.dtype}",
            )
        self._raw = np.ascontiguousarray(array)
        self._spacing = _validate_spacing(spacing)
        self._window_level = float(window_level)
        self._window_width = float(window_width)
        self._truth_source = str(truth_source)
        self.source_path = source_path
        self.file_sha256 = file_sha256
        self._validate_window(self._window_width, self._window_level)

        finite = self._raw[np.isfinite(self._raw)]
        if finite.size == 0:
            raise VolRenderError(E_INPUT_INVALID, "volume contains no finite voxels")
        self._raw_min = float(np.min(finite))
        self._raw_max = float(np.max(finite))
        if self._raw_max <= self._raw_min:
            self._raw_max = self._raw_min + 1.0
            self._truth_source += "+degenerate_span"

        raw_hash = hashlib.sha256(
            np.ascontiguousarray(self._raw).tobytes()
            + np.asarray(self._raw.shape, dtype="<u8").tobytes()
            + np.asarray(self._raw.dtype.str.encode("ascii")).tobytes()
        ).hexdigest()
        self.volume_id = raw_hash
        self._mapping_revision = 0
        self._normalized = self._compute_normalized()
        self._windowed, self._mapping_digest = self._compute_windowed()

    @classmethod
    def from_file(cls, path: str | Path) -> "VolumeData":
        parsed: VolumeFile = read_volume_file(path)
        return cls(
            parsed.raw,
            spacing=parsed.header.spacing,
            window_level=parsed.header.window_level,
            window_width=parsed.header.window_width,
            truth_source="file_minmax",
            source_path=parsed.path,
            file_sha256=parsed.file_sha256,
        )

    @property
    def raw(self) -> np.ndarray:
        return self._raw

    @property
    def normalized(self) -> np.ndarray:
        return self._normalized

    @property
    def windowed(self) -> np.ndarray:
        return self._windowed

    @property
    def shape(self) -> tuple[int, int, int]:
        return tuple(int(v) for v in self._raw.shape)

    @property
    def shape_xyz(self) -> tuple[int, int, int]:
        nz, ny, nx = self.shape
        return nx, ny, nz

    @property
    def spacing(self) -> tuple[float, float, float]:
        return self._spacing

    @property
    def window_width(self) -> float:
        return self._window_width

    @property
    def window_level(self) -> float:
        return self._window_level

    @property
    def normalization_reference(self) -> NormalizationReference:
        return NormalizationReference(
            raw_min=self._raw_min,
            raw_max=self._raw_max,
            truth_source=self._truth_source,
            raw_dtype=str(self._raw.dtype),
            window_width=self._window_width,
            window_level=self._window_level,
            spacing=self._spacing,
        )

    @property
    def mapping_revision(self) -> int:
        return self._mapping_revision

    @property
    def mapping_digest(self) -> str:
        return self._mapping_digest

    @property
    def world_extent(self) -> tuple[float, float, float]:
        nx, ny, nz = self.shape_xyz
        sx, sy, sz = self._spacing
        return nx * sx, ny * sy, nz * sz

    def _compute_normalized(self) -> np.ndarray:
        span = self._raw_max - self._raw_min
        normalized = (self._raw.astype(np.float64) - self._raw_min) / span
        return np.clip(normalized, 0.0, 1.0)

    def _compute_windowed(self) -> tuple[np.ndarray, str]:
        span = self._raw_max - self._raw_min
        level_n = (self._window_level - self._raw_min) / span
        width_n = self._window_width / span
        lower = level_n - 0.5 * width_n
        windowed = np.clip((self._normalized - lower) / width_n, 0.0, 1.0)
        digest = hashlib.sha256(
            np.ascontiguousarray(windowed, dtype="<f8").tobytes()
        ).hexdigest()
        return windowed, digest

    @staticmethod
    def _validate_window(width: float, level: float) -> None:
        if not np.isfinite(width) or width <= 0.0:
            raise VolRenderError(E_INPUT_INVALID, "window width must be positive")
        if not np.isfinite(level):
            raise VolRenderError(E_INPUT_INVALID, "window level must be finite")

    def _rebuild(self, reason: str) -> MappingChange:
        old_revision = self._mapping_revision
        old_digest = self._mapping_digest
        old_values = self._windowed
        new_values, new_digest = self._compute_windowed()
        diff = np.abs(new_values - old_values)
        self._windowed = new_values
        self._mapping_digest = new_digest
        self._mapping_revision += 1
        return MappingChange(
            reason=reason,
            old_revision=old_revision,
            new_revision=self._mapping_revision,
            max_abs_diff=float(np.max(diff)),
            mean_abs_diff=float(np.mean(diff)),
            changed_voxels=int(np.count_nonzero(diff)),
            old_digest=old_digest,
            new_digest=new_digest,
        )

    def set_window(self, width: float, level: float) -> MappingChange:
        """Update window parameters and always rebuild the mapping."""

        self._validate_window(width, level)
        self._window_width = float(width)
        self._window_level = float(level)
        return self._rebuild("window")

    def set_spacing(self, spacing: Iterable[float]) -> MappingChange:
        """Update spacing and rebuild the mapping and world-space metadata."""

        self._spacing = _validate_spacing(spacing)
        return self._rebuild("spacing")

    def mapping_fingerprint(self) -> str:
        reference = self.normalization_reference.as_dict()
        payload = json.dumps(
            {
                "volume_id": self.volume_id,
                "mapping_digest": self._mapping_digest,
                "reference": reference,
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(payload).hexdigest()

    def sample_windowed(self, coords: np.ndarray) -> np.ndarray:
        """Trilinear sample in array coordinates ``(x, y, z)``.

        Coordinates are clamped to valid voxel centers. Callers performing ray
        marching already clip hits to the volume AABB.
        """

        from .sampling import sample_trilinear

        return sample_trilinear(self._windowed, coords)
