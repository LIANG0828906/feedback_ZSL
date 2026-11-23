"""Deterministic robust homography estimation and quality metrics."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Any

import cv2
import numpy as np

from .config import HomographyConfig
from .errors import ErrorCode, WorkbenchError
from .features import MatchSet
from .geometry import (
    corner_correspondence_error,
    normalize_homography,
    relative_homography_error,
)


@dataclass(frozen=True)
class HomographyEstimate:
    homography_a_to_b: np.ndarray
    inlier_mask: np.ndarray
    residuals: np.ndarray
    inlier_points_a: np.ndarray
    inlier_points_b: np.ndarray
    residual_median: float
    residual_p95: float
    covariance_condition: float
    truth_corner_error: float | None
    truth_relative_error: float | None
    truth_ok: bool | None
    iterations: int

    @property
    def inliers(self) -> int:
        return int(np.count_nonzero(self.inlier_mask))

    def to_dict(self, total_matches: int) -> dict[str, Any]:
        return {
            "matches": total_matches,
            "inliers": self.inliers,
            "inlier_ratio": self.inliers / total_matches if total_matches else 0.0,
            "residual_median": self.residual_median,
            "residual_p95": self.residual_p95,
            "covariance_condition": self.covariance_condition,
            "truth_corner_error": self.truth_corner_error,
            "truth_relative_error": self.truth_relative_error,
            "truth_ok": self.truth_ok,
            "iterations": self.iterations,
        }


def _normalize_points(points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    pts = np.asarray(points, dtype=np.float64).reshape(-1, 2)
    center = np.mean(pts, axis=0)
    centered = pts - center
    mean_distance = float(np.mean(np.linalg.norm(centered, axis=1)))
    scale = np.sqrt(2.0) / mean_distance if mean_distance > 1e-12 else 1.0
    transform = np.array(
        [[scale, 0.0, -scale * center[0]], [0.0, scale, -scale * center[1]], [0.0, 0.0, 1.0]],
        dtype=np.float64,
    )
    homogeneous = np.column_stack([pts, np.ones(len(pts))])
    transformed = homogeneous @ transform.T
    return transformed[:, :2], transform


def _dlt(src: np.ndarray, dst: np.ndarray) -> np.ndarray | None:
    if len(src) < 4 or len(dst) < 4:
        return None
    src_norm, t_src = _normalize_points(src)
    dst_norm, t_dst = _normalize_points(dst)
    rows: list[np.ndarray] = []
    for (x, y), (u, v) in zip(src_norm, dst_norm, strict=True):
        rows.append(np.array([-x, -y, -1.0, 0.0, 0.0, 0.0, u * x, u * y, u]))
        rows.append(np.array([0.0, 0.0, 0.0, -x, -y, -1.0, v * x, v * y, v]))
    matrix = np.asarray(rows, dtype=np.float64)
    try:
        _, _, vh = np.linalg.svd(matrix, full_matrices=False)
    except np.linalg.LinAlgError:
        return None
    normalized = vh[-1].reshape(3, 3)
    if abs(normalized[2, 2]) < 1e-14:
        return None
    h = np.linalg.inv(t_dst) @ normalized @ t_src
    if not np.all(np.isfinite(h)) or abs(h[2, 2]) < 1e-14:
        return None
    h = h / h[2, 2]
    determinant = float(np.linalg.det(h))
    if not np.isfinite(determinant) or abs(determinant) < 1e-12:
        return None
    return h


def _sample_is_degenerate(points: np.ndarray) -> bool:
    """Return whether four sample points cannot determine a homography."""

    pts = np.asarray(points, dtype=np.float64).reshape(-1, 2)
    if len(pts) != 4 or len(np.unique(pts, axis=0)) < 4:
        return True
    spread = float(max(np.ptp(pts[:, 0]), np.ptp(pts[:, 1])))
    if not np.isfinite(spread) or spread <= 1e-12:
        return True
    normalized = (pts - np.mean(pts, axis=0)) / spread
    for first in range(2):
        for second in range(first + 1, 3):
            for third in range(second + 1, 4):
                edge_a = normalized[second] - normalized[first]
                edge_b = normalized[third] - normalized[first]
                area = abs(edge_a[0] * edge_b[1] - edge_a[1] * edge_b[0])
                if area <= 1e-8:
                    return True
    return False


def _configuration_is_degenerate(points: np.ndarray) -> bool:
    """Return whether a point set is collinear or has fewer than four points."""

    pts = np.asarray(points, dtype=np.float64).reshape(-1, 2)
    if len(pts) < 4 or len(np.unique(pts, axis=0)) < 4:
        return True
    centered = pts - np.mean(pts, axis=0)
    singular_values = np.linalg.svd(centered, compute_uv=False)
    if len(singular_values) < 2 or singular_values[0] <= 1e-12:
        return True
    return bool(singular_values[1] <= 1e-9 * singular_values[0])


def _project(h: np.ndarray, points: np.ndarray) -> np.ndarray:
    pts = np.asarray(points, dtype=np.float64).reshape(-1, 2)
    homogeneous = np.column_stack([pts, np.ones(len(pts))])
    mapped = homogeneous @ h.T
    denom = mapped[:, 2]
    result = np.full((len(pts), 2), np.inf, dtype=np.float64)
    valid = np.abs(denom) > 1e-12
    result[valid] = mapped[valid, :2] / denom[valid, None]
    return result


def _symmetric_errors(
    h: np.ndarray, src: np.ndarray, dst: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    forward = _project(h, src)
    forward_error = np.linalg.norm(forward - dst, axis=1)
    try:
        inverse = np.linalg.inv(h)
        backward = _project(inverse, dst)
        backward_error = np.linalg.norm(backward - src, axis=1)
    except np.linalg.LinAlgError:
        backward_error = np.full(len(src), np.inf)
    error = np.maximum(forward_error, backward_error)
    error[~np.isfinite(error)] = np.inf
    return error, forward_error


def _forward_errors(h: np.ndarray, src: np.ndarray, dst: np.ndarray) -> np.ndarray:
    forward = _project(h, src)
    error = np.linalg.norm(forward - dst, axis=1)
    error[~np.isfinite(error)] = np.inf
    return error


def _covariance_condition(
    h: np.ndarray,
    src: np.ndarray,
    dst: np.ndarray,
    residuals: np.ndarray,
    regularization: float,
) -> float:
    if len(src) < 6:
        return 1e18
    h = normalize_homography(h)
    u = src[:, 0]
    v = src[:, 1]
    projected = _project(h, src)
    x = projected[:, 0]
    y = projected[:, 1]
    w = h[2, 0] * u + h[2, 1] * v + h[2, 2]
    valid = np.abs(w) > 1e-12
    if not np.all(valid):
        return 1e18
    jacobian = np.zeros((2 * len(src), 8), dtype=np.float64)
    jacobian[0::2, 0] = u / w
    jacobian[0::2, 1] = v / w
    jacobian[0::2, 2] = 1.0 / w
    jacobian[0::2, 6] = -u * x / w
    jacobian[0::2, 7] = -v * x / w
    jacobian[1::2, 3] = u / w
    jacobian[1::2, 4] = v / w
    jacobian[1::2, 5] = 1.0 / w
    jacobian[1::2, 6] = -u * y / w
    jacobian[1::2, 7] = -v * y / w
    normal = jacobian.T @ jacobian
    normal += np.eye(8, dtype=np.float64) * max(regularization, 1e-15)
    dof = max(2 * len(src) - 8, 1)
    sigma_squared = float(np.sum(residuals**2)) / dof
    try:
        covariance = sigma_squared * np.linalg.pinv(normal)
        condition = float(np.linalg.cond(covariance))
    except np.linalg.LinAlgError:
        return 1e18
    if not np.isfinite(condition):
        return 1e18
    return min(max(condition, 1.0), 1e18)


def _pair_seed(pair_key: str, consensus_threshold: float, iterations: int) -> int:
    payload = f"{pair_key}|{consensus_threshold:.12g}|{iterations}".encode("utf-8")
    return int.from_bytes(hashlib.sha256(payload).digest()[:8], "little", signed=False)


def estimate_homography(
    matches: MatchSet,
    image_size_a: tuple[int, int],
    config: HomographyConfig,
    *,
    pair_key: str,
    truth_homography: np.ndarray | None = None,
    truth_uses_forward_direction: bool = True,
) -> HomographyEstimate:
    """Deterministic RANSAC + normalized DLT homography estimation."""

    config.validate()
    src = np.asarray(matches.points_a, dtype=np.float64)
    dst = np.asarray(matches.points_b, dtype=np.float64)
    if len(src) < max(4, config.min_inliers):
        raise WorkbenchError(
            ErrorCode.HOMO,
            f"{len(src)} matches cannot satisfy min_inliers={config.min_inliers}",
            field="homography.min_inliers",
            details={"matches": len(src), "minimum": config.min_inliers},
        )
    consensus_threshold = config.consensus_threshold
    rng = np.random.default_rng(
        _pair_seed(pair_key, consensus_threshold, config.max_iterations)
    )
    best_h: np.ndarray | None = None
    best_mask: np.ndarray | None = None
    best_key: tuple[int, float, float] | None = None
    iterations_run = 0
    no_improvement = 0

    for iteration in range(config.max_iterations):
        iterations_run = iteration + 1
        sample = rng.choice(len(src), size=4, replace=False)
        if len(np.unique(sample)) < 4:
            continue
        sample_src = src[sample]
        sample_dst = dst[sample]
        if _sample_is_degenerate(sample_src) or _sample_is_degenerate(sample_dst):
            continue
        try:
            sampled, _ = cv2.findHomography(
                sample_src.reshape(4, 1, 2),
                sample_dst.reshape(4, 1, 2),
                0,
            )
        except cv2.error:
            sampled = None
        if sampled is not None and np.all(np.isfinite(sampled)):
            try:
                h = normalize_homography(sampled)
            except WorkbenchError:
                h = _dlt(sample_src, sample_dst)
        else:
            h = _dlt(sample_src, sample_dst)
        if h is None:
            continue
        errors = _forward_errors(h, src, dst)
        mask = errors <= consensus_threshold
        inlier_count = int(np.count_nonzero(mask))
        if inlier_count < 4:
            continue
        residuals = errors[mask]
        key = (
            inlier_count,
            -float(np.median(residuals)),
            -float(np.sum(np.minimum(residuals, consensus_threshold) ** 2)),
        )
        if best_key is None or key > best_key:
            best_key = key
            best_h = h
            best_mask = mask
            no_improvement = 0
        else:
            no_improvement += 1
        if inlier_count >= max(config.min_inliers, int(0.45 * len(src))) and (
            no_improvement > 90
        ):
            break

    if best_h is None or best_mask is None:
        raise WorkbenchError(
            ErrorCode.HOMO,
            "RANSAC did not find a finite homography",
            field="homography",
            details={"matches": len(src), "iterations": iterations_run},
        )

    inlier_mask = best_mask
    for _ in range(2):
        if int(np.count_nonzero(inlier_mask)) < 4:
            break
        refined = _dlt(src[inlier_mask], dst[inlier_mask])
        if refined is None:
            break
        errors = _forward_errors(refined, src, dst)
        new_mask = errors <= consensus_threshold
        if new_mask.sum() < 4:
            break
        best_h = refined
        inlier_mask = new_mask

    errors, _ = _symmetric_errors(best_h, src, dst)
    inlier_mask = errors <= config.reproj_threshold
    inlier_errors = errors[inlier_mask]
    inlier_count = int(np.count_nonzero(inlier_mask))
    if inlier_count < config.min_inliers:
        raise WorkbenchError(
            ErrorCode.HOMO,
            f"only {inlier_count} inliers, minimum is {config.min_inliers}",
            field="homography.min_inliers",
            details={"inliers": inlier_count, "minimum": config.min_inliers},
        )
    if _configuration_is_degenerate(src[inlier_mask]) or _configuration_is_degenerate(
        dst[inlier_mask]
    ):
        raise WorkbenchError(
            ErrorCode.HOMO,
            "RANSAC inliers are degenerate and cannot determine a homography",
            field="homography",
            details={"inliers": inlier_count},
        )
    condition = _covariance_condition(
        best_h,
        src[inlier_mask],
        dst[inlier_mask],
        inlier_errors,
        config.covariance_regularization,
    )
    truth_corner_error: float | None = None
    truth_relative_error: float | None = None
    truth_ok: bool | None = None
    if truth_homography is not None:
        truth = normalize_homography(truth_homography)
        estimated_for_truth = best_h if truth_uses_forward_direction else np.linalg.inv(best_h)
        truth_corner_error = corner_correspondence_error(
            estimated_for_truth, truth, image_size_a[0], image_size_a[1]
        )
        truth_relative_error = relative_homography_error(estimated_for_truth, truth)
        truth_ok = truth_corner_error <= config.truth_corner_tolerance

    ordered_inliers = np.flatnonzero(inlier_mask)
    return HomographyEstimate(
        homography_a_to_b=best_h,
        inlier_mask=inlier_mask,
        residuals=errors,
        inlier_points_a=src[ordered_inliers],
        inlier_points_b=dst[ordered_inliers],
        residual_median=float(np.median(inlier_errors)),
        residual_p95=float(np.percentile(inlier_errors, 95)),
        covariance_condition=condition,
        truth_corner_error=truth_corner_error,
        truth_relative_error=truth_relative_error,
        truth_ok=truth_ok,
        iterations=iterations_run,
    )

