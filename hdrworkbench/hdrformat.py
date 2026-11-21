"""自定义 HDR 帧序列二进制格式。

文件布局（小端）::

    magic        8 字节 固定 b"HDRSEQ01"
    width        uint32  像素宽
    height       uint32  像素高
    frame_count  uint32  帧数
    bit_depth    uint16  支持 8 / 10 / 12 / 16
    colorspace   uint8   1=Rec.709 2=Rec.2020 3=Display-P3
    transfer     uint8   1=SDR gamma 2=PQ 3=HLG
    peak_nits    uint32  峰值亮度 cd/m^2
    reserved     4 字节  必须全 0
    frame data   紧凑交织 RGB；8-bit 用 uint8，其余统一 uint16 容器

10-bit / 12-bit 样本存放在 uint16 低有效位，取值上界为 2**bit_depth - 1。
"""

from __future__ import annotations

import os
import struct
from dataclasses import dataclass

import numpy as np

from .errors import HDRInvalidError

MAGIC = b"HDRSEQ01"
HEADER_SIZE = 32
HEADER_STRUCT = struct.Struct("<8sIIIHBBI4x")

CS_REC709 = 1
CS_REC2020 = 2
CS_DISPLAY_P3 = 3
COLORSPACES = (CS_REC709, CS_REC2020, CS_DISPLAY_P3)
COLORSPACE_NAMES = {
    CS_REC709: "Rec.709",
    CS_REC2020: "Rec.2020",
    CS_DISPLAY_P3: "Display-P3",
}

TF_SDR = 1
TF_PQ = 2
TF_HLG = 3
TRANSFERS = (TF_SDR, TF_PQ, TF_HLG)
TRANSFER_NAMES = {TF_SDR: "SDR", TF_PQ: "PQ", TF_HLG: "HLG"}

SUPPORTED_BIT_DEPTHS = (8, 10, 12, 16)


@dataclass(frozen=True)
class Header:
    width: int
    height: int
    frame_count: int
    bit_depth: int
    colorspace: int
    transfer: int
    peak_nits: int

    @property
    def code_max(self) -> int:
        return (1 << self.bit_depth) - 1

    @property
    def bytes_per_sample(self) -> int:
        return 1 if self.bit_depth == 8 else 2

    @property
    def frame_bytes(self) -> int:
        return self.width * self.height * 3 * self.bytes_per_sample

    def as_dict(self) -> dict:
        return {
            "width": self.width,
            "height": self.height,
            "frame_count": self.frame_count,
            "bit_depth": self.bit_depth,
            "colorspace": self.colorspace,
            "transfer": self.transfer,
            "peak_nits": self.peak_nits,
        }


def pack_header(header: Header) -> bytes:
    return HEADER_STRUCT.pack(
        MAGIC,
        header.width,
        header.height,
        header.frame_count,
        header.bit_depth,
        header.colorspace,
        header.transfer,
        header.peak_nits,
    )


def parse_header(blob: bytes) -> Header:
    """解析并校验头部；头部级问题 bad_frame 记为 -1。"""
    if len(blob) < HEADER_SIZE:
        raise HDRInvalidError(
            "文件短于固定头部，头部字段缺失",
            bad_frame=-1,
            reason="header_truncated",
        )
    (
        magic,
        width,
        height,
        frame_count,
        bit_depth,
        colorspace,
        transfer,
        peak_nits,
    ) = HEADER_STRUCT.unpack(blob[:HEADER_SIZE])
    if magic != MAGIC:
        raise HDRInvalidError(
            f"魔数不匹配: {magic!r}", bad_frame=-1, reason="bad_magic"
        )
    if width == 0 or height == 0 or frame_count == 0:
        raise HDRInvalidError(
            "分辨率或帧数为 0", bad_frame=-1, reason="zero_dimension"
        )
    if bit_depth not in SUPPORTED_BIT_DEPTHS:
        raise HDRInvalidError(
            f"不支持的位深 {bit_depth}", bad_frame=-1, reason="bad_bit_depth"
        )
    if colorspace not in COLORSPACES:
        raise HDRInvalidError(
            f"未知色彩空间码 {colorspace}",
            bad_frame=-1,
            reason="unknown_colorspace",
        )
    if transfer not in TRANSFERS:
        raise HDRInvalidError(
            f"未知传递函数码 {transfer}",
            bad_frame=-1,
            reason="unknown_transfer",
        )
    if peak_nits == 0:
        raise HDRInvalidError(
            "峰值亮度为 0", bad_frame=-1, reason="bad_peak_nits"
        )
    return Header(
        width,
        height,
        frame_count,
        bit_depth,
        colorspace,
        transfer,
        peak_nits,
    )


