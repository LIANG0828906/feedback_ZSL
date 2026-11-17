from __future__ import annotations

import json
import os
import shutil
import time
from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Callable

import numpy as np

from . import RENDERER_NAME, RENDERER_VERSION, __version__
from .artifacts import (
    encode_depth_f32,
    encode_rgba_png,
    image_metadata,
)
from .compare import compare_render_dirs, write_comparison
from .errors import FrameRejected, RenderInterrupted
from .io_utils import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_json_bytes,
    pretty_json_text,
    sha256_bytes,
)
from .renderer import (
    RAY_DIRECTION_RULE,
    RAY_START_RULE,
    chunk_layout_hash,
    make_chunk_layout,
    render_tile,
)
from .scene import Scene
from .snapshot import SnapshotManager
from .statistics import (
    contribution_statistics,
    convergence_report,
    suspicious_pixels,
)
from .validation import load_validated_scene

IMAGE_FILE = "frame.rgba.png"
DEPTH_FILE = "frame.depth.f32"
STATS_FILE = "frame.stats.json"
REPORT_FILE = "frame_report.json"
TRANSFER_SCAN_FILE = "transfer_scan.json"
SUSPICIOUS_FILE = "suspicious_pixels.json"
DIFF_FILE = "diff.json"


@dataclass
class RenderOutcome:
    status: str
    exit_code: int
    frame_id: str
    out_dir: Path
    report: dict[str, Any]


def _array_hash(array: np.ndarray) -> str:
    return sha256_bytes(np.ascontiguousarray(array).tobytes())


def _clear_known_artifacts(out_dir: Path) -> None:
    for name in (
        IMAGE_FILE,
        DEPTH_FILE,
        STATS_FILE,
        REPORT_FILE,
        TRANSFER_SCAN_FILE,
        SUSPICIOUS_FILE,
        DIFF_FILE,
        "resume_report.json",
    ):
        (out_dir / name).unlink(missing_ok=True)
    state_dir = out_dir / ".volverify-state"
    if state_dir.exists():
        shutil.rmtree(state_dir)


def _chunk_delay_seconds() -> float:
    value = os.environ.get("VOLVERIFY_CHUNK_DELAY_MS", "0")
    try:
        return max(0.0, float(value) / 1000.0)
    except ValueError:
        return 0.0


def _delay_between_chunks(
    seconds: float, should_stop: Callable[[], bool]
) -> None:
    if seconds <= 0.0:
        return
    deadline = time.monotonic() + seconds
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0.0:
            return
        if should_stop():
            raise RenderInterrupted(
                int(getattr(should_stop, "signal_number", 15))
            )
        time.sleep(min(0.01, remaining))


def _rejection_outcome(
    frame_id: str,
    out_dir: Path,
    issues: list[dict[str, Any]],
    convergence: dict[str, Any] | None = None,
) -> RenderOutcome:
    _clear_known_artifacts(out_dir)
    report = {
        "schema_version": 1,
        "frame_id": frame_id,
        "command": "render",
        "status": "rejected",
        "issues": issues,
        "convergence": convergence,
        "artifacts_written": [],
    }
    atomic_write_json(out_dir / REPORT_FILE, report)
    return RenderOutcome(
        status="rejected",
        exit_code=2,
        frame_id=frame_id,
        out_dir=out_dir,
        report=report,
    )


def _interrupted_without_snapshot(
    scene: Scene | None,
    frame_id: str,
    out_dir: Path,
    signal_number: int,
    phase: str,
    total_chunks: int,
) -> RenderOutcome:
    event = {
        "schema_version": 1,
        "frame_id": frame_id,
        "code": "INTERRUPTED",
        "status": "interrupted",
        "signal": signal_number,
        "phase": phase,
        "completed_chunks": 0,
        "total_chunks": total_chunks,
        "next_chunk_id": 0,
        "fingerprint": (
            scene.input_fingerprint(RENDERER_VERSION) if scene else None
        ),
    }
    atomic_write_json(out_dir / "recovery_report.json", event)
    return RenderOutcome(
        status="interrupted",
        exit_code=128 + signal_number,
        frame_id=frame_id,
        out_dir=out_dir,
        report=event,
    )


