"""Three-path deterministic orchestration with resumable block commits."""

from __future__ import annotations

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Iterable

import numpy as np

from .checkpoint import CheckpointStore, SnapshotLoad, StoredBlock, snapshot_identity
from .compression import (
    BlockOutcome,
    BlockProcessor,
    CompressionConfig,
    ReconstructionCache,
    error_propagation_edges,
    partition_image,
)
from .errors import ALGORITHM_VERSION, WorkbenchError, parameter_error, require
from .metrics import compose_maps, psnr_from_mse
from .state import BlockStateMatrix, COMMITTED, FAILED, PENDING, PROCESSING
from .util import array_content_hash

VALID_MODES = ("realtime", "resume", "recompute")


@dataclass
class PipelineResult:
    image: np.ndarray
    reconstructed: np.ndarray
    heatmap: np.ndarray
    ssim_map: np.ndarray | None
    edge_map: np.ndarray | None
    truth_edge_mask: np.ndarray
    metrics: dict[str, Any] | None
    block_summaries: dict[int, dict[str, Any]]
    block_states: list[str]
    commit_order: list[int]
    complete: bool
    progress: dict[str, int]
    mode: str
    config: CompressionConfig
    grid: tuple[int, int]
    snapshot_id: str
    resumed_blocks: int
    processed_blocks: int
    failed_blocks: list[int]
    checkpoint_invalidated_reason: str | None
    error_propagation: list[dict[str, Any]]
    algorithm_version: str = ALGORITHM_VERSION
    elapsed_seconds: float = 0.0
    image_key: str = ""
    image_hash: str = ""

    def summary(self) -> dict[str, Any]:
        return {
            "status": "ok",
            "mode": self.mode,
            "complete": self.complete,
            "snapshot_id": self.snapshot_id,
            "algorithm_version": self.algorithm_version,
            "image_key": self.image_key,
            "image_hash": self.image_hash,
            "image_shape": list(self.image.shape),
            "grid": list(self.grid),
            "config": self.config.to_dict(),
            "progress": self.progress,
            "commit_order": self.commit_order,
            "resumed_blocks": self.resumed_blocks,
            "processed_blocks": self.processed_blocks,
            "failed_blocks": self.failed_blocks,
            "checkpoint_invalidated_reason": self.checkpoint_invalidated_reason,
            "metrics": self.metrics,
            "elapsed_seconds": self.elapsed_seconds,
        }


def _apply_outcome(
    entries: dict[int, dict[str, Any]],
    reconstructions: dict[int, np.ndarray],
    states: BlockStateMatrix,
    outcome: BlockOutcome,
) -> None:
    if outcome.state == COMMITTED:
        entries[outcome.index] = {
            **outcome.stats,
            "ssim_slice": outcome.ssim_slice,
            "error_slice": outcome.error_slice,
            "edge_slice": outcome.edge_slice,
            "quantized": outcome.quantized,
            "reconstruction": outcome.reconstruction,
            "params": outcome.param,
            "input_hash": outcome.input_hash,
        }
        reconstructions[outcome.index] = outcome.reconstruction
        states.transition(outcome.index, COMMITTED)
    elif outcome.state == FAILED:
        states.transition(outcome.index, FAILED)
    else:
        raise WorkbenchError("E_STATE", "worker returned an unexpected terminal state", {"block_index": outcome.index, "state": outcome.state})


def _stored_entries(records: dict[int, StoredBlock]) -> tuple[dict[int, dict[str, Any]], dict[int, np.ndarray]]:
    entries: dict[int, dict[str, Any]] = {}
    reconstructions: dict[int, np.ndarray] = {}
    for index in sorted(records):
        record = records[index]
        entries[index] = {
            **record.as_evaluation_entry(),
            "params": record.params,
            "quantized": record.quantized,
            "reconstruction": record.reconstruction,
            "input_hash": record.input_hash,
        }
        reconstructions[index] = record.reconstruction
    return entries, reconstructions


