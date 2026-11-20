"""Dependency-free CPU point renderer (software projection fallback)."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ..cloud import PointCloud
from .camera import OrbitCamera


@dataclass
class RenderResult:
    width: int
    height: int
    pixels: np.ndarray  # uint8 (H, W, 3), RGB
    backend: str

    def write_ppm(self, path: str) -> None:
        height, width = self.pixels.shape[:2]
        header = f"P6\n{width} {height}\n255\n".encode("ascii")
        with open(path, "wb") as handle:
            handle.write(header)
            handle.write(self.pixels.tobytes())


def _blend(base: np.ndarray, color: np.ndarray, alpha: float) -> np.ndarray:
    return np.rint(base * (1.0 - alpha) + color * alpha).astype(np.uint8)


class CPURenderer:
    """Software renderer: perspective project points into an RGB buffer."""

    backend = "cpu"

    def __init__(self, width: int = 640, height: int = 480,
                 background=(18, 22, 28)):
        self.width = int(width)
        self.height = int(height)
        self.background = np.array(background, dtype=np.uint8)

    def render(self, cloud: PointCloud, camera: OrbitCamera,
               highlight_indices: np.ndarray | None = None,
               highlight_color=(255, 215, 0),
               diff_added: np.ndarray | None = None,
               diff_deleted: np.ndarray | None = None,
               diff_color_changed: np.ndarray | None = None
               ) -> RenderResult:
        vp = camera.view_proj(self.width, self.height)
        n = cloud.count
        pixels = np.tile(self.background,
                         (self.height, self.width, 1)).astype(np.uint8)
        if n == 0:
            return RenderResult(self.width, self.height, pixels, self.backend)

        homo = np.concatenate(
            [cloud.xyz, np.ones((n, 1), dtype=np.float64)], axis=1)
        clip = homo @ vp.T
        valid = clip[:, 3] > 0
        ndc = np.zeros((n, 3), dtype=np.float64)
        np.divide(clip[:, :3], clip[:, 3:4], out=ndc, where=valid[:, None])
        sx = ((ndc[:, 0] + 1.0) * 0.5 * self.width).astype(np.int64)
        sy = ((1.0 - (ndc[:, 1] + 1.0) * 0.5) * self.height).astype(np.int64)
        on_screen = valid & (sx >= 0) & (sx < self.width) \
            & (sy >= 0) & (sy < self.height)

        depth = clip[:, 2] / clip[:, 3]
        order = np.argsort(depth)[::-1]  # far to near
        colors = cloud.rgb.copy()
        highlight = np.zeros(n, dtype=bool)
        if highlight_indices is not None and highlight_indices.size:
            highlight[highlight_indices] = True
        added = np.zeros(n, dtype=bool)
        if diff_added is not None and diff_added.size:
            added[diff_added] = True
        changed = np.zeros(n, dtype=bool)
        if diff_color_changed is not None and diff_color_changed.size:
            changed[diff_color_changed] = True

        hl = np.array(highlight_color, dtype=np.uint8)
        green = np.array([0, 230, 118], dtype=np.uint8)
        blue = np.array([80, 160, 255], dtype=np.uint8)
        for i in order:
            if not on_screen[i]:
                continue
            color = colors[i]
            if highlight[i]:
                color = hl
            elif added[i]:
                color = green
            elif changed[i]:
                color = blue
            pixels[sy[i], sx[i]] = color
        return RenderResult(self.width, self.height, pixels, self.backend)

    def close(self) -> None:
        return None
