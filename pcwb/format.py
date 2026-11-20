"""Custom binary point-cloud format (``.pclb``).

Little-endian on-disk layout, version 1::

    Offset  Size  Field
    0       4     magic           ASCII "PCLB"
    4       2     format_version  uint16, always 1
    6       2     header_size     uint16, always 80
    8       4     point_count     uint32
    12      24    bbox            6 * float32 (min xyz, max xyz)
    36      40    reserved        zero bytes
    76      4     header_crc32    CRC32 of header bytes [0, 76)
    80      ...   point records, each 15 bytes:
                    x, y, z : 3 * float32
                    r, g, b : 3 * uint8

The content hash is SHA-256 over the *whole* file (header + records), so it
covers the CRC, the bounding box and every byte of every point. All arithmetic
that decides file contents is performed in float64 and cast to float32 once at
serialization time to keep output byte-deterministic.
"""

from __future__ import annotations

import hashlib
import struct
import zlib
from dataclasses import dataclass

import numpy as np

from .errors import E_POINTCLOUD_INVALID, PointCloudError

MAGIC = b"PCLB"
FORMAT_VERSION = 1
HEADER_SIZE = 80
POINT_RECORD_SIZE = 15

# struct format of the first 76 header bytes (before the trailing CRC32)
_HEADER_PREFIX = struct.Struct("<4sHHI6f40x")
_CRC_STRUCT = struct.Struct("<I")
# numpy record dtype matching the 15-byte point record
POINT_DTYPE = np.dtype([
    ("x", "<f4"), ("y", "<f4"), ("z", "<f4"),
    ("r", "u1"), ("g", "u1"), ("b", "u1"),
])


@dataclass(frozen=True)
class BoundingBox:
    """Axis-aligned bounding box stored as min/max corners."""

    xmin: float
    ymin: float
    zmin: float
    xmax: float
    ymax: float
    zmax: float

    @classmethod
    def from_array(cls, bbox: np.ndarray) -> "BoundingBox":
        bbox = np.asarray(bbox, dtype=np.float64).reshape(6)
        return cls(float(bbox[0]), float(bbox[1]), float(bbox[2]),
                   float(bbox[3]), float(bbox[4]), float(bbox[5]))

    def as_array(self) -> np.ndarray:
        return np.array(
            [self.xmin, self.ymin, self.zmin, self.xmax, self.ymax, self.zmax],
            dtype=np.float64,
        )


def _invalid(message: str) -> PointCloudError:
    return PointCloudError(E_POINTCLOUD_INVALID, message)


def compute_bbox(xyz: np.ndarray) -> np.ndarray:
    """Return the tight ``[min3, max3]`` bounding box of ``xyz``.

    An empty cloud gets an all-zero, non-inverted box.
    """
    xyz = np.asarray(xyz, dtype=np.float64)
    if xyz.size == 0:
        return np.zeros(6, dtype=np.float64)
    mins = np.min(xyz, axis=0)
    maxs = np.max(xyz, axis=0)
    return np.concatenate([mins, maxs]).astype(np.float64)


def _build_header(point_count: int, bbox: np.ndarray) -> bytes:
    prefix = _HEADER_PREFIX.pack(
        MAGIC,
        FORMAT_VERSION,
        HEADER_SIZE,
        int(point_count),
        *(float(v) for v in np.asarray(bbox, dtype=np.float64).reshape(6)),
    )
    crc = zlib.crc32(prefix) & 0xFFFFFFFF
    return prefix + _CRC_STRUCT.pack(crc)


def encode_points(xyz: np.ndarray, rgb: np.ndarray) -> bytes:
    """Serialize point records deterministically (float64 -> float32 once)."""
    xyz = np.asarray(xyz, dtype=np.float64)
    rgb = np.asarray(rgb, dtype=np.uint8)
    if xyz.ndim != 2 or xyz.shape[1] != 3:
        raise _invalid(f"xyz must have shape (N, 3), got {xyz.shape}")
    if rgb.shape != xyz.shape:
        raise _invalid(f"rgb shape {rgb.shape} does not match xyz {xyz.shape}")
    xyz32 = np.ascontiguousarray(xyz, dtype="<f4")
    rgb8 = np.ascontiguousarray(rgb, dtype=np.uint8)
    records = np.empty(xyz32.shape[0], dtype=POINT_DTYPE)
    records["x"], records["y"], records["z"] = (
        xyz32[:, 0], xyz32[:, 1], xyz32[:, 2])
    records["r"], records["g"], records["b"] = (
        rgb8[:, 0], rgb8[:, 1], rgb8[:, 2])
    return records.tobytes()


