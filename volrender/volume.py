"""Custom binary volume loading and deterministic window mapping."""

from __future__ import annotations

import hashlib
import math
import os
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, VolumeRenderError, require

MAGIC = b"VSRENDER"
FORMAT_VERSION = 1
HEADER_SIZE = 128

SCALAR_TYPES: dict[int, tuple[str, np.dtype[Any]]] = {
    2: ("uint16", np.dtype("<u2")),
    4: ("float32", np.dtype("<f4")),
}

_HEADER_STRUCT = struct.Struct("<8sHHHHIIIfffffQQQQIIIQ28x")


def _finite_float(value: float, field: str, code: ErrorCode = ErrorCode.E_HEADER_INVALID) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise VolumeRenderError(code, "field is not numeric", field=field) from exc
    if not math.isfinite(result):
        raise VolumeRenderError(code, "field must be finite", field=field)
    return result


@dataclass(frozen=True)
class VolumeHeader:
    version: int
    header_size: int
    scalar_type_id: int
    scalar_type: str
    flags: int
    dims: tuple[int, int, int]
    spacing: tuple[float, float, float]
    window_center: float
    window_width: float
    density_offset: int
    density_bytes: int
    mask_offset: int
    mask_bytes: int
    mask_dims: tuple[int, int, int]
    voxel_count: int

    @property
    def has_mask(self) -> bool:
        return bool(self.flags & 1)

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "header_size": self.header_size,
            "scalar_type_id": self.scalar_type_id,
            "scalar_type": self.scalar_type,
            "flags": self.flags,
            "dims": list(self.dims),
            "spacing": list(self.spacing),
            "window_center": self.window_center,
            "window_width": self.window_width,
            "density_offset": self.density_offset,
            "density_bytes": self.density_bytes,
            "mask_offset": self.mask_offset,
            "mask_bytes": self.mask_bytes,
            "mask_dims": list(self.mask_dims),
            "voxel_count": self.voxel_count,
        }


@dataclass(frozen=True)
class WindowReference:
    """Values that make a particular window mapping reproducible."""

    center: float
    width: float
    raw_low: float
    raw_high: float
    mapped_min: float
    mapped_max: float
    mapped_mean: float

    def to_dict(self) -> dict[str, float]:
        return {
            "center": self.center,
            "width": self.width,
            "raw_low": self.raw_low,
            "raw_high": self.raw_high,
            "mapped_min": self.mapped_min,
            "mapped_max": self.mapped_max,
            "mapped_mean": self.mapped_mean,
        }


@dataclass(frozen=True)
class WindowDelta:
    old: WindowReference
    new: WindowReference
    max_abs_difference: float
    affected_voxels: int
    total_voxels: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "old": self.old.to_dict(),
            "new": self.new.to_dict(),
            "max_abs_difference": self.max_abs_difference,
            "affected_voxels": self.affected_voxels,
            "total_voxels": self.total_voxels,
        }


def validate_window(center: float, width: float, field_prefix: str = "window") -> tuple[float, float]:
    center_value = _finite_float(center, f"{field_prefix}_center")
    width_value = _finite_float(width, f"{field_prefix}_width")
    if width_value <= 0.0:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "window width must be greater than zero",
            field=f"{field_prefix}_width",
        )
    return center_value, width_value


def map_window(raw: np.ndarray, center: float, width: float) -> tuple[np.ndarray, WindowReference]:
    """Map a scalar field to [0, 1] with a DICOM-like center/width window."""

    center_value, width_value = validate_window(center, width)
    if not isinstance(raw, np.ndarray):
        raw = np.asarray(raw)
    low = center_value - width_value / 2.0
    high = center_value + width_value / 2.0
    scaled = (raw.astype(np.float64, copy=False) - low) / width_value
    mapped = np.clip(scaled, 0.0, 1.0).astype(np.float32)
    reference = WindowReference(
        center=center_value,
        width=width_value,
        raw_low=float(low),
        raw_high=float(high),
        mapped_min=float(mapped.min()) if mapped.size else 0.0,
        mapped_max=float(mapped.max()) if mapped.size else 0.0,
        mapped_mean=float(mapped.mean()) if mapped.size else 0.0,
    )
    return mapped, reference


def compare_window_mappings(
    raw: np.ndarray,
    old_center: float,
    old_width: float,
    new_center: float,
    new_width: float,
) -> tuple[WindowDelta, np.ndarray, np.ndarray]:
    old_mapped, old_ref = map_window(raw, old_center, old_width)
    new_mapped, new_ref = map_window(raw, new_center, new_width)
    difference = np.abs(old_mapped.astype(np.float64) - new_mapped.astype(np.float64))
    affected = int(np.count_nonzero(difference > 0.0))
    delta = WindowDelta(
        old=old_ref,
        new=new_ref,
        max_abs_difference=float(difference.max()) if difference.size else 0.0,
        affected_voxels=affected,
        total_voxels=int(raw.size),
    )
    return delta, old_mapped, new_mapped