def _merge_persisted_chunks(
    manager: SnapshotManager,
    manifest: dict[str, Any],
    scene: Scene,
) -> tuple[SimpleNamespace, dict[str, Any], list[dict[str, Any]]]:
    width = scene.camera.width
    height = scene.camera.height
    rgba = np.zeros((height, width, 4), dtype=np.uint8)
    depth = np.zeros((height, width), dtype=np.float32)
    entry_t = np.full((height, width), -1.0, dtype=np.float64)
    exit_t = np.full((height, width), -1.0, dtype=np.float64)
    peak_density = np.full((height, width), -1.0, dtype=np.float64)
    peak_t = np.full((height, width), -1.0, dtype=np.float64)
    peak_index = np.full((height, width, 3), -1, dtype=np.int16)
    index_min = np.full((height, width, 3), -1, dtype=np.int16)
    index_max = np.full((height, width, 3), -1, dtype=np.int16)

    light_rgb = {
        light.id: np.zeros(3, dtype=np.float64) for light in scene.lights
    }
    self_rgb = np.zeros(3, dtype=np.float64)
    sample_count = 0
    actual_step_min: float | None = None
    actual_step_max: float | None = None
    chunk_stats: list[dict[str, Any]] = []

    summaries = sorted(
        manifest.get("completed", []), key=lambda item: item["chunk_id"]
    )
    layout_by_id = {
        chunk.chunk_id: chunk for chunk in make_chunk_layout(
            width,
            height,
            scene.render.chunk_width,
            scene.render.chunk_height,
        )
    }
    for summary in summaries:
        chunk_id = int(summary["chunk_id"])
        chunk = layout_by_id[chunk_id]
        arrays = manager.load_chunk(summary)
        if _array_hash(arrays["rgba"]) != summary["rgba_sha256"]:
            raise ValueError(f"chunk {chunk_id} rgba array hash mismatch")
        if _array_hash(arrays["depth"]) != summary["depth_sha256"]:
            raise ValueError(f"chunk {chunk_id} depth array hash mismatch")
        y1 = chunk.y0 + chunk.height
        x1 = chunk.x0 + chunk.width
        rgba[chunk.y0 : y1, chunk.x0 : x1] = arrays["rgba"]
        depth[chunk.y0 : y1, chunk.x0 : x1] = arrays["depth"]
        entry_t[chunk.y0 : y1, chunk.x0 : x1] = arrays["entry_t"]
        exit_t[chunk.y0 : y1, chunk.x0 : x1] = arrays["exit_t"]
        peak_density[chunk.y0 : y1, chunk.x0 : x1] = arrays[
            "peak_density"
        ]
        peak_t[chunk.y0 : y1, chunk.x0 : x1] = arrays["peak_t"]
        peak_index[chunk.y0 : y1, chunk.x0 : x1] = arrays["peak_index"]
        index_min[chunk.y0 : y1, chunk.x0 : x1] = arrays["index_min"]
        index_max[chunk.y0 : y1, chunk.x0 : x1] = arrays["index_max"]
        sample_count += int(summary["sample_count"])
        if summary["sample_count"]:
            chunk_min = float(summary["actual_step_min"])
            chunk_max = float(summary["actual_step_max"])
            actual_step_min = (
                chunk_min
                if actual_step_min is None
                else min(actual_step_min, chunk_min)
            )
            actual_step_max = (
                chunk_max
                if actual_step_max is None
                else max(actual_step_max, chunk_max)
            )
        self_rgb += np.asarray(summary["self_rgb"], dtype=np.float64)
        for light_id, values in summary["light_rgb"].items():
            light_rgb[light_id] += np.asarray(values, dtype=np.float64)
        chunk_stats.append(
            {
                "chunk_id": chunk_id,
                "x0": chunk.x0,
                "y0": chunk.y0,
                "width": chunk.width,
                "height": chunk.height,
                "rgba_sha256": summary["rgba_sha256"],
                "depth_sha256": summary["depth_sha256"],
                "sample_count": int(summary["sample_count"]),
                "actual_step_min": float(summary["actual_step_min"]),
                "actual_step_max": float(summary["actual_step_max"]),
            }
        )

    if len(summaries) != len(layout_by_id):
        raise ValueError(
            "not all persisted chunks are available for final assembly"
        )

    if actual_step_min is None:
        actual_step_min = 0.0
    if actual_step_max is None:
        actual_step_max = 0.0
    merged = SimpleNamespace(
        rgba=rgba,
        depth=depth,
        entry_t=entry_t,
        exit_t=exit_t,
        peak_density=peak_density,
        peak_t=peak_t,
        peak_index=peak_index,
        index_min=index_min,
        index_max=index_max,
        sample_count=sample_count,
        actual_step_min=actual_step_min,
        actual_step_max=actual_step_max,
        self_rgb=self_rgb,
        light_rgb=light_rgb,
    )
    totals = {
        "sample_count": sample_count,
        "actual_step_min": actual_step_min,
        "actual_step_max": actual_step_max,
        "self_rgb": self_rgb,
        "light_rgb": light_rgb,
        "chunks": chunk_stats,
    }
    return merged, totals, chunk_stats


