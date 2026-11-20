from __future__ import annotations

import binascii
import io
import os
import struct
import tempfile
from pathlib import Path
from typing import Iterable

import numpy as np

from .constants import (
    ATLAS_FORMAT_VERSION,
    ATLAS_MAGIC,
    BIT_DEPTHS,
    CHANNELS,
    COLOR_SPACE_TO_CODE,
    HEADER_SIZE,
    STORAGE_FLOAT16_RGBA,
    TRANSFER_TO_CODE,
    parse_color_code,
    parse_transfer_code,
    require_color_space,
    require_transfer,
)
from .errors import AtlasError, ErrorCode
from .hashing import sha256_bytes
from .model import AtlasHeader, AtlasImage, TileSpec
from .transfer import decode_transfer

HEADER_STRUCT = struct.Struct("<8sHHIIBBBBHHfIIII")


def _invalid(field: str, message: str, **details: object) -> AtlasError:
    return AtlasError(
        ErrorCode.E_ATLAS_INVALID,
        message,
        field=field,
        details=details or None,
    )


def _build_tiles(width: int, height: int, cols: int, rows: int) -> tuple[TileSpec, ...]:
    tiles: list[TileSpec] = []
    for row in range(rows):
        y0 = row * height // rows
        y1 = (row + 1) * height // rows
        for col in range(cols):
            x0 = col * width // cols
            x1 = (col + 1) * width // cols
            tiles.append(
                TileSpec(
                    index=len(tiles),
                    row=row,
                    col=col,
                    x0=x0,
                    y0=y0,
                    x1=x1,
                    y1=y1,
                )
            )
    return tuple(tiles)


def _header_crc(fields: tuple[object, ...]) -> int:
    without_crc = list(fields[:-1]) + [0]
    return binascii.crc32(HEADER_STRUCT.pack(*without_crc)) & 0xFFFFFFFF


def _pack_header(
    *,
    width: int,
    height: int,
    bit_depth: int,
    color_space: str,
    transfer: str,
    grid_cols: int,
    grid_rows: int,
    source_peak_nits: float,
    pipeline_version: int,
    payload_size: int,
    payload_crc32: int,
) -> bytes:
    base = (
        ATLAS_MAGIC,
        ATLAS_FORMAT_VERSION,
        HEADER_SIZE,
        int(width),
        int(height),
        int(bit_depth),
        COLOR_SPACE_TO_CODE[color_space],
        TRANSFER_TO_CODE[transfer],
        STORAGE_FLOAT16_RGBA,
        int(grid_cols),
        int(grid_rows),
        float(source_peak_nits),
        int(pipeline_version),
        int(payload_size),
        int(payload_crc32),
    )
    crc = _header_crc(base + (0,))
    return HEADER_STRUCT.pack(*base, crc)


def _validate_header_values(
    *,
    format_version: int,
    header_size: int,
    width: int,
    height: int,
    bit_depth: int,
    color_code: int,
    transfer_code: int,
    storage_code: int,
    grid_cols: int,
    grid_rows: int,
    source_peak_nits: float,
    payload_size: int,
) -> tuple[str, str]:
    if format_version != ATLAS_FORMAT_VERSION:
        raise _invalid(
            "format_version",
            f"unsupported atlas format version {format_version}",
            expected=ATLAS_FORMAT_VERSION,
        )
    if header_size != HEADER_SIZE:
        raise _invalid("header_size", f"header_size must be {HEADER_SIZE}", value=header_size)
    if width <= 0:
        raise _invalid("width", "width must be positive", value=width)
    if height <= 0:
        raise _invalid("height", "height must be positive", value=height)
    if bit_depth not in BIT_DEPTHS:
        raise _invalid(
            "bit_depth",
            f"bit_depth must be one of {list(BIT_DEPTHS)}",
            value=bit_depth,
        )
    color_space = parse_color_code(color_code)
    transfer = parse_transfer_code(transfer_code)
    if storage_code != STORAGE_FLOAT16_RGBA:
        raise _invalid(
            "storage_format",
            "storage_format must identify RGBA16F half-float data",
            value=storage_code,
        )
    if grid_cols <= 0:
        raise _invalid("grid_cols", "grid_cols must be positive", value=grid_cols)
    if grid_rows <= 0:
        raise _invalid("grid_rows", "grid_rows must be positive", value=grid_rows)
    if width % grid_cols != 0:
        raise _invalid(
            "grid_cols",
            "tile grid columns must evenly divide width",
            value=grid_cols,
            width=width,
        )
    if height % grid_rows != 0:
        raise _invalid(
            "grid_rows",
            "tile grid rows must evenly divide height",
            value=grid_rows,
            height=height,
        )
    if not np.isfinite(source_peak_nits) or not 1.0 <= source_peak_nits <= 10000.0:
        raise _invalid(
            "source_peak_nits",
            "source_peak_nits must be within [1, 10000]",
            value=source_peak_nits,
        )
    expected = width * height * CHANNELS * 2
    if payload_size != expected:
        raise _invalid(
            "payload_size",
            "header payload_size does not match width, height, and RGBA16F channels",
            value=payload_size,
            expected=expected,
        )
    return color_space, transfer