def encode_cloud(xyz: np.ndarray, rgb: np.ndarray,
                 bbox: np.ndarray | None = None) -> bytes:
    """Serialize a full point cloud, computing the bbox when omitted."""
    xyz = np.asarray(xyz, dtype=np.float64)
    rgb = np.asarray(rgb, dtype=np.uint8)
    if xyz.shape != rgb.shape:
        raise _invalid(f"xyz/rgb shape mismatch: {xyz.shape} vs {rgb.shape}")
    if bbox is None:
        bbox = compute_bbox(xyz)
    bbox = np.asarray(bbox, dtype=np.float64).reshape(6)
    header = _build_header(xyz.shape[0], bbox)
    records = encode_points(xyz, rgb)
    return header + records


def write_cloud(path: str, xyz: np.ndarray, rgb: np.ndarray,
                bbox: np.ndarray | None = None) -> str:
    """Write a ``.pclb`` file and return its content SHA-256 (hex)."""
    blob = encode_cloud(xyz, rgb, bbox)
    with open(path, "wb") as handle:
        handle.write(blob)
    return content_hash(blob)


def content_hash(blob: bytes) -> str:
    """SHA-256 hex digest of serialized cloud bytes."""
    return hashlib.sha256(blob).hexdigest()


def _parse_header(blob: bytes) -> tuple[int, np.ndarray]:
    if len(blob) < HEADER_SIZE:
        raise _invalid(f"file shorter than header: {len(blob)} bytes")
    prefix = blob[: HEADER_SIZE - 4]
    (magic, version, header_size, point_count,
     xmin, ymin, zmin, xmax, ymax, zmax) = _HEADER_PREFIX.unpack(prefix)
    (stored_crc,) = _CRC_STRUCT.unpack(blob[HEADER_SIZE - 4: HEADER_SIZE])
    if magic != MAGIC:
        raise _invalid(f"bad magic {magic!r}, expected {MAGIC!r}")
    if version != FORMAT_VERSION:
        raise _invalid(f"unsupported format version {version}")
    if header_size != HEADER_SIZE:
        raise _invalid(f"unexpected header size {header_size}")
    actual_crc = zlib.crc32(prefix) & 0xFFFFFFFF
    if actual_crc != stored_crc:
        raise _invalid("header CRC32 mismatch (corrupt header)")
    bbox = np.array([xmin, ymin, zmin, xmax, ymax, zmax], dtype=np.float64)
    return point_count, bbox


def parse_cloud(blob: bytes, validate: bool = True
                ) -> tuple[np.ndarray, np.ndarray, np.ndarray, str]:
    """Parse serialized bytes.

    Returns ``(xyz float64, rgb uint8, bbox float64, sha256_hex)``.
    Raises :class:`PointCloudError` with ``E_POINTCLOUD_INVALID`` on any
    structural problem or out-of-bounds coordinate.
    """
    point_count, bbox = _parse_header(blob)
    expected_size = HEADER_SIZE + point_count * POINT_RECORD_SIZE
    if len(blob) != expected_size:
        raise _invalid(
            f"length {len(blob)} does not match point_count {point_count} "
            f"(expected {expected_size})"
        )
    records = np.frombuffer(
        blob, dtype=POINT_DTYPE, count=point_count, offset=HEADER_SIZE)
    xyz = np.empty((point_count, 3), dtype=np.float64)
    xyz[:, 0] = records["x"].astype(np.float64)
    xyz[:, 1] = records["y"].astype(np.float64)
    xyz[:, 2] = records["z"].astype(np.float64)
    rgb = np.empty((point_count, 3), dtype=np.uint8)
    rgb[:, 0] = records["r"]
    rgb[:, 1] = records["g"]
    rgb[:, 2] = records["b"]
    if validate:
        validate_cloud(xyz, bbox)
    return xyz, rgb, bbox, content_hash(blob)


def validate_cloud(xyz: np.ndarray, bbox: np.ndarray) -> None:
    """Raise ``E_POINTCLOUD_INVALID`` if any coordinate leaves the bbox."""
    xyz = np.asarray(xyz, dtype=np.float64)
    bbox = np.asarray(bbox, dtype=np.float64).reshape(6)
    mins = bbox[:3]
    maxs = bbox[3:]
    if np.any(mins > maxs):
        raise _invalid(f"inverted bounding box: {bbox.tolist()}")
    if xyz.size and np.any((xyz < mins) | (xyz > maxs)):
        bad = int(np.argmax(np.any((xyz < mins) | (xyz > maxs), axis=1)))
        raise _invalid(
            f"point {bad} coordinate {xyz[bad].tolist()} outside bbox "
            f"{bbox.tolist()}"
        )


def read_cloud(path: str, validate: bool = True
               ) -> tuple[np.ndarray, np.ndarray, np.ndarray, str]:
    """Read and validate a ``.pclb`` file. See :func:`parse_cloud`."""
    try:
        with open(path, "rb") as handle:
            blob = handle.read()
    except OSError as exc:
        raise _invalid(f"cannot read {path}: {exc}") from exc
    return parse_cloud(blob, validate=validate)
