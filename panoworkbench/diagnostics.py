"""Visual diagnostics for matches, registration, seams, and error heatmaps."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import cv2
import numpy as np

from .config import FeatureConfig
from .dataset import Dataset
from .features import detect_features, match_features
from .geometry import apply_homography, normalize_homography, warp_to_canvas
from .seam import SeamPath
from .state import PairRecord


def match_diagnostic(
    dataset: Dataset,
    image_a: int,
    image_b: int,
    config: FeatureConfig,
    *,
    max_lines: int = 180,
) -> np.ndarray:
    image_a_data = dataset.read_image(image_a, color=True)
    image_b_data = dataset.read_image(image_b, color=True)
    features_a = detect_features(image_a_data, image_a, config)
    features_b = detect_features(image_b_data, image_b, config)
    matches = match_features(features_a, features_b, config)
    count = min(matches.count, max_lines)
    source = matches.source_indices[:count]
    target = matches.target_indices[:count]
    distances = matches.distances[:count]
    draw_matches = [
        cv2.DMatch(int(index), int(target[index]), float(distances[index]))
        for index in range(count)
    ]
    keypoints_a = [
        cv2.KeyPoint(float(point[0]), float(point[1]), 5.0)
        for point in features_a.points
    ]
    keypoints_b = [
        cv2.KeyPoint(float(point[0]), float(point[1]), 5.0)
        for point in features_b.points
    ]
    visual = cv2.drawMatches(
        image_a_data,
        keypoints_a,
        image_b_data,
        keypoints_b,
        draw_matches,
        None,
        matchColor=(40, 230, 80),
        singlePointColor=(180, 180, 180),
        flags=cv2.DrawMatchesFlags_NOT_DRAW_SINGLE_POINTS,
    )
    cv2.putText(
        visual,
        f"{image_a}->{image_b} matches={matches.count}",
        (12, 28),
        cv2.FONT_HERSHEY_SIMPLEX,
        0.75,
        (30, 240, 255),
        2,
        cv2.LINE_AA,
    )
    return visual


def registration_overlay(
    dataset: Dataset,
    image_a: int,
    image_b: int,
    homography_b_to_a: np.ndarray,
) -> np.ndarray:
    base = dataset.read_image(image_a, color=True)
    moving = dataset.read_image(image_b, color=True)
    homography = normalize_homography(homography_b_to_a)
    warped, mask = warp_to_canvas(moving, homography, (base.shape[1], base.shape[0]))
    overlay = base.copy()
    alpha = 0.52
    blended = cv2.addWeighted(base, 1.0 - alpha, warped, alpha, 0)
    overlay[mask] = blended[mask]
    edges = cv2.warpPerspective(
        np.full((moving.shape[0], moving.shape[1]), 255, dtype=np.uint8),
        homography,
        (base.shape[1], base.shape[0]),
        flags=cv2.INTER_NEAREST,
    )
    contours, _ = cv2.findContours(edges, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    cv2.drawContours(overlay, contours, -1, (30, 240, 255), 2)
    cv2.putText(
        overlay,
        f"registration {image_b}->{image_a}",
        (12, 28),
        cv2.FONT_HERSHEY_SIMPLEX,
        0.75,
        (30, 240, 255),
        2,
        cv2.LINE_AA,
    )
    return overlay


def seam_overlay(
    panorama: np.ndarray,
    seams: tuple[tuple[str, SeamPath], ...] | list[tuple[str, SeamPath]],
    *,
    only_pair_id: str | None = None,
) -> np.ndarray:
    visual = panorama.copy()
    drawn = 0
    for pair_id, seam in seams:
        if only_pair_id is not None and pair_id != only_pair_id:
            continue
        points = np.asarray(seam.points_xy, dtype=np.int32)
        if len(points) >= 2:
            cv2.polylines(visual, [points.reshape(-1, 1, 2)], False, (0, 255, 255), 2)
        drawn += 1
    cv2.putText(
        visual,
        f"seams={drawn}",
        (12, 30),
        cv2.FONT_HERSHEY_SIMPLEX,
        0.8,
        (0, 255, 255),
        2,
        cv2.LINE_AA,
    )
    return visual


def registration_error_heatmap(
    dataset: Dataset,
    records: list[PairRecord],
    homographies_to_base: dict[int, np.ndarray],
    canvas_size: tuple[int, int],
    origin: tuple[int, int],
) -> np.ndarray:
    width, height = canvas_size
    error_map = np.zeros((height, width), dtype=np.float32)
    count_map = np.zeros((height, width), dtype=np.float32)
    for record in records:
        if record.result is None:
            continue
        result = record.result
        if not result.inlier_points_a:
            continue
        if record.image_a not in homographies_to_base:
            continue
        h_to_base = homographies_to_base[record.image_a]
        translation = np.array(
            [[1.0, 0.0, -float(origin[0])], [0.0, 1.0, -float(origin[1])], [0.0, 0.0, 1.0]]
        )
        h_to_canvas = translation @ h_to_base
        points = apply_homography(
            h_to_canvas, np.asarray(result.inlier_points_a, dtype=np.float64)
        )
        residuals = (
            np.asarray(result.inlier_residuals, dtype=np.float64)
            if result.inlier_residuals
            else np.full(len(points), result.residual_p95, dtype=np.float64)
        )
        for point, residual in zip(points, residuals, strict=True):
            x, y = int(round(point[0])), int(round(point[1]))
            if 0 <= x < width and 0 <= y < height:
                cv2.circle(
                    error_map,
                    (x, y),
                    4,
                    float(residual),
                    -1,
                    lineType=cv2.LINE_8,
                )
                cv2.circle(count_map, (x, y), 4, 1.0, -1, lineType=cv2.LINE_8)
    error_map = cv2.GaussianBlur(error_map, (0, 0), 3.5)
    count_map = cv2.GaussianBlur(count_map, (0, 0), 3.5)
    error_map = np.divide(
        error_map, np.maximum(count_map, 1e-6), out=np.zeros_like(error_map)
    )
    positive = error_map[error_map > 0]
    if len(positive):
        upper = float(np.percentile(positive, 99))
    else:
        upper = 1.0
    normalized = np.clip(error_map / max(upper, 1e-6), 0.0, 1.0)
    heat = cv2.applyColorMap(
        np.rint(normalized * 255.0).astype(np.uint8), cv2.COLORMAP_TURBO
    )
    heat[count_map <= 1e-5] = (15, 15, 15)
    return heat


def save_png(path: str | Path, image: np.ndarray) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    ok, data = cv2.imencode(".png", image, [cv2.IMWRITE_PNG_COMPRESSION, 3])
    if not ok:
        raise OSError(f"failed to encode {target}")
    temporary = target.with_suffix(target.suffix + ".tmp")
    temporary.write_bytes(data.tobytes())
    temporary.replace(target)
