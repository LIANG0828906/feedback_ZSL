"""Loader and validator for the local SVOL v1 binary volume format.

Header layout (little endian, 128 bytes):

======  ======  =====================================================
offset  size    field
======  ======  =====================================================
0       4       magic ``SVOL``
4       2       format version (1)
6       2       header size (128)
8       12      dimensions x, y, z (uint32 each)
20      1       scalar type code
21      1       mask type code
22      2       flags/reserved
24      24      spacing x, y, z (float64 each)
48      8       window width (float64)
56      8       window level (float64)
64      8       absolute mask offset (uint64)
72      4       mask component count
76      12      reserved (three uint32)
88      8       scalar normalization reference maximum
96      32      reserved
======  ======  =====================================================

The scalar plane starts at ``header_size`` and the mask plane starts at
``mask_offset``.  A valid v1 file has no trailing bytes.
"""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import math
from pathlib import Path
import struct
from typing import Any, BinaryIO

import numpy as np

from .errors import ResourceError, VolumeInvalidError


MAGIC = b"SVOL"
FORMAT_VERSION = 1
HEADER_SIZE = 128
MASK_OFFSET_OFFSET = 64
SCALAR_REFERENCE_OFFSET = 88
SPACING_OFFSET = 24
WINDOW_WIDTH_OFFSET = 48
WINDOW_LEVEL_OFFSET = 56

# Kept deliberately explicit: an unknown code is a hard validation failure,
# never an implicit conversion to a different width.
SCALAR_TYPES: dict[int, tuple[np.dtype[Any], str]] = {
    1: (np.dtype("<u1"), "uint8"),
    2: (np.dtype("<u2"), "uint16"),
    3: (np.dtype("<u4"), "uint32"),
    4: (np.dtype("<i1"), "int8"),
    5: (np.dtype("<i2"), "int16"),
    6: (np.dtype("<f4"), "float32"),
    7: (np.dtype("<f8"), "float64"),
}

_HEADER_STRUCT = struct.Struct("<4sHHIIIBBHdddddQIIIId32s")


@dataclass(frozen=True)
class SVOLHeader:
    magic: bytes
    version: int
    header_size: int
    dims: tuple[int, int, int]
    scalar_type_code: int
    mask_type_code: int
    flags: int
    mask_components: int
    spacing: tuple[float, float, float]
    window_width: float
    window_level: float
    mask_offset: int
    scalar_reference_max: float

    @property
    def shape_xyz(self) -> tuple[int, int, int]:
        return self.dims

    @property
    def scalar_dtype(self) -> np.dtype[Any]:
        return SCALAR_TYPES[self.scalar_type_code][0]

    @property
    def mask_dtype(self) -> np.dtype[Any]:
        return SCALAR_TYPES[self.mask_type_code][0]

    @property
    def scalar_type_name(self) -> str:
        return SCALAR_TYPES[self.scalar_type_code][1]

    @property
    def mask_type_name(self) -> str:
        return SCALAR_TYPES[self.mask_type_code][1]

    @property
    def voxel_count(self) -> int:
        return int(self.dims[0]) * int(self.dims[1]) * int(self.dims[2])

    @property
    def scalar_nbytes(self) -> int:
        return self.voxel_count * self.scalar_dtype.itemsize

    @property
    def mask_nbytes(self) -> int:
        return self.voxel_count * self.mask_dtype.itemsize

    @property
    def scalar_end(self) -> int:
        return self.header_size + self.scalar_nbytes

    @property
    def expected_file_size(self) -> int:
        return self.mask_offset + self.mask_nbytes

    @property
    def window_low(self) -> float:
        return self.window_level - self.window_width / 2.0

    @property
    def window_high(self) -> float:
        return self.window_level + self.window_width / 2.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "dims": list(self.dims),
            "header_size": self.header_size,
            "magic": self.magic.decode("ascii", errors="replace"),
            "mask_offset": self.mask_offset,
            "mask_type": self.mask_type_name,
            "scalar_reference_max": self.scalar_reference_max,
            "scalar_type": self.scalar_type_name,
            "spacing": list(self.spacing),
            "version": self.version,
            "window_high": self.window_high,
            "window_level": self.window_level,
            "window_low": self.window_low,
            "window_width": self.window_width,
        }


@dataclass
class Volume:
    header: SVOLHeader
    scalar: np.ndarray
    mask: np.ndarray
    normalization: dict[str, Any]
    source: str
    volume_sha256: str

    @property
    def dims(self) -> tuple[int, int, int]:
        return self.header.dims

    @property
    def spacing(self) -> tuple[float, float, float]:
        return self.header.spacing

    @property
    def resident_bytes(self) -> int:
        return int(self.scalar.nbytes + self.mask.nbytes)

    def close(self) -> None:
        # Arrays are owned copies; this method exists so callers can use a
        # context manager or release references explicitly before shutdown.
        del self.scalar
        del self.mask


