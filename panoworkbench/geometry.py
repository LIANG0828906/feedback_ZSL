"""Homography and canvas geometry helpers."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable

import cv2
import numpy as np

from .errors import ErrorCode, WorkbenchError


@dataclass(frozen=True)
class CanvasGeometry:
    origin_x: int
    origin_y: int
    width: int
    height: int
    raw_min_x: float
    raw_min_y: float
    raw_max_x: float
    raw_max_y: float

    @property
    def translation(self) -> np.ndarray:
        return np.array(
            [[1.0, 0.0, -float(self.origin_x)], [0.0, 1.0, -float(self.origin_y)], [0.0, 0.0, 1.0]],
            dtype=np.float64,
        )

    def to_dict(self) -> dict[str, float | int]:
        return {
            "origin_x": self.origin_x,
            "origin_y": self.origin_y,
            "width": self.width,
            "height": self.height,
            "raw_min_x": self.raw_min_x,
            "raw_min_y": self.raw_min_y,
            "raw_max_x": self.raw_max_x,
            "raw_max_y": self.raw_max_y,
        }


def normalize_homography(matrix: np.ndarray | Iterable[Iterable[float]]) -> np.ndarray:
    h = np.asarray(matrix, dtype=np.float64)
    if h.shape != (3, 3):
        raise WorkbenchError(
            ErrorCode.META, "homography must be a 3x3 matrix", field="homography"
        )
    if not np.all(np.isfinite(h)):
        raise WorkbenchError(
            ErrorCode.META,
            "homography contains non-finite values",
            field="homography",
        )
    scale = float(h[2, 2])
    if abs(scale) < 1e-14:
        # A finite projective point at infinity can still be a valid matrix.
        norm = np.linalg.norm(h)
        if not math.isfinite(norm) or norm < 1e-14:
            raise WorkbenchError(
                ErrorCode.META, "homography is singular", field="homography"
            )
        return h / norm
    h = h / scale
    if abs(float(np.linalg.det(h))) < 1e-12:
        raise WorkbenchError(
            ErrorCode.META, "homography is not invertible", field="homography"
        )
    return h


def invert_homography(matrix: np.ndarray) -> np.ndarray:
    h = normalize_homography(matrix)
    try:
        inverse = np.linalg.inv(h)
    except np.linalg.LinAlgError as exc:
        raise WorkbenchError(
            ErrorCode.META, "homography is not invertible", field="homography"
        ) from exc
    return normalize_homography(inverse)


def apply_homography(matrix: np.ndarray, points: np.ndarray) -> np.ndarray:
    pts = np.asarray(points, dtype=np.float64).reshape(-1, 2)
    h = np.asarray(matrix, dtype=np.float64)
    homogeneous = np.column_stack([pts, np.ones(len(pts), dtype=np.float64)])
    mapped = homogeneous @ h.T
    denom = mapped[:, 2]
    if np.any(np.abs(denom) < 1e-14):
        raise WorkbenchError(
            ErrorCode.HOMO,
            "homography maps a point to infinity",
            field="homography",
        )
    return mapped[:, :2] / denom[:, None]


def image_corners(width: int, height: int) -> np.ndarray:
    return np.array(
        [[0.0, 0.0], [float(width), 0.0], [float(width), float(height)], [0.0, float(height)]],
        dtype=np.float64,
    )


def compute_canvas_geometry(
    homographies_to_base: dict[int, np.ndarray],
    image_sizes: dict[int, tuple[int, int]],
    *,
    pad: int = 0,
) -> CanvasGeometry:
    if not homographies_to_base:
        raise WorkbenchError(ErrorCode.HOMO, "no registered images for canvas")
    all_corners: list[np.ndarray] = []
    for index, h in homographies_to_base.items():
        if index not in image_sizes:
            continue
        width, height = image_sizes[index]
        all_corners.append(apply_homography(h, image_corners(width, height)))
    if not all_corners:
        raise WorkbenchError(ErrorCode.HOMO, "no image corners for canvas")
    points = np.vstack(all_corners)
    min_x = float(np.min(points[:, 0])) - pad
    min_y = float(np.min(points[:, 1])) - pad
    max_x = float(np.max(points[:, 0])) + pad
    max_y = float(np.max(points[:, 1])) + pad
    origin_x = int(math.floor(min_x))
    origin_y = int(math.floor(min_y))
    width = int(math.ceil(max_x) - origin_x)
    height = int(math.ceil(max_y) - origin_y)
    if width <= 0 or height <= 0:
        raise WorkbenchError(ErrorCode.HOMO, "computed canvas is empty")
    return CanvasGeometry(
        origin_x=origin_x,
        origin_y=origin_y,
        width=width,
        height=height,
        raw_min_x=min_x,
        raw_min_y=min_y,
        raw_max_x=max_x,
        raw_max_y=max_y,
    )


def translation_matrix(tx: float, ty: float) -> np.ndarray:
    return np.array([[1.0, 0.0, tx], [0.0, 1.0, ty], [0.0, 0.0, 1.0]])


def rotation_matrix(angle_deg: float) -> np.ndarray:
    theta = math.radians(angle_deg)
    c, s = math.cos(theta), math.sin(theta)
    return np.array([[c, -s, 0.0], [s, c, 0.0], [0.0, 0.0, 1.0]])


def scale_matrix(scale: float) -> np.ndarray:
    return np.array([[scale, 0.0, 0.0], [0.0, scale, 0.0], [0.0, 0.0, 1.0]])


def corner_correspondence_error(
    estimated: np.ndarray,
    truth: np.ndarray,
    width: int,
    height: int,
) -> float:
    corners = image_corners(width, height)
    estimated_points = apply_homography(estimated, corners)
    truth_points = apply_homography(truth, corners)
    return float(np.max(np.linalg.norm(estimated_points - truth_points, axis=1)))


def relative_homography_error(estimated: np.ndarray, truth: np.ndarray) -> float:
    est = normalize_homography(estimated)
    gt = normalize_homography(truth)
    denom = max(float(np.linalg.norm(gt)), 1e-12)
    return float(np.linalg.norm(est - gt) / denom)


def warp_to_canvas(
    image: np.ndarray,
    homography_to_canvas: np.ndarray,
    size: tuple[int, int],
    *,
    interpolation: int = cv2.INTER_LINEAR,
) -> tuple[np.ndarray, np.ndarray]:
    width, height = size
    warped = cv2.warpPerspective(
        image,
        homography_to_canvas,
        (width, height),
        flags=interpolation,
        borderMode=cv2.BORDER_CONSTANT,
        borderValue=(0, 0, 0),
    )
    source_mask = np.full((image.shape[0], image.shape[1]), 255, dtype=np.uint8)
    mask = cv2.warpPerspective(
        source_mask,
        homography_to_canvas,
        (width, height),
        flags=cv2.INTER_NEAREST,
        borderMode=cv2.BORDER_CONSTANT,
        borderValue=0,
    )
    return warped, mask > 0
