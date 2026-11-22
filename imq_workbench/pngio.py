"""Small deterministic PNG codec plus validated NumPy image loading.

The codec deliberately supports only 8-bit, non-interlaced grayscale and RGB
PNGs. This is sufficient for generated deterministic fixtures and keeps the
runtime independent of GUI, Pillow, or network-installed image codecs.
"""

from __future__ import annotations

import struct
import zlib
from pathlib import Path
from typing import Union

import numpy as np

from .errors import WorkbenchError

PathLike = Union[str, Path]
_PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


def _chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    )


def write_png(path: PathLike, image: np.ndarray) -> None:
    array = np.asarray(image)
    if array.dtype != np.uint8:
        raise WorkbenchError("E_PARA", "PNG output requires uint8 pixels", {"field": "image"})
    if array.ndim == 2:
        height, width = array.shape
        channels = 1
        color_type = 0
        rows = array
    elif array.ndim == 3 and array.shape[2] == 3:
        height, width, channels = array.shape
        color_type = 2
        rows = array
    else:
        raise WorkbenchError(
            "E_PARA",
            "PNG output supports grayscale (H,W) or RGB (H,W,3)",
            {"field": "image", "shape": list(array.shape)},
        )

    ihdr = struct.pack(">IIBBBBB", width, height, 8, color_type, 0, 0, 0)
    filtered_rows = []
    for row in rows:
        filtered_rows.append(b"\x00")
        filtered_rows.append(np.ascontiguousarray(row).tobytes())
    compressed = zlib.compress(b"".join(filtered_rows), level=6)
    payload = _PNG_SIGNATURE + _chunk(b"IHDR", ihdr) + _chunk(b"IDAT", compressed) + _chunk(b"IEND", b"")

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temp = target.with_name(target.name + ".tmp")
    temp.write_bytes(payload)
    temp.replace(target)


def _paeth(left: int, above: int, upper_left: int) -> int:
    estimate = left + above - upper_left
    dist_left = abs(estimate - left)
    dist_above = abs(estimate - above)
    dist_upper_left = abs(estimate - upper_left)
    if dist_left <= dist_above and dist_left <= dist_upper_left:
        return left
    if dist_above <= dist_upper_left:
        return above
    return upper_left


def _unfilter_scanline(filter_type: int, filtered: bytes, previous: bytes, bpp: int) -> bytes:
    result = bytearray(len(filtered))
    for index, value in enumerate(filtered):
        left = result[index - bpp] if index >= bpp else 0
        above = previous[index] if previous else 0
        upper_left = previous[index - bpp] if previous and index >= bpp else 0
        if filter_type == 0:
            predictor = 0
        elif filter_type == 1:
            predictor = left
        elif filter_type == 2:
            predictor = above
        elif filter_type == 3:
            predictor = (left + above) // 2
        elif filter_type == 4:
            predictor = _paeth(left, above, upper_left)
        else:
            raise WorkbenchError("E_DECODE", "invalid PNG row filter", {"filter": filter_type})
        result[index] = (value + predictor) & 0xFF
    return bytes(result)


def read_png(path: PathLike) -> np.ndarray:
    source = Path(path)
    try:
        raw = source.read_bytes()
    except OSError as exc:
        raise WorkbenchError("E_DECODE", "cannot read image file", {"path": str(source), "reason": str(exc)}) from exc
    if not raw.startswith(_PNG_SIGNATURE):
        raise WorkbenchError("E_DECODE", "invalid PNG signature", {"path": str(source)})

    offset = len(_PNG_SIGNATURE)
    header = None
    compressed_parts: list[bytes] = []
    try:
        while offset + 12 <= len(raw):
            length = struct.unpack(">I", raw[offset : offset + 4])[0]
            kind = raw[offset + 4 : offset + 8]
            payload = raw[offset + 8 : offset + 8 + length]
            crc_bytes = raw[offset + 8 + length : offset + 12 + length]
            if len(payload) != length or len(crc_bytes) != 4:
                raise ValueError("truncated chunk")
            expected_crc = struct.unpack(">I", crc_bytes)[0]
            actual_crc = zlib.crc32(kind + payload) & 0xFFFFFFFF
            if expected_crc != actual_crc:
                raise ValueError("chunk CRC mismatch")
            offset += 12 + length
            if kind == b"IHDR":
                header = struct.unpack(">IIBBBBB", payload)
            elif kind == b"IDAT":
                compressed_parts.append(payload)
            elif kind == b"IEND":
                break
        if header is None:
            raise ValueError("missing IHDR")
        if not compressed_parts:
            raise ValueError("missing IDAT")
    except (ValueError, struct.error, zlib.error) as exc:
        raise WorkbenchError("E_DECODE", "invalid or corrupted PNG", {"path": str(source), "reason": str(exc)}) from exc

    width, height, bit_depth, color_type, compression, filter_method, interlace = header
    if bit_depth != 8 or compression != 0 or filter_method != 0 or interlace != 0:
        raise WorkbenchError(
            "E_DECODE",
            "unsupported PNG encoding",
            {"path": str(source), "bit_depth": bit_depth, "color_type": color_type, "interlace": interlace},
        )
    if color_type == 0:
        channels = 1
    elif color_type == 2:
        channels = 3
    else:
        raise WorkbenchError("E_DECODE", "only grayscale and RGB PNGs are supported", {"color_type": color_type})

    stride = width * channels
    try:
        inflated = zlib.decompress(b"".join(compressed_parts))
    except zlib.error as exc:
        raise WorkbenchError("E_DECODE", "corrupted PNG image data", {"path": str(source), "reason": str(exc)}) from exc
    expected = height * (stride + 1)
    if len(inflated) != expected:
        raise WorkbenchError(
            "E_DECODE",
            "PNG payload size mismatch",
            {"path": str(source), "expected": expected, "actual": len(inflated)},
        )

    previous = b""
    output_rows = []
    position = 0
    for _ in range(height):
        filter_type = inflated[position]
        filtered = inflated[position + 1 : position + 1 + stride]
        position += stride + 1
        row = _unfilter_scanline(filter_type, filtered, previous, channels)
        previous = row
        output_rows.append(np.frombuffer(row, dtype=np.uint8))
    image = np.stack(output_rows, axis=0)
    if channels == 1:
        return image.reshape(height, width)
    return image.reshape(height, width, channels)


def load_image(path: PathLike) -> np.ndarray:
    """Load a validated single-channel or RGB image from .png or .npy."""

    source = Path(path)
    suffix = source.suffix.lower()
    try:
        if suffix == ".png":
            image = read_png(source)
        elif suffix == ".npy":
            image = np.load(source, allow_pickle=False)
        else:
            raise WorkbenchError("E_DECODE", "unsupported image extension", {"path": str(source)})
    except WorkbenchError:
        raise
    except (OSError, ValueError, EOFError) as exc:
        raise WorkbenchError("E_DECODE", "image cannot be decoded", {"path": str(source), "reason": str(exc)}) from exc

    if image.dtype != np.uint8:
        raise WorkbenchError("E_DECODE", "image must contain uint8 pixels", {"path": str(source), "dtype": str(image.dtype)})
    if image.ndim not in (2, 3) or (image.ndim == 3 and image.shape[2] not in (1, 3)):
        raise WorkbenchError("E_DECODE", "image must be grayscale or RGB", {"path": str(source), "shape": list(image.shape)})
    if image.ndim == 3 and image.shape[2] == 1:
        image = image[:, :, 0]
    return np.ascontiguousarray(image)
