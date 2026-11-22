"""Block DCT codec with quantisation telemetry and halo-aware evaluation."""

from __future__ import annotations

import hashlib
import math
import threading
import time
from dataclasses import dataclass, field
from concurrent.futures import Future
from typing import Any, Callable, Mapping

import numpy as np

from .dct import dct2_channels, idct2_channels
from .errors import ALGORITHM_VERSION, WorkbenchError, parameter_error, require
from .metrics import evaluate_region
ALLOWED_BLOCK_SIZES = (4, 8, 16, 32, 64)
MIN_QUANT_STEP = 0.1
MAX_QUANT_STEP = 512.0
MAX_EVALUATION_WINDOW = 63
MAX_WORKERS = 8


@dataclass(frozen=True)
class CompressionConfig:
    block_size: int = 16
    quant_step: float = 8.0
    evaluation_window: int = 7
    max_workers: int = 2
    max_buffer_pixels: int = 8_000_000

    def validate(self) -> None:
        if isinstance(self.block_size, bool) or not isinstance(self.block_size, int):
            raise parameter_error("block_size", "block_size must be an integer", value=self.block_size)
        if self.block_size not in ALLOWED_BLOCK_SIZES:
            raise parameter_error(
                "block_size",
                "block_size is outside the supported range",
                value=self.block_size,
                allowed=list(ALLOWED_BLOCK_SIZES),
            )
        if isinstance(self.quant_step, bool) or not isinstance(self.quant_step, (int, float)):
            raise parameter_error("quant_step", "quant_step must be numeric", value=self.quant_step)
        if not math.isfinite(float(self.quant_step)) or not (MIN_QUANT_STEP <= float(self.quant_step) <= MAX_QUANT_STEP):
            raise parameter_error(
                "quant_step",
                "quant_step is outside the supported range",
                value=self.quant_step,
                minimum=MIN_QUANT_STEP,
                maximum=MAX_QUANT_STEP,
            )
        if isinstance(self.evaluation_window, bool) or not isinstance(self.evaluation_window, int):
            raise parameter_error("evaluation_window", "evaluation_window must be an integer", value=self.evaluation_window)
        if self.evaluation_window < 3 or self.evaluation_window > MAX_EVALUATION_WINDOW or self.evaluation_window % 2 == 0:
            raise parameter_error(
                "evaluation_window",
                "evaluation_window must be odd and within 3..63",
                value=self.evaluation_window,
            )
        if isinstance(self.max_workers, bool) or not isinstance(self.max_workers, int) or not (1 <= self.max_workers <= MAX_WORKERS):
            raise parameter_error(
                "max_workers",
                f"max_workers must be an integer within 1..{MAX_WORKERS}",
                value=self.max_workers,
            )
        if (
            isinstance(self.max_buffer_pixels, bool)
            or not isinstance(self.max_buffer_pixels, int)
            or self.max_buffer_pixels <= 0
        ):
            raise parameter_error(
                "max_buffer_pixels",
                "max_buffer_pixels must be a positive integer",
                value=self.max_buffer_pixels,
            )
        band = self.overlap_band
        if band > self.block_size:
            raise WorkbenchError(
                "E_BAND",
                "evaluation window requires more overlap than the block can supply",
                {
                    "eval_window": self.evaluation_window,
                    "required_band": band,
                    "max_band": self.block_size,
                },
            )

    @property
    def overlap_band(self) -> int:
        return (self.evaluation_window - 1) // 2

    def validate_for_image(self, image: np.ndarray) -> None:
        self.validate()
        channels = 1 if image.ndim == 2 else image.shape[2]
        diameter = self.block_size + 2 * self.overlap_band
        required_pixels = diameter * diameter * channels + self.block_size * self.block_size * channels
        if required_pixels > self.max_buffer_pixels:
            raise parameter_error(
                "max_buffer_pixels",
                "per-thread pixel buffer limit is smaller than one block working set",
                required=required_pixels,
                configured=self.max_buffer_pixels,
            )
        if self.evaluation_window > min(image.shape[:2]):
            raise WorkbenchError(
                "E_BAND",
                "evaluation window is larger than the image edge band",
                {
                    "eval_window": self.evaluation_window,
                    "image_height": int(image.shape[0]),
                    "image_width": int(image.shape[1]),
                },
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "block_size": self.block_size,
            "quant_step": float(self.quant_step),
            "evaluation_window": self.evaluation_window,
            "overlap_band": self.overlap_band,
            "max_workers": self.max_workers,
            "max_buffer_pixels": self.max_buffer_pixels,
        }


