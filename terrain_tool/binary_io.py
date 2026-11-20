"""自定义二进制格式读写。

所有头部使用小端、标准对齐（``struct`` 无填充），体数据为原始字节。

高度场文件 ``.hgt`` 布局（共 44 字节头 + float32 体数据）::

    magic        4s   b"HGT1"
    width        I    列数
    height       I    行数
    cell_size    f    格距（世界单位，> 0）
    dtype_code   H    1 = float32
    flags        H    保留位
    valid_x      I    有效矩形左
    valid_y      I    有效矩形上
    valid_w      I    有效矩形宽
    valid_h      I    有效矩形高
    base_version I    补丁基线版本（已应用补丁序号）
    reserved     I    保留

材质文件 ``.mat`` 布局（32 字节头 + uint8/uint16 体数据）::

    magic        4s   b"MAT1"
    width        I
    height       I
    dtype_code   H    8 = uint8, 16 = uint16
    flags        H
    base_version I
    reserved     I x3

补丁文件 ``.ptc`` 布局（40 字节头 + float32 或 uint8/uint16 体数据）::

    magic        4s   b"PTC1"
    kind         H    1 = 高程补丁, 2 = 材质补丁
    dtype_code   H
    x, y, w, h   I x4 目标矩形（含端点，像素坐标）
    seq          I    补丁序号（1 起）
    cell_size    f
    base_version I
    reserved     I x2
"""

from __future__ import annotations

import os
import struct
from dataclasses import dataclass

import numpy as np

from .errors import ErrorCode, TerrainError

HGT_MAGIC = b"HGT1"
MAT_MAGIC = b"MAT1"
PTC_MAGIC = b"PTC1"

HGT_HEADER = struct.Struct("<4sIIfHHIIIIII")  # 44
MAT_HEADER = struct.Struct("<4sIIHHIIII")     # 32
PTC_HEADER = struct.Struct("<4sHHIIIIIfIII")  # 44

DTYPE_FLOAT32 = 1
DTYPE_UINT8 = 8
DTYPE_UINT16 = 16

KIND_HEIGHT = 1
KIND_MATERIAL = 2

_CODE_TO_DTYPE = {
    DTYPE_FLOAT32: np.dtype(np.float32),
    DTYPE_UINT8: np.dtype(np.uint8),
    DTYPE_UINT16: np.dtype(np.uint16),
}
_DTYPE_TO_CODE = {v: k for k, v in _CODE_TO_DTYPE.items()}


@dataclass
class HeightField:
    elevations: np.ndarray  # float32, shape (height, width)
    cell_size: float
    valid_rect: tuple[int, int, int, int]  # x, y, w, h
    base_version: int

    @property
    def width(self) -> int:
        return self.elevations.shape[1]

    @property
    def height(self) -> int:
        return self.elevations.shape[0]


@dataclass
class MaterialMap:
    indices: np.ndarray  # uint8/uint16, shape (height, width)
    base_version: int

    @property
    def width(self) -> int:
        return self.indices.shape[1]

    @property
    def height(self) -> int:
        return self.indices.shape[0]


@dataclass
class Patch:
    kind: int  # KIND_HEIGHT / KIND_MATERIAL
    rect: tuple[int, int, int, int]  # x, y, w, h
    cell_size: float
    base_version: int
    seq: int
    data: np.ndarray  # shape (h, w)

    @property
    def is_height(self) -> bool:
        return self.kind == KIND_HEIGHT


