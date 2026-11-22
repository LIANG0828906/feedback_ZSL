"""Deterministic image operations and chunk overlap handling."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterator, List, Sequence, Tuple

import cv2
import numpy as np

from .config import STAGES, PipelineConfig
from .errors import EdgePipeError, ErrorCode


@dataclass(frozen=True)
class Block:
    index: int
    row: int
    col: int
    y0: int
    y1: int
    x0: int
    x1: int

    @property
    def block_id(self) -> str:
        return f"b{self.index:04d}"

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def width(self) -> int:
        return self.x1 - self.x0


@dataclass(frozen=True)
class BlockGrid:
    height: int
    width: int
    block_size: int
    rows: int
    cols: int
    blocks: Tuple[Block, ...]

    @classmethod
    def create(cls, height: int, width: int, block_size: int) -> "BlockGrid":
        rows = int(math.ceil(height / block_size))
        cols = int(math.ceil(width / block_size))
        blocks: List[Block] = []
        index = 0
        for row in range(rows):
            y0 = row * block_size
            y1 = min(height, y0 + block_size)
            for col in range(cols):
                x0 = col * block_size
                x1 = min(width, x0 + block_size)
                blocks.append(Block(index, row, col, y0, y1, x0, x1))
                index += 1
        return cls(height, width, block_size, rows, cols, tuple(blocks))

    def by_id(self, block_id: str) -> Block:
        for block in self.blocks:
            if block.block_id == block_id:
                return block
        raise EdgePipeError(
            ErrorCode.PARA,
            f"未知分块编号: {block_id}",
            field="block_id",
        )

    def block_at(self, y: int, x: int) -> Block:
        if not (0 <= y < self.height and 0 <= x < self.width):
            raise EdgePipeError(
                ErrorCode.PARA,
                "像素坐标越界",
                field="coordinate",
                details={"y": y, "x": x, "height": self.height, "width": self.width},
            )
        row = y // self.block_size
        col = x // self.block_size
        return self.blocks[row * self.cols + col]


def iter_stage_block_pairs(num_blocks: int) -> Iterator[Tuple[int, str, int]]:
    """Canonical commit order: stage-major, then ascending block id."""

    sequence = 0
    for stage_index, stage in enumerate(STAGES):
        for block_index in range(num_blocks):
            yield sequence, stage, block_index
            sequence += 1


def normalize_image(image: np.ndarray) -> np.ndarray:
    if image.ndim == 2:
        return image
    if image.ndim == 3 and image.shape[2] == 1:
        return image[:, :, 0]
    return image


def channel_views(image: np.ndarray) -> Tuple[List[np.ndarray], bool]:
    """Return a channel list plus whether the original had a singleton axis."""

    if image.ndim == 2:
        return [image], False
    if image.shape[2] == 1:
        return [image[:, :, 0]], True
    return [image[:, :, index] for index in range(image.shape[2])], False


def stack_channels(channels: Sequence[np.ndarray], singleton_axis: bool) -> np.ndarray:
    if len(channels) == 1 and not singleton_axis:
        return channels[0]
    return np.stack(channels, axis=2)


def gradient_magnitude(image: np.ndarray, operator: str) -> np.ndarray:
    src = image.astype(np.float32)
    if operator == "sobel":
        gx = cv2.Sobel(src, cv2.CV_32F, 1, 0, ksize=3)
        gy = cv2.Sobel(src, cv2.CV_32F, 0, 1, ksize=3)
    elif operator == "scharr":
        gx = cv2.Scharr(src, cv2.CV_32F, 1, 0)
        gy = cv2.Scharr(src, cv2.CV_32F, 0, 1)
    elif operator == "prewitt":
        kx = np.array([[-1, 0, 1], [-1, 0, 1], [-1, 0, 1]], dtype=np.float32)
        ky = kx.T
        gx = cv2.filter2D(src, cv2.CV_32F, kx)
        gy = cv2.filter2D(src, cv2.CV_32F, ky)
    else:
        raise EdgePipeError(
            ErrorCode.PARA,
            f"不支持的梯度算子: {operator}",
            field="gradient_operator",
        )
    return cv2.magnitude(gx, gy)


def gradient_threshold(
    image: np.ndarray,
    operator: str,
    low_threshold: float,
    high_threshold: float,
) -> np.ndarray:
    """Dual-threshold edges with a deterministic one-pixel hysteresis.

    A weak edge pixel is retained only when one of its 8 neighbours is strong.
    This deliberately avoids ``cv2.Canny``'s unbounded global hysteresis so a
    finite overlap band is sufficient at chunk boundaries.
    """

    if image is None or image.size == 0:
        raise EdgePipeError(ErrorCode.PARA, "图像不能为空", field="image")
    if low_threshold > high_threshold:
        raise EdgePipeError(
            ErrorCode.PARA,
            "low_threshold 不得大于 high_threshold",
            field="low_threshold",
            details={
                "low_threshold": float(low_threshold),
                "high_threshold": float(high_threshold),
            },
        )

    channels, singleton_axis = channel_views(image)
    outputs: List[np.ndarray] = []
    kernel = np.ones((3, 3), dtype=np.uint8)
    for channel in channels:
        magnitude = gradient_magnitude(channel, operator)
        strong = magnitude >= float(high_threshold)
        weak = magnitude >= float(low_threshold)
        strong_u8 = strong.astype(np.uint8)
        near_strong = cv2.dilate(
            strong_u8,
            kernel,
            borderType=cv2.BORDER_CONSTANT,
            borderValue=0,
        ).astype(bool)
        edge = strong | (weak & near_strong)
        outputs.append(edge.astype(np.uint8) * 255)
    return stack_channels(outputs, singleton_axis)


def morphology_stage(
    image: np.ndarray,
    operation: str,
    kernel_size: int,
    iterations: int,
) -> np.ndarray:
    channels, singleton_axis = channel_views(image)
    kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (kernel_size, kernel_size))
    morph = cv2.MORPH_OPEN if operation == "open" else cv2.MORPH_CLOSE
    outputs = [
        cv2.morphologyEx(channel, morph, kernel, iterations=iterations)
        for channel in channels
    ]
    return stack_channels(outputs, singleton_axis)


def zhang_suen_iteration(mask: np.ndarray) -> np.ndarray:
    """One Zhang-Suen thinning iteration (two sub-passes)."""

    image = (mask != 0).astype(np.uint8)
    for sub_pass in (0, 1):
        padded = np.pad(image, 1, mode="constant")
        p2 = padded[:-2, 1:-1]
        p3 = padded[:-2, 2:]
        p4 = padded[1:-1, 2:]
        p5 = padded[2:, 2:]
        p6 = padded[2:, 1:-1]
        p7 = padded[2:, :-2]
        p8 = padded[1:-1, :-2]
        p9 = padded[:-2, :-2]

        neighbours = p2 + p3 + p4 + p5 + p6 + p7 + p8 + p9
        transitions = (
            ((p2 == 0) & (p3 == 1)).astype(np.uint8)
            + ((p3 == 0) & (p4 == 1)).astype(np.uint8)
            + ((p4 == 0) & (p5 == 1)).astype(np.uint8)
            + ((p5 == 0) & (p6 == 1)).astype(np.uint8)
            + ((p6 == 0) & (p7 == 1)).astype(np.uint8)
            + ((p7 == 0) & (p8 == 1)).astype(np.uint8)
            + ((p8 == 0) & (p9 == 1)).astype(np.uint8)
            + ((p9 == 0) & (p2 == 1)).astype(np.uint8)
        )
        common = (
            (image == 1)
            & (neighbours >= 2)
            & (neighbours <= 6)
            & (transitions == 1)
        )
        if sub_pass == 0:
            removable = common & ((p2 * p4 * p6) == 0) & ((p4 * p6 * p8) == 0)
        else:
            removable = common & ((p2 * p4 * p8) == 0) & ((p2 * p6 * p8) == 0)
        image = image & (~removable).astype(np.uint8)
    return image.astype(np.uint8) * 255


def thin_stage(image: np.ndarray, iterations: int) -> np.ndarray:
    channels, singleton_axis = channel_views(image)
    outputs: List[np.ndarray] = []
    for channel in channels:
        current = (channel != 0).astype(np.uint8) * 255
        for _ in range(iterations):
            updated = zhang_suen_iteration(current)
            if np.array_equal(updated, current):
                break
            current = updated
        outputs.append(current)
    return stack_channels(outputs, singleton_axis)


def prune_stage(image: np.ndarray, max_length: int) -> np.ndarray:
    """Remove 8-connected components whose bounding diameter is too short."""

    channels, singleton_axis = channel_views(image)
    outputs: List[np.ndarray] = []
    for channel in channels:
        binary = (channel != 0).astype(np.uint8)
        count, labels, stats, _ = cv2.connectedComponentsWithStats(
            binary, connectivity=8
        )
        result = binary.copy()
        for label in range(1, count):
            width = int(stats[label, cv2.CC_STAT_WIDTH])
            height = int(stats[label, cv2.CC_STAT_HEIGHT])
            diameter = math.hypot(max(0, width - 1), max(0, height - 1))
            if diameter <= float(max_length):
                result[labels == label] = 0
        outputs.append(result.astype(np.uint8) * 255)
    return stack_channels(outputs, singleton_axis)


def apply_stage(image: np.ndarray, stage: str, config: PipelineConfig) -> np.ndarray:
    if stage not in STAGES:
        raise EdgePipeError(
            ErrorCode.PARA,
            f"未知阶段: {stage}",
            field="stage",
            details={"allowed": list(STAGES)},
        )
    config.validate_image(image)
    if stage == "gradient":
        return gradient_threshold(
            image,
            config.gradient_operator,
            config.low_threshold,
            config.high_threshold,
        )
    if stage == "open":
        if not config.open_enabled:
            return image.copy()
        return morphology_stage(
            image, "open", config.open_kernel_size, config.open_iterations
        )
    if stage == "close":
        if not config.close_enabled:
            return image.copy()
        return morphology_stage(
            image, "close", config.close_kernel_size, config.close_iterations
        )
    if stage == "thin":
        if not config.thin_enabled:
            return image.copy()
        return thin_stage(image, config.thinning_iterations)
    if not config.prune_enabled:
        return image.copy()
    return prune_stage(image, config.prune_length)


def process_block_stage(
    stage: str,
    source: np.ndarray,
    block: Block,
    config: PipelineConfig,
) -> np.ndarray:
    """Process one expanded block and return only its non-overlapping core."""

    band = config.effective_band_width
    y0 = max(0, block.y0 - band)
    y1 = min(source.shape[0], block.y1 + band)
    x0 = max(0, block.x0 - band)
    x1 = min(source.shape[1], block.x1 + band)
    crop = source[y0:y1, x0:x1]
    processed = apply_stage(crop, stage, config)
    local_y0 = block.y0 - y0
    local_y1 = local_y0 + block.height
    local_x0 = block.x0 - x0
    local_x1 = local_x0 + block.width
    return np.ascontiguousarray(processed[local_y0:local_y1, local_x0:local_x1])


def paste_core(
    destination: np.ndarray, core: np.ndarray, block: Block
) -> None:
    expected = (block.height, block.width) + destination.shape[2:]
    if core.shape != expected:
        raise EdgePipeError(
            ErrorCode.INTERNAL,
            "分块输出尺寸不一致",
            details={"expected": list(expected), "actual": list(core.shape)},
        )
    destination[block.y0 : block.y1, block.x0 : block.x1] = core


def assemble_stage_mask(
    grid: BlockGrid,
    channel_shape: Tuple[int, ...],
    cores: dict,
) -> np.ndarray:
    result = np.zeros((grid.height, grid.width) + channel_shape, dtype=np.uint8)
    for block in grid.blocks:
        if block.block_id not in cores:
            raise EdgePipeError(
                ErrorCode.INTERNAL,
                "阶段组装缺少分块输出",
                details={"block_id": block.block_id},
            )
        paste_core(result, cores[block.block_id], block)
    return result


def run_whole_pipeline(
    image: np.ndarray, config: PipelineConfig
) -> Tuple[np.ndarray, dict]:
    """Reference path: every stage runs once on the whole image."""

    current = image.copy()
    stage_masks = {}
    for stage in STAGES:
        current = apply_stage(current, stage, config)
        stage_masks[stage] = current.copy()
    return current, stage_masks
