from __future__ import annotations

import hashlib
import io
from pathlib import Path
from typing import Any

import numpy as np

from .camera import Camera
from .io_utils import atomic_write_bytes, atomic_write_json, canonical_json_bytes
from .render import RenderConfig
from .samples import (
    SampleTerrain,
    generate_sample_set,
    load_sample,
    sample_camera,
    write_baseline_trajectory,
)
from .service import RenderService


def _save_npy(path: Path, array: np.ndarray) -> str:
    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array), allow_pickle=False)
    payload = buffer.getvalue()
    atomic_write_bytes(path, payload)
    return hashlib.sha256(payload).hexdigest()


def _demo_camera(terrain, variant: str) -> Camera:
    camera = sample_camera(terrain, variant)
    return Camera(
        eye=camera.eye,
        target=camera.target,
        up=camera.up,
        fov_y_degrees=camera.fov_y_degrees,
        width=96,
        height=72,
        near=camera.near,
        far=camera.far,
    )


def run_demo(
    outdir: str | Path,
    seed: int = 20261120,
    screen_error_threshold: float = 1.5,
    seam_tolerance: float = 1.0,
    block_size: int = 16,
) -> dict[str, Any]:
    base = Path(outdir)
    samples_dir = base / "samples"
    runs_dir = base / "runs"
    snapshots_dir = base / "snapshots"
    logs_dir = base / "logs"
    for directory in (samples_dir, runs_dir, snapshots_dir, logs_dir):
        directory.mkdir(parents=True, exist_ok=True)

    samples = generate_sample_set(samples_dir, seed=seed)
    baseline_path = write_baseline_trajectory(
        samples,
        logs_dir / "baseline_trajectory.jsonl",
        screen_error_threshold=screen_error_threshold,
        seam_tolerance=seam_tolerance,
        block_size=block_size,
    )
    demo_trace: list[bytes] = []
    reports: list[dict[str, Any]] = []

    for sample in sorted(samples, key=lambda item: item.name):
        terrain = load_sample(sample, block_size=block_size)
        camera = _demo_camera(terrain, "overview")
        service = RenderService(terrain)
        run_dir = runs_dir / sample.name
        run_dir.mkdir(parents=True, exist_ok=True)
        snapshot_path = snapshots_dir / f"{sample.name}.json"
        first = service.render(
            camera,
            screen_error_threshold,
            seam_tolerance,
            max_level=4,
            config=RenderConfig(precision="float64"),
            snapshot_path=snapshot_path,
            heightfield_name=sample.heightfield_path.name,
            material_name=sample.material_path.name,
        )
        color_file_hash = _save_npy(run_dir / "color.npy", first.color)
        depth_file_hash = _save_npy(run_dir / "depth.npy", first.depth)
        heat_file_hash = _save_npy(run_dir / "error_heatmap.npy", first.error_heatmap)
        second = service.render(
            camera,
            screen_error_threshold,
            seam_tolerance,
            max_level=4,
            config=RenderConfig(precision="float64"),
        )
        repeat_identical = (
            np.array_equal(first.color, second.color)
            and np.array_equal(first.depth, second.depth)
            and np.array_equal(first.error_heatmap, second.error_heatmap)
        )
        float32 = service.render(
            camera,
            screen_error_threshold,
            seam_tolerance,
            max_level=4,
            config=RenderConfig(precision="float32"),
        )
        precision_diff = float32.report["pixel_diff"]
        snapshot = service.last_snapshot
        if snapshot is None:
            raise RuntimeError("snapshot was not committed")
        trace_payload = {
            "terrain": sample.name,
            "heightfield_sha256": sample.height_digest,
            "camera": camera.to_dict(),
            "screen_error_threshold": screen_error_threshold,
            "seam_tolerance": seam_tolerance,
            "block_levels": first.report["levels"],
            "error_summary": first.report["error_summary"],
            "seam_status": first.seam_result.summary(),
            "buffer_hashes": first.report["buffers"],
            "snapshot_sha256": snapshot.digest,
            "repeat_render_identical": repeat_identical,
            "precision_diff": precision_diff,
        }
        trace_payload["content_hash"] = hashlib.sha256(
            canonical_json_bytes(trace_payload)
        ).hexdigest()
        demo_trace.append(canonical_json_bytes(trace_payload) + b"\n")
        reports.append(
            {
                "terrain": sample.name,
                "heightfield_sha256": sample.height_digest,
                "material_sha256": sample.material_digest,
                "combined_buffer_sha256": first.combined_digest,
                "saved_array_files": {
                    "color.npy": color_file_hash,
                    "depth.npy": depth_file_hash,
                    "error_heatmap.npy": heat_file_hash,
                },
                "snapshot_sha256": snapshot.digest,
                "block_levels": first.report["levels"],
                "error_summary": first.report["error_summary"],
                "seam_summary": first.seam_result.summary(),
                "normal_report": first.report["normals"],
                "repeat_render_identical": repeat_identical,
                "precision_switch_diff": precision_diff,
            }
        )

    atomic_write_bytes(logs_dir / "demo_trajectory.jsonl", b"".join(demo_trace))
    report: dict[str, Any] = {
        "format": "terrainviz-demo-report",
        "version": 1,
        "seed": int(seed),
        "screen_error_threshold": screen_error_threshold,
        "seam_tolerance": seam_tolerance,
        "block_size": block_size,
        "sample_count": len(samples),
        "terrain_reports": reports,
        "all_repeat_renders_identical": all(
            item["repeat_render_identical"] for item in reports
        ),
        "baseline_trajectory_sha256": hashlib.sha256(
            baseline_path.read_bytes()
        ).hexdigest(),
    }
    report["report_hash"] = hashlib.sha256(canonical_json_bytes(report)).hexdigest()
    atomic_write_json(base / "report.json", report)
    return report