def parse_header(header_bytes: bytes) -> VolumeHeader:
    if len(header_bytes) != HEADER_SIZE:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            f"header must be exactly {HEADER_SIZE} bytes",
            field="header_size",
            details={"actual": len(header_bytes)},
        )

    unpacked = _HEADER_STRUCT.unpack(header_bytes)
    (
        magic,
        version,
        header_size,
        scalar_type_id,
        flags,
        nx,
        ny,
        nz,
        sx,
        sy,
        sz,
        window_center,
        window_width,
        density_offset,
        density_bytes,
        mask_offset,
        mask_bytes,
        mask_nx,
        mask_ny,
        mask_nz,
        voxel_count,
    ) = unpacked

    if magic != MAGIC:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "magic does not match VSRENDER",
            field="magic",
            details={"actual": magic.hex()},
        )
    if version != FORMAT_VERSION:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "unsupported format version",
            field="version",
            details={"expected": FORMAT_VERSION, "actual": version},
        )
    if flags & ~1:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "unsupported header flags are set",
            field="flags",
            details={"actual": int(flags)},
        )
    if header_size != HEADER_SIZE:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "declared header size is not supported",
            field="header_size",
            details={"expected": HEADER_SIZE, "actual": header_size},
        )
    if scalar_type_id not in SCALAR_TYPES:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "unsupported scalar type",
            field="scalar_type",
            details={"actual": scalar_type_id, "supported": sorted(SCALAR_TYPES)},
        )
    dims = (int(nx), int(ny), int(nz))
    if any(value <= 0 for value in dims):
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "volume dimensions must be positive",
            field="dims",
            details={"dims": list(dims)},
        )
    spacing = (
        _finite_float(sx, "spacing_x"),
        _finite_float(sy, "spacing_y"),
        _finite_float(sz, "spacing_z"),
    )
    for index, value in enumerate(spacing):
        if value <= 0.0:
            raise VolumeRenderError(
                ErrorCode.E_HEADER_INVALID,
                "voxel spacing must be positive",
                field=("spacing_x", "spacing_y", "spacing_z")[index],
                details={"actual": value},
            )

    center_value, width_value = validate_window(window_center, window_width)
    scalar_name, dtype = SCALAR_TYPES[scalar_type_id]
    expected_density_bytes = math.prod(dims) * dtype.itemsize
    if int(voxel_count) != math.prod(dims):
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "voxel_count does not equal dimensions product",
            field="voxel_count",
            details={"expected": math.prod(dims), "actual": int(voxel_count)},
        )
    if int(density_offset) != HEADER_SIZE:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "density offset must start immediately after the header",
            field="density_offset",
            details={"expected": HEADER_SIZE, "actual": int(density_offset)},
        )
    if int(density_bytes) != expected_density_bytes:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "density byte count does not match dimensions and scalar type",
            field="density_bytes",
            details={"expected": expected_density_bytes, "actual": int(density_bytes)},
        )

    header = VolumeHeader(
        version=int(version),
        header_size=int(header_size),
        scalar_type_id=int(scalar_type_id),
        scalar_type=scalar_name,
        flags=int(flags),
        dims=dims,
        spacing=spacing,
        window_center=center_value,
        window_width=width_value,
        density_offset=int(density_offset),
        density_bytes=int(density_bytes),
        mask_offset=int(mask_offset),
        mask_bytes=int(mask_bytes),
        mask_dims=(int(mask_nx), int(mask_ny), int(mask_nz)),
        voxel_count=int(voxel_count),
    )
    _validate_mask_layout(header)
    return header


def _validate_mask_layout(header: VolumeHeader) -> None:
    density_end = header.density_offset + header.density_bytes
    if not header.has_mask:
        if header.mask_bytes != 0 or header.mask_offset not in (0, density_end):
            raise VolumeRenderError(
                ErrorCode.E_MASK_INVALID,
                "mask fields are present without the mask flag",
                field="mask_offset",
            )
        return

    if any(value <= 0 for value in header.mask_dims):
        raise VolumeRenderError(
            ErrorCode.E_MASK_INVALID,
            "mask dimensions must be positive",
            field="mask_dims",
        )
    if header.mask_dims != header.dims:
        raise VolumeRenderError(
            ErrorCode.E_MASK_INVALID,
            "mask dimensions must equal density dimensions",
            field="mask_dims",
            details={"density_dims": list(header.dims), "mask_dims": list(header.mask_dims)},
        )
    expected_mask_bytes = math.prod(header.mask_dims)
    if header.mask_bytes != expected_mask_bytes:
        raise VolumeRenderError(
            ErrorCode.E_MASK_INVALID,
            "mask byte count does not match mask dimensions",
            field="mask_bytes",
            details={"expected": expected_mask_bytes, "actual": header.mask_bytes},
        )
    if header.mask_offset < density_end:
        raise VolumeRenderError(
            ErrorCode.E_MASK_INVALID,
            "mask offset overlaps density data",
            field="mask_offset",
            details={"density_end": density_end, "actual": header.mask_offset},
        )


