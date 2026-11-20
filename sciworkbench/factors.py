"""Calibration factors and the deterministic error model inputs."""

from __future__ import annotations

import math
import os
import struct
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Dict, Optional, Tuple

import numpy as np

from .errors import E_FACTOR_INVALID, WorkbenchError
from .model import SequenceStructure
from .util import canonical_json_bytes, hash_array, sha256_bytes


CALIBRATION_MAGIC = b"SCICAL01"
CALIBRATION_SCHEMA = 1

STEP_NAMES = (
    "dark_subtract",
    "flat_divide",
    "bad_pixel_repair",
    "block_gain",
    "noise_suppress",
)


@dataclass(frozen=True)
class NoiseParams:
    sigma: float = 0.8
    coeff_bits: int = 12

    def validate(self) -> None:
        if not math.isfinite(self.sigma) or self.sigma < 0.0:
            raise WorkbenchError(
                E_FACTOR_INVALID, "noise sigma must be finite and non-negative"
            )
        if self.coeff_bits < 1 or self.coeff_bits > 52:
            raise WorkbenchError(
                E_FACTOR_INVALID, "filter coefficient bits must be in [1, 52]"
            )

    def canonical_dict(self) -> dict:
        return {"sigma": float(self.sigma), "coeff_bits": int(self.coeff_bits)}


@dataclass(frozen=True)
class ErrorModel:
    dark_error: float = 0.5
    flat_rel_error: float = 1.0e-4
    flat_floor: float = 1.0e-3
    threshold: float = 8.0

    def validate(self) -> None:
        values = (
            self.dark_error,
            self.flat_rel_error,
            self.flat_floor,
            self.threshold,
        )
        if not all(math.isfinite(value) for value in values):
            raise WorkbenchError(E_FACTOR_INVALID, "error-model values must be finite")
        if self.dark_error < 0.0 or self.threshold < 0.0:
            raise WorkbenchError(
                E_FACTOR_INVALID, "dark error and threshold must be non-negative"
            )
        if self.flat_floor <= 0.0:
            raise WorkbenchError(E_FACTOR_INVALID, "flat denominator floor must be positive")
        if self.flat_rel_error < 0.0 or self.flat_rel_error >= 1.0:
            raise WorkbenchError(
                E_FACTOR_INVALID, "flat relative error must be in [0, 1)"
            )

    def canonical_dict(self) -> dict:
        return {
            "dark_error": float(self.dark_error),
            "flat_rel_error": float(self.flat_rel_error),
            "flat_floor": float(self.flat_floor),
            "threshold": float(self.threshold),
        }


def _frozen_float_array(value: Any, name: str) -> np.ndarray:
    arr = np.ascontiguousarray(np.asarray(value, dtype=np.float64))
    if not np.all(np.isfinite(arr)):
        raise WorkbenchError(
            E_FACTOR_INVALID, "{} contains non-finite values".format(name)
        )
    arr.setflags(write=False)
    return arr


def _frozen_bool_array(value: Any, name: str) -> np.ndarray:
    arr = np.ascontiguousarray(np.asarray(value, dtype=np.bool_))
    arr.setflags(write=False)
    return arr