def _write_completed_frame(
    scene: Scene,
    manager: SnapshotManager,
    manifest: dict[str, Any],
    fingerprint: str,
    convergence: dict[str, Any],
    should_stop: Callable[[], bool],
) -> tuple[str, dict[str, Any], dict[str, Any]]:
    def check_stop() -> None:
        if should_stop():
            raise RenderInterrupted(
                int(getattr(should_stop, "signal_number", 15))
            )

    merged, totals, chunk_stats = _merge_persisted_chunks(
        manager, manifest, scene
    )
    check_stop()

    transfer_scan = {
        "schema_version": 1,
        "frame_id": scene.frame_id,
        "transfer": scene.transfer.scan(
            scene.transfer.default_scan_densities()
        ),
    }
    transfer_scan_text = pretty_json_text(transfer_scan)
    transfer_scan_bytes = transfer_scan_text.encode("utf-8")
    transfer_scan_sha = sha256_bytes(transfer_scan_bytes)

    suspicious = suspicious_pixels(scene, merged)
    suspicious["frame_id"] = scene.frame_id
    suspicious_text = pretty_json_text(suspicious)
    suspicious_bytes = suspicious_text.encode("utf-8")
    suspicious_sha = sha256_bytes(suspicious_bytes)
    check_stop()

    metadata = image_metadata(
        frame_id=scene.frame_id,
        scene_fingerprint=fingerprint,
        requested_step=scene.sampling.reference_step,
        actual_step_min=totals["actual_step_min"],
        actual_step_max=totals["actual_step_max"],
        convergence=convergence,
        ray_start_rule=RAY_START_RULE,
        ray_direction_rule=RAY_DIRECTION_RULE,
        width=scene.camera.width,
        height=scene.camera.height,
    )
    image_bytes, image_sha = encode_rgba_png(merged.rgba, metadata)
    depth_bytes, depth_sha = encode_depth_f32(merged.depth)
    check_stop()

    atomic_write_bytes(manager.out_dir / IMAGE_FILE, image_bytes)
    atomic_write_bytes(manager.out_dir / DEPTH_FILE, depth_bytes)
    atomic_write_bytes(
        manager.out_dir / TRANSFER_SCAN_FILE, transfer_scan_bytes
    )
    atomic_write_bytes(
        manager.out_dir / SUSPICIOUS_FILE, suspicious_bytes
    )

    lights_payload, self_payload = contribution_statistics(
        scene, totals["light_rgb"], totals["self_rgb"]
    )
    sampling_payload = {
        "requested_step": scene.sampling.reference_step,
        "ray_start_rule": RAY_START_RULE,
        "ray_direction_rule": RAY_DIRECTION_RULE,
        "interval_rule": "fixed step from volume entry; final partial interval",
        "actual_step_min": totals["actual_step_min"],
        "actual_step_max": totals["actual_step_max"],
        "total_samples": totals["sample_count"],
        "width": scene.camera.width,
        "height": scene.camera.height,
    }
    chunks_payload = {
        "chunk_size": [
            scene.render.chunk_width,
            scene.render.chunk_height,
        ],
        "layout_hash": chunk_layout_hash(
            make_chunk_layout(
                scene.camera.width,
                scene.camera.height,
                scene.render.chunk_width,
                scene.render.chunk_height,
            )
        ),
        "total": len(chunk_stats),
        "completed": len(chunk_stats),
        "completed_chunks": chunk_stats,
    }
    suspicious_stats = {
        "opacity_jump_threshold": scene.sampling.suspicious_opacity_jump_threshold,
        "pair_count": suspicious["pair_count"],
        "pixel_count": suspicious["pixel_count"],
        "list_path": SUSPICIOUS_FILE,
        "list_sha256": suspicious_sha,
    }
    core = {
        "scene_fingerprint": fingerprint,
        "inputs": {
            "scene_json_sha256": scene.scene_json_sha256,
            "volume_sha256": scene.volume.data_sha256,
        },
        "image": {
            "path": IMAGE_FILE,
            "sha256": image_sha,
            "width": scene.camera.width,
            "height": scene.camera.height,
        },
        "depth": {
            "path": DEPTH_FILE,
            "sha256": depth_sha,
            "dtype": "<f4",
            "shape": [scene.camera.height, scene.camera.width],
            "order": "C",
        },
        "sampling": sampling_payload,
        "convergence": convergence,
        "lights": lights_payload,
        "self_emission": self_payload,
        "suspicious": suspicious_stats,
        "transfer_scan": {
            "path": TRANSFER_SCAN_FILE,
            "sha256": transfer_scan_sha,
        },
        "chunks": chunks_payload,
    }
    frame_summary_sha = sha256_bytes(canonical_json_bytes(core))
    stats = {
        "schema_version": 1,
        "frame_id": scene.frame_id,
        "renderer": {
            "name": RENDERER_NAME,
            "version": __version__,
            "renderer_version": RENDERER_VERSION,
        },
        "status": "rendered",
        **core,
        "frame_summary_sha256": frame_summary_sha,
    }
    atomic_write_json(manager.out_dir / STATS_FILE, stats)
    check_stop()
    manager.mark_complete(
        manifest, frame_summary_sha, image_sha, depth_sha
    )
    report = {
        "schema_version": 1,
        "frame_id": scene.frame_id,
        "command": "render",
        "status": "rendered",
        "issues": [],
        "artifacts_written": [
            IMAGE_FILE,
            DEPTH_FILE,
            STATS_FILE,
            TRANSFER_SCAN_FILE,
            SUSPICIOUS_FILE,
        ],
        "frame_summary_sha256": frame_summary_sha,
    }
    atomic_write_json(manager.out_dir / REPORT_FILE, report)
    check_stop()
    return frame_summary_sha, stats, report