def _assemble_reconstruction(
    shape: tuple[int, ...],
    entries: dict[int, dict[str, Any]],
    grid: tuple[int, int],
    block_size: int,
) -> np.ndarray:
    height, width = shape[:2]
    if len(shape) == 2:
        result = np.zeros((height, width), dtype=np.uint8)
    else:
        result = np.zeros((height, width, shape[2]), dtype=np.uint8)
    rows, columns = grid
    for index in sorted(entries):
        row, column = divmod(index, columns)
        y0 = row * block_size
        x0 = column * block_size
        reconstruction = entries[index]["reconstruction"]
        y1 = min(height, y0 + reconstruction.shape[0])
        x1 = min(width, x0 + reconstruction.shape[1])
        result[y0:y1, x0:x1] = reconstruction
    return result


def _compute_metrics(entries: dict[int, dict[str, Any]], evaluation_window: int) -> dict[str, Any]:
    pixel_count = sum(int(entry["pixel_count"]) for entry in entries.values())
    pixel_sse = sum(float(entry["pixel_sse"]) for entry in entries.values())
    ssim_sum = sum(float(entry["ssim_sum"]) for entry in entries.values())
    edge_sum = sum(float(entry["edge_sum"]) for entry in entries.values())
    edge_count = sum(int(entry["edge_count"]) for entry in entries.values())
    if pixel_count <= 0:
        raise WorkbenchError("E_STATE", "cannot compute metrics without committed pixels")
    mse = pixel_sse / pixel_count
    quant_steps = sorted({float(entry.get("params", {}).get("quant_step", 0.0)) for entry in entries.values()})
    evaluation_windows = sorted(
        {int(entry.get("params", {}).get("evaluation_window", evaluation_window)) for entry in entries.values()}
    )
    return {
        "psnr_db": psnr_from_mse(mse),
        "mse": float(mse),
        "ssim": float(ssim_sum / pixel_count),
        "edge_fidelity": float(edge_sum / edge_count) if edge_count else 1.0,
        "truth_edge_pixels": int(edge_count),
        "evaluated_truth_edge_pixels": int(edge_count),
        "evaluation_window": evaluation_window,
        "evaluation_windows": evaluation_windows,
        "quant_steps": quant_steps,
        "frozen_parameters_mixed": len(quant_steps) > 1 or len(evaluation_windows) > 1,
        "merge_rule": "exclusive_block_crops_with_window_linked_halo",
    }


def _load_or_create_snapshot(
    store: CheckpointStore,
    *,
    mode: str,
    image: np.ndarray,
    image_key: str,
    image_hash: str,
    config: CompressionConfig,
    total_blocks: int,
    grid: tuple[int, int],
) -> tuple[str, SnapshotLoad]:
    snapshot_id = snapshot_identity(image_key, image_hash, config.block_size, image.shape)
    if mode in {"realtime", "recompute"}:
        store.delete_snapshot(snapshot_id)
        store.create_snapshot(
            snapshot_id=snapshot_id,
            image_key=image_key,
            image_hash=image_hash,
            block_size=config.block_size,
            shape=image.shape,
            grid=grid,
            total_blocks=total_blocks,
            config=config.to_dict(),
            replace=False,
        )
        return snapshot_id, SnapshotLoad(snapshot_id, True, None, {})

    if store.has_committed_snapshot_with_other_block_size(image_key, config.block_size):
        raise parameter_error(
            "block_size",
            "block_size is partition-defining and cannot change while committed checkpoints exist",
            requested=config.block_size,
            recovery="use --mode recompute to intentionally start a new partition",
        )
    loaded = store.load_compatible(
        image=image,
        image_key=image_key,
        image_hash=image_hash,
        block_size=config.block_size,
        shape=image.shape,
    )
    if loaded.valid:
        return snapshot_id, loaded

    store.delete_snapshot(snapshot_id)
    store.create_snapshot(
        snapshot_id=snapshot_id,
        image_key=image_key,
        image_hash=image_hash,
        block_size=config.block_size,
        shape=image.shape,
        grid=grid,
        total_blocks=total_blocks,
        config=config.to_dict(),
        replace=False,
    )
    return snapshot_id, loaded


