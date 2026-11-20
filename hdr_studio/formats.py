from __future__ import annotations

import struct
import zlib
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import ErrorCode, HDRError
from .model import (
    COLOR_SPACE_CODES,
    COLOR_SPACE_TO_CODE,
    TRANSFER_CODES,
    TRANSFER_TO_CODE,
    ColorSpace,
    TransferFunction,
)
from .util import sha256_hex

MAGIC = b"HDRSEQ01"
HEADER_BODY_STRUCT = struct.Struct("<8sHIIIIHBBfQ")
HEADER_STRUCT = struct.Struct("<8sHIIIIHBBfQI")
HEADER_SIZE = HEADER_STRUCT.size
CURRENT_FORMAT_VERSION = 1
VALID_BIT_DEPTHS = frozenset({8, 10, 12, 16})


def bytes_per_sample(bit_depth: int) -> int:
    return 1 if bit_depth <= 8 else 2


def expected_frame_bytes(width: int, height: int, bit_depth: int) -> int:
    return width * height * 3 * bytes_per_sample(bit_depth)


def _invalid(message: str, **details: object) -> HDRError:
    return HDRError(ErrorCode.HDR_INVALID, message, details=dict(details))


def build_header(
    *,
    width: int,
    height: int,
    frame_count: int,
    bit_depth: int,
    color_space: ColorSpace,
    transfer: TransferFunction,
    peak_luminance: float,
) -> bytes:
    frame_bytes = expected_frame_bytes(width, height, bit_depth)
    body = HEADER_BODY_STRUCT.pack(
        MAGIC,
        CURRENT_FORMAT_VERSION,
        HEADER_SIZE,
        width,
        height,
        frame_count,
        bit_depth,
        COLOR_SPACE_TO_CODE[color_space],
        TRANSFER_TO_CODE[transfer],
        float(peak_luminance),
        frame_bytes,
    )
    return body + struct.pack("<I", zlib.crc32(body) & 0xFFFFFFFF)


@dataclass(frozen=True, slots=True)
class HDRHeader:
    width: int
    height: int
    frame_count: int
    bit_depth: int
    color_space: ColorSpace
    transfer: TransferFunction
    peak_luminance: float
    frame_bytes: int
    header_bytes: bytes

    @property
    def header_hash(self) -> str:
        return sha256_hex(self.header_bytes)

    @property
    def max_code(self) -> int:
        return (1 << self.bit_depth) - 1


