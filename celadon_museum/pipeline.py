"""End-to-end tile render, resumable snapshots, verification, and reports."""

from __future__ import annotations

import io
import json
import signal
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np
from PIL import Image

from .errors import ResumeMismatchError
from .math3d import sha256_array, sha256_bytes
from .render import (
    RenderContext,
    build_render_context,
    compute_penumbra_trend,
    connected_spot_components,
    render_tile,
)
from .scene import Scene
from .snapshot import (
    SnapshotState,
    SnapshotStore,
    TileRecord,
    _atomic_write_bytes,
)


@dataclass
class RenderResult:
    status: str
    exit_code: int
    report_path: Path | None
    resume_report_path: Path | None
    report: dict[str, Any]


class _Signals:
    def __init__(self, enabled: bool):
        self.enabled = enabled
        self.requested = False
        self.previous: dict[int, Any] = {}

    def __enter__(self):
        if self.enabled and hasattr(signal, "SIGINT"):
            for signum in (signal.SIGINT, signal.SIGTERM):
                self.previous[signum] = signal.getsignal(signum)
                signal.signal(signum, self._handle)
        return self

    def _handle(self, signum, frame):  # noqa: ANN001
        self.requested = True

    def __exit__(self, exc_type, exc, tb):
        for signum, handler in self.previous.items():
            signal.signal(signum, handler)
        return False


def _clear_final_outputs(output_dir: Path) -> None:
    for name in (
        "rgba.png",
        "rgba.npy",
        "optical_depth.npy",
        "optical_depth.png",
        "projection_map.npy",
        "projection_map.png",
        "report.json",
        "resume_report.json",
        "resume_diff.json",
    ):
        path = output_dir / name
        if path.exists():
            path.unlink()


def _png_bytes(array: np.ndarray, mode: str | None = None) -> bytes:
    image = Image.fromarray(array, mode=mode) if mode else Image.fromarray(array)
    buffer = io.BytesIO()
    image.save(buffer, format="PNG", compress_level=9)
    return buffer.getvalue()


def _write_png(path: Path, array: np.ndarray, mode: str | None = None) -> None:
    _atomic_write_bytes(path, _png_bytes(array, mode=mode))


def _npy_bytes(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array), allow_pickle=False)
    return buffer.getvalue()


def _aggregate_layer_stats(
    scene: Scene, records: list[TileRecord], context: RenderContext
) -> list[dict[str, Any]]:
    keys = sorted(scene.layers)
    visible = {key: 0 for key in keys}
    transmittance_sum = {key: 0.0 for key in keys}
    for record in records:
        stats = record.stats
        for key in keys:
            label = f"{key[0]}:{key[1]}"
            visible[key] += int(stats["layer_visible"].get(label, 0))
            transmittance_sum[key] += float(
                stats["layer_transmittance_sum"].get(label, 0.0)
            )
    result: list[dict[str, Any]] = []
    for key in keys:
        layer = scene.layers[key]
        count = visible[key]
        average = transmittance_sum[key] / count if count else 0.0
        textures = context.layer_by_key[key].textures
        result.append(
            {
                "plate_id": key[0],
                "layer_id": key[1],
                "kind": layer.kind,
                "visible_fragments": count,
                "average_transmittance": round(float(average), 9),
                "crackle_junction_samples": textures.junction_samples,
                "crackle_digest": textures.crackle_digest,
                "carve_digest": textures.carve_digest,
            }
        )
    return result


def _tile_manifest(records: list[TileRecord]) -> list[dict[str, Any]]:
    return [
        {
            "tile_id": record.tile_id,
            "row": record.row,
            "col": record.col,
            "color_digest": record.color_digest,
            "depth_digest": record.depth_digest,
            "projection_digest": record.projection_digest,
        }
        for record in records
    ]


def _partial_projection_image(scene: Scene, records: list[TileRecord], store: SnapshotStore):
    projection = np.zeros((scene.height, scene.width), dtype=np.float64)
    for record in records:
        _, _, tile_projection = store.verify_recorded_tile(record)
        y0 = record.row * scene.tile_size
        x0 = record.col * scene.tile_size
        y1 = y0 + tile_projection.shape[0]
        x1 = x0 + tile_projection.shape[1]
        projection[y0:y1, x0:x1] = tile_projection
    return projection