@dataclass(frozen=True)
class FactorSet:
    dark: np.ndarray
    flat: np.ndarray
    bad_mask: np.ndarray
    block_gain: np.ndarray
    noise: NoiseParams
    error: ErrorModel
    frac_bits: int

    def __post_init__(self) -> None:
        object.__setattr__(self, "dark", _frozen_float_array(self.dark, "dark"))
        object.__setattr__(self, "flat", _frozen_float_array(self.flat, "flat"))
        object.__setattr__(
            self, "bad_mask", _frozen_bool_array(self.bad_mask, "bad_mask")
        )
        object.__setattr__(
            self, "block_gain", _frozen_float_array(self.block_gain, "block_gain")
        )
        if not isinstance(self.noise, NoiseParams):
            object.__setattr__(
                self,
                "noise",
                NoiseParams(float(self.noise.get("sigma")), int(self.noise.get("coeff_bits"))),
            )
        if not isinstance(self.error, ErrorModel):
            object.__setattr__(self, "error", ErrorModel(**dict(self.error)))
        self.noise.validate()
        self.error.validate()
        if self.frac_bits < 0 or self.frac_bits > 52:
            raise WorkbenchError(
                E_FACTOR_INVALID, "fixed-point fraction bits must be in [0, 52]"
            )
        if np.any(self.block_gain < 0.0):
            raise WorkbenchError(E_FACTOR_INVALID, "block gains must be non-negative")

    @classmethod
    def defaults(
        cls,
        width: int,
        height: int,
        block_rows: int,
        block_cols: int,
        *,
        frac_bits: int = 12,
    ) -> "FactorSet":
        return cls(
            dark=np.zeros((height, width), dtype=np.float64),
            flat=np.ones((height, width), dtype=np.float64),
            bad_mask=np.zeros((height, width), dtype=np.bool_),
            block_gain=np.ones((block_rows, block_cols), dtype=np.float64),
            noise=NoiseParams(),
            error=ErrorModel(),
            frac_bits=frac_bits,
        )

    def validate(self, structure: SequenceStructure) -> None:
        height = structure.height
        width = structure.width
        frames = structure.frame_count
        for name in ("dark", "flat"):
            arr = getattr(self, name)
            if arr.shape not in ((height, width), (frames, height, width)):
                raise WorkbenchError(
                    E_FACTOR_INVALID,
                    "{} shape {} does not match (H,W) or (F,H,W)".format(
                        name, arr.shape
                    ),
                )
        if self.bad_mask.shape not in (
            (height, width),
            (frames, height, width),
        ):
            raise WorkbenchError(
                E_FACTOR_INVALID,
                "bad_mask shape {} does not match (H,W) or (F,H,W)".format(
                    self.bad_mask.shape
                ),
            )
        if self.block_gain.shape != (structure.block_rows, structure.block_cols):
            raise WorkbenchError(
                E_FACTOR_INVALID,
                "block_gain shape {} does not match block grid {}".format(
                    self.block_gain.shape,
                    (structure.block_rows, structure.block_cols),
                ),
            )

    def replace(self, field_name: str, value: Any) -> "FactorSet":
        if field_name not in (
            "dark",
            "flat",
            "bad_mask",
            "block_gain",
            "noise",
            "error",
            "frac_bits",
        ):
            raise WorkbenchError(
                E_FACTOR_INVALID, "unknown influence factor {}".format(field_name)
            )
        return replace(self, **{field_name: value})

    def _version_material(self) -> Dict[str, str]:
        return {
            "dark": hash_array(self.dark),
            "flat": hash_array(self.flat),
            "bad_mask": hash_array(self.bad_mask),
            "block_gain": hash_array(self.block_gain),
            "noise": sha256_bytes(canonical_json_bytes(self.noise.canonical_dict())),
            "error": sha256_bytes(canonical_json_bytes(self.error.canonical_dict())),
            "frac_bits": sha256_bytes(str(int(self.frac_bits)).encode("ascii")),
        }

    def factor_versions(self) -> Dict[str, str]:
        return dict(self._version_material())

    def factor_hash(self, mode: str) -> str:
        return sha256_bytes(
            canonical_json_bytes(
                {"mode": mode, "factors": self._version_material()}
            )
        )

    def block_factor_versions(
        self, structure: SequenceStructure, frame_index: int, block_index: int
    ) -> Dict[str, str]:
        block = structure.block(frame_index, block_index)
        y0 = max(0, block.y0 - 1)
        y1 = min(structure.height, block.y1 + 1)
        x0 = max(0, block.x0 - 1)
        x1 = min(structure.width, block.x1 + 1)

        def frame_slice(name: str, default: float) -> np.ndarray:
            arr = getattr(self, name)
            if arr.ndim == 2:
                return arr[y0:y1, x0:x1]
            return arr[frame_index, y0:y1, x0:x1]

        bad = (
            self.bad_mask[y0:y1, x0:x1]
            if self.bad_mask.ndim == 2
            else self.bad_mask[frame_index, y0:y1, x0:x1]
        )
        return {
            "dark": hash_array(frame_slice("dark", 0.0)),
            "flat": hash_array(frame_slice("flat", 1.0)),
            "bad_mask": hash_array(bad),
            "block_gain": hash_array(self.block_gain),
            "noise": sha256_bytes(canonical_json_bytes(self.noise.canonical_dict())),
            "error": sha256_bytes(canonical_json_bytes(self.error.canonical_dict())),
            "frac_bits": sha256_bytes(str(int(self.frac_bits)).encode("ascii")),
        }

    def canonical_dict(self) -> dict:
        return {
            "dark": {"shape": list(self.dark.shape), "sha256": hash_array(self.dark)},
            "flat": {"shape": list(self.flat.shape), "sha256": hash_array(self.flat)},
            "bad_mask": {
                "shape": list(self.bad_mask.shape),
                "sha256": hash_array(self.bad_mask),
            },
            "block_gain": {
                "shape": list(self.block_gain.shape),
                "sha256": hash_array(self.block_gain),
            },
            "noise": self.noise.canonical_dict(),
            "error": self.error.canonical_dict(),
            "frac_bits": int(self.frac_bits),
        }


def _frame_array(array: np.ndarray, frame_index: int) -> np.ndarray:
    if array.ndim == 2:
        return array
    return array[frame_index]


