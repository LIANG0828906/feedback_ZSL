from __future__ import annotations

from pathlib import Path

import numpy as np

from .errors import ErrorCode, WorkbenchError
from .raster import rgba8


def save_rgba_png(image: np.ndarray, path: str | Path) -> Path:
    try:
        from PySide6.QtGui import QImage
    except ImportError as exc:
        raise WorkbenchError(
            ErrorCode.E_STAGE,
            f"PySide6 is required to export PNG images: {exc}",
        ) from exc
    rgba = np.ascontiguousarray(rgba8(image))
    height, width, _ = rgba.shape
    qimage = QImage(
        rgba.data,
        width,
        height,
        int(rgba.strides[0]),
        QImage.Format.Format_RGBA8888,
    ).copy()
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    if not qimage.save(str(output), "PNG"):
        raise WorkbenchError(ErrorCode.E_STAGE, f"failed to save PNG to {output}")
    return output


def save_coverage_png(coverage: np.ndarray, path: str | Path) -> Path:
    gray = np.clip(coverage, 0.0, 1.0)
    rgba = np.empty((*gray.shape, 4), dtype=np.float32)
    rgba[..., :3] = gray[..., None]
    rgba[..., 3] = 1.0
    return save_rgba_png(rgba, path)


def save_heatmap_png(heatmap: np.ndarray, path: str | Path) -> Path:
    intensity = np.clip(heatmap, 0.0, 1.0)
    rgba = np.empty((*intensity.shape, 4), dtype=np.float32)
    rgba[..., 0] = np.clip(intensity * 1.8, 0.0, 1.0)
    rgba[..., 1] = np.clip(intensity * 0.55, 0.0, 1.0)
    rgba[..., 2] = np.clip((1.0 - intensity) * 0.08, 0.0, 1.0)
    rgba[..., 3] = np.where(intensity > 0.0, 1.0, 0.0)
    return save_rgba_png(rgba, path)

