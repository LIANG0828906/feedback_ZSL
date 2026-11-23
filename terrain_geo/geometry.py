"""Analytic geometric transforms: homography, radial distortion, affine.

Coordinate convention: pixel (i, j) has center (x=j, y=i). A stage maps
points from the source frame to the destination frame. Image resampling uses
the inverse direction (destination pixel -> source sample point).

Radial model with frame-local center and radius::

    x_d = c + (1 + k1*s^2 + k2*s^4) * (x - c),  s = |x - c| / radius

The stage forward direction is undistortion (distorted -> undistorted),
evaluated by Newton inversion of the monotone radial polynomial.
"""

from __future__ import annotations

import numpy as np

from .errors import GeoError, ErrorCode


def apply_homography(H: np.ndarray, points: np.ndarray) -> np.ndarray:
    """Forward map Nx2 points with a 3x3 homography (normalized last row)."""
    pts = np.atleast_2d(np.asarray(points, dtype=np.float64))
    ones = np.ones((pts.shape[0], 1), dtype=np.float64)
    homog = np.concatenate([pts, ones], axis=1) @ np.asarray(H).T
    w = homog[:, 2:3]
    if np.any(np.abs(w) < 1e-12):
        raise GeoError(ErrorCode.E_PARA, "homography maps a point to infinity",
                       field="H")
    return homog[:, :2] / w


def invert_homography(H: np.ndarray) -> np.ndarray:
    H = np.asarray(H, dtype=np.float64)
    try:
        return np.linalg.inv(H)
    except np.linalg.LinAlgError as exc:
        raise GeoError(ErrorCode.E_PARA, f"singular homography: {exc}",
                       field="H") from exc


def _normalize(center, radius, points):
    c = np.asarray(center, dtype=np.float64)
    delta = points - c
    r2 = np.einsum("ij,ij->i", delta, delta) / (radius * radius)
    return delta, np.maximum(r2, 0.0)


def distort_points(points, center, radius, k1, k2):
    """Apply the radial distortion polynomial (undistorted -> distorted)."""
    pts = np.atleast_2d(np.asarray(points, dtype=np.float64))
    delta, r2 = _normalize(center, radius, pts)
    scale = 1.0 + k1 * r2 + k2 * r2 * r2
    return np.asarray(center, dtype=np.float64) + delta * scale[:, None]


def undistort_points(points, center, radius, k1, k2,
                     max_iter: int = 30, tol: float = 1e-12) -> np.ndarray:
    """Invert the radial polynomial (distorted -> undistorted) via Newton's."""
    pts = np.atleast_2d(np.asarray(points, dtype=np.float64))
    c = np.asarray(center, dtype=np.float64)
    d = pts - c
    rd = np.linalg.norm(d / radius, axis=1)
    ru = rd.copy()
    for _ in range(max_iter):
        ru2 = ru * ru
        g = ru * (1.0 + k1 * ru2 + k2 * ru2 * ru2) - rd
        gp = 1.0 + 3.0 * k1 * ru2 + 5.0 * k2 * ru2 * ru2
        step = g / np.where(np.abs(gp) < 1e-12, 1e-12, gp)
        ru = np.maximum(ru - step, 0.0)
        if np.max(np.abs(step)) < tol:
            break
    direction = np.zeros_like(d)
    nonzero = rd > 1e-15
    direction[nonzero] = d[nonzero] / rd[nonzero, None]
    return c + direction * (ru * radius)[:, None]


def affine_forward(points, scale, translate):
    pts = np.atleast_2d(np.asarray(points, dtype=np.float64))
    s = np.asarray(scale, dtype=np.float64)
    t = np.asarray(translate, dtype=np.float64)
    return pts * s + t


def affine_inverse(points, scale, translate):
    pts = np.atleast_2d(np.asarray(points, dtype=np.float64))
    s = np.asarray(scale, dtype=np.float64)
    t = np.asarray(translate, dtype=np.float64)
    return (pts - t) / s


def numeric_jacobian(inverse_fn, points: np.ndarray,
                     eps: float = 1e-3) -> np.ndarray:
    """Forward-difference Jacobian of inverse_fn at Nx2 points -> Nx2x2."""
    pts = np.atleast_2d(np.asarray(points, dtype=np.float64))
    base = inverse_fn(pts)
    jac = np.empty((pts.shape[0], 2, 2), dtype=np.float64)
    for axis in range(2):
        bumped = pts.copy()
        bumped[:, axis] += eps
        moved = inverse_fn(bumped)
        jac[:, :, axis] = (moved - base) / eps
    return jac
