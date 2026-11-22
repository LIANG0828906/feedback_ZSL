"""本地确定性图像容器格式与编解码。

为避免对外部图像库/在线服务的依赖，样本使用自描述的 ``.cimg`` 容器：

    magic(8) = b"CWBIMG01"
    header   = JSON(utf-8) + b"\\n"
    payload  = raw little-endian 像素数据

header 中声明 height/width/channels/depth(8|16)，解码时严格校验位深与
负载长度，任何魔数、JSON、长度或位深不一致都返回 E_DECODE/E_DEPTH。
"""

from __future__ import annotations

import json

import numpy as np

from .errors import CwbError, E_DECODE, E_DEPTH

MAGIC = b"CWBIMG01"
_DTYPES = {8: np.uint8, 16: np.uint16}


def encode_image(path: str, data: np.ndarray, depth: int) -> None:
    """把 HxWxC 的 uint8/uint16 数组原子写入 .cimg 容器。"""
    if data.ndim == 2:
        data = data[..., None]
    if depth not in _DTYPES or data.dtype != _DTYPES[depth]:
        raise CwbError(E_DEPTH, f"数组 dtype 与声明位深 {depth} 不一致",
                       tag="depth")
    header = {
        "height": int(data.shape[0]),
        "width": int(data.shape[1]),
        "channels": int(data.shape[2]),
        "depth": depth,
    }
    header_bytes = json.dumps(header, separators=(",", ":")).encode("utf-8")
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        fh.write(MAGIC)
        fh.write(header_bytes)
        fh.write(b"\n")
        fh.write(np.ascontiguousarray(data).tobytes())
    import os
    os.replace(tmp, path)


def decode_image(path: str, expected_depth: int | None = None) -> np.ndarray:
    """解码 .cimg 容器，返回 HxWxC 数组；损坏时返回 E_DECODE。"""
    try:
        with open(path, "rb") as fh:
            blob = fh.read()
        if blob[: len(MAGIC)] != MAGIC:
            raise CwbError(E_DECODE, f"魔数非法: {path}")
        nl = blob.index(b"\n", len(MAGIC))
        header = json.loads(blob[len(MAGIC): nl].decode("utf-8"))
        payload = blob[nl + 1:]
    except CwbError:
        raise
    except Exception as exc:  # 截断、JSON 错误等统一归为解码失败
        raise CwbError(E_DECODE, f"图像损坏无法解码: {path} ({exc})") from exc

    depth = header.get("depth")
    if depth not in _DTYPES:
        raise CwbError(E_DECODE, f"位深声明非法: {depth}", tag="depth")
    if expected_depth is not None and depth != expected_depth:
        raise CwbError(E_DEPTH,
                       f"实际位深 {depth} 与期望 {expected_depth} 不符",
                       tag="depth")
    h, w, c = header["height"], header["width"], header["channels"]
    dtype = _DTYPES[depth]
    need = h * w * c * dtype().itemsize
    if len(payload) != need:
        raise CwbError(E_DECODE,
                       f"负载长度 {len(payload)} 与声明 {need} 不符（截断/损坏）")
    data = np.frombuffer(payload, dtype=dtype).reshape(h, w, c)
    return np.array(data, copy=True)


def probe_depth(path: str) -> int:
    """只读 header 返回位深声明，用于运行前清单校验。"""
    with open(path, "rb") as fh:
        blob = fh.read(4096)
    if blob[: len(MAGIC)] != MAGIC:
        raise CwbError(E_DECODE, f"魔数非法: {path}")
    nl = blob.index(b"\n", len(MAGIC))
    header = json.loads(blob[len(MAGIC): nl].decode("utf-8"))
    if header.get("depth") not in _DTYPES:
        raise CwbError(E_DEPTH, "位深声明非法", tag="depth")
    return int(header["depth"])