def load_heightfield(path: str) -> HeightField:
    """解析高度场；任何头字段矛盾/长度不符/非有限高程都抛 E_TERRAIN_INVALID。"""
    try:
        with open(path, "rb") as fh:
            header = fh.read(HGT_HEADER.size)
            if len(header) != HGT_HEADER.size:
                raise TerrainError(
                    ErrorCode.E_TERRAIN_INVALID, "高度场头不完整", {"path": path}
                )
            (
                magic,
                width,
                height,
                cell_size,
                dtype_code,
                flags,
                vx,
                vy,
                vw,
                vh,
                base_version,
                _reserved,
            ) = HGT_HEADER.unpack(header)
            body = fh.read()
    except OSError as exc:
        raise TerrainError(
            ErrorCode.E_TERRAIN_INVALID, f"无法读取高度场: {exc}", {"path": path}
        ) from exc

    problems: list[str] = []
    if magic != HGT_MAGIC:
        problems.append("magic 不匹配")
    if dtype_code != DTYPE_FLOAT32:
        problems.append(f"不支持的高程类型 {dtype_code}")
    if width == 0 or height == 0:
        problems.append("宽高必须为正")
    if not np.isfinite(cell_size) or cell_size <= 0:
        problems.append("格距必须为正有限值")
    if vw == 0 or vh == 0:
        problems.append("有效矩形面积必须为正")
    if not (0 <= vx and vx + vw <= width and 0 <= vy and vy + vh <= height):
        problems.append("有效矩形超出栅格范围")
    if base_version < 0:
        problems.append("基线版本非法")
    expected = width * height * 4
    if len(body) != expected:
        problems.append(f"体数据长度 {len(body)} 与期望 {expected} 不符")
    if problems:
        raise TerrainError(
            ErrorCode.E_TERRAIN_INVALID, "; ".join(problems), {"path": path}
        )

    elevations = np.frombuffer(body, dtype=np.float32, count=width * height).reshape(
        height, width
    )
    if not np.all(np.isfinite(elevations)):
        raise TerrainError(
            ErrorCode.E_TERRAIN_INVALID, "存在非有限高程（NaN/Inf）", {"path": path}
        )
    return HeightField(
        elevations=np.array(elevations, copy=True),
        cell_size=float(cell_size),
        valid_rect=(int(vx), int(vy), int(vw), int(vh)),
        base_version=int(base_version),
    )


def save_heightfield(path: str, hf: HeightField) -> None:
    vx, vy, vw, vh = (int(v) for v in hf.valid_rect)
    header = HGT_HEADER.pack(
        HGT_MAGIC,
        hf.width,
        hf.height,
        float(hf.cell_size),
        DTYPE_FLOAT32,
        0,
        vx,
        vy,
        vw,
        vh,
        int(hf.base_version),
        0,
    )
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        fh.write(header)
        fh.write(np.ascontiguousarray(hf.elevations, dtype=np.float32).tobytes())
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def load_material(path: str) -> MaterialMap:
    try:
        with open(path, "rb") as fh:
            header = fh.read(MAT_HEADER.size)
            if len(header) != MAT_HEADER.size:
                raise TerrainError(
                    ErrorCode.E_TERRAIN_INVALID, "材质头不完整", {"path": path}
                )
            magic, width, height, dtype_code, flags, base_version, r1, r2, r3 = (
                MAT_HEADER.unpack(header)
            )
            body = fh.read()
    except OSError as exc:
        raise TerrainError(
            ErrorCode.E_TERRAIN_INVALID, f"无法读取材质图: {exc}", {"path": path}
        ) from exc

    problems: list[str] = []
    if magic != MAT_MAGIC:
        problems.append("magic 不匹配")
    if dtype_code not in _CODE_TO_DTYPE or dtype_code == DTYPE_FLOAT32:
        problems.append(f"不支持的材质索引类型 {dtype_code}")
    if width == 0 or height == 0:
        problems.append("宽高必须为正")
    if base_version < 0:
        problems.append("基线版本非法")
    itemsize = _CODE_TO_DTYPE[dtype_code].itemsize
    expected = width * height * itemsize
    if len(body) != expected:
        problems.append(f"体数据长度 {len(body)} 与期望 {expected} 不符")
    if problems:
        raise TerrainError(
            ErrorCode.E_TERRAIN_INVALID, "; ".join(problems), {"path": path}
        )

    indices = np.frombuffer(
        body, dtype=_CODE_TO_DTYPE[dtype_code], count=width * height
    ).reshape(height, width)
    return MaterialMap(indices=np.array(indices, copy=True), base_version=int(base_version))


