from __future__ import annotations

from collections import deque
from typing import Iterable

import numpy as np

from .config import PipelineConfig
from .stages import Stage


_OPERATOR_COEFFS = {
    "sobel": (1, 2, 1, 4.0),
    "prewitt": (1, 1, 1, 3.0),
    "scharr": (3, 10, 3, 16.0),
}


def _with_channels(array: np.ndarray) -> tuple[np.ndarray, bool]:
    if array.ndim == 2:
        return array[:, :, None], True
    if array.ndim == 3:
        return array, False
    raise ValueError(f"expected 2D or 3D array, got {array.shape}")


def _restore_channels(array: np.ndarray, was_2d: bool) -> np.ndarray:
    return array[:, :, 0] if was_2d else array


def gradient_magnitude(
    image: np.ndarray,
    operator: str = "sobel",
) -> np.ndarray:
    work, was_2d = _with_channels(np.asarray(image))
    if operator not in _OPERATOR_COEFFS:
        raise ValueError(f"unknown gradient operator: {operator}")
    a, b, c, max_axis = _OPERATOR_COEFFS[operator]
    padded = np.pad(
        work.astype(np.float32, copy=False),
        ((1, 1), (1, 1), (0, 0)),
        mode="edge",
    )
    p00 = padded[:-2, :-2]
    p01 = padded[:-2, 1:-1]
    p02 = padded[:-2, 2:]
    p10 = padded[1:-1, :-2]
    p12 = padded[1:-1, 2:]
    p20 = padded[2:, :-2]
    p21 = padded[2:, 1:-1]
    p22 = padded[2:, 2:]
    gx = (
        -a * p00
        + a * p02
        - b * p10
        + b * p12
        - c * p20
        + c * p22
    )
    gy = (
        -a * p00
        - b * p01
        - c * p02
        + c * p20
        + b * p21
        + a * p22
    )
    magnitude = np.sqrt((gx * gx + gy * gy).astype(np.float32, copy=False))
    magnitude *= np.float32(1.0 / (max_axis * np.sqrt(2.0)))
    np.clip(magnitude, 0.0, 255.0, out=magnitude)
    return np.ascontiguousarray(_restore_channels(magnitude, was_2d))


def dual_threshold(
    magnitude: np.ndarray,
    low_threshold: float,
    high_threshold: float,
) -> np.ndarray:
    work, was_2d = _with_channels(np.asarray(magnitude))
    strong = work >= high_threshold
    weak = work >= low_threshold
    supported = dilate_bool(strong, 3, "square")
    edge = weak & supported
    return np.ascontiguousarray(_restore_channels(edge.astype(np.uint8), was_2d))


def _structure_offsets(size: int, structure: str) -> list[tuple[int, int]]:
    if size <= 1:
        return [(0, 0)]
    radius = size // 2
    offsets: list[tuple[int, int]] = []
    for dy in range(-radius, radius + 1):
        for dx in range(-radius, radius + 1):
            if structure == "cross" and abs(dy) + abs(dx) > radius:
                continue
            offsets.append((dy, dx))
    return offsets


def dilate_bool(mask: np.ndarray, size: int, structure: str = "cross") -> np.ndarray:
    work, was_2d = _with_channels(np.asarray(mask, dtype=bool))
    if size <= 1:
        return np.ascontiguousarray(_restore_channels(work.copy(), was_2d))
    radius = size // 2
    padded = np.pad(
        work,
        ((radius, radius), (radius, radius), (0, 0)),
        mode="constant",
        constant_values=False,
    )
    height, width = work.shape[:2]
    output = np.zeros_like(work, dtype=bool)
    for dy, dx in _structure_offsets(size, structure):
        oy = radius + dy
        ox = radius + dx
        output |= padded[oy : oy + height, ox : ox + width]
    return np.ascontiguousarray(_restore_channels(output, was_2d))


def erode_bool(mask: np.ndarray, size: int, structure: str = "cross") -> np.ndarray:
    work, was_2d = _with_channels(np.asarray(mask, dtype=bool))
    if size <= 1:
        return np.ascontiguousarray(_restore_channels(work.copy(), was_2d))
    radius = size // 2
    padded = np.pad(
        work,
        ((radius, radius), (radius, radius), (0, 0)),
        mode="constant",
        constant_values=True,
    )
    height, width = work.shape[:2]
    output = np.ones_like(work, dtype=bool)
    for dy, dx in _structure_offsets(size, structure):
        oy = radius + dy
        ox = radius + dx
        output &= padded[oy : oy + height, ox : ox + width]
    return np.ascontiguousarray(_restore_channels(output, was_2d))


def binary_open(mask: np.ndarray, size: int, structure: str = "cross") -> np.ndarray:
    return dilate_bool(erode_bool(mask, size, structure), size, structure)


def binary_close(mask: np.ndarray, size: int, structure: str = "cross") -> np.ndarray:
    return erode_bool(dilate_bool(mask, size, structure), size, structure)


def _neighbours(mask: np.ndarray) -> tuple[np.ndarray, ...]:
    height, width = mask.shape
    padded = np.pad(mask, 1, mode="constant", constant_values=False)
    p2 = padded[0:height, 1 : width + 1]
    p3 = padded[0:height, 2 : width + 2]
    p4 = padded[1 : height + 1, 2 : width + 2]
    p5 = padded[2 : height + 2, 2 : width + 2]
    p6 = padded[2 : height + 2, 1 : width + 1]
    p7 = padded[2 : height + 2, 0:width]
    p8 = padded[1 : height + 1, 0:width]
    p9 = padded[0:height, 0:width]
    return p2, p3, p4, p5, p6, p7, p8, p9


