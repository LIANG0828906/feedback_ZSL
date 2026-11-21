"""自定义二进制点云格式（PCWB v1）的读写与内容哈希。

布局（全部小端）：
    偏移  大小  字段
    0     4     magic = b'PCWB'
    4     4     format_version = 1 (uint32)
    8     4     header_size = 44 (uint32)
    12    8     point_count (uint64)
    20    12    bbox_min: 3 x float32
    32    12    bbox_max: 3 x float32
    44    ...   点数据：每点 15 字节 = XYZ 3 x float32 + RGB 3 x uint8

内容哈希 = 点数据区（与文件布局逐字节一致）的 SHA-256，
因此同一版本点云重复写盘/切块均得到逐字节一致的哈希口径。
"""

from __future__ import annotations

import hashlib
import os
import struct
import tempfile
from dataclasses import dataclass

import numpy as np

from .errors import PointcloudInvalid

MAGIC = b"PCWB"
FORMAT_VERSION = 1
HEADER_STRUCT = struct.Struct("<4sIIQ6f")
HEADER_SIZE = HEADER_STRUCT.size  # 44
POINT_STRUCT = struct.Struct("<3f3B")
POINT_SIZE = POINT_STRUCT.size  # 15

POINT_DTYPE = np.dtype([("xyz", "<f4", (3,)), ("rgb", "u1", (3,))])
assert POINT_DTYPE.itemsize == POINT_SIZE


@dataclass
class PointCloud:
    points: np.ndarray  # (N, 3) float32
    colors: np.ndarray  # (N, 3) uint8

    def __post_init__(self):
        self.points = np.ascontiguousarray(self.points, dtype=np.float32)
        self.colors = np.ascontiguousarray(self.colors, dtype=np.uint8)
        if self.points.ndim != 2 or self.points.shape[1] != 3:
            raise PointcloudInvalid("points 形状非法", field="points")
        if self.colors.shape != self.points.shape:
            raise PointcloudInvalid("colors 形状与 points 不一致", field="colors")

    @property
    def count(self) -> int:
        return int(self.points.shape[0])

    def to_records(self) -> np.ndarray:
        rec = np.empty(self.count, dtype=POINT_DTYPE)
        rec["xyz"] = self.points
        rec["rgb"] = self.colors
        return rec

    def content_bytes(self) -> bytes:
        return self.to_records().tobytes()

    def content_hash(self) -> str:
        return content_hash(self.points, self.colors)


def content_hash(points: np.ndarray, colors: np.ndarray) -> str:
    rec = np.empty(points.shape[0], dtype=POINT_DTYPE)
    rec["xyz"] = np.ascontiguousarray(points, dtype=np.float32)
    rec["rgb"] = np.ascontiguousarray(colors, dtype=np.uint8)
    return hashlib.sha256(rec.tobytes()).hexdigest()


def bbox_of(points: np.ndarray):
    if points.shape[0] == 0:
        zeros = np.zeros(3, dtype=np.float32)
        return zeros, zeros.copy()
    return points.min(axis=0), points.max(axis=0)


def read(path: str) -> PointCloud:
    with open(path, "rb") as fh:
        data = fh.read()
    if len(data) < HEADER_SIZE:
        raise PointcloudInvalid("文件长度小于头部", field="header_size")
    magic, version, header_size, count, *bbox = HEADER_STRUCT.unpack_from(data, 0)
    if magic != MAGIC:
        raise PointcloudInvalid("magic 不匹配", field="magic")
    if version != FORMAT_VERSION:
        raise PointcloudInvalid("格式版本不支持", field="format_version")
    if header_size != HEADER_SIZE:
        raise PointcloudInvalid("头部长度字段非法", field="header_size")
    bbox_min = np.array(bbox[0:3], dtype=np.float32)
    bbox_max = np.array(bbox[3:6], dtype=np.float32)
    if not (np.all(np.isfinite(bbox_min)) and np.all(np.isfinite(bbox_max))):
        raise PointcloudInvalid("包围盒含非有限值", field="bbox")
    if np.any(bbox_min > bbox_max):
        raise PointcloudInvalid("包围盒 min 大于 max", field="bbox")
    expected = HEADER_SIZE + count * POINT_SIZE
    if len(data) != expected:
        raise PointcloudInvalid(
            "点数与文件长度不一致",
            field="point_count",
            declared=int(count),
            expected_bytes=int(expected),
            actual_bytes=len(data),
        )
    rec = np.frombuffer(data, dtype=POINT_DTYPE, count=int(count), offset=HEADER_SIZE)
    points = np.array(rec["xyz"], dtype=np.float32)
    colors = np.array(rec["rgb"], dtype=np.uint8)
    if not np.all(np.isfinite(points)):
        bad = int(np.argmin(np.isfinite(points).all(axis=1)))
        raise PointcloudInvalid("坐标含非有限值", point_index=bad)
    below = np.any(points < bbox_min, axis=1)
    above = np.any(points > bbox_max, axis=1)
    if np.any(below) or np.any(above):
        bad = int(np.argmax(below | above))
        raise PointcloudInvalid("坐标越出包围盒", point_index=bad)
    return PointCloud(points, colors)


def encode(cloud: PointCloud) -> bytes:
    bbox_min, bbox_max = bbox_of(cloud.points)
    header = HEADER_STRUCT.pack(
        MAGIC, FORMAT_VERSION, HEADER_SIZE, cloud.count, *bbox_min, *bbox_max
    )
    return header + cloud.content_bytes()


def write(path: str, cloud: PointCloud) -> str:
    """按相同布局写出文件，返回内容哈希。"""
    payload = encode(cloud)
    with open(path, "wb") as fh:
        fh.write(payload)
    return cloud.content_hash()


def write_atomic(path: str, cloud: PointCloud) -> str:
    """临时文件 + 原子重命名；失败不破坏目标文件。"""
    payload = encode(cloud)
    directory = os.path.dirname(os.path.abspath(path))
    fd, tmp = tempfile.mkstemp(prefix=".pcwb-tmp-", dir=directory)
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(payload)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return cloud.content_hash()