def parse_header(data: bytes | bytearray | memoryview | BinaryIO) -> SVOLHeader:
    """Parse and validate the header without reading the voxel payload."""

    if hasattr(data, "read"):
        handle = data  # type: ignore[assignment]
        raw = handle.read(HEADER_SIZE)
    else:
        raw = bytes(data[:HEADER_SIZE])
    if len(raw) < HEADER_SIZE:
        raise VolumeInvalidError(
            "DATA_TRUNCATED",
            f"头部截断: 需要 {HEADER_SIZE} 字节, 实际 {len(raw)} 字节",
            field="header",
            offset=len(raw),
        )
    fields = _HEADER_STRUCT.unpack(raw)
    (
        magic,
        version,
        header_size,
        dim_x,
        dim_y,
        dim_z,
        scalar_code,
        mask_code,
        flags,
        spacing_x,
        spacing_y,
        spacing_z,
        window_width,
        window_level,
        mask_offset,
        mask_components,
        _reserved_a,
        _reserved_b,
        _reserved_c,
        scalar_reference,
        _reserved,
    ) = fields

    if magic != MAGIC:
        raise VolumeInvalidError(
            "BAD_MAGIC",
            f"头部 magic={magic!r}, 期望 {MAGIC!r}",
            field="magic",
            offset=0,
        )
    if version != FORMAT_VERSION:
        raise VolumeInvalidError(
            "UNSUPPORTED_VERSION",
            f"不支持的 SVOL 版本 {version}, 仅支持 {FORMAT_VERSION}",
            field="version",
            offset=4,
        )
    if header_size < HEADER_SIZE:
        raise VolumeInvalidError(
            "BAD_HEADER_SIZE",
            f"header_size={header_size} 小于最小头部 {HEADER_SIZE}",
            field="header_size",
            offset=6,
        )
    dims = (dim_x, dim_y, dim_z)
    if any(dim <= 0 for dim in dims):
        raise VolumeInvalidError(
            "BAD_DIMENSIONS",
            f"维度必须为正: {dims}",
            field="dims",
            offset=8,
        )
    if scalar_code not in SCALAR_TYPES:
        raise VolumeInvalidError(
            "UNKNOWN_SCALAR_TYPE",
            f"未知标量类型代码 {scalar_code}",
            field="scalar_type",
            offset=20,
        )
    if mask_code not in SCALAR_TYPES:
        raise VolumeInvalidError(
            "UNKNOWN_MASK_TYPE",
            f"未知掩码类型代码 {mask_code}",
            field="mask_type",
            offset=21,
        )
    spacings = (spacing_x, spacing_y, spacing_z)
    for index, value in enumerate(spacings):
        if not math.isfinite(value) or value <= 0.0:
            raise VolumeInvalidError(
                "BAD_SPACING",
                f"体素间距必须为有限正数: spacing[{index}]={value!r}",
                field=f"spacing[{index}]",
                offset=SPACING_OFFSET + index * 8,
            )
    if not math.isfinite(window_width) or window_width <= 0.0:
        raise VolumeInvalidError(
            "WINDOW_NON_FINITE",
            f"窗宽窗位非有限或窗宽非正: width={window_width!r} level={window_level!r}",
            field="window_width",
            offset=WINDOW_WIDTH_OFFSET,
        )
    if not math.isfinite(window_level):
        raise VolumeInvalidError(
            "WINDOW_NON_FINITE",
            f"窗宽窗位非有限或窗宽非正: width={window_width!r} level={window_level!r}",
            field="window_level",
            offset=WINDOW_LEVEL_OFFSET,
        )
    if not math.isfinite(scalar_reference):
        raise VolumeInvalidError(
            "NON_FINITE_REFERENCE",
            f"标量归一化参考值非有限: {scalar_reference!r}",
            field="scalar_reference_max",
            offset=SCALAR_REFERENCE_OFFSET,
        )
    count = dim_x * dim_y * dim_z
    scalar_bytes = count * SCALAR_TYPES[scalar_code][0].itemsize
    scalar_end = header_size + scalar_bytes
    if mask_offset < scalar_end:
        raise VolumeInvalidError(
            "MASK_OFFSET_CONFLICT",
            f"mask_offset={mask_offset} 与标量数据区间 [{header_size},{scalar_end}) 重叠",
            field="mask_offset",
            offset=MASK_OFFSET_OFFSET,
        )
    return SVOLHeader(
        magic=magic,
        version=version,
        header_size=header_size,
        dims=dims,
        scalar_type_code=scalar_code,
        mask_type_code=mask_code,
        flags=flags,
        mask_components=mask_components,
        spacing=spacings,
        window_width=window_width,
        window_level=window_level,
        mask_offset=int(mask_offset),
        scalar_reference_max=scalar_reference,
    )