@dataclass(frozen=True)
class BlockGeometry:
    index: int
    row: int
    column: int
    y0: int
    x0: int
    y1: int
    x1: int

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    def to_dict(self) -> dict[str, int]:
        return {
            "index": self.index,
            "row": self.row,
            "column": self.column,
            "y0": self.y0,
            "x0": self.x0,
            "y1": self.y1,
            "x1": self.x1,
        }


def partition_image(height: int, width: int, block_size: int) -> tuple[list[BlockGeometry], tuple[int, int]]:
    rows = math.ceil(height / block_size)
    columns = math.ceil(width / block_size)
    blocks = []
    for row in range(rows):
        for column in range(columns):
            y0 = row * block_size
            x0 = column * block_size
            blocks.append(
                BlockGeometry(
                    index=row * columns + column,
                    row=row,
                    column=column,
                    y0=y0,
                    x0=x0,
                    y1=min(height, y0 + block_size),
                    x1=min(width, x0 + block_size),
                )
            )
    return blocks, (rows, columns)


def block_at(height: int, width: int, block_size: int, row: int, column: int) -> BlockGeometry:
    y0 = row * block_size
    x0 = column * block_size
    columns = math.ceil(width / block_size)
    return BlockGeometry(
        index=row * columns + column,
        row=row,
        column=column,
        y0=y0,
        x0=x0,
        y1=min(height, y0 + block_size),
        x1=min(width, x0 + block_size),
    )


def _symmetric_indices(indices: np.ndarray, length: int) -> np.ndarray:
    if length <= 1:
        return np.zeros_like(indices)
    period = 2 * length
    folded = np.mod(indices, period)
    return np.where(folded < length, folded, period - 1 - folded)


def block_input_hash(image: np.ndarray, geometry: BlockGeometry) -> str:
    digest = hashlib.sha256()
    digest.update(f"block={geometry.index};y={geometry.y0}:{geometry.y1};x={geometry.x0}:{geometry.x1};".encode("ascii"))
    digest.update(np.ascontiguousarray(image[geometry.y0 : geometry.y1, geometry.x0 : geometry.x1]).tobytes())
    return digest.hexdigest()


@dataclass
class BlockOutcome:
    index: int
    geometry: BlockGeometry
    state: str
    param: dict[str, Any]
    input_hash: str
    quantized: np.ndarray | None = None
    reconstruction: np.ndarray | None = None
    ssim_slice: np.ndarray | None = None
    error_slice: np.ndarray | None = None
    edge_slice: np.ndarray | None = None
    stats: dict[str, Any] = field(default_factory=dict)
    duration_ms: float = 0.0
    buffer_pixels: int = 0
    error: dict[str, Any] | None = None

    def evaluation_json(self) -> dict[str, Any]:
        payload = {
            key: value
            for key, value in self.stats.items()
            if key not in {"ssim_slice", "error_slice", "edge_slice"}
        }
        return payload


