"""最小化的纯 Python PNG 编解码器（仅依赖 numpy 与标准库 zlib）。

支持 8 位、非隔行、RGB（color type 2）PNG。工作台的样本全部由
:func:`encode_rgb` 写出，因此读回路径是封闭且确定的；解码器同时校验
PNG 签名、IHDR 与每个 chunk 的 CRC，损坏文件会抛出异常并由上层转成
``E_DECODE``。
"""
from __future__ import annotations

import struct
import zlib

import numpy as np

from segworkbench.core.errors import ErrorCode, fail

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


class PngDecodeError(ValueError):
    """PNG 字节流不合法。"""


def _chunk(tag: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + tag
        + payload
        + struct.pack(">I", zlib.crc32(tag + payload) & 0xFFFFFFFF)
    )


def encode_rgb(image: np.ndarray) -> bytes:
    """把 HxWx3 uint8 数组编码为 PNG 字节流。"""
    if image.ndim != 3 or image.shape[2] != 3 or image.dtype != np.uint8:
        raise fail(
            ErrorCode.E_PARA,
            "encode_rgb 仅接受 HxWx3 uint8 数组",
            shape=None if image.ndim != 3 else list(image.shape),
            dtype=str(image.dtype),
        )
    height, width, _ = image.shape
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    contiguous = np.ascontiguousarray(image)
    # 每行前补一个 filter type 0 字节。
    line_bytes = np.empty(height * (1 + width * 3), dtype=np.uint8)
    row_stride = 1 + width * 3
    for row in range(height):
        line_bytes[row * row_stride] = 0
        line_bytes[row * row_stride + 1 : (row + 1) * row_stride] = contiguous[row].reshape(-1)
    idat = zlib.compress(line_bytes.tobytes(), level=6)
    return PNG_SIGNATURE + _chunk(b"IHDR", ihdr) + _chunk(b"IDAT", idat) + _chunk(b"IEND", b"")


def _read_chunks(blob: bytes):
    if not blob.startswith(PNG_SIGNATURE):
        raise PngDecodeError("PNG 签名不匹配")
    offset = len(PNG_SIGNATURE)
    chunks = []
    saw_ihdr = False
    while offset < len(blob):
        if offset + 8 > len(blob):
            raise PngDecodeError("chunk 头被截断")
        length = struct.unpack(">I", blob[offset : offset + 4])[0]
        tag = blob[offset + 4 : offset + 8]
        start = offset + 8
        end = start + length
        if end + 4 > len(blob):
            raise PngDecodeError(f"chunk {tag!r} 数据被截断")
        payload = blob[start:end]
        crc_actual = zlib.crc32(tag + payload) & 0xFFFFFFFF
        crc_expect = struct.unpack(">I", blob[end : end + 4])[0]
        if crc_actual != crc_expect:
            raise PngDecodeError(f"chunk {tag!r} CRC 校验失败")
        if tag == b"IHDR":
            saw_ihdr = True
        chunks.append((tag, payload))
        offset = end + 4
        if tag == b"IEND":
            break
    if not saw_ihdr:
        raise PngDecodeError("缺少 IHDR")
    return chunks


def _paeth_predictor(left: int, above: int, upper_left: int) -> int:
    candidate = left + above - upper_left
    pa = abs(candidate - left)
    pb = abs(candidate - above)
    pc = abs(candidate - upper_left)
    if pa <= pb and pa <= pc:
        return left
    if pb <= pc:
        return above
    return upper_left


def decode_rgb(blob: bytes) -> np.ndarray:
    """解码 PNG 字节流为 HxWx3 uint8 数组。"""
    try:
        chunks = _read_chunks(blob)
        ihdr = next(payload for tag, payload in chunks if tag == b"IHDR")
        width, height, bit_depth, color_type, compression, flt, interlace = struct.unpack(
            ">IIBBBBB", ihdr
        )
        if (bit_depth, color_type, compression, flt, interlace) != (8, 2, 0, 0, 0):
            raise PngDecodeError("仅支持 8 位 RGB 非隔行 PNG")
        compressed = b"".join(payload for tag, payload in chunks if tag == b"IDAT")
        raw = zlib.decompress(compressed)
        stride = width * 3
        if len(raw) != height * (stride + 1):
            raise PngDecodeError("解压后扫描线长度不一致")
        image = np.zeros((height, stride), dtype=np.uint8)
        previous = np.zeros(stride, dtype=np.uint8)
        for row in range(height):
            line_start = row * (stride + 1)
            filter_type = raw[line_start]
            line = np.frombuffer(raw, dtype=np.uint8, count=stride, offset=line_start + 1).copy()
            if filter_type == 1:
                for idx in range(3, stride):
                    line[idx] = (line[idx] + line[idx - 3]) & 0xFF
            elif filter_type == 2:
                line = (line + previous) & 0xFF
            elif filter_type == 3:
                for idx in range(stride):
                    left = line[idx - 3] if idx >= 3 else 0
                    line[idx] = (line[idx] + ((left + int(previous[idx])) >> 1)) & 0xFF
            elif filter_type == 4:
                for idx in range(stride):
                    left = int(line[idx - 3]) if idx >= 3 else 0
                    up = int(previous[idx])
                    up_left = int(previous[idx - 3]) if idx >= 3 else 0
                    line[idx] = (line[idx] + _paeth_predictor(left, up, up_left)) & 0xFF
            elif filter_type != 0:
                raise PngDecodeError(f"未知 filter type: {filter_type}")
            image[row] = line
            previous = line
        return image.reshape(height, width, 3).copy()
    except PngDecodeError:
        raise
    except (zlib.error, struct.error, StopIteration, ValueError, OSError) as exc:
        raise PngDecodeError(f"PNG 解码失败: {exc}") from exc


def write_png(path: str, image: np.ndarray) -> None:
    try:
        with open(path, "wb") as handle:
            handle.write(encode_rgb(image))
    except OSError as exc:
        raise fail(ErrorCode.E_IO, f"写 PNG 失败: {path}", path=path) from exc


def read_png(path: str) -> np.ndarray:
    try:
        with open(path, "rb") as handle:
            blob = handle.read()
    except OSError as exc:
        raise fail(ErrorCode.E_DECODE, f"图像无法读取: {path}", path=path) from exc
    try:
        return decode_rgb(blob)
    except PngDecodeError as exc:
        raise fail(ErrorCode.E_DECODE, f"图像解码失败: {path}", path=path, detail=str(exc)) from exc