def _transitions(neighbours: Iterable[np.ndarray]) -> np.ndarray:
    sequence = list(neighbours)
    count = np.zeros_like(sequence[0], dtype=np.uint8)
    for previous, current in zip(sequence, sequence[1:] + sequence[:1]):
        count += (~previous) & current
    return count


def _zhang_suen_2d(mask: np.ndarray, iterations: int) -> np.ndarray:
    current = np.asarray(mask, dtype=bool).copy()
    for _ in range(iterations):
        p2, p3, p4, p5, p6, p7, p8, p9 = _neighbours(current)
        neighbour_count = sum(
            neighbour.astype(np.uint8)
            for neighbour in (p2, p3, p4, p5, p6, p7, p8, p9)
        )
        transitions = _transitions((p2, p3, p4, p5, p6, p7, p8, p9))
        step1 = (
            current
            & (neighbour_count >= 2)
            & (neighbour_count <= 6)
            & (transitions == 1)
            & (~(p2 & p4 & p6))
            & (~(p4 & p6 & p8))
        )
        after_step1 = current & ~step1
        p2, p3, p4, p5, p6, p7, p8, p9 = _neighbours(after_step1)
        neighbour_count = sum(
            neighbour.astype(np.uint8)
            for neighbour in (p2, p3, p4, p5, p6, p7, p8, p9)
        )
        transitions = _transitions((p2, p3, p4, p5, p6, p7, p8, p9))
        step2 = (
            after_step1
            & (neighbour_count >= 2)
            & (neighbour_count <= 6)
            & (transitions == 1)
            & (~(p2 & p4 & p8))
            & (~(p2 & p6 & p8))
        )
        updated = after_step1 & ~step2
        if np.array_equal(updated, current):
            break
        current = updated
    return current


def zhang_suen_thin(mask: np.ndarray, iterations: int) -> np.ndarray:
    work, was_2d = _with_channels(np.asarray(mask, dtype=bool))
    output = np.empty_like(work, dtype=bool)
    for channel in range(work.shape[2]):
        output[:, :, channel] = _zhang_suen_2d(work[:, :, channel], iterations)
    return np.ascontiguousarray(
        _restore_channels(output.astype(np.uint8), was_2d)
    )


def _protected_border_mask(
    shape: tuple[int, int],
    *,
    top: bool,
    bottom: bool,
    left: bool,
    right: bool,
) -> np.ndarray:
    height, width = shape
    protected = np.zeros((height, width), dtype=bool)
    if top:
        protected[0, :] = True
    if bottom:
        protected[-1, :] = True
    if left:
        protected[:, 0] = True
    if right:
        protected[:, -1] = True
    return protected


def _prune_components_2d(
    mask: np.ndarray,
    length_threshold: int,
    protected: np.ndarray,
) -> np.ndarray:
    foreground = np.asarray(mask, dtype=bool)
    height, width = foreground.shape
    visited = np.zeros_like(foreground, dtype=bool)
    output = foreground.copy()
    flat_foreground = foreground.ravel()
    flat_visited = visited.ravel()
    for start in np.flatnonzero(flat_foreground):
        if flat_visited[start]:
            continue
        queue: deque[int] = deque([int(start)])
        flat_visited[start] = True
        component: list[int] = []
        touches_protected = False
        while queue:
            flat_index = queue.popleft()
            component.append(flat_index)
            y, x = divmod(flat_index, width)
            if protected[y, x]:
                touches_protected = True
            y0 = max(0, y - 1)
            y1 = min(height, y + 2)
            x0 = max(0, x - 1)
            x1 = min(width, x + 2)
            for ny in range(y0, y1):
                base = ny * width
                for nx in range(x0, x1):
                    neighbour = base + nx
                    if flat_foreground[neighbour] and not flat_visited[neighbour]:
                        flat_visited[neighbour] = True
                        queue.append(neighbour)
        if len(component) <= length_threshold and not touches_protected:
            output.ravel()[component] = False
    return output


def prune_short_components(
    mask: np.ndarray,
    length_threshold: int,
    *,
    protected_borders: tuple[bool, bool, bool, bool] = (False, False, False, False),
) -> np.ndarray:
    work, was_2d = _with_channels(np.asarray(mask, dtype=bool))
    height, width = work.shape[:2]
    protected = _protected_border_mask(
        (height, width),
        top=protected_borders[0],
        bottom=protected_borders[1],
        left=protected_borders[2],
        right=protected_borders[3],
    )
    output = np.empty_like(work, dtype=bool)
    for channel in range(work.shape[2]):
        output[:, :, channel] = _prune_components_2d(
            work[:, :, channel],
            length_threshold,
            protected,
        )
    return np.ascontiguousarray(
        _restore_channels(output.astype(np.uint8), was_2d)
    )


def apply_stage(
    stage: Stage,
    source: np.ndarray,
    config: PipelineConfig,
    *,
    protected_borders: tuple[bool, bool, bool, bool] = (False, False, False, False),
) -> np.ndarray:
    if stage is Stage.GRADIENT:
        return gradient_magnitude(source, config.gradient_operator)
    if stage is Stage.THRESHOLD:
        return dual_threshold(source, config.low_threshold, config.high_threshold)
    if stage is Stage.OPEN:
        return binary_open(source, config.open_size, config.structure).astype(
            np.uint8, copy=False
        )
    if stage is Stage.CLOSE:
        return binary_close(source, config.close_size, config.structure).astype(
            np.uint8, copy=False
        )
    if stage is Stage.THIN:
        return zhang_suen_thin(source, config.thin_iterations)
    if stage is Stage.PRUNE:
        return prune_short_components(
            source,
            config.prune_length,
            protected_borders=protected_borders,
        )
    raise ValueError(f"unknown stage: {stage}")