def calibration_header(factors: FactorSet, structure: SequenceStructure) -> dict:
    return {
        "schema": CALIBRATION_SCHEMA,
        "width": structure.width,
        "height": structure.height,
        "frame_count": structure.frame_count,
        "block_rows": structure.block_rows,
        "block_cols": structure.block_cols,
        "frac_bits": int(factors.frac_bits),
        "noise": factors.noise.canonical_dict(),
        "error": factors.error.canonical_dict(),
        "arrays": {
            "dark": {
                "dtype": "<f8",
                "shape": list(factors.dark.shape),
            },
            "flat": {
                "dtype": "<f8",
                "shape": list(factors.flat.shape),
            },
            "bad_mask": {
                "dtype": "|u1",
                "shape": list(factors.bad_mask.shape),
            },
            "block_gain": {
                "dtype": "<f8",
                "shape": list(factors.block_gain.shape),
            },
        },
    }


def save_factors(path: os.PathLike, factors: FactorSet, structure: SequenceStructure) -> None:
    factors.validate(structure)
    header_value = calibration_header(factors, structure)
    arrays = (
        np.ascontiguousarray(factors.dark, dtype="<f8"),
        np.ascontiguousarray(factors.flat, dtype="<f8"),
        np.ascontiguousarray(factors.bad_mask, dtype=np.uint8),
        np.ascontiguousarray(factors.block_gain, dtype="<f8"),
    )
    digest = b""
    for arr in arrays:
        digest += np.ascontiguousarray(arr).tobytes(order="C")
    header_value["payload_sha256"] = sha256_bytes(digest)
    header_bytes = canonical_json_bytes(header_value)
    prefix = CALIBRATION_MAGIC + struct.pack("<I", len(header_bytes))
    payload = prefix + header_bytes + digest
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temp = destination.with_name(
        ".{}.tmp-{}-{}".format(destination.name, os.getpid(), id(factors))
    )
    try:
        with temp.open("wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, destination)
    finally:
        if temp.exists():
            temp.unlink()


def load_factors(path: os.PathLike, structure: SequenceStructure) -> FactorSet:
    source = Path(path)
    try:
        raw = source.read_bytes()
    except OSError as exc:
        raise WorkbenchError(
            E_FACTOR_INVALID,
            "cannot read calibration file: {}".format(exc),
            path=str(source),
        ) from exc
    if len(raw) < 12 or raw[:8] != CALIBRATION_MAGIC:
        raise WorkbenchError(
            E_FACTOR_INVALID, "bad calibration magic", path=str(source)
        )
    (header_length,) = struct.unpack("<I", raw[8:12])
    header_end = 12 + header_length
    if header_end > len(raw):
        raise WorkbenchError(
            E_FACTOR_INVALID, "calibration header is truncated", path=str(source)
        )
    try:
        import json

        header_value = json.loads(raw[12:header_end].decode("utf-8"))
    except Exception as exc:
        raise WorkbenchError(
            E_FACTOR_INVALID, "calibration header is not valid JSON", path=str(source)
        ) from exc
    body = raw[header_end:]
    if sha256_bytes(body) != header_value.get("payload_sha256"):
        raise WorkbenchError(
            E_FACTOR_INVALID, "calibration payload hash mismatch", path=str(source)
        )
    if (
        header_value.get("width") != structure.width
        or header_value.get("height") != structure.height
        or header_value.get("frame_count") != structure.frame_count
        or header_value.get("block_rows") != structure.block_rows
        or header_value.get("block_cols") != structure.block_cols
    ):
        raise WorkbenchError(
            E_FACTOR_INVALID,
            "calibration dimensions do not match the sequence",
            path=str(source),
        )
    specs = header_value.get("arrays") or {}
    offset = 0

    def read_array(name: str) -> np.ndarray:
        nonlocal offset
        try:
            spec = specs[name]
            shape = tuple(int(item) for item in spec["shape"])
            dtype = np.dtype(spec["dtype"])
        except (KeyError, TypeError, ValueError) as exc:
            raise WorkbenchError(
                E_FACTOR_INVALID,
                "calibration array field is missing: {}".format(name),
                path=str(source),
            ) from exc
        count = int(np.prod(shape, dtype=np.int64))
        byte_count = count * dtype.itemsize
        if offset + byte_count > len(body):
            raise WorkbenchError(
                E_FACTOR_INVALID,
                "calibration array is truncated: {}".format(name),
                path=str(source),
            )
        arr = np.frombuffer(body, dtype=dtype, count=count, offset=offset).reshape(shape)
        offset += byte_count
        return arr.copy()

    dark = read_array("dark")
    flat = read_array("flat")
    bad = read_array("bad_mask").astype(np.bool_)
    gain = read_array("block_gain")
    if offset != len(body):
        raise WorkbenchError(
            E_FACTOR_INVALID, "calibration has trailing payload bytes", path=str(source)
        )
    return FactorSet(
        dark=dark,
        flat=flat,
        bad_mask=bad,
        block_gain=gain,
        noise=NoiseParams(**header_value["noise"]),
        error=ErrorModel(**header_value["error"]),
        frac_bits=int(header_value["frac_bits"]),
    )