def decode_atlas_bytes(data: bytes, *, source_name: str = "<bytes>") -> AtlasImage:
    if len(data) < HEADER_SIZE:
        raise _invalid("header", "file is shorter than the fixed header", size=len(data))
    fields = HEADER_STRUCT.unpack_from(data, 0)
    (
        magic,
        format_version,
        header_size,
        width,
        height,
        bit_depth,
        color_code,
        transfer_code,
        storage_code,
        grid_cols,
        grid_rows,
        source_peak_nits,
        pipeline_version,
        payload_size,
        payload_crc32,
        header_crc32,
    ) = fields
    if magic != ATLAS_MAGIC:
        raise _invalid("magic", "atlas magic does not match ATLSHDR1", value=magic.hex())
    expected_crc = _header_crc(fields)
    if header_crc32 != expected_crc:
        raise _invalid(
            "header_crc32",
            "header CRC mismatch; the header is corrupt",
            value=f"{header_crc32:08x}",
            expected=f"{expected_crc:08x}",
        )
    color_space, transfer = _validate_header_values(
        format_version=format_version,
        header_size=header_size,
        width=width,
        height=height,
        bit_depth=bit_depth,
        color_code=color_code,
        transfer_code=transfer_code,
        storage_code=storage_code,
        grid_cols=grid_cols,
        grid_rows=grid_rows,
        source_peak_nits=source_peak_nits,
        payload_size=payload_size,
    )
    actual_payload_size = len(data) - HEADER_SIZE
    if actual_payload_size != payload_size:
        raise _invalid(
            "payload_size",
            "actual payload length does not match header payload_size",
            value=actual_payload_size,
            expected=payload_size,
        )
    payload = data[HEADER_SIZE:]
    actual_crc = binascii.crc32(payload) & 0xFFFFFFFF
    if actual_crc != payload_crc32:
        raise _invalid(
            "payload_crc32",
            "payload CRC mismatch; channel data is corrupt",
            value=f"{actual_crc:08x}",
            expected=f"{payload_crc32:08x}",
        )
    encoded = np.frombuffer(payload, dtype="<f2").reshape(height, width, CHANNELS).copy()
    if not np.all(np.isfinite(encoded)):
        raise _invalid("pixels", "channel data contains non-finite half floats")
    linear = np.array(encoded, dtype=np.float32, copy=True)
    linear[..., :3] = decode_transfer(
        encoded[..., :3].astype(np.float64),
        transfer,
        float(source_peak_nits),
    ).astype(np.float32)
    header = AtlasHeader(
        format_version=format_version,
        width=width,
        height=height,
        bit_depth=bit_depth,
        color_space=color_space,
        transfer_function=transfer,
        storage_format="rgba16f",
        grid_cols=grid_cols,
        grid_rows=grid_rows,
        source_peak_nits=float(source_peak_nits),
        pipeline_version=pipeline_version,
        payload_size=payload_size,
        payload_crc32=payload_crc32,
        header_crc32=header_crc32,
    )
    tiles = _build_tiles(width, height, grid_cols, grid_rows)
    return AtlasImage(
        header=header,
        encoded=encoded,
        linear=linear,
        tiles=tiles,
        source_hash=sha256_bytes(data),
        payload_hash=sha256_bytes(payload),
        raw_bytes=bytes(data),
    )


