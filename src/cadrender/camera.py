"""相机、投影与坐标约定。

统一右手坐标系、看向 -Z 的观察空间与透视投影。预览与精确两条渲染路径
共用同一 :class:`Camera` 实例，保证内外参、近远裁剪平面完全一致。
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import E_PARA, ParaError


def normalize(v: np.ndarray) -> np.ndarray:
    n = np.linalg.norm(v)
    return v / n if n > 1e-15 else v


@dataclass(frozen=True)
class Camera:
    eye: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    near: float
    far: float
    width: int
    height: int

    def __post_init__(self) -> None:
        if self.near <= 0 or self.far <= self.near:
            raise ParaError("裁剪平面非法，要求 0 < near < far",
                            module="camera", field="near/far",
                            context={"near": self.near, "far": self.far})
        if not (0.0 < self.fov_y_deg < 180.0):
            raise ParaError("fov_y 必须位于 (0,180) 度",
                            module="camera", field="fov_y_deg")
        if self.width <= 0 or self.height <= 0:
            raise ParaError("画面尺寸必须为正", module="camera",
                            field="width/height")

    def view_matrix(self) -> np.ndarray:
        eye = np.asarray(self.eye, dtype=np.float64)
        f = normalize(np.asarray(self.target) - eye)
        s = normalize(np.cross(f, np.asarray(self.up, dtype=np.float64)))
        u = np.cross(s, f)
        m = np.eye(4)
        m[0, :3] = s
        m[1, :3] = u
        m[2, :3] = -f
        m[0, 3] = -s @ eye
        m[1, 3] = -u @ eye
        m[2, 3] = f @ eye
        return m

    def proj_matrix(self) -> np.ndarray:
        aspect = self.width / self.height
        f = 1.0 / np.tan(np.radians(self.fov_y_deg) / 2.0)
        n, fp = self.near, self.far
        m = np.zeros((4, 4))
        m[0, 0] = f / aspect
        m[1, 1] = f
        m[2, 2] = (fp + n) / (n - fp)
        m[2, 3] = (2 * fp * n) / (n - fp)
        m[3, 2] = -1.0
        return m

    def view_proj(self) -> np.ndarray:
        return self.proj_matrix() @ self.view_matrix()

    def frame_signature(self) -> tuple:
        return (
            tuple(np.round(np.asarray(self.eye), 9)),
            tuple(np.round(np.asarray(self.target), 9)),
            tuple(np.round(np.asarray(self.up), 9)),
            round(self.fov_y_deg, 9), round(self.near, 9),
            round(self.far, 9), self.width, self.height,
        )
