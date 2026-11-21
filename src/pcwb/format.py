"""The PCWB binary format.

Layout (little endian)::

    magic            8 bytes  b"PCWB\\x00\\x00\\x00\\x01"
    point_count      uint64
    bounds_min       float32 x 3
    bounds_max       float32 x 3
    points           point_count records
    point record     float32 x 3, uint8 x 3 (XYZ then RGB)

Header size is 40 bytes and every point record is 15 bytes.  Therefore a file
must be exactly ``40 + 15 * point_count`` bytes.
"""

from __future__ import annotations

import os
import struct
import tempfile
from pathlib import Path

import numpy as np

from .errors import PointCloudInvalid
from .model import Bounds, PointCloud
from .util import fsync_directory, sha256_hex

MAGIC = b"PCWB\x00\x00\x00\x01"
HEADER_STRUCT = struct.Struct("<8sQ3f3f")
POINT_STRUCT = struct.Struct("<3f3B")
HEADER_SIZE = HEADER_STRUCT.size
POINT_SIZE = POINT_STRUCT.size

_POINT_DTYPE = np.dtype(
    [
        ("xyz", "<f4", (3,)),
        ("rgb", "u1", (3,)),
    ]
)


def _validate_file_length(point_count: int, actual_length: int) -> None:
    expected = HEADER_SIZE + point_count * POINT_SIZE
    if actual_length != expected:
        raise PointCloudInvalid(
            "file length does not match the declared point count",
            expected_length=expected,
            actual_length=actual_length,
            point_count=point_count,
        )


def parse_point_cloud(data: bytes | bytearray | memoryview) -> PointCloud:
    raw = bytes(data)
    if len(raw) < HEADER_SIZE:
        raise PointCloudInvalid(
            "file is shorter than the PCWB header",
            expected_length=HEADER_SIZE,
            actual_length=len(raw),
        )
    magic, point_count, min_x, min_y, min_z, max_x, max_y, max_z = (
        HEADER_STRUCT.unpack_from(raw, 0)
    )
    if magic != MAGIC:
        raise PointCloudInvalid("invalid PCWB magic or format version")
    _validate_file_length(int(point_count), len(raw))
    bounds = Bounds((min_x, min_y, min_z), (max_x, max_y, max_z))
    if point_count == 0:
        xyz = np.empty((0, 3), dtype=np.float32)
        rgb = np.empty((0, 3), dtype=np.uint8)
        return PointCloud(xyz, rgb, bounds)

    records = np.frombuffer(
        raw,
        dtype=_POINT_DTYPE,
        count=int(point_count),
        offset=HEADER_SIZE,
    )
    xyz = np.ascontiguousarray(records["xyz"], dtype=np.float32)
    rgb = np.ascontiguousarray(records["rgb"], dtype=np.uint8)
    finite_mask = np.isfinite(xyz).all(axis=1)
    if not finite_mask.all():
        bad = int(np.flatnonzero(~finite_mask)[0])
        raise PointCloudInvalid(
            "point coordinates contain NaN or infinity", point_index=bad
        )
    inside = bounds.contains(xyz, epsilon=0.0)
    if not inside.all():
        bad = int(np.flatnonzero(~inside)[0])
        axis = int(
            np.flatnonzero(
                ~((xyz[bad] >= bounds.minimum) & (xyz[bad] <= bounds.maximum))
            )[0]
        )
        raise PointCloudInvalid(
            "point lies outside the header bounding box",
            point_index=bad,
            axis=axis,
            point=xyz[bad].tolist(),
        )
    return PointCloud(xyz, rgb, bounds)


def read_point_cloud(path: str | os.PathLike[str]) -> PointCloud:
    source = Path(path)
    try:
        data = source.read_bytes()
    except OSError as exc:
        raise PointCloudInvalid(f"cannot read point-cloud file: {exc}") from exc
    return parse_point_cloud(data)


def serialize_point_cloud(cloud: PointCloud) -> bytes:
    """Serialize deterministically after validating the persisted bounds."""

    xyz = np.asarray(cloud.xyz, dtype="<f4")
    rgb = np.asarray(cloud.rgb, dtype=np.uint8)
    if not np.isfinite(xyz).all():
        bad = int(np.flatnonzero(~np.isfinite(xyz).all(axis=1))[0])
        raise PointCloudInvalid("point coordinates must be finite", point_index=bad)
    inside = cloud.bounds.contains(xyz)
    if not inside.all():
        bad = int(np.flatnonzero(~inside)[0])
        raise PointCloudInvalid("point lies outside the bounding box", point_index=bad)
    header = HEADER_STRUCT.pack(
        MAGIC,
        int(cloud.count),
        *cloud.bounds.minimum,
        *cloud.bounds.maximum,
    )
    if cloud.count == 0:
        return header
    records = np.empty(cloud.count, dtype=_POINT_DTYPE)
    records["xyz"] = xyz
    records["rgb"] = rgb
    return header + records.tobytes(order="C")


def content_hash(cloud: PointCloud) -> str:
    return sha256_hex(serialize_point_cloud(cloud))


def write_point_cloud(
    path: str | os.PathLike[str],
    cloud: PointCloud,
    *,
    atomic: bool = True,
) -> str:
    """Write a cloud and return its SHA-256 content hash.

    Atomic writes use a same-directory temporary file, fsync, and replace so a
    reader never observes a partially written header or record.
    """

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    payload = serialize_point_cloud(cloud)
    digest = sha256_hex(payload)
    if not atomic:
        target.write_bytes(payload)
        return digest

    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
        fsync_directory(target.parent)
    except Exception:  # noqa: BLE001 - cleanup must preserve the original error
        try:
            temporary.unlink(missing_ok=True)
        finally:
            raise
    return digest
