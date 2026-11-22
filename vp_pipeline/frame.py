"""Frame result type and frame-level hashing."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .serialization import bytes_sha256, canonical_sha256
from .version import ALGORITHM_VERSION


@dataclass
class FrameResult:
    frame_index: int
    width: int
    height: int
    channels: int
    fps_num: int
    fps_den: int
    pixels: np.ndarray
    mean_alpha: float
    mean_linear_luminance: float
    opaque_pixels: int
    visible_layer_ids: tuple[str, ...]
    attribution: np.ndarray | None = None

    @property
    def shape(self) -> tuple[int, int, int]:
        return (self.height, self.width, self.channels)

    @property
    def pixel_bytes(self) -> bytes:
        return np.ascontiguousarray(self.pixels, dtype=np.uint8).tobytes()

    @property
    def content_hash(self) -> str:
        header = {
            "algorithm_version": ALGORITHM_VERSION,
            "frame_index": self.frame_index,
            "width": self.width,
            "height": self.height,
            "channels": self.channels,
            "fps_num": self.fps_num,
            "fps_den": self.fps_den,
        }
        return canonical_sha256(
            {
                "header": header,
                "pixel_sha256": bytes_sha256(self.pixel_bytes),
            }
        )

    def stats_dict(self) -> dict[str, Any]:
        return {
            "frame": self.frame_index,
            "width": self.width,
            "height": self.height,
            "channels": self.channels,
            "fps_num": self.fps_num,
            "fps_den": self.fps_den,
            "mean_alpha": self.mean_alpha,
            "mean_linear_luminance": self.mean_linear_luminance,
            "opaque_pixels": self.opaque_pixels,
            "visible_layer_ids": list(self.visible_layer_ids),
            "content_hash": self.content_hash,
        }

    @classmethod
    def from_pixels(cls, pixels: np.ndarray, stats: dict[str, Any]) -> "FrameResult":
        return cls(
            frame_index=int(stats["frame"]),
            width=int(stats["width"]),
            height=int(stats["height"]),
            channels=int(stats["channels"]),
            fps_num=int(stats["fps_num"]),
            fps_den=int(stats["fps_den"]),
            pixels=np.ascontiguousarray(pixels, dtype=np.uint8),
            mean_alpha=float(stats.get("mean_alpha", 0.0)),
            mean_linear_luminance=float(stats.get("mean_linear_luminance", 0.0)),
            opaque_pixels=int(stats.get("opaque_pixels", 0)),
            visible_layer_ids=tuple(stats.get("visible_layer_ids", [])),
        )
