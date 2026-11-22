"""Deterministic color/gradient region growing and mask post-processing."""

from __future__ import annotations

import time
from collections import deque
from dataclasses import dataclass
from typing import Any

import numpy as np

from .config import SegmentationParams
from .errors import BusinessError
from .imageops import (
    cleanup_label_mask,
    downsample_rgb_median,
    gradient_magnitude,
    upsample_labels_nearest,
)


@dataclass
class SegmentationResult:
    initial_labels: np.ndarray
    final_labels: np.ndarray
    seeds: list[tuple[int, int]]
    mode: str
    factor: int
    elapsed_seconds: float
    coarse_shape: list[int]
    seed_source: str


def _color_distance(image: np.ndarray, color: np.ndarray) -> np.ndarray:
    delta = image.astype(np.float32) - color.reshape(1, 1, 3)
    return np.sqrt(np.sum(delta * delta, axis=2))


def _background_mask(
    image: np.ndarray, gradient: np.ndarray, color_tolerance: float, gradient_threshold: float
) -> np.ndarray:
    """Select the smooth border-connected background with an edge-aware flood fill."""

    height, width = image.shape[:2]
    rgb = image.astype(np.float32, copy=False)
    bg_step = max(5.0, min(21.0, color_tolerance * 0.60))
    visited = np.zeros((height, width), dtype=bool)
    queue: deque[int] = deque()
    flat_visited = visited.reshape(-1)
    flat_rgb = rgb.reshape(-1, 3)
    for x in range(width):
        for y in (0, height - 1):
            index = y * width + x
            if not flat_visited[index]:
                flat_visited[index] = True
                queue.append(index)
    for y in range(height):
        for x in (0, width - 1):
            index = y * width + x
            if not flat_visited[index]:
                flat_visited[index] = True
                queue.append(index)

    while queue:
        index = queue.popleft()
        y, x = divmod(index, width)
        current = flat_rgb[index]
        if y > 0:
            nxt = index - width
            if not flat_visited[nxt]:
                delta = flat_rgb[nxt] - current
                if float(delta @ delta) <= bg_step * bg_step:
                    flat_visited[nxt] = True
                    queue.append(nxt)
        if y + 1 < height:
            nxt = index + width
            if not flat_visited[nxt]:
                delta = flat_rgb[nxt] - current
                if float(delta @ delta) <= bg_step * bg_step:
                    flat_visited[nxt] = True
                    queue.append(nxt)
        if x > 0:
            nxt = index - 1
            if not flat_visited[nxt]:
                delta = flat_rgb[nxt] - current
                if float(delta @ delta) <= bg_step * bg_step:
                    flat_visited[nxt] = True
                    queue.append(nxt)
        if x + 1 < width:
            nxt = index + 1
            if not flat_visited[nxt]:
                delta = flat_rgb[nxt] - current
                if float(delta @ delta) <= bg_step * bg_step:
                    flat_visited[nxt] = True
                    queue.append(nxt)
        # Gradient is consulted as a soft guard at strongly textured boundaries.
        if gradient[y, x] > max(gradient_threshold * 2.5, 350.0):
            pass
    return visited