class HDRSequence:
    """Validated reader for the custom little-endian HDR sequence format."""

    def __init__(self, path: Path | str) -> None:
        self.path = Path(path)
        self.header = self._parse_and_validate()

    def _parse_and_validate(self) -> HDRHeader:
        try:
            file_size = self.path.stat().st_size
        except OSError as exc:
            raise _invalid(f"cannot stat HDR sequence: {exc}") from exc
        if file_size < HEADER_SIZE:
            raise _invalid(
                "header is incomplete", file_size=file_size, required_bytes=HEADER_SIZE
            )
        try:
            with self.path.open("rb") as handle:
                header_bytes = handle.read(HEADER_SIZE)
        except OSError as exc:
            raise _invalid(f"cannot read HDR sequence: {exc}") from exc
        if len(header_bytes) != HEADER_SIZE:
            raise _invalid("header is incomplete", bytes_read=len(header_bytes))

        (
            magic,
            version,
            header_size,
            width,
            height,
            frame_count,
            bit_depth,
            color_code,
            transfer_code,
            peak_luminance,
            frame_bytes,
        ) = HEADER_BODY_STRUCT.unpack(header_bytes[:-4])
        stored_crc = struct.unpack("<I", header_bytes[-4:])[0]
        actual_crc = zlib.crc32(header_bytes[:-4]) & 0xFFFFFFFF

        if magic != MAGIC:
            raise _invalid("header magic is invalid", magic=magic.hex())
        if version != CURRENT_FORMAT_VERSION:
            raise _invalid("unsupported HDR sequence version", version=version)
        if header_size != HEADER_SIZE:
            raise _invalid(
                "header field is missing or header size is unsupported",
                header_size=header_size,
                expected=HEADER_SIZE,
            )
        if stored_crc != actual_crc:
            raise _invalid("header checksum mismatch")
        if width <= 0 or height <= 0 or frame_count <= 0:
            raise _invalid(
                "dimensions and frame count must be positive",
                width=width,
                height=height,
                frame_count=frame_count,
            )
        if bit_depth not in VALID_BIT_DEPTHS:
            raise _invalid("unsupported bit depth", bit_depth=bit_depth)
        if color_code not in COLOR_SPACE_CODES:
            raise _invalid("unknown color space code", color_space_code=color_code)
        if transfer_code not in TRANSFER_CODES:
            raise _invalid("unknown transfer function code", transfer_code=transfer_code)
        if not np.isfinite(peak_luminance) or peak_luminance <= 0:
            raise _invalid("peak luminance must be finite and positive", peak=peak_luminance)

        expected_frame_size = expected_frame_bytes(width, height, bit_depth)
        if frame_bytes != expected_frame_size:
            raise _invalid(
                "declared frame data length does not match bit depth and dimensions",
                declared_frame_bytes=frame_bytes,
                expected_frame_bytes=expected_frame_size,
            )
        expected_total = HEADER_SIZE + frame_count * frame_bytes
        if file_size != expected_total:
            raise _invalid(
                "file length does not match declared frame count and frame size",
                file_size=file_size,
                expected_file_size=expected_total,
            )
        return HDRHeader(
            width=width,
            height=height,
            frame_count=frame_count,
            bit_depth=bit_depth,
            color_space=COLOR_SPACE_CODES[color_code],
            transfer=TRANSFER_CODES[transfer_code],
            peak_luminance=float(peak_luminance),
            frame_bytes=frame_bytes,
            header_bytes=header_bytes,
        )

    @property
    def max_code(self) -> int:
        return self.header.max_code

    def read_frame(self, frame_index: int) -> np.ndarray:
        header = self.header
        if frame_index < 0 or frame_index >= header.frame_count:
            raise _invalid(
                "frame index is outside the sequence",
                frame_index=frame_index,
                frame_count=header.frame_count,
            )
        offset = HEADER_SIZE + frame_index * header.frame_bytes
        try:
            with self.path.open("rb") as handle:
                handle.seek(offset)
                raw = handle.read(header.frame_bytes)
        except OSError as exc:
            raise _invalid(f"cannot read frame {frame_index}: {exc}") from exc
        if len(raw) != header.frame_bytes:
            raise _invalid(
                "frame data is truncated",
                frame_index=frame_index,
                bytes_read=len(raw),
                expected=header.frame_bytes,
            )
        dtype = np.uint8 if bytes_per_sample(header.bit_depth) == 1 else np.dtype("<u2")
        values = np.frombuffer(raw, dtype=dtype).copy()
        if header.bit_depth < 16 and values.size and int(values.max()) > header.max_code:
            raise _invalid(
                "sample value exceeds declared bit depth",
                frame_index=frame_index,
                max_code=header.max_code,
                found=int(values.max()),
            )
        return values.reshape((header.height, header.width, 3))

    def read_frame_bytes(self, frame_index: int) -> bytes:
        header = self.header
        if frame_index < 0 or frame_index >= header.frame_count:
            raise _invalid("frame index is outside the sequence", frame_index=frame_index)
        offset = HEADER_SIZE + frame_index * header.frame_bytes
        try:
            with self.path.open("rb") as handle:
                handle.seek(offset)
                raw = handle.read(header.frame_bytes)
        except OSError as exc:
            raise _invalid(f"cannot read frame {frame_index}: {exc}") from exc
        if len(raw) != header.frame_bytes:
            raise _invalid("frame data is truncated", frame_index=frame_index)
        return raw

    def close(self) -> None:
        """Compatibility no-op: readers never retain file handles."""