def render_scene(
    scene_path: Path | str,
    out_dir: Path | str,
    resume: bool = False,
    should_stop: Callable[[], bool] | None = None,
    compare_full: Path | str | None = None,
) -> RenderOutcome:
    scene_path = Path(scene_path)
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    stop = should_stop or (lambda: False)

    try:
        scene = load_validated_scene(scene_path)
    except FrameRejected as rejected:
        issues = [issue.to_dict() for issue in rejected.issues]
        return _rejection_outcome(rejected.frame_id, output, issues)

    if not scene.validate_aspect():
        return _rejection_outcome(
            scene.frame_id,
            output,
            [
                {
                    "code": "CAMERA_ASPECT_INVALID",
                    "scope": "camera.aspect_ratio",
                    "message": "aspect_ratio does not match image dimensions",
                }
            ],
        )

    try:
        convergence = convergence_report(scene, should_stop=stop)
    except RenderInterrupted as interrupted:
        layout = make_chunk_layout(
            scene.camera.width,
            scene.camera.height,
            scene.render.chunk_width,
            scene.render.chunk_height,
        )
        return _interrupted_without_snapshot(
            scene,
            scene.frame_id,
            output,
            interrupted.signal_number,
            "convergence",
            len(layout),
        )

    if not convergence["passed"]:
        issue = {
            "code": "CONVERGENCE_FAILED",
            "scope": "sampling",
            "message": "step refinement did not converge within the declared tolerance or was not monotonic",
            "details": convergence,
        }
        return _rejection_outcome(
            scene.frame_id, output, [issue], convergence=convergence
        )

    fingerprint = scene.input_fingerprint(RENDERER_VERSION)
    if not resume:
        (output / "resume_report.json").unlink(missing_ok=True)
        (output / DIFF_FILE).unlink(missing_ok=True)
    layout = make_chunk_layout(
        scene.camera.width,
        scene.camera.height,
        scene.render.chunk_width,
        scene.render.chunk_height,
    )
    manager = SnapshotManager(output, scene, fingerprint, layout)
    state = manager.initialize(resume)
    manifest = state.manifest

    try:
        completed_ids = {
            int(summary["chunk_id"]) for summary in manifest["completed"]
        }
        for chunk in layout:
            if stop():
                raise RenderInterrupted(
                    int(getattr(stop, "signal_number", 15))
                )
            if chunk.chunk_id in completed_ids:
                continue
            tile = render_tile(
                scene,
                scene.sampling.reference_step,
                chunk,
                should_stop=stop,
            )
            manager.save_chunk(tile, manifest)
            completed_ids.add(chunk.chunk_id)
            _delay_between_chunks(_chunk_delay_seconds(), stop)
        if stop():
            raise RenderInterrupted(
                int(getattr(stop, "signal_number", 15))
            )
        frame_hash, stats, report = _write_completed_frame(
            scene,
            manager,
            manifest,
            fingerprint,
            convergence,
            stop,
        )
    except RenderInterrupted as interrupted:
        event = manager.mark_interrupted(
            manifest,
            interrupted.signal_number,
            "render",
        )
        return RenderOutcome(
            status="interrupted",
            exit_code=128 + interrupted.signal_number,
            frame_id=scene.frame_id,
            out_dir=output,
            report=event,
        )

    if state.recovery_event is not None:
        report = dict(report)
        report["snapshot_event"] = state.recovery_event
    if compare_full is not None:
        baseline = Path(compare_full)
        try:
            comparison = compare_render_dirs(scene, output, baseline)
        except (OSError, ValueError) as exc:
            comparison = {
                "schema_version": 1,
                "status": "error",
                "code": "DIFF_BASELINE_UNREADABLE",
                "error": str(exc),
                "left": str(output),
                "right": str(baseline),
                "chunks": [],
            }
        write_comparison(comparison, output / DIFF_FILE)
        if comparison["status"] != "identical":
            report = dict(report)
            report["diff_report"] = DIFF_FILE
            report["diff_status"] = comparison["status"]
            report["diff_code"] = comparison.get("code")
            atomic_write_json(output / REPORT_FILE, report)
            return RenderOutcome(
                status="different",
                exit_code=4,
                frame_id=scene.frame_id,
                out_dir=output,
                report=report,
            )

    return RenderOutcome(
        status="rendered",
        exit_code=0,
        frame_id=scene.frame_id,
        out_dir=output,
        report=report,
    )