def save_material(path: str, mat: MaterialMap) -> None:
    dtype_code = _DTYPE_TO_CODE[mat.indices.dtype]
    header = MAT_HEADER.pack(
        MAT_MAGIC, mat.width, mat.height, dtype_code, 0, int(mat.base_version), 0, 0, 0
    )
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        fh.write(header)
        fh.write(np.ascontiguousarray(mat.indices).tobytes())
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def load_patch(path: str) -> Patch:
    """解析补丁；非法文件抛 E_PATCH_INVALID，越界/版本由引擎层判定。"""
    try:
        with open(path, "rb") as fh:
            header = fh.read(PTC_HEADER.size)
            if len(header) != PTC_HEADER.size:
                raise TerrainError(
                    ErrorCode.E_PATCH_INVALID, "补丁头不完整", {"path": path}
                )
            (
                magic,
                kind,
                dtype_code,
                x,
                y,
                w,
                h,
                seq,
                cell_size,
                base_version,
                _r1,
                _r2,
            ) = PTC_HEADER.unpack(header)
            body = fh.read()
    except OSError as exc:
        raise TerrainError(
            ErrorCode.E_PATCH_INVALID, f"无法读取补丁: {exc}", {"path": path}
        ) from exc

    problems: list[str] = []
    if magic != PTC_MAGIC:
        problems.append("magic 不匹配")
    if kind not in (KIND_HEIGHT, KIND_MATERIAL):
        problems.append(f"未知补丁类型 {kind}")
    if dtype_code not in _CODE_TO_DTYPE:
        problems.append(f"不支持的数据类型 {dtype_code}")
    if kind == KIND_HEIGHT and dtype_code != DTYPE_FLOAT32:
        problems.append("高程补丁必须为 float32")
    if kind == KIND_MATERIAL and dtype_code not in (DTYPE_UINT8, DTYPE_UINT16):
        problems.append("材质补丁必须为 uint8/uint16")
    if w == 0 or h == 0:
        problems.append("补丁矩形面积必须为正")
    if not np.isfinite(cell_size) or cell_size <= 0:
        problems.append("补丁格距非法")
    if base_version < 0 or seq <= 0:
        problems.append("基线版本/序号非法")
    if problems:
        raise TerrainError(
            ErrorCode.E_PATCH_INVALID, "; ".join(problems), {"path": path}
        )

    expected = w * h * _CODE_TO_DTYPE[dtype_code].itemsize
    if len(body) != expected:
        raise TerrainError(
            ErrorCode.E_PATCH_INVALID,
            f"补丁体数据长度 {len(body)} 与期望 {expected} 不符",
            {"path": path},
        )
    data = np.frombuffer(body, dtype=_CODE_TO_DTYPE[dtype_code], count=w * h).reshape(h, w)
    if kind == KIND_HEIGHT and not np.all(np.isfinite(data)):
        raise TerrainError(
            ErrorCode.E_PATCH_INVALID, "补丁含非有限高程", {"path": path}
        )
    return Patch(
        kind=int(kind),
        rect=(int(x), int(y), int(w), int(h)),
        cell_size=float(cell_size),
        base_version=int(base_version),
        seq=int(seq),
        data=np.array(data, copy=True),
    )


def save_patch(path: str, patch: Patch) -> None:
    dtype_code = _DTYPE_TO_CODE[patch.data.dtype]
    x, y, w, h = patch.rect
    header = PTC_HEADER.pack(
        PTC_MAGIC,
        patch.kind,
        dtype_code,
        x,
        y,
        w,
        h,
        int(patch.seq),
        float(patch.cell_size),
        int(patch.base_version),
        0,
        0,
    )
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        fh.write(header)
        fh.write(np.ascontiguousarray(patch.data).tobytes())
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)