@dataclass
class VolumeData:
    header: VolumeHeader
    density: np.ndarray
    mask: np.ndarray | None
    path: Path | None = None
    file_sha256: str = ""
    content_sha256: str = ""
    density_sha256: str = ""
    mask_sha256: str = ""

    @property
    def dims(self) -> tuple[int, int, int]:
        return self.header.dims

    @property
    def spacing(self) -> tuple[float, float, float]:
        return self.header.spacing

    @property
    def has_mask(self) -> bool:
        return self.mask is not None

    def normalize(
        self, center: float | None = None, width: float | None = None
    ) -> tuple[np.ndarray, WindowReference]:
        return map_window(
            self.density,
            self.header.window_center if center is None else center,
            self.header.window_width if width is None else width,
        )

    def compare_windows(self, old_center: float, old_width: float, new_center: float, new_width: float) -> WindowDelta:
        delta, _, _ = compare_window_mappings(self.density, old_center, old_width, new_center, new_width)
        return delta


def _hash_bytes(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def _compute_content_hash(header: VolumeHeader, density: np.ndarray, mask: np.ndarray | None) -> str:
    digest = hashlib.sha256()
    metadata = {
        "format": FORMAT_VERSION,
        "dims": header.dims,
        "spacing": header.spacing,
        "scalar_type": header.scalar_type,
        "mask_dims": header.mask_dims if mask is not None else None,
    }
    for key in sorted(metadata):
        digest.update(key.encode("ascii"))
        digest.update(b"\0")
        digest.update(repr(metadata[key]).encode("utf-8"))
        digest.update(b"\0")
    digest.update(np.ascontiguousarray(density).tobytes(order="C"))
    if mask is not None:
        digest.update(np.ascontiguousarray(mask, dtype=np.uint8).tobytes(order="C"))
    return digest.hexdigest()


def load_volume(path: str | os.PathLike[str]) -> VolumeData:
    source = Path(path)
    try:
        file_size = source.stat().st_size
        with source.open("rb") as stream:
            header_bytes = stream.read(HEADER_SIZE)
            header = parse_header(header_bytes)

            density_end = header.density_offset + header.density_bytes
            if density_end > file_size:
                raise VolumeRenderError(
                    ErrorCode.E_HEADER_INVALID,
                    "file is shorter than the density byte range",
                    field="file_size",
                    details={"required": density_end, "actual": file_size},
                )

            if header.has_mask:
                mask_end = header.mask_offset + header.mask_bytes
                if header.mask_offset > file_size:
                    raise VolumeRenderError(
                        ErrorCode.E_MASK_INVALID,
                        "mask offset is outside the file",
                        field="mask_offset",
                        details={"file_size": file_size, "actual": header.mask_offset},
                    )
                if mask_end > file_size:
                    raise VolumeRenderError(
                        ErrorCode.E_MASK_INVALID,
                        "file is shorter than the declared mask range",
                        field="mask_bytes",
                        details={"required": mask_end, "actual": file_size},
                    )
                expected_end = mask_end
            else:
                expected_end = density_end

            if file_size != expected_end:
                raise VolumeRenderError(
                    ErrorCode.E_HEADER_INVALID,
                    "file length does not equal the declared data ranges",
                    field="file_size",
                    details={"expected": expected_end, "actual": file_size},
                )

            _, dtype = SCALAR_TYPES[header.scalar_type_id]
            stream.seek(header.density_offset)
            raw_density_bytes = stream.read(header.density_bytes)
            if len(raw_density_bytes) != header.density_bytes:
                raise VolumeRenderError(
                    ErrorCode.E_HEADER_INVALID,
                    "density data is truncated",
                    field="density_bytes",
                    details={"expected": header.density_bytes, "actual": len(raw_density_bytes)},
                )
            density = np.frombuffer(raw_density_bytes, dtype=dtype).reshape(header.dims).copy()

            mask: np.ndarray | None = None
            if header.has_mask:
                stream.seek(header.mask_offset)
                raw_mask_bytes = stream.read(header.mask_bytes)
                if len(raw_mask_bytes) != header.mask_bytes:
                    raise VolumeRenderError(
                        ErrorCode.E_MASK_INVALID,
                        "mask data is truncated",
                        field="mask_bytes",
                        details={"expected": header.mask_bytes, "actual": len(raw_mask_bytes)},
                    )
                mask = np.frombuffer(raw_mask_bytes, dtype=np.uint8).reshape(header.mask_dims).copy()

            stream.seek(0)
            file_bytes = stream.read()
    except VolumeRenderError:
        raise
    except OSError as exc:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "volume file could not be read",
            field="path",
            details={"path": str(source), "error": str(exc)},
        ) from exc

    density_hash = _hash_bytes(np.ascontiguousarray(density).tobytes(order="C"))
    mask_hash = _hash_bytes(mask.tobytes(order="C")) if mask is not None else ""
    return VolumeData(
        header=header,
        density=density,
        mask=mask,
        path=source,
        file_sha256=_hash_bytes(file_bytes),
        content_sha256=_compute_content_hash(header, density, mask),
        density_sha256=density_hash,
        mask_sha256=mask_hash,
    )