def validate_scene_only(
    scene_path: Path | str,
) -> dict[str, Any]:
    try:
        scene = load_validated_scene(scene_path)
    except FrameRejected as rejected:
        return {
            "schema_version": 1,
            "frame_id": rejected.frame_id,
            "status": "rejected",
            "issues": [issue.to_dict() for issue in rejected.issues],
            "transfer_scan": None,
        }
    return {
        "schema_version": 1,
        "frame_id": scene.frame_id,
        "status": "valid",
        "issues": [],
        "scene_fingerprint": scene.input_fingerprint(RENDERER_VERSION),
        "volume_sha256": scene.volume.data_sha256,
        "transfer_scan": scene.transfer.scan(
            scene.transfer.default_scan_densities()
        ),
    }


def _sanitize_frame_id(frame_id: str) -> str:
    allowed = [
        character
        if character.isalnum() or character in {"_", ".", "-"} else "_"
        for character in frame_id
    ]
    sanitized = "".join(allowed).strip("._-")
    return sanitized or "frame"


def render_batch(
    manifest_path: Path | str,
    out_dir: Path | str,
    resume: bool = False,
    should_stop: Callable[[], bool] | None = None,
) -> tuple[int, dict[str, Any]]:
    manifest_path = Path(manifest_path)
    root = Path(out_dir)
    root.mkdir(parents=True, exist_ok=True)
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        report = {
            "schema_version": 1,
            "command": "batch",
            "status": "rejected",
            "issues": [
                {
                    "code": "BATCH_MANIFEST_INVALID",
                    "scope": "manifest",
                    "message": "batch manifest could not be read",
                    "details": {"error": str(exc)},
                }
            ],
            "frames": [],
        }
        atomic_write_json(root / "batch_report.json", report)
        return 2, report

    frames = manifest.get("frames") if isinstance(manifest, dict) else None
    if not isinstance(frames, list):
        report = {
            "schema_version": 1,
            "command": "batch",
            "status": "rejected",
            "issues": [
                {
                    "code": "BATCH_MANIFEST_INVALID",
                    "scope": "manifest.frames",
                    "message": "manifest.frames must be an array",
                }
            ],
            "frames": [],
        }
        atomic_write_json(root / "batch_report.json", report)
        return 2, report

    outcomes: list[dict[str, Any]] = []
    any_rejected = False
    interrupted_outcome: RenderOutcome | None = None
    for index, frame_value in enumerate(frames):
        if should_stop is not None and should_stop():
            break
        if not isinstance(frame_value, dict) or not isinstance(
            frame_value.get("scene"), str
        ):
            any_rejected = True
            outcomes.append(
                {
                    "index": index,
                    "frame_id": f"frame_{index:04d}",
                    "status": "rejected",
                    "exit_code": 2,
                    "issues": [
                        {
                            "code": "BATCH_FRAME_INVALID",
                            "scope": f"frames[{index}]",
                            "message": "frame entry must contain a scene path",
                        }
                    ],
                }
            )
            continue
        scene_path = (manifest_path.parent / frame_value["scene"]).resolve()
        declared_id = frame_value.get("frame_id")
        fallback_id = (
            declared_id
            if isinstance(declared_id, str) and declared_id
            else scene_path.stem
        )
        frame_out = root / _sanitize_frame_id(fallback_id)
        outcome = render_scene(
            scene_path,
            frame_out,
            resume=resume,
            should_stop=should_stop,
        )
        outcomes.append(
            {
                "index": index,
                "frame_id": outcome.frame_id,
                "status": outcome.status,
                "exit_code": outcome.exit_code,
                "out_dir": str(frame_out),
                "report": outcome.report,
            }
        )
        if outcome.status == "interrupted":
            interrupted_outcome = outcome
            break
        if outcome.status in {"rejected", "different"}:
            any_rejected = True

    if interrupted_outcome is not None:
        status = "interrupted"
        exit_code = interrupted_outcome.exit_code
    elif should_stop is not None and should_stop():
        signal_number = int(getattr(should_stop, "signal_number", 15))
        event = {
            "schema_version": 1,
            "command": "batch",
            "code": "INTERRUPTED",
            "status": "interrupted",
            "phase": "batch",
            "signal": signal_number,
            "frames_processed": len(outcomes),
            "frames_total": len(frames),
        }
        atomic_write_json(root / "recovery_report.json", event)
        status = "interrupted"
        exit_code = 128 + signal_number
    elif any_rejected:
        status = "completed_with_rejections"
        exit_code = 2
    else:
        status = "completed"
        exit_code = 0
    aggregate = {
        "schema_version": 1,
        "command": "batch",
        "status": status,
        "frames_total": len(frames),
        "frames_processed": len(outcomes),
        "frames_rendered": sum(
            1 for outcome in outcomes if outcome["status"] == "rendered"
        ),
        "frames_rejected": sum(
            1 for outcome in outcomes if outcome["status"] == "rejected"
        ),
        "frames_interrupted": sum(
            1 for outcome in outcomes if outcome["status"] == "interrupted"
        ),
        "frames": outcomes,
    }
    atomic_write_json(root / "batch_report.json", aggregate)
    return exit_code, aggregate