def _report_payload(
    scene: Scene,
    context: RenderContext,
    records: list[TileRecord],
    store: SnapshotStore,
    *,
    status: str,
    resume_count: int,
    discarded_snapshots: int,
    verification: dict[str, Any] | None,
    image_arrays: tuple[np.ndarray, np.ndarray, np.ndarray] | None,
) -> dict[str, Any]:
    tiles_x, tiles_y = scene.tile_grid()
    total_tiles = tiles_x * tiles_y
    layer_stats = _aggregate_layer_stats(scene, records, context)
    projection = (
        image_arrays[2]
        if image_arrays is not None
        else _partial_projection_image(scene, records, store)
    )
    spot_count, spots = connected_spot_components(projection)
    soft_samples = sum(int(record.stats.get("soft_shadow_samples", 0)) for record in records)
    penumbra = compute_penumbra_trend(context)
    complete = status == "complete" and len(records) == total_tiles and image_arrays is not None
    if complete:
        rgba, depth, _ = image_arrays
        image_digest = sha256_array(rgba)
        depth_digest = sha256_array(depth)
        rgba_section = {
            "complete": True,
            "path": "rgba.png",
            "npy_path": "rgba.npy",
            "width": int(rgba.shape[1]),
            "height": int(rgba.shape[0]),
            "digest": image_digest,
        }
        depth_section = {
            "complete": True,
            "path_npy": "optical_depth.npy",
            "path_png": "optical_depth.png",
            "width": int(depth.shape[1]),
            "height": int(depth.shape[0]),
            "digest": depth_digest,
            "min": round(float(np.min(depth)), 9),
            "max": round(float(np.max(depth)), 9),
            "average": round(float(np.mean(depth)), 9),
        }
        projection_section = {
            "complete": True,
            "path_npy": "projection_map.npy",
            "path_png": "projection_map.png",
            "digest": sha256_array(projection),
        }
    else:
        rgba_section = {
            "complete": False,
            "path": None,
            "npy_path": None,
            "width": scene.width,
            "height": scene.height,
            "digest": None,
        }
        depth_section = {
            "complete": False,
            "path_npy": None,
            "path_png": None,
            "width": scene.width,
            "height": scene.height,
            "digest": None,
            "min": None,
            "max": None,
            "average": None,
        }
        projection_section = {
            "complete": False,
            "path_npy": None,
            "path_png": None,
            "digest": None,
        }
    report = {
        "schema": "celadon-report-v1",
        "status": status,
        "scene": {
            "source": scene.source_name,
            "digest": scene.digest,
            "width": scene.width,
            "height": scene.height,
        },
        "image": rgba_section,
        "optical_depth": depth_section,
        "projection_map": projection_section,
        "tiles": {
            "tile_size": scene.tile_size,
            "tiles_x": tiles_x,
            "tiles_y": tiles_y,
            "total": total_tiles,
            "completed_count": len(records),
            "complete": len(records) == total_tiles,
            "completed": _tile_manifest(records),
        },
        "layers": layer_stats,
        "projection": {
            "spot_count": spot_count,
            "spots": [{**spot} for spot in spots[:24]],
            "soft_shadow_samples": soft_samples,
            "sample_density": int(scene.render["soft_shadow_samples"]),
            "penumbra_trend": penumbra,
        },
        "recovery": {
            "resume_count": resume_count,
            "discarded_snapshots": discarded_snapshots,
            "snapshot_used": resume_count > 0,
        },
        "verification": verification
        if verification is not None
        else {"performed": False, "match": None},
    }
    return report


def _resume_report_payload(
    scene: Scene,
    state: SnapshotState,
    status: str,
    *,
    next_tile_id: int | None,
    verification: dict[str, Any] | None,
) -> dict[str, Any]:
    tiles_x, tiles_y = scene.tile_grid()
    total = tiles_x * tiles_y
    return {
        "schema": "celadon-resume-v1",
        "status": status,
        "scene_digest": scene.digest,
        "tile_size": scene.tile_size,
        "tiles_x": tiles_x,
        "tiles_y": tiles_y,
        "total_tiles": total,
        "completed_tile_ids": state.completed_ids,
        "next_tile_id": next_tile_id,
        "resume_count": state.resume_count,
        "tile_color_digests": [
            {"tile_id": record.tile_id, "color_digest": record.color_digest}
            for record in state.records
        ],
        "verification": verification
        if verification is not None
        else {"performed": False, "match": None},
    }


