from __future__ import annotations

import struct
from pathlib import Path

import numpy as np
from PyQt5.QtGui import QImage

from .errors import DecodeError, MetaError


_PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"
_PNG_COLOR_CHANNELS = {0: 1, 2: 3, 3: 3, 4: 2, 6: 4}


def read_png_header(path: Path) -> dict[str, int]:
    try:
        with path.open("rb") as handle:
            signature = handle.read(8)
            if signature != _PNG_SIGNATURE:
                raise DecodeError("PNG 签名无效", details={"path": str(path)})
            length_raw = handle.read(4)
            chunk_type = handle.read(4)
            if len(length_raw) != 4 or chunk_type != b"IHDR":
                raise DecodeError("PNG 缺少 IHDR", details={"path": str(path)})
            length = struct.unpack(">I", length_raw)[0]
            if length < 13:
                raise DecodeError("PNG IHDR 长度无效", details={"path": str(path)})
            payload = handle.read(13)
            if len(payload) != 13:
                raise DecodeError("PNG IHDR 截断", details={"path": str(path)})
    except OSError as exc:
        raise DecodeError(
            "无法读取图像文件",
            details={"path": str(path), "reason": str(exc)},
        ) from exc

    width, height = struct.unpack(">II", payload[:8])
    bit_depth = payload[8]
    color_type = payload[9]
    if bit_depth != 8:
        raise MetaError(
            "仅支持 8 位通道图像",
            field="bit_depth",
            details={"path": str(path), "bit_depth": bit_depth},
        )
    channels = _PNG_COLOR_CHANNELS.get(color_type)
    if channels is None:
        raise MetaError(
            "不支持的 PNG 颜色类型",
            field="channels",
            details={"path": str(path), "color_type": color_type},
        )
    return {
        "width": int(width),
        "height": int(height),
        "channels": int(channels),
        "bit_depth": int(bit_depth),
        "color_type": int(color_type),
    }


def write_png(path: Path, array: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    qimage = array_to_qimage(array)
    if not qimage.save(str(path), "PNG"):
        raise OSError(f"无法写入 PNG: {path}")


def read_png(path: Path, channels: int) -> np.ndarray:
    qimage = QImage(str(path))
    if qimage.isNull():
        raise DecodeError("图像解码失败", details={"path": str(path)})
    if channels == 1:
        converted = qimage.convertToFormat(QImage.Format_Grayscale8)
        width = converted.width()
        height = converted.height()
        stride = converted.bytesPerLine()
        raw = np.frombuffer(
            converted.bits().asstring(converted.byteCount()),
            dtype=np.uint8,
        )
        raw = raw.reshape(height, stride)
        return np.array(raw[:, :width], dtype=np.uint8, copy=True, order="C")
    if channels == 3:
        converted = qimage.convertToFormat(QImage.Format_RGB888)
        width = converted.width()
        height = converted.height()
        stride = converted.bytesPerLine()
        raw = np.frombuffer(
            converted.bits().asstring(converted.byteCount()),
            dtype=np.uint8,
        )
        raw = raw.reshape(height, stride)
        channels_view = raw[:, : width * 3].reshape(height, width, 3)
        return np.array(channels_view, dtype=np.uint8, copy=True, order="C")
    raise MetaError(
        "加载器仅支持 1 或 3 通道",
        field="channels",
        details={"channels": channels, "path": str(path)},
    )


def array_to_qimage(array: np.ndarray) -> QImage:
    if array.dtype != np.uint8:
        array = np.clip(np.asarray(array), 0, 255).astype(np.uint8)
    if array.ndim == 2:
        contiguous = np.ascontiguousarray(array)
        height, width = contiguous.shape
        return QImage(
            contiguous.data,
            width,
            height,
            width,
            QImage.Format_Grayscale8,
        ).copy()
    if array.ndim == 3 and array.shape[2] in {3, 4}:
        height, width, channels = array.shape
        if channels == 4:
            contiguous = np.ascontiguousarray(array[:, :, :3])
            return QImage(
                contiguous.data,
                width,
                height,
                width * 3,
                QImage.Format_RGB888,
            ).copy()
        contiguous = np.ascontiguousarray(array)
        return QImage(
            contiguous.data,
            width,
            height,
            width * 3,
            QImage.Format_RGB888,
        ).copy()
    raise ValueError(f"unsupported image shape: {array.shape}")

