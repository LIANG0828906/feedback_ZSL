"""Image decoding, hashing, export and visual-difference helpers."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image, UnidentifiedImageError

from .errors import WorkbenchError


_MODE_TO_CHANNELS = {"L": 1, "LA": 2, "RGB": 3, "RGBA": 4}
_SUPPORTED_MODES = frozenset(_MODE_TO_CHANNELS)


@dataclass(frozen=True)
class DecodedImage:
    path: Path
    array: np.ndarray
    mode: str
    width: int
    height: int
    channels: int
    content_hash: str

    @property
    def dtype(self) -> str:
        return str(self.array.dtype)

    @property
    def shape(self) -> tuple[int, ...]:
        return tuple(self.array.shape)

    def metadata(self) -> dict[str, Any]:
        return {
            "path": str(self.path),
            "mode": self.mode,
            "width": self.width,
            "height": self.height,
            "channels": self.channels,
            "dtype": self.dtype,
            "shape": list(self.shape),
            "content_hash": self.content_hash,
        }


def hash_image(array: np.ndarray, mode: str) -> str:
    canonical = np.ascontiguousarray(array)
    digest = hashlib.sha256()
    digest.update(mode.encode("ascii"))
    digest.update(b"\0")
    digest.update(str(canonical.dtype).encode("ascii"))
    digest.update(b"\0")
    digest.update(repr(tuple(canonical.shape)).encode("ascii"))
    digest.update(b"\0")
    digest.update(canonical.tobytes(order="C"))
    return digest.hexdigest()


def decode_image(path: str | Path) -> DecodedImage:
    image_path = Path(path)
    if not image_path.is_file():
        raise WorkbenchError("E_DECODE", f"输入文件不存在: {image_path}")
    try:
        with Image.open(image_path) as probe:
            probe.verify()
        with Image.open(image_path) as image:
            image.load()
            mode = image.mode
            if mode == "1":
                image = image.convert("L")
                mode = "L"
            elif mode == "P":
                image = image.convert("RGBA")
                mode = "RGBA"
            elif mode not in _SUPPORTED_MODES:
                if "A" in image.getbands():
                    image = image.convert("RGBA")
                    mode = "RGBA"
                else:
                    image = image.convert("RGB")
                    mode = "RGB"
            array = np.array(image, dtype=np.uint8, copy=True)
            width, height = image.size
    except (UnidentifiedImageError, OSError, ValueError, SyntaxError) as exc:
        raise WorkbenchError(
            "E_DECODE",
            f"无法解码输入图像 {image_path.name}: {exc}",
        ) from exc

    expected_channels = _MODE_TO_CHANNELS[mode]
    expected_shape = (
        (height, width)
        if expected_channels == 1
        else (height, width, expected_channels)
    )
    if array.shape != expected_shape:
        raise WorkbenchError(
            "E_DECODE",
            f"图像通道布局异常，期望 {expected_shape}，实际 {array.shape}",
        )
    array = np.ascontiguousarray(array)
    return DecodedImage(
        path=image_path,
        array=array,
        mode=mode,
        width=width,
        height=height,
        channels=expected_channels,
        content_hash=hash_image(array, mode),
    )


def save_png(array: np.ndarray, mode: str, path: str | Path) -> None:
    if mode not in _SUPPORTED_MODES:
        raise WorkbenchError("E_ENCODE", f"不支持导出模式: {mode}")
    image = Image.frombytes(mode, (array.shape[1], array.shape[0]), array.tobytes())
    image.save(Path(path), format="PNG")


def first_mismatch(
    left: np.ndarray,
    right: np.ndarray,
) -> dict[str, int] | None:
    if left.shape != right.shape:
        return {"x": 0, "y": 0, "channel": 0}
    if left.dtype != right.dtype:
        return {"x": 0, "y": 0, "channel": 0}
    different = left != right
    if not np.any(different):
        return None
    if different.ndim == 2:
        y, x = np.argwhere(different)[0]
        return {"x": int(x), "y": int(y), "channel": 0}
    y, x, channel = np.argwhere(different)[0]
    return {"x": int(x), "y": int(y), "channel": int(channel)}


def difference_heatmap(
    original: np.ndarray,
    enhanced: np.ndarray,
) -> np.ndarray:
    if original.shape != enhanced.shape:
        raise WorkbenchError("E_DIFF", "差异热力图输入尺寸不一致")
    left = original.astype(np.int16, copy=False)
    right = enhanced.astype(np.int16, copy=False)
    if left.ndim == 2:
        magnitude = np.abs(left - right)
    else:
        magnitude = np.max(np.abs(left - right), axis=2)
    value = np.clip(magnitude.astype(np.float32) / 255.0, 0.0, 1.0)

    stops = np.array([0.0, 0.18, 0.42, 0.68, 1.0], dtype=np.float32)
    colors = np.array(
        [
            [8, 10, 24],
            [52, 30, 122],
            [196, 42, 92],
            [246, 171, 48],
            [255, 250, 224],
        ],
        dtype=np.float32,
    )
    heatmap = np.empty((*value.shape, 3), dtype=np.uint8)
    for channel in range(3):
        heatmap[..., channel] = np.rint(
            np.interp(value, stops, colors[:, channel])
        ).astype(np.uint8)
    return heatmap