def _json_bytes(payload: dict[str, Any]) -> bytes:
    return (
        json.dumps(
            payload,
            ensure_ascii=False,
            indent=2,
            sort_keys=False,
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    _atomic_write_bytes(path, _json_bytes(payload))


def _assemble(
    scene: Scene, records: list[TileRecord], store: SnapshotStore
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    rgba = np.zeros((scene.height, scene.width, 4), dtype=np.uint8)
    depth = np.zeros((scene.height, scene.width), dtype=np.float32)
    projection = np.zeros((scene.height, scene.width), dtype=np.float32)
    for record in records:
        tile_rgba, tile_depth, tile_projection = store.verify_recorded_tile(record)
        y0 = record.row * scene.tile_size
        x0 = record.col * scene.tile_size
        y1 = y0 + tile_rgba.shape[0]
        x1 = x0 + tile_rgba.shape[1]
        expected_shape = (min(scene.tile_size, scene.height - y0), min(scene.tile_size, scene.width - x0))
        if tile_rgba.shape[:2] != expected_shape or tile_depth.shape != expected_shape:
            raise ValueError("tile dimensions do not match scene image geometry")
        rgba[y0:y1, x0:x1] = tile_rgba
        depth[y0:y1, x0:x1] = tile_depth
        projection[y0:y1, x0:x1] = tile_projection
    return rgba, depth, projection


def _diffs_between(
    tile_id: int,
    expected_color: np.ndarray,
    actual_color: np.ndarray,
    expected_depth: np.ndarray,
    actual_depth: np.ndarray,
    expected_projection: np.ndarray,
    actual_projection: np.ndarray,
    limit: int = 16,
) -> tuple[int, list[dict[str, Any]]]:
    differences: list[dict[str, Any]] = []
    total = 0
    channels = ("r", "g", "b", "a")
    if expected_color.shape != actual_color.shape:
        return 1, [
            {
                "tile_id": tile_id,
                "pixel": [0, 0],
                "channel": "shape",
                "expected": list(expected_color.shape),
                "actual": list(actual_color.shape),
            }
        ]
    for y in range(expected_color.shape[0]):
        for x in range(expected_color.shape[1]):
            for channel_index, channel in enumerate(channels):
                if int(actual_color[y, x, channel_index]) != int(
                    expected_color[y, x, channel_index]
                ):
                    total += 1
                    if len(differences) < limit:
                        differences.append(
                            {
                                "tile_id": tile_id,
                                "pixel": [x, y],
                                "channel": channel,
                                "expected": int(expected_color[y, x, channel_index]),
                                "actual": int(actual_color[y, x, channel_index]),
                            }
                        )
            if float(actual_depth[y, x]) != float(expected_depth[y, x]):
                total += 1
                if len(differences) < limit:
                    differences.append(
                        {
                            "tile_id": tile_id,
                            "pixel": [x, y],
                            "channel": "depth",
                            "expected": float(expected_depth[y, x]),
                            "actual": float(actual_depth[y, x]),
                        }
                    )
            if float(actual_projection[y, x]) != float(expected_projection[y, x]):
                total += 1
                if len(differences) < limit:
                    differences.append(
                        {
                            "tile_id": tile_id,
                            "pixel": [x, y],
                            "channel": "projection",
                            "expected": float(expected_projection[y, x]),
                            "actual": float(actual_projection[y, x]),
                        }
                    )
    return total, differences


def _verify_resumed(
    scene: Scene,
    context: RenderContext,
    state: SnapshotState,
) -> tuple[dict[str, Any], list[dict[str, Any]], int]:
    total_differences = 0
    first_differences: list[dict[str, Any]] = []
    match = True
    for record in state.records:
        expected_color, expected_depth, expected_projection = state.read_tile(record)
        actual_color, actual_depth, actual_projection, _ = render_tile(
            context, record.tile_id, record.row, record.col
        )
        count, differences = _diffs_between(
            record.tile_id,
            expected_color,
            actual_color,
            expected_depth,
            actual_depth,
            expected_projection,
            actual_projection,
            limit=16 - len(first_differences),
        )
        total_differences += count
        first_differences.extend(differences)
        if count:
            match = False
    return (
        {
            "performed": True,
            "match": match,
            "tile_count": len(state.records),
            "difference_count": total_differences,
        },
        first_differences,
        total_differences,
    )


def _interrupted_result(
    scene: Scene,
    context: RenderContext,
    state: SnapshotState,
    store: SnapshotStore,
    out: Path,
    discarded_snapshots: int,
) -> RenderResult:
    total = scene.tile_grid()[0] * scene.tile_grid()[1]
    next_tile_id = len(state.records) if len(state.records) < total else None
    report = _report_payload(
        scene,
        context,
        state.records,
        store,
        status="interrupted",
        resume_count=state.resume_count,
        discarded_snapshots=discarded_snapshots,
        verification={"performed": False, "match": None},
        image_arrays=None,
    )
    resume_report = _resume_report_payload(
        scene,
        state,
        "interrupted",
        next_tile_id=next_tile_id,
        verification={"performed": False, "match": None},
    )
    _write_json(out / "report.json", report)
    _write_json(out / "resume_report.json", resume_report)
    return RenderResult(
        status="interrupted",
        exit_code=0,
        report_path=out / "report.json",
        resume_report_path=out / "resume_report.json",
        report=report,
    )


def render_scene(
    scene: Scene,
    output_dir: str | Path,
    *,
    stop_after_tiles: int | None = None,
    reset: bool = False,
    verify_resume: bool = True,
    install_signal_handlers: bool = True,
    progress: Callable[[int, int], None] | None = None,
) -> RenderResult:
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    with _Signals(install_signal_handlers) as signals:
        context = build_render_context(scene)
        store = SnapshotStore(out)
        discarded_snapshots = 0
        if reset:
            _clear_final_outputs(out)
            store.reset(scene)
            state = SnapshotState(scene, [], 0, store)
        else:
            loaded = store.load(scene)
            state = loaded.state
            if loaded.discarded:
                discarded_snapshots = 1
                _clear_final_outputs(out)
        tiles_x, tiles_y = scene.tile_grid()
        total_tiles = tiles_x * tiles_y
        rendered_this_run = 0
        interrupted = bool(signals.requested)
        for tile_id in range(len(state.records), total_tiles):
            if signals.requested:
                interrupted = True
                break
            row = tile_id // tiles_x
            col = tile_id % tiles_x
            rgba, depth, projection, stats = render_tile(context, tile_id, row, col)
            store.commit_tile(
                scene,
                state,
                tile_id,
                row,
                col,
                rgba,
                depth,
                projection,
                stats,
            )
            rendered_this_run += 1
            if progress is not None:
                progress(len(state.records), total_tiles)
            if stop_after_tiles is not None and rendered_this_run >= stop_after_tiles:
                interrupted = True
                break
        if signals.requested:
            interrupted = True
        if interrupted:
            return _interrupted_result(
                scene,
                context,
                state,
                store,
                out,
                discarded_snapshots,
            )

        image_arrays = _assemble(scene, state.records, store)
        verification = {"performed": False, "match": None}
        if verify_resume and state.resume_count > 0:
            verification, differences, total_differences = _verify_resumed(
                scene, context, state
            )
            if total_differences:
                diff_payload = {
                    "schema": "celadon-resume-diff-v1",
                    "error": {
                        "code": "E_RESUME_MISMATCH",
                        "reason": "rescanned tiles differ from the committed snapshot",
                        "difference_count": total_differences,
                        "differences": differences,
                    },
                }
                _write_json(out / "resume_diff.json", diff_payload)
                raise ResumeMismatchError(
                    "rescanned tiles differ from the committed snapshot",
                    differences,
                    total_differences,
                )

        rgba, depth, projection = image_arrays
        _write_png(out / "rgba.png", rgba)
        _atomic_write_bytes(out / "rgba.npy", _npy_bytes(rgba))
        _atomic_write_bytes(out / "optical_depth.npy", _npy_bytes(depth))
        depth_scale = max(float(scene.render["depth_scale"]), 1.0e-9)
        depth_display = np.rint(
            np.clip(depth / depth_scale, 0.0, 1.0) * 65535.0
        ).astype(np.uint16)
        _write_png(out / "optical_depth.png", depth_display)
        maximum = float(np.max(projection)) if projection.size else 0.0
        if maximum > 1.0e-9:
            projection_display = np.rint(
                np.clip(projection / maximum, 0.0, 1.0) * 255.0
            ).astype(np.uint8)
        else:
            projection_display = np.zeros_like(projection, dtype=np.uint8)
        _write_png(out / "projection_map.png", projection_display)
        _atomic_write_bytes(out / "projection_map.npy", _npy_bytes(projection))
        report = _report_payload(
            scene,
            context,
            state.records,
            store,
            status="complete",
            resume_count=state.resume_count,
            discarded_snapshots=discarded_snapshots,
            verification=verification,
            image_arrays=image_arrays,
        )
        resume_report = _resume_report_payload(
            scene,
            state,
            "complete",
            next_tile_id=None,
            verification=verification,
        )
        _write_json(out / "report.json", report)
        _write_json(out / "resume_report.json", resume_report)
        return RenderResult(
            status="complete",
            exit_code=0,
            report_path=out / "report.json",
            resume_report_path=out / "resume_report.json",
            report=report,
        )