def pack_header(header: VolumeHeader) -> bytes:
    if header.scalar_type_id not in SCALAR_TYPES:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "unsupported scalar type",
            field="scalar_type",
        )
    mask_nx, mask_ny, mask_nz = header.mask_dims if header.has_mask else (0, 0, 0)
    return _HEADER_STRUCT.pack(
        MAGIC,
        header.version,
        header.header_size,
        header.scalar_type_id,
        header.flags,
        *header.dims,
        *header.spacing,
        header.window_center,
        header.window_width,
        header.density_offset,
        header.density_bytes,
        header.mask_offset,
        header.mask_bytes,
        mask_nx,
        mask_ny,
        mask_nz,
        header.voxel_count,
    )


def write_volume(
    path: str | os.PathLike[str],
    density: np.ndarray,
    spacing: tuple[float, float, float],
    window_center: float,
    window_width: float,
    mask: np.ndarray | None = None,
    *,
    scalar_type: str = "uint16",
) -> VolumeHeader:
    """Write a volume using the exact layout consumed by :func:`load_volume`."""

    density_array = np.asarray(density)
    if density_array.ndim != 3:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "density must be three dimensional",
            field="dims",
        )
    dims = tuple(int(value) for value in density_array.shape)
    scalar_type_id = next((key for key, value in SCALAR_TYPES.items() if value[0] == scalar_type), None)
    if scalar_type_id is None:
        raise VolumeRenderError(
            ErrorCode.E_HEADER_INVALID,
            "unsupported scalar type",
            field="scalar_type",
        )
    center_value, width_value = validate_window(window_center, window_width)
    density_bytes = int(density_array.size) * SCALAR_TYPES[scalar_type_id][1].itemsize
    density_offset = HEADER_SIZE
    mask_offset = 0
    mask_bytes = 0
    mask_dims = (0, 0, 0)
    flags = 0
    if mask is not None:
        mask_array = np.asarray(mask, dtype=np.uint8)
        if mask_array.shape != dims:
            raise VolumeRenderError(
                ErrorCode.E_MASK_INVALID,
                "mask dimensions must equal density dimensions",
                field="mask_dims",
                details={"density_dims": list(dims), "mask_dims": list(mask_array.shape)},
            )
        flags = 1
        mask_offset = density_offset + density_bytes
        mask_bytes = int(mask_array.size)
        mask_dims = dims

    header = VolumeHeader(
        version=FORMAT_VERSION,
        header_size=HEADER_SIZE,
        scalar_type_id=scalar_type_id,
        scalar_type=scalar_type,
        flags=flags,
        dims=dims,
        spacing=tuple(float(value) for value in spacing),
        window_center=center_value,
        window_width=width_value,
        density_offset=density_offset,
        density_bytes=density_bytes,
        mask_offset=mask_offset,
        mask_bytes=mask_bytes,
        mask_dims=mask_dims,
        voxel_count=int(density_array.size),
    )
    # Round-trip through the parser so malformed writer inputs cannot create files.
    parse_header(pack_header(header))
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temp_path = target.with_name(f".{target.name}.tmp-{os.getpid()}")
    try:
        with temp_path.open("wb") as stream:
            stream.write(pack_header(header))
            stream.write(np.ascontiguousarray(density_array, dtype=SCALAR_TYPES[scalar_type_id][1]).tobytes(order="C"))
            if mask is not None:
                stream.write(np.ascontiguousarray(mask, dtype=np.uint8).tobytes(order="C"))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_path, target)
    finally:
        if temp_path.exists():
            temp_path.unlink()
    return header