def run_pipeline(
    image: np.ndarray,
    truth_edge_mask: np.ndarray,
    config: CompressionConfig,
    *,
    checkpoint_path: str | Path,
    image_key: str,
    mode: str = "realtime",
    cancel_event: threading.Event | None = None,
    progress_callback: Callable[[int, int, int, str], None] | None = None,
    only_indices: Iterable[int] | None = None,
    processor: BlockProcessor | None = None,
) -> PipelineResult:
    """Run the same deterministic block pipeline in one of three modes."""

    started = time.perf_counter()
    if mode not in VALID_MODES:
        raise parameter_error("mode", "mode must be realtime, resume, or recompute", value=mode)
    config.validate_for_image(image)
    if image.dtype != np.uint8:
        raise parameter_error("image", "image must use uint8 pixels", dtype=str(image.dtype))
    if image.ndim not in (2, 3) or (image.ndim == 3 and image.shape[2] not in (1, 3)):
        raise parameter_error("image", "image must be grayscale or RGB", shape=list(image.shape))
    require(truth_edge_mask.shape == image.shape[:2], "E_PARA", "truth edge mask shape must match image", field="truth_edge_mask")
    height, width = image.shape[:2]
    blocks, grid = partition_image(height, width, config.block_size)
    total_blocks = len(blocks)
    image_hash = array_content_hash(image)
    codec = processor or BlockProcessor()

    store = CheckpointStore(checkpoint_path)
    try:
        snapshot_id, loaded = _load_or_create_snapshot(
            store,
            mode=mode,
            image=image,
            image_key=image_key,
            image_hash=image_hash,
            config=config,
            total_blocks=total_blocks,
            grid=grid,
        )
        records = loaded.blocks
        entries, reconstructions = _stored_entries(records)
        states = BlockStateMatrix(total_blocks)
        states.set_committed_from_checkpoint(sorted(records))
        commit_order = sorted(records)
        resumed_blocks = len(records)
        processed_blocks = 0
        failed_blocks: list[int] = []

        requested = None if only_indices is None else {int(index) for index in only_indices}
        if requested is not None:
            for index in requested:
                if index < 0 or index >= total_blocks:
                    raise parameter_error("only_indices", "requested block index is outside the grid", block_index=index)
                if states.state(index) == COMMITTED:
                    raise WorkbenchError("E_STATE", "committed blocks are frozen and cannot be retried", {"block_index": index})

        pending = [
            block.index
            for block in blocks
            if states.state(block.index) == PENDING and (requested is None or block.index in requested)
        ]
        event = cancel_event or threading.Event()
        reconstruction_cache = ReconstructionCache()
        if progress_callback is not None:
            progress_callback(len(commit_order), total_blocks, -1, "loaded")

        with ThreadPoolExecutor(max_workers=config.max_workers, thread_name_prefix="imq-block") as executor:
            offset = 0
            while offset < len(pending):
                if event.is_set():
                    break
                chunk = pending[offset : offset + config.max_workers]
                offset += len(chunk)
                for index in chunk:
                    states.transition(index, PROCESSING)
                    if progress_callback is not None:
                        progress_callback(len(commit_order), total_blocks, index, PROCESSING)

                def task(index: int) -> BlockOutcome:
                    geometry = blocks[index]
                    return codec.process_block(
                        image,
                        truth_edge_mask,
                        geometry,
                        config,
                        quant_step=config.quant_step,
                        committed_reconstructions=reconstructions,
                        reconstruction_cache=reconstruction_cache,
                    )

                for outcome in executor.map(task, chunk):
                    if outcome.state == COMMITTED:
                        store.commit_block(snapshot_id, outcome)
                        _apply_outcome(entries, reconstructions, states, outcome)
                        commit_order.append(outcome.index)
                        processed_blocks += 1
                        if progress_callback is not None:
                            progress_callback(len(commit_order), total_blocks, outcome.index, COMMITTED)
                    else:
                        _apply_outcome(entries, reconstructions, states, outcome)
                        failed_blocks.append(outcome.index)
                        if progress_callback is not None:
                            progress_callback(len(commit_order), total_blocks, outcome.index, FAILED)

        complete = len(entries) == total_blocks and not failed_blocks
        store.mark_complete(snapshot_id, complete)
        reconstructed = _assemble_reconstruction(image.shape, entries, grid, config.block_size)
        if complete:
            ssim_map, heatmap, edge_map = compose_maps(image.shape, entries, grid, config.block_size)
            metrics = _compute_metrics(entries, config.evaluation_window)
        else:
            ssim_map = None
            edge_map = None
            heatmap = np.zeros((height, width), dtype=np.float32)
            for index, entry in entries.items():
                row, column = divmod(index, grid[1])
                y0 = row * config.block_size
                x0 = column * config.block_size
                error_slice = entry["error_slice"]
                heatmap[y0 : y0 + error_slice.shape[0], x0 : x0 + error_slice.shape[1]] = error_slice
            metrics = None

        block_summaries = {}
        for index, entry in entries.items():
            geometry = blocks[index]
            block_summaries[index] = {
                "state": states.state(index),
                "geometry": geometry.to_dict(),
                "params": entry.get("params", {}),
                "input_hash": entry.get("input_hash", ""),
                "evaluation": {
                    key: value
                    for key, value in entry.items()
                    if key
                    not in {
                        "ssim_slice",
                        "error_slice",
                        "edge_slice",
                        "quantized",
                        "reconstruction",
                        "params",
                        "input_hash",
                    }
                },
            }
        for index in failed_blocks:
            geometry = blocks[index]
            block_summaries[index] = {
                "state": FAILED,
                "geometry": geometry.to_dict(),
                "params": {"quant_step": config.quant_step, "block_size": config.block_size},
                "evaluation": None,
            }
        propagation = error_propagation_edges(entries, grid)
        return PipelineResult(
            image=image,
            reconstructed=reconstructed,
            heatmap=heatmap,
            ssim_map=ssim_map,
            edge_map=edge_map,
            truth_edge_mask=truth_edge_mask,
            metrics=metrics,
            block_summaries=block_summaries,
            block_states=states.states(),
            commit_order=commit_order,
            complete=complete,
            progress={"committed": len(entries), "total": total_blocks},
            mode=mode,
            config=config,
            grid=grid,
            snapshot_id=snapshot_id,
            resumed_blocks=resumed_blocks,
            processed_blocks=processed_blocks,
            failed_blocks=sorted(failed_blocks),
            checkpoint_invalidated_reason=loaded.invalidated_reason,
            error_propagation=propagation,
            elapsed_seconds=time.perf_counter() - started,
            image_key=image_key,
            image_hash=image_hash,
        )
    finally:
        store.close()