def read_atlas(path: str | Path) -> AtlasImage:
    try:
        data = Path(path).read_bytes()
    except OSError as exc:
        raise AtlasError(
            ErrorCode.E_IO,
            f"cannot read atlas: {exc}",
            field="path",
            details={"path": str(path)},
        ) from None
    return decode_atlas_bytes(data, source_name=str(path))


def decode_atlas(path: str | Path) -> AtlasImage:
    return read_atlas(path)


def encode_atlas_bytes(
    encoded: np.ndarray,
    *,
    bit_depth: int,
    color_space: str,
    transfer: str,
    grid_cols: int,
    grid_rows: int,
    source_peak_nits: float,
    pipeline_version: int = 0,
    width: int | None = None,
    height: int | None = None,
) -> bytes:
    require_color_space(color_space)
    require_transfer(transfer)
    array = np.asarray(encoded)
    if array.ndim != 3 or array.shape[2] != CHANNELS:
        raise _invalid(
            "pixels",
            "encoded channels must have shape (height, width, 4)",
            shape=list(array.shape),
        )
    resolved_height = int(height if height is not None else array.shape[0])
    resolved_width = int(width if width is not None else array.shape[1])
    if array.shape[0] != resolved_height or array.shape[1] != resolved_width:
        raise _invalid(
            "pixels",
            "provided dimensions do not match the encoded channel array",
            shape=list(array.shape),
            width=resolved_width,
            height=resolved_height,
        )
    if bit_depth not in BIT_DEPTHS:
        raise _invalid("bit_depth", f"bit_depth must be one of {list(BIT_DEPTHS)}")
    if grid_cols <= 0 or grid_rows <= 0:
        raise _invalid("grid", "tile grid dimensions must be positive")
    if resolved_width % grid_cols != 0:
        raise _invalid("grid_cols", "tile grid columns must evenly divide width")
    if resolved_height % grid_rows != 0:
        raise _invalid("grid_rows", "tile grid rows must evenly divide height")
    if not np.all(np.isfinite(array)):
        raise _invalid("pixels", "encoded channel data contains non-finite values")
    if not np.isfinite(source_peak_nits) or not 1.0 <= source_peak_nits <= 10000.0:
        raise _invalid("source_peak_nits", "source_peak_nits must be within [1, 10000]")
    payload = np.ascontiguousarray(array, dtype="<f2").tobytes(order="C")
    payload_crc = binascii.crc32(payload) & 0xFFFFFFFF
    header = _pack_header(
        width=resolved_width,
        height=resolved_height,
        bit_depth=bit_depth,
        color_space=color_space,
        transfer=transfer,
        grid_cols=grid_cols,
        grid_rows=grid_rows,
        source_peak_nits=float(source_peak_nits),
        pipeline_version=int(pipeline_version),
        payload_size=len(payload),
        payload_crc32=payload_crc,
    )
    return header + payload


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{target.name}.",
        suffix=".tmp",
        dir=str(target.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
        try:
            dir_fd = os.open(target.parent, os.O_RDONLY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
        except OSError:
            pass
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def write_atlas(
    path: str | Path,
    encoded: np.ndarray,
    *,
    bit_depth: int,
    color_space: str,
    transfer: str,
    grid_cols: int,
    grid_rows: int,
    source_peak_nits: float,
    pipeline_version: int = 0,
) -> str:
    data = encode_atlas_bytes(
        encoded,
        bit_depth=bit_depth,
        color_space=color_space,
        transfer=transfer,
        grid_cols=grid_cols,
        grid_rows=grid_rows,
        source_peak_nits=source_peak_nits,
        pipeline_version=pipeline_version,
    )
    atomic_write_bytes(path, data)
    return sha256_bytes(data)