@dataclass
class FrameSequence:
    """已通过完整校验的帧序列，frames 为归一化到 [0,1] 的 double 数组。"""

    header: Header
    raw_frames: list[np.ndarray]  # 原始整数码值，形状 (H, W, 3)

    @property
    def pixel_count(self) -> int:
        return self.header.width * self.header.height

    def normalized_frames(self) -> list[np.ndarray]:
        scale = float(self.header.code_max)
        return [frame.astype(np.float64) / scale for frame in self.raw_frames]


def _read_exact(path: str) -> bytes:
    with open(path, "rb") as handle:
        return handle.read()


def load_sequence(path: str) -> FrameSequence:
    """完整加载并校验序列，定位首个异常帧号（0 基；头部错误为 -1）。"""
    blob = _read_exact(path)
    header = parse_header(blob)
    payload = blob[HEADER_SIZE:]
    frame_bytes = header.frame_bytes
    expected = frame_bytes * header.frame_count
    if len(payload) < expected:
        present = len(payload) // frame_bytes
        raise HDRInvalidError(
            f"帧数据截断：期望 {header.frame_count} 帧，第 {present} 帧不完整",
            bad_frame=present,
            reason="frame_truncated",
        )
    if len(payload) > expected:
        raise HDRInvalidError(
            "帧数据长度超出头部声明，存在尾随字节",
            bad_frame=header.frame_count,
            reason="trailing_bytes",
        )

    dtype = np.uint8 if header.bit_depth == 8 else np.uint16
    shape = (
        header.frame_count,
        header.height,
        header.width,
        3,
    )
    packed = np.frombuffer(payload, dtype=dtype).reshape(shape)
    if header.bit_depth in (10, 12):
        over_range = np.any(packed > header.code_max)
        if over_range:
            flat_index = int(np.argmax(packed > header.code_max))
            samples_per_frame = header.width * header.height * 3
            bad_frame = flat_index // samples_per_frame
            raise HDRInvalidError(
                f"{header.bit_depth}-bit 样本超出码值上界",
                bad_frame=bad_frame,
                reason="code_over_range",
            )
    raw_frames = [packed[index].copy() for index in range(header.frame_count)]
    return FrameSequence(header=header, raw_frames=raw_frames)


def write_sequence(path: str, header: Header, raw_frames: list[np.ndarray]) -> None:
    """确定性写盘：临时文件 + fsync + 原子重命名。"""
    if len(raw_frames) != header.frame_count:
        raise HDRInvalidError(
            "写盘帧数与头部声明不一致",
            bad_frame=len(raw_frames),
            reason="frame_count_mismatch",
        )
    dtype = np.uint8 if header.bit_depth == 8 else np.uint16
    tmp_path = f"{path}.tmp"
    with open(tmp_path, "wb") as handle:
        handle.write(pack_header(header))
        for index, frame in enumerate(raw_frames):
            if frame.shape != (header.height, header.width, 3):
                raise HDRInvalidError(
                    f"第 {index} 帧分辨率与头部不一致",
                    bad_frame=index,
                    reason="frame_shape_mismatch",
                )
            handle.write(np.asarray(frame, dtype=dtype).tobytes())
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp_path, path)