def validate_payload(header: SVOLHeader, actual_size: int) -> None:
    if actual_size < header.header_size:
        raise VolumeInvalidError(
            "DATA_TRUNCATED",
            f"头部截断: header_size={header.header_size}, 实际文件 {actual_size} 字节",
            field="header",
            offset=actual_size,
        )
    if actual_size < header.scalar_end:
        count = header.voxel_count
        raise VolumeInvalidError(
            "DATA_TRUNCATED",
            (
                f"体素数据截断: 需要 {header.scalar_end} 字节, 实际 {actual_size} 字节 "
                f"(维度乘积 {header.dims[0]}x{header.dims[1]}x{header.dims[2]}="
                f"{count}, 类型 {header.scalar_type_name})"
            ),
            field="voxel_data",
            offset=actual_size,
        )
    if actual_size < header.expected_file_size:
        raise VolumeInvalidError(
            "DATA_TRUNCATED",
            (
                f"掩码数据截断: 需要文件总长 {header.expected_file_size} 字节, "
                f"实际 {actual_size} 字节 (mask_offset={header.mask_offset}, "
                f"类型 {header.mask_type_name})"
            ),
            field="mask_data",
            offset=actual_size,
        )
    if actual_size != header.expected_file_size:
        raise VolumeInvalidError(
            "DIM_LENGTH_MISMATCH",
            (
                f"数据长度 {actual_size} 与头部声明的维度乘积不一致: "
                f"期望文件总长 {header.expected_file_size} 字节"
            ),
            field="data_length",
            offset=header.expected_file_size,
        )


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _normalize_scalar(raw: np.ndarray, header: SVOLHeader) -> np.ndarray:
    if raw.dtype.kind == "f":
        invalid = np.flatnonzero(~np.isfinite(raw))
        if invalid.size:
            offset = header.header_size + int(invalid[0]) * raw.dtype.itemsize
            raise VolumeInvalidError(
                "NON_FINITE_VOXEL",
                f"标量体素包含非有限值: 首个位置 {int(invalid[0])}",
                field="voxel_data",
                offset=offset,
            )
    low = header.window_low
    width = header.window_width
    # float64 intermediate avoids overflow/rounding surprises for uint32 data.
    values = (raw.astype(np.float64) - low) / width
    return np.clip(values, 0.0, 1.0).astype(np.float32, copy=False)


def _normalize_mask(raw: np.ndarray, *, base_offset: int) -> tuple[np.ndarray, float]:
    if raw.size == 0:
        return np.zeros(raw.shape, dtype=np.float32), 0.0
    if raw.dtype.kind == "f":
        invalid = np.flatnonzero(~np.isfinite(raw))
        if invalid.size:
            raise VolumeInvalidError(
                "NON_FINITE_MASK",
                f"掩码包含非有限值: 首个位置 {int(invalid[0])}",
                field="mask_data",
                offset=base_offset + int(invalid[0]) * raw.dtype.itemsize,
            )
    if float(np.min(raw)) < 0.0:
        invalid = np.flatnonzero(raw < 0)
        raise VolumeInvalidError(
            "NEGATIVE_MASK_LABEL",
            f"掩码包含负标签: {int(raw[invalid[0]])}",
            field="mask_data",
            offset=base_offset + int(invalid[0]) * raw.dtype.itemsize,
        )
    max_label = float(np.max(raw))
    if not math.isfinite(max_label) or max_label < 0.0:
        raise VolumeInvalidError(
            "INVALID_MASK_RANGE",
            f"掩码最大值非法: {max_label!r}",
            field="mask_data",
            offset=0,
        )
    if max_label == 0.0:
        return np.zeros(raw.shape, dtype=np.float32), 0.0
    return (np.clip(raw.astype(np.float64), 0.0, max_label) / max_label).astype(
        np.float32, copy=False
    ), max_label