def _auto_seeds(
    image: np.ndarray,
    gradient: np.ndarray,
    params: SegmentationParams,
) -> list[tuple[int, int]]:
    height, width = image.shape[:2]
    background = _background_mask(
        image,
        gradient,
        params.color_tolerance,
        params.gradient_threshold,
    )
    border_color = np.median(
        np.concatenate(
            [
                image[:2].reshape(-1, 3),
                image[-2:].reshape(-1, 3),
                image[:, :2].reshape(-1, 3),
                image[:, -2:].reshape(-1, 3),
            ]
        ),
        axis=0,
    ).astype(np.float32)
    distance_from_bg = _color_distance(image, border_color)
    stride = max(3, int(round(np.sqrt(height * width) / 220.0)))
    ys = np.arange(stride // 2, height, stride, dtype=np.int64)
    xs = np.arange(stride // 2, width, stride, dtype=np.int64)
    yy, xx = np.meshgrid(ys, xs, indexing="ij")
    flat_y = yy.reshape(-1)
    flat_x = xx.reshape(-1)
    candidate = (
        ~background[flat_y, flat_x]
        & (distance_from_bg[flat_y, flat_x] > max(6.0, params.color_tolerance * 0.48))
        & (gradient[flat_y, flat_x] <= max(params.gradient_threshold, 35.0))
    )
    flat_y = flat_y[candidate]
    flat_x = flat_x[candidate]
    if len(flat_y) == 0:
        return []

    colors = image[flat_y, flat_x].astype(np.int32)
    grads = gradient[flat_y, flat_x]
    dists = distance_from_bg[flat_y, flat_x]
    quant_step = max(6, int(round(params.color_tolerance * 0.42)))
    keys = colors // quant_step
    order = np.lexsort((flat_x, flat_y, grads, -dists, keys[:, 2], keys[:, 1], keys[:, 0]))
    bins: dict[tuple[int, int, int], dict[str, Any]] = {}
    for idx in order:
        key = (int(keys[idx, 0]), int(keys[idx, 1]), int(keys[idx, 2]))
        entry = bins.get(key)
        if entry is None:
            bins[key] = {
                "count": 1,
                "sum_rgb": colors[idx].astype(np.float64),
                "best_index": int(idx),
            }
        else:
            entry["count"] += 1
            entry["sum_rgb"] += colors[idx]
            best = entry["best_index"]
            if (grads[idx], -dists[idx], flat_y[idx], flat_x[idx]) < (
                grads[best],
                -dists[best],
                flat_y[best],
                flat_x[best],
            ):
                entry["best_index"] = int(idx)

    min_cluster = max(2, int(round(len(order) * 0.012)))
    cluster_sep = max(9.0, params.color_tolerance * 0.72)
    clusters: list[dict[str, Any]] = []
    for key in sorted(bins):
        entry = bins[key]
        mean = entry["sum_rgb"] / entry["count"]
        best_cluster = None
        best_distance = float("inf")
        for cluster in clusters:
            distance = float(np.linalg.norm(mean - cluster["mean"]))
            if distance <= cluster_sep and distance < best_distance:
                best_cluster = cluster
                best_distance = distance
        if best_cluster is None:
            clusters.append(
                {
                    "count": int(entry["count"]),
                    "sum_rgb": entry["sum_rgb"].copy(),
                    "mean": mean,
                    "best_index": int(entry["best_index"]),
                }
            )
        else:
            best_cluster["count"] += int(entry["count"])
            best_cluster["sum_rgb"] += entry["sum_rgb"]
            best_cluster["mean"] = best_cluster["sum_rgb"] / best_cluster["count"]
            best = best_cluster["best_index"]
            candidate_index = int(entry["best_index"])
            if (grads[candidate_index], -dists[candidate_index]) < (
                grads[best],
                -dists[best],
            ):
                best_cluster["best_index"] = candidate_index

    viable = [cluster for cluster in clusters if cluster["count"] >= min_cluster]
    viable.sort(
        key=lambda cluster: (
            -cluster["count"],
            round(float(cluster["mean"][0]), 3),
            round(float(cluster["mean"][1]), 3),
            round(float(cluster["mean"][2]), 3),
        )
    )
    if params.expected_regions is not None:
        viable = viable[: params.expected_regions]
    seeds = []
    for cluster in viable:
        index = int(cluster["best_index"])
        seeds.append((int(flat_x[index]), int(flat_y[index])))
    return seeds


def _grow_one(
    labels: np.ndarray,
    accepted_flat: np.ndarray,
    seed_xy: tuple[int, int],
    label: int,
) -> int:
    height, width = labels.shape
    x, y = int(seed_xy[0]), int(seed_xy[1])
    seed_index = y * width + x
    flat_labels = labels.reshape(-1)
    if flat_labels[seed_index] != 0:
        return 0
    flat_labels[seed_index] = label
    queue: deque[int] = deque([seed_index])
    count = 1
    while queue:
        index = queue.popleft()
        row, col = divmod(index, width)
        if row > 0:
            nxt = index - width
            if accepted_flat[nxt] and flat_labels[nxt] == 0:
                flat_labels[nxt] = label
                queue.append(nxt)
                count += 1
        if col > 0:
            nxt = index - 1
            if accepted_flat[nxt] and flat_labels[nxt] == 0:
                flat_labels[nxt] = label
                queue.append(nxt)
                count += 1
        if col + 1 < width:
            nxt = index + 1
            if accepted_flat[nxt] and flat_labels[nxt] == 0:
                flat_labels[nxt] = label
                queue.append(nxt)
                count += 1
        if row + 1 < height:
            nxt = index + width
            if accepted_flat[nxt] and flat_labels[nxt] == 0:
                flat_labels[nxt] = label
                queue.append(nxt)
                count += 1
    return count


def _grow_labels(
    image: np.ndarray,
    gradient: np.ndarray,
    params: SegmentationParams,
    seeds: list[tuple[int, int]],
) -> np.ndarray:
    height, width = image.shape[:2]
    labels = np.zeros((height, width), dtype=np.uint16)
    rgb = image.astype(np.float32, copy=False)
    gradient_limit = max(params.gradient_threshold, 1e-6)
    for label, seed in enumerate(seeds, start=1):
        seed_color = rgb[int(seed[1]), int(seed[0])]
        distance = _color_distance(image, seed_color)
        color_ok = distance <= params.color_tolerance
        gradient_ok = gradient <= params.gradient_threshold
        color_score = np.clip(1.0 - distance / params.color_tolerance, 0.0, 1.0)
        gradient_score = np.clip(1.0 - gradient / gradient_limit, 0.0, 1.0)
        confidence = 0.72 * color_score + 0.28 * gradient_score
        accepted = (
            color_ok
            & gradient_ok
            & (confidence >= params.confidence_threshold)
            & (labels == 0)
        )
        _grow_one(labels, accepted.reshape(-1), seed, label)
    return labels


def _segment_core(
    image: np.ndarray, params: SegmentationParams
) -> tuple[np.ndarray, list[tuple[int, int]], str]:
    gradient = gradient_magnitude(image)
    if params.auto_seeds:
        seeds = _auto_seeds(image, gradient, params)
        source = "auto"
    else:
        seeds = [(int(x), int(y)) for x, y in params.seeds]
        source = "manual"
    if not seeds:
        raise BusinessError(
            "E_REGION",
            "未能选取任何种子区域",
            details={
                "missing_regions": [],
                "extra_regions": [],
                "hint": "降低颜色容差/梯度阈值，或提供手动种子点",
            },
        )
    return _grow_labels(image, gradient, params, seeds), seeds, source


def segment_image(
    image: np.ndarray,
    params: SegmentationParams,
    *,
    mode: str = "fine",
    factor: int = 1,
) -> SegmentationResult:
    params.validate(image_shape=image.shape[:2])
    if mode not in {"fine", "downsampled"}:
        raise BusinessError(
            "E_PARA",
            "模式必须为 fine 或 downsampled",
            field="mode",
            details={"actual": mode},
        )
    if factor not in {1, 2, 4, 8, 16}:
        raise BusinessError(
            "E_PARA",
            "降采样倍率必须为 1/2/4/8/16",
            field="factor",
            details={"actual": factor},
        )
    started = time.perf_counter()
    if mode == "fine":
        factor = 1
    if factor <= 1:
        raw, seeds, source = _segment_core(image, params)
        initial = raw
        final = cleanup_label_mask(raw, params.morph_size, params.min_area)
        coarse_shape = [int(image.shape[0]), int(image.shape[1])]
    else:
        coarse = downsample_rgb_median(image, factor)
        coarse_raw, coarse_seeds, source = _segment_core(coarse, params)
        initial = upsample_labels_nearest(
            coarse_raw, image.shape[:2], factor
        ).astype(np.uint16)
        final = cleanup_label_mask(initial, params.morph_size, params.min_area)
        seeds = [
            (
                min(image.shape[1] - 1, int(x) * factor + factor // 2),
                min(image.shape[0] - 1, int(y) * factor + factor // 2),
            )
            for x, y in coarse_seeds
        ]
        coarse_shape = [int(coarse.shape[0]), int(coarse.shape[1])]
    elapsed = time.perf_counter() - started
    return SegmentationResult(
        initial_labels=initial,
        final_labels=final,
        seeds=seeds,
        mode=mode,
        factor=factor,
        elapsed_seconds=elapsed,
        coarse_shape=coarse_shape,
        seed_source=source,
    )