def _block_index_for_pixel(y: int, x: int, block_size: int, grid: tuple[int, int]) -> int:
    rows, columns = grid
    row = min(max(y // block_size, 0), rows - 1)
    column = min(max(x // block_size, 0), columns - 1)
    return row * columns + column


def _first_true(mask: np.ndarray) -> tuple[int, ...] | None:
    flat = np.flatnonzero(mask.reshape(-1))
    if flat.size == 0:
        return None
    return tuple(int(value) for value in np.unravel_index(int(flat[0]), mask.shape))


def compare_pipeline_results(
    first: PipelineResult,
    second: PipelineResult,
    *,
    metric_tolerance: float = 1e-6,
    map_tolerance: float = 1e-5,
) -> dict[str, Any]:
    """Compare all observable outputs and raise E_DIFF at the first divergence."""

    if first.image.shape != second.image.shape:
        raise WorkbenchError(
            "E_DIFF",
            "pipeline image shapes differ",
            {"stage": "shape", "first_shape": list(first.image.shape), "second_shape": list(second.image.shape)},
        )
    if first.reconstructed.shape != second.reconstructed.shape:
        raise WorkbenchError(
            "E_DIFF",
            "pipeline reconstruction shapes differ",
            {
                "stage": "reconstruction_shape",
                "first_shape": list(first.reconstructed.shape),
                "second_shape": list(second.reconstructed.shape),
                "block_index": None,
                "first_pixel": None,
            },
        )
    if first.reconstructed.dtype != second.reconstructed.dtype:
        raise WorkbenchError(
            "E_DIFF",
            "pipeline reconstruction dtypes differ",
            {"stage": "reconstruction_dtype", "block_index": None, "first_pixel": None},
        )

    pixel_mismatch = first.reconstructed != second.reconstructed
    if np.any(pixel_mismatch):
        coordinate = _first_true(pixel_mismatch)
        assert coordinate is not None
        y, x = coordinate[0], coordinate[1]
        raise WorkbenchError(
            "E_DIFF",
            "first reconstructed pixel differs",
            {
                "stage": "reconstruction",
                "first_pixel": {"y": y, "x": x},
                "block_index": _block_index_for_pixel(y, x, first.config.block_size, first.grid),
            },
        )

    heatmap_delta = np.abs(first.heatmap.astype(np.float64) - second.heatmap.astype(np.float64))
    if np.any(heatmap_delta > map_tolerance):
        coordinate = _first_true(heatmap_delta > map_tolerance)
        assert coordinate is not None
        y, x = coordinate[0], coordinate[1]
        raise WorkbenchError(
            "E_DIFF",
            "first heatmap value differs",
            {
                "stage": "heatmap",
                "first_pixel": {"y": y, "x": x},
                "block_index": _block_index_for_pixel(y, x, first.config.block_size, first.grid),
                "delta": float(heatmap_delta[y, x]),
            },
        )

    if first.complete != second.complete:
        raise WorkbenchError(
            "E_DIFF",
            "pipeline completion flags differ",
            {"stage": "completion", "first": first.complete, "second": second.complete, "block_index": None, "first_pixel": None},
        )
    if first.complete:
        if first.ssim_map is None or second.ssim_map is None:
            raise WorkbenchError("E_DIFF", "a complete pipeline has no SSIM map", {"stage": "ssim_map"})
        ssim_delta = np.abs(first.ssim_map.astype(np.float64) - second.ssim_map.astype(np.float64))
        if np.any(ssim_delta > map_tolerance):
            coordinate = _first_true(ssim_delta > map_tolerance)
            assert coordinate is not None
            y, x = coordinate[0], coordinate[1]
            raise WorkbenchError(
                "E_DIFF",
                "first SSIM map value differs",
                {
                    "stage": "ssim_map",
                    "first_pixel": {"y": y, "x": x},
                    "block_index": _block_index_for_pixel(y, x, first.config.block_size, first.grid),
                    "delta": float(ssim_delta[y, x]),
                },
            )
        assert first.metrics is not None and second.metrics is not None
        for key in ("mse", "psnr_db", "ssim", "edge_fidelity"):
            if abs(float(first.metrics[key]) - float(second.metrics[key])) > metric_tolerance:
                raise WorkbenchError(
                    "E_DIFF",
                    "quality metric differs",
                    {
                        "stage": "metrics",
                        "metric": key,
                        "first_value": float(first.metrics[key]),
                        "second_value": float(second.metrics[key]),
                        "block_index": None,
                        "first_pixel": None,
                    },
                )

    if first.commit_order != second.commit_order:
        limit = min(len(first.commit_order), len(second.commit_order))
        divergence = next(
            (index for index in range(limit) if first.commit_order[index] != second.commit_order[index]),
            limit,
        )
        raise WorkbenchError(
            "E_DIFF",
            "block commit sequences differ",
            {
                "stage": "commit_sequence",
                "sequence_offset": divergence,
                "block_index": first.commit_order[divergence] if divergence < len(first.commit_order) else None,
                "first_pixel": None,
            },
        )

    return {
        "status": "ok",
        "mode_a": first.mode,
        "mode_b": second.mode,
        "pixels_compared": int(first.reconstructed.size),
        "blocks_compared": len(first.commit_order),
        "metric_tolerance": metric_tolerance,
        "map_tolerance": map_tolerance,
        "metrics": first.metrics,
    }