def load_volume(
    source: str | Path | bytes | bytearray | memoryview,
    *,
    source_name: str | None = None,
    window_override: tuple[float, float] | None = None,
) -> Volume:
    """Load, validate and normalize a complete SVOL file.

    No image output happens here.  Every validation failure is raised before
    voxel arrays are returned to the caller.
    """

    if isinstance(source, (bytes, bytearray, memoryview)):
        data = bytes(source)
        header = parse_header(data)
        actual_size = len(data)
        validate_payload(header, actual_size)
        scalar_raw = np.frombuffer(
            data, dtype=header.scalar_dtype, count=header.voxel_count, offset=header.header_size
        )
        mask_raw = np.frombuffer(
            data, dtype=header.mask_dtype, count=header.voxel_count, offset=header.mask_offset
        )
        name = source_name or "<memory>"
        volume_hash = _sha256_bytes(data)
    else:
        path = Path(source)
        name = source_name or str(path)
        try:
            file_size = path.stat().st_size
            with path.open("rb") as handle:
                header = parse_header(handle)
            validate_payload(header, file_size)
        except OSError as exc:
            raise ResourceError("READ_FAILED", f"无法读取体数据 {path}: {exc}") from exc
        try:
            scalar_raw = np.fromfile(
                path, dtype=header.scalar_dtype, count=header.voxel_count, offset=header.header_size
            )
            mask_raw = np.fromfile(
                path, dtype=header.mask_dtype, count=header.voxel_count, offset=header.mask_offset
            )
        except (OSError, ValueError) as exc:
            raise ResourceError("READ_FAILED", f"读取体素数据失败 {path}: {exc}") from exc
        if scalar_raw.size != header.voxel_count or mask_raw.size != header.voxel_count:
            raise VolumeInvalidError(
                "DATA_TRUNCATED",
                "读取过程中体素数量不足",
                field="voxel_data",
                offset=min(scalar_raw.size, mask_raw.size) * header.scalar_dtype.itemsize,
            )
        volume_hash = _sha256_file(path)

    active_header = header
    if window_override is not None:
        override_width, override_level = (float(window_override[0]), float(window_override[1]))
        if not math.isfinite(override_width) or override_width <= 0.0:
            raise VolumeInvalidError(
                "WINDOW_NON_FINITE",
                f"窗宽必须为有限正数: {override_width!r}",
                field="window_width",
                offset=WINDOW_WIDTH_OFFSET,
            )
        if not math.isfinite(override_level):
            raise VolumeInvalidError(
                "WINDOW_NON_FINITE",
                f"窗位必须为有限数: {override_level!r}",
                field="window_level",
                offset=WINDOW_LEVEL_OFFSET,
            )
        active_header = SVOLHeader(
            **{
                **header.__dict__,
                "window_width": override_width,
                "window_level": override_level,
            }
        )
    scalar = _normalize_scalar(scalar_raw, active_header).reshape(header.dims)
    mask, max_label = _normalize_mask(mask_raw, base_offset=header.mask_offset)
    mask = mask.reshape(header.dims)
    normalization = {
        "scalar": {
            "reference_max": header.scalar_reference_max,
            "source": (
                "render.window_override"
                if window_override is not None
                else "header.window_width/window_level"
            ),
            "window_high": active_header.window_high,
            "window_low": active_header.window_low,
            "truth_source": (
                "caller-supplied preview window"
                if window_override is not None
                else "SVOL header window fields"
            ),
        },
        "mask": {
            "max_label": max_label,
            "source": "data.max(mask)",
            "truth_source": "raw segmentation labels",
        },
    }
    return Volume(
        header=active_header,
        scalar=scalar,
        mask=mask,
        normalization=normalization,
        source=name,
        volume_sha256=volume_hash,
    )


def build_header(
    *,
    dims: tuple[int, int, int],
    spacing: tuple[float, float, float],
    window_width: float,
    window_level: float,
    scalar_type_code: int = 2,
    mask_type_code: int = 1,
    scalar_reference_max: float = 65535.0,
    mask_offset: int | None = None,
) -> bytes:
    """Create a canonical 128-byte SVOL v1 header."""

    if scalar_type_code not in SCALAR_TYPES or mask_type_code not in SCALAR_TYPES:
        raise VolumeInvalidError(
            "UNKNOWN_SCALAR_TYPE",
            "写头时使用了未知标量/掩码类型",
            field="scalar_type",
            offset=20,
        )
    count = int(dims[0]) * int(dims[1]) * int(dims[2])
    scalar_bytes = count * SCALAR_TYPES[scalar_type_code][0].itemsize
    if mask_offset is None:
        mask_offset = HEADER_SIZE + scalar_bytes
    flags = 0
    fields = (
        MAGIC,
        FORMAT_VERSION,
        HEADER_SIZE,
        int(dims[0]),
        int(dims[1]),
        int(dims[2]),
        int(scalar_type_code),
        int(mask_type_code),
        flags,
        float(spacing[0]),
        float(spacing[1]),
        float(spacing[2]),
        float(window_width),
        float(window_level),
        int(mask_offset),
        1,
        0,
        0,
        0,
        float(scalar_reference_max),
        b"\x00" * 32,
    )
    return _HEADER_STRUCT.pack(*fields)

