"""Volume model: manifest parsing, calibration validation, payload decoding.

A volume on disk is a directory containing:

* ``manifest.json`` -- axis order, shape, spacing, gray calibration
  (slope/intercept), declared gray range, dtype and payload file name.
* ``volume.npy``    -- the scalar payload (NumPy ``.npy``).
* ``truth.json``    -- optional analytic ground truth for embedded geometry.

Loading is all-or-nothing: :meth:`VolumeModel.load` either returns a fully
initialised model or raises :class:`VolViewError`; a failed load never leaves
a half-initialised model behind (the registry only stores successes).
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple

import numpy as np

from .errors import ErrorCode, VolViewError

WORLD_AXES = ("X", "Y", "Z")
MANIFEST_SCHEMA_VERSION = 1


def _fail(code: str, field_: str, message: str, **details) -> None:
    raise VolViewError(code, field_, message, **details)


@dataclass(frozen=True)
class Manifest:
    volume_id: str
    axis_order: Tuple[str, str, str]  # axis_order[i] = world axis of array dim i
    shape: Tuple[int, int, int]       # voxels per array dim
    spacing: Dict[str, float]         # mm per world axis
    slope: float
    intercept: float
    gray_range: Tuple[float, float]   # declared calibrated gray range
    dtype: str
    data_file: str
    schema_version: int = MANIFEST_SCHEMA_VERSION

    @staticmethod
    def parse(raw: dict, source: str = "manifest.json") -> "Manifest":
        """Validate the manifest declaration (E_AXIS / E_CAL chains)."""
        if not isinstance(raw, dict):
            _fail(ErrorCode.E_DECODE, source, "manifest is not a JSON object")

        volume_id = raw.get("volume_id")
        if not isinstance(volume_id, str) or not volume_id:
            _fail(ErrorCode.E_CAL, "volume_id", "missing or invalid volume id")

        # --- axis order -------------------------------------------------
        axis_order = raw.get("axis_order")
        if not isinstance(axis_order, str) or len(axis_order) != 3:
            _fail(ErrorCode.E_AXIS, "axis_order",
                  f"axis_order must declare exactly 3 axes, got {axis_order!r}",
                  declared=axis_order)
        axis_order = axis_order.upper()
        for letter in WORLD_AXES:
            if letter not in axis_order:
                _fail(ErrorCode.E_AXIS, "axis_order",
                      f"axis {letter} missing/duplicated in {axis_order!r}",
                      conflict_axis=letter, declared=axis_order)

        # --- shape ------------------------------------------------------
        shape = raw.get("shape")
        if (not isinstance(shape, list) or len(shape) != 3
                or any(not isinstance(n, int) or n <= 0 for n in shape)):
            _fail(ErrorCode.E_AXIS, "shape",
                  f"shape must be 3 positive integers, got {shape!r}",
                  conflict_axis=axis_order[0])

        # --- spacing ----------------------------------------------------
        spacing = raw.get("spacing")
        if not isinstance(spacing, dict):
            _fail(ErrorCode.E_CAL, "spacing", "spacing must be an object keyed by axis")
        parsed_spacing: Dict[str, float] = {}
        for letter in WORLD_AXES:
            value = spacing.get(letter)
            if value is None:
                _fail(ErrorCode.E_CAL, f"spacing.{letter}",
                      f"spacing for axis {letter} is missing")
            value = float(value)
            if not np.isfinite(value) or value <= 0.0:
                _fail(ErrorCode.E_CAL, f"spacing.{letter}",
                      f"spacing for axis {letter} must be positive, got {value}")
            parsed_spacing[letter] = value

        # --- gray calibration -------------------------------------------
        slope = raw.get("slope")
        if slope is None:
            _fail(ErrorCode.E_CAL, "slope", "calibration slope is missing")
        slope = float(slope)
        if not np.isfinite(slope) or slope == 0.0:
            _fail(ErrorCode.E_CAL, "slope", f"slope must be finite and non-zero, got {slope}")

        intercept = raw.get("intercept")
        if intercept is None:
            _fail(ErrorCode.E_CAL, "intercept", "calibration intercept is missing")
        intercept = float(intercept)
        if not np.isfinite(intercept):
            _fail(ErrorCode.E_CAL, "intercept",
                  f"intercept must be finite, got {intercept}")

        gray_range = raw.get("gray_range")
        if (not isinstance(gray_range, list) or len(gray_range) != 2
                or not all(np.isfinite(float(v)) for v in gray_range)
                or not float(gray_range[0]) < float(gray_range[1])):
            _fail(ErrorCode.E_CAL, "gray_range",
                  f"gray_range must be [min, max] with min < max, got {gray_range!r}")

        dtype = raw.get("dtype")
        if not isinstance(dtype, str) or not dtype:
            _fail(ErrorCode.E_CAL, "dtype", "dtype declaration is missing")

        data_file = raw.get("data_file", "volume.npy")

        return Manifest(
            volume_id=volume_id,
            axis_order=(axis_order[0], axis_order[1], axis_order[2]),
            shape=(int(shape[0]), int(shape[1]), int(shape[2])),
            spacing=parsed_spacing,
            slope=slope,
            intercept=intercept,
            gray_range=(float(gray_range[0]), float(gray_range[1])),
            dtype=dtype,
            data_file=data_file,
            schema_version=int(raw.get("schema_version", MANIFEST_SCHEMA_VERSION)),
        )


@dataclass
class VolumeModel:
    """Fully validated scalar volume with geometric information."""

    manifest: Manifest
    data: np.ndarray               # raw values, dims follow manifest.axis_order
    content_hash: str              # sha256 over payload bytes + canonical manifest
    truth: Optional[dict] = None
    source_dir: str = ""

    # -- geometry helpers ------------------------------------------------
    @property
    def dim_of_axis(self) -> Dict[str, int]:
        return {letter: self.manifest.axis_order.index(letter) for letter in WORLD_AXES}

    @property
    def shape_of_axis(self) -> Dict[str, int]:
        return {letter: self.manifest.shape[self.dim_of_axis[letter]] for letter in WORLD_AXES}

    def extent(self, axis: str) -> float:
        """World-coordinate extent [0, extent] of the volume along ``axis``."""
        n = self.shape_of_axis[axis]
        return (n - 1) * self.manifest.spacing[axis]

    def center_world(self) -> Tuple[float, float, float]:
        return tuple(self.extent(a) / 2.0 for a in WORLD_AXES)  # type: ignore[return-value]

    def world_to_index(self, axis: str, world: float) -> float:
        return world / self.manifest.spacing[axis]

    def index_to_world(self, axis: str, index: float) -> float:
        return index * self.manifest.spacing[axis]

    def calibrated(self, array: np.ndarray) -> np.ndarray:
        """Apply gray calibration (slope/intercept) in float64."""
        return np.asarray(array, dtype=np.float64) * self.manifest.slope + self.manifest.intercept

    # -- loading -----------------------------------------------------------
    @classmethod
    def load(cls, volume_dir: str) -> "VolumeModel":
        """Load and fully validate a volume directory (all-or-nothing)."""
        manifest_path = os.path.join(volume_dir, "manifest.json")
        try:
            with open(manifest_path, "r", encoding="utf-8") as fh:
                raw_manifest = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            _fail(ErrorCode.E_DECODE, "manifest.json", f"cannot read manifest: {exc}")

        manifest = Manifest.parse(raw_manifest)

        data_path = os.path.join(volume_dir, manifest.data_file)
        try:
            with open(data_path, "rb") as fh:
                payload = fh.read()
        except OSError as exc:
            _fail(ErrorCode.E_DECODE, manifest.data_file, f"cannot read payload: {exc}")

        try:
            import io
            data = np.load(io.BytesIO(payload), allow_pickle=False)
        except Exception as exc:  # truncated / corrupt payload
            _fail(ErrorCode.E_DECODE, manifest.data_file,
                  f"payload cannot be decoded: {exc}")

        if tuple(data.shape) != manifest.shape:
            conflict = next(
                (manifest.axis_order[d] for d in range(3)
                 if d >= data.ndim or data.shape[d] != manifest.shape[d]),
                manifest.axis_order[0],
            )
            _fail(ErrorCode.E_AXIS, "shape",
                  f"declared shape {manifest.shape} != payload shape {tuple(data.shape)}",
                  conflict_axis=conflict)

        if not np.issubdtype(data.dtype, np.number):
            _fail(ErrorCode.E_DECODE, manifest.data_file,
                  f"payload dtype {data.dtype} is not numeric")

        truth = None
        truth_path = os.path.join(volume_dir, "truth.json")
        if os.path.exists(truth_path):
            try:
                with open(truth_path, "r", encoding="utf-8") as fh:
                    truth = json.load(fh)
            except (OSError, json.JSONDecodeError) as exc:
                _fail(ErrorCode.E_DECODE, "truth.json", f"cannot read truth: {exc}")

        canonical = json.dumps(raw_manifest, sort_keys=True).encode("utf-8")
        content_hash = hashlib.sha256(payload + b"\x00" + canonical).hexdigest()

        return cls(manifest=manifest, data=data, content_hash=content_hash,
                   truth=truth, source_dir=volume_dir)


class VolumeRegistry:
    """Caches loaded volumes; failed loads never leave entries behind."""

    def __init__(self) -> None:
        self._volumes: Dict[str, VolumeModel] = {}

    def get(self, volume_dir: str) -> VolumeModel:
        key = os.path.abspath(volume_dir)
        if key not in self._volumes:
            model = VolumeModel.load(volume_dir)  # raises -> nothing stored
            self._volumes[key] = model
        return self._volumes[key]

    def __contains__(self, volume_dir: str) -> bool:
        return os.path.abspath(volume_dir) in self._volumes