class ReconstructionCache:
    """Thread-safe cache: each uncommitted block is reconstructed at most once."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._values: dict[int, np.ndarray] = {}
        self._inflight: dict[int, Future[np.ndarray]] = {}

    def put(self, index: int, reconstruction: np.ndarray) -> None:
        with self._lock:
            self._values[index] = reconstruction

    def get_or_compute(self, index: int, factory: Callable[[], np.ndarray]) -> np.ndarray:
        with self._lock:
            cached = self._values.get(index)
            if cached is not None:
                return cached
            future = self._inflight.get(index)
            owner = future is None
            if future is None:
                future = Future()
                self._inflight[index] = future
        if owner:
            try:
                value = factory()
            except BaseException as exc:
                with self._lock:
                    self._inflight.pop(index, None)
                future.set_exception(exc)
                raise
            with self._lock:
                self._values[index] = value
                self._inflight.pop(index, None)
            future.set_result(value)
            return value
        return future.result()


class BlockProcessor:
    """Stateless codec core; safe for concurrent block calls."""

    def _compress(
        self,
        block: np.ndarray,
        block_size: int,
        quant_step: float,
    ) -> tuple[np.ndarray, np.ndarray, dict[str, Any]]:
        channels = 1 if block.ndim == 2 else block.shape[2]
        height, width = block.shape[:2]
        if block.ndim == 2:
            padded = np.pad(block.astype(np.float64), ((0, block_size - height), (0, block_size - width)), mode="symmetric")
        else:
            padded = np.pad(
                block.astype(np.float64),
                ((0, block_size - height), (0, block_size - width), (0, 0)),
                mode="symmetric",
            )
        coefficients = dct2_channels(padded)
        quantized = np.rint(coefficients / quant_step).astype(np.int32)
        dequantized = quantized.astype(np.float64) * quant_step
        coefficient_error = dequantized - coefficients
        reconstructed_padded = idct2_channels(dequantized)
        reconstructed = np.rint(np.clip(reconstructed_padded, 0.0, 255.0)).astype(np.uint8)
        reconstructed = reconstructed[:height, :width]
        stats = {
            "coefficient_count": int(quantized.size),
            "coefficient_mse": float(np.mean(coefficient_error * coefficient_error)),
            "coefficient_mae": float(np.mean(np.abs(coefficient_error))),
            "coefficient_max_abs": float(np.max(np.abs(coefficient_error), initial=0.0)),
            "quantized_nonzero_ratio": float(np.count_nonzero(quantized) / quantized.size),
            "transform_block_size": block_size,
            "quant_step": float(quant_step),
            "padded_pixels": int((block_size * block_size - height * width) * channels),
            "valid_pixels": int(height * width * channels),
        }
        return quantized, reconstructed, stats

    def _block_reconstruction(
        self,
        image: np.ndarray,
        geometry: BlockGeometry,
        quant_step: float,
        block_size: int,
    ) -> np.ndarray:
        block = image[geometry.y0 : geometry.y1, geometry.x0 : geometry.x1]
        _, reconstructed, _ = self._compress(block, block_size, quant_step)
        return reconstructed

    def _build_reconstruction_extension(
        self,
        image: np.ndarray,
        ys: np.ndarray,
        xs: np.ndarray,
        target: BlockOutcome,
        block_size: int,
        uncommitted_quant_step: float,
        committed_reconstructions: Mapping[int, np.ndarray],
        reconstruction_cache: ReconstructionCache | None,
    ) -> np.ndarray:
        height, width = image.shape[:2]
        columns = math.ceil(width / block_size)
        source_rows = range(int(ys.min()) // block_size, int(ys.max()) // block_size + 1)
        source_columns = range(int(xs.min()) // block_size, int(xs.max()) // block_size + 1)
        y_min = int(ys.min())
        x_min = int(xs.min())
        y_max = int(ys.max()) + 1
        x_max = int(xs.max()) + 1
        reconstructed_full = {}
        for row in source_rows:
            for column in source_columns:
                block = block_at(height, width, block_size, row, column)
                if block.index == target.index:
                    reconstructed_full[block.index] = target.reconstruction
                elif block.index in committed_reconstructions:
                    reconstructed_full[block.index] = committed_reconstructions[block.index]
                elif reconstruction_cache is not None:
                    reconstructed_full[block.index] = reconstruction_cache.get_or_compute(
                        block.index,
                        lambda selected=block: self._block_reconstruction(
                            image, selected, uncommitted_quant_step, block_size
                        ),
                    )
                else:
                    reconstructed_full[block.index] = self._block_reconstruction(
                        image, block, uncommitted_quant_step, block_size
                    )

        channels = 1 if image.ndim == 2 else image.shape[2]
        if channels > 1:
            mini = np.empty((y_max - y_min, x_max - x_min, channels), dtype=np.uint8)
        else:
            mini = np.empty((y_max - y_min, x_max - x_min), dtype=np.uint8)
        for index, reconstruction in reconstructed_full.items():
            block = block_at(
                height,
                width,
                block_size,
                index // columns,
                index % columns,
            )
            dst_y0 = max(block.y0, y_min)
            dst_y1 = min(block.y1, y_max)
            dst_x0 = max(block.x0, x_min)
            dst_x1 = min(block.x1, x_max)
            src_y0 = dst_y0 - block.y0
            src_y1 = dst_y1 - block.y0
            src_x0 = dst_x0 - block.x0
            src_x1 = dst_x1 - block.x0
            mini[
                dst_y0 - y_min : dst_y1 - y_min,
                dst_x0 - x_min : dst_x1 - x_min,
            ] = reconstruction[src_y0:src_y1, src_x0:src_x1]
        return np.ascontiguousarray(mini[ys - y_min][:, xs - x_min])

    def process_block(
        self,
        image: np.ndarray,
        truth_edge_mask: np.ndarray,
        geometry: BlockGeometry,
        config: CompressionConfig,
        *,
        quant_step: float | None = None,
        committed_reconstructions: Mapping[int, np.ndarray] | None = None,
        reconstruction_cache: ReconstructionCache | None = None,
    ) -> BlockOutcome:
        started = time.perf_counter()
        block_quant = float(config.quant_step if quant_step is None else quant_step)
        committed = committed_reconstructions or {}
        raw_block = image[geometry.y0 : geometry.y1, geometry.x0 : geometry.x1]
        input_hash = block_input_hash(image, geometry)
        band = config.overlap_band
        height, width = image.shape[:2]
        ys = _symmetric_indices(np.arange(geometry.y0 - band, geometry.y1 + band), height)
        xs = _symmetric_indices(np.arange(geometry.x0 - band, geometry.x1 + band), width)
        channels = 1 if image.ndim == 2 else image.shape[2]
        diameter = geometry.height + 2 * band
        other_width = geometry.width + 2 * band
        buffer_pixels = diameter * other_width * channels + config.block_size * config.block_size * channels

        try:
            if buffer_pixels > config.max_buffer_pixels:
                raise parameter_error(
                    "max_buffer_pixels",
                    "block working set exceeds the per-thread pixel buffer limit",
                    required=buffer_pixels,
                    configured=config.max_buffer_pixels,
                    block_index=geometry.index,
                )
            quantized, reconstructed, compression_stats = self._compress(raw_block, config.block_size, block_quant)
            target = BlockOutcome(
                index=geometry.index,
                geometry=geometry,
                state="processing",
                param={
                    "block_size": config.block_size,
                    "quant_step": block_quant,
                    "evaluation_window": config.evaluation_window,
                    "overlap_band": band,
                    "algorithm_version": ALGORITHM_VERSION,
                },
                input_hash=input_hash,
                quantized=quantized,
                reconstruction=reconstructed,
            )
            if reconstruction_cache is not None:
                reconstruction_cache.put(geometry.index, reconstructed)
            extended_original = np.ascontiguousarray(image[ys][:, xs])
            extended_reconstruction = self._build_reconstruction_extension(
                image,
                ys,
                xs,
                target,
                config.block_size,
                block_quant,
                committed,
                reconstruction_cache,
            )
            extended_truth = np.ascontiguousarray(truth_edge_mask[ys][:, xs])
            evaluation = evaluate_region(
                extended_original,
                extended_reconstruction,
                extended_truth,
                evaluation_window=config.evaluation_window,
                y0=band,
                y1=band + geometry.height,
                x0=band,
                x1=band + geometry.width,
            )
            target.stats = {**compression_stats, **{key: value for key, value in evaluation.items() if not key.endswith("_slice")}}
            target.stats["pixel_sse"] = evaluation["pixel_sse"]
            target.stats["pixel_mse"] = evaluation["pixel_mse"]
            target.stats["pixel_max_abs"] = evaluation["pixel_max_abs"]
            target.stats["pixel_psnr_db"] = (
                100.0
                if evaluation["pixel_mse"] <= 0.0
                else min(100.0, 10.0 * math.log10((255.0 * 255.0) / evaluation["pixel_mse"]))
            )
            target.stats["inverse_transform_error_ratio"] = (
                evaluation["pixel_mse"] / compression_stats["coefficient_mse"]
                if compression_stats["coefficient_mse"] > 0.0
                else 0.0
            )
            target.ssim_slice = evaluation["ssim_slice"]
            target.error_slice = evaluation["error_slice"]
            target.edge_slice = evaluation["edge_slice"]
            target.state = "committed"
            target.buffer_pixels = buffer_pixels
            target.duration_ms = (time.perf_counter() - started) * 1000.0
            return target
        except WorkbenchError as exc:
            return BlockOutcome(
                index=geometry.index,
                geometry=geometry,
                state="failed",
                param={
                    "block_size": config.block_size,
                    "quant_step": block_quant,
                    "evaluation_window": config.evaluation_window,
                    "algorithm_version": ALGORITHM_VERSION,
                },
                input_hash=input_hash,
                buffer_pixels=buffer_pixels,
                duration_ms=(time.perf_counter() - started) * 1000.0,
                error=exc.to_dict(),
            )
        except Exception as exc:  # isolate a single block from the rest of the pool
            return BlockOutcome(
                index=geometry.index,
                geometry=geometry,
                state="failed",
                param={
                    "block_size": config.block_size,
                    "quant_step": block_quant,
                    "evaluation_window": config.evaluation_window,
                    "algorithm_version": ALGORITHM_VERSION,
                },
                input_hash=input_hash,
                buffer_pixels=buffer_pixels,
                duration_ms=(time.perf_counter() - started) * 1000.0,
                error={"status": "error", "code": "E_BLOCK", "message": str(exc), "details": {"block_index": geometry.index}},
            )


def error_propagation_edges(
    block_entries: Mapping[int, Mapping[str, Any]],
    grid: tuple[int, int],
) -> list[dict[str, Any]]:
    rows, columns = grid
    edges = []
    for index in sorted(block_entries):
        row, column = divmod(index, columns)
        current = block_entries[index]
        for relation, neighbor in (("right", index + 1), ("bottom", index + columns)):
            if relation == "right" and column + 1 >= columns:
                continue
            if relation == "bottom" and row + 1 >= rows:
                continue
            if neighbor not in block_entries:
                continue
            other = block_entries[neighbor]
            current_sse = float(current.get("pixel_sse", 0.0))
            other_sse = float(other.get("pixel_sse", 0.0))
            current_count = max(int(current.get("pixel_count", 1)), 1)
            other_count = max(int(other.get("pixel_count", 1)), 1)
            current_mse = current_sse / current_count
            other_mse = other_sse / other_count
            edges.append(
                {
                    "from_block": index,
                    "to_block": neighbor,
                    "relation": relation,
                    "quant_step_from": float(current.get("quant_step", 0.0)),
                    "quant_step_to": float(other.get("quant_step", 0.0)),
                    "coefficient_mse_from": float(current.get("coefficient_mse", 0.0)),
                    "coefficient_mse_to": float(other.get("coefficient_mse", 0.0)),
                    "pixel_mse_from": current_mse,
                    "pixel_mse_to": other_mse,
                    "pixel_mse_delta": other_mse - current_mse,
                }
            )
    return edges
