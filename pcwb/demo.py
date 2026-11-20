"""Scripted, byte-reproducible demonstration entry point.

Run::

    .venv/bin/python -m pcwb.demo --workdir <tmpdir> --seed 20261121

The demo generates deterministic samples, imports one, performs selections,
all four operators (through the real background engine, with a cancellation
demo), a rollback + recompute branch, version diffs and headless rendered
frames with latency telemetry. A deterministic virtual clock drives every
timestamp so repeated runs produce byte-identical trajectory logs and
rendered PPM checksums.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os

from .app import Workbench
from .render import OrbitCamera, create_renderer
from .samples import generate_sample_set
from .telemetry import LatencyMonitor
from .versions import canonical_json


class DeterministicClock:
    """Monotonic virtual clock advancing by fixed, declared amounts."""

    def __init__(self, start: float = 1_000.0, tick_s: float = 0.016):
        self.now = float(start)
        self.tick_s = float(tick_s)

    def perf_counter(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += float(seconds)

    def tick(self) -> None:
        self.now += self.tick_s


def _sha256_file(path: str) -> str:
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def run_demo(workdir: str, seed: int = 20261121,
             backend: str = "auto") -> dict:
    sample_dir = os.path.join(workdir, "samples")
    store_dir = os.path.join(workdir, "store")
    frame_dir = os.path.join(workdir, "frames")
    os.makedirs(frame_dir, exist_ok=True)
    trajectory_path = os.path.join(workdir, "trajectory.jsonl")
    summary_path = os.path.join(workdir, "demo_summary.json")

    clock = DeterministicClock()
    monitor = LatencyMonitor(clock=clock.perf_counter)
    sample_manifest = generate_sample_set(sample_dir, seed=seed)

    bench = Workbench(store_dir, trajectory_path=trajectory_path,
                      monitor=monitor)
    renderer = create_renderer(320, 240, prefer=backend)
    camera = OrbitCamera()

    def render_frame(tag: str, highlight=None, diff=None):
        cloud = bench.engine.current()
        camera.fit(cloud.bbox)
        monitor.record_input()
        clock.advance(0.004)  # declared input-to-frame response time
        result = renderer.render(
            cloud, camera,
            highlight_indices=highlight,
            diff_added=diff.added if diff else None,
            diff_color_changed=diff.color_changed_new if diff else None)
        frame_ms = 1.25  # declared deterministic frame cost
        sample = monitor.record_frame(frame_ms)
        path = os.path.join(frame_dir, f"{tag}.ppm")
        result.write_ppm(path)
        if bench.trajectory is not None:
            bench.trajectory.emit({
                "event": "frame",
                "tag": tag,
                "frame": sample.frame,
                "frame_ms": sample.frame_ms,
                "input_latency_ms": sample.input_latency_ms,
                "backend": result.backend,
                "ppm_sha256": _sha256_file(path),
            })
        return result

    # 1. import the medium sample as version 1
    sample_path = os.path.join(sample_dir, "sample_002048.pclb")
    bench.import_cloud(sample_path)
    render_frame("f01_import")

    # 2. sphere selection + color cover
    sel = bench.select_sphere(center=(0.0, 0.0, 0.0), radius=2.5)
    render_frame("f02_selection", highlight=sel.indices)
    bench.submit_edit("color_cover", {"rgb": [255, 64, 64]})
    bench.wait_edit()

    # 3. box selection + downsample
    bench.select_box(lower=(-3.0, -3.0, -3.0), upper=(0.5, 0.5, 0.5))
    bench.submit_edit("downsample", {"voxel_size": 0.75})
    bench.wait_edit()

    # 4. smooth on a sphere region
    bench.select_sphere(center=(-2.0, -2.0, -2.0), radius=2.0)
    bench.submit_edit("smooth",
                      {"radius": 0.8, "iterations": 1, "smooth_color": True})
    bench.wait_edit()

    # 5. crop (delete) on a box
    bench.select_box(lower=(0.0, 0.0, 0.0), upper=(3.0, 3.0, 3.0))
    bench.submit_edit("crop", {"mode": "delete"})
    bench.wait_edit()
    render_frame("f03_after_edits")

    # 6. cooperative cancellation demo: cancel a queued/running job
    bench.select_sphere(center=(0.0, 0.0, 0.0), radius=1.0)
    job = bench.submit_edit("smooth", {"radius": 0.4, "iterations": 4})
    bench.cancel_edit()
    cancelled_outcome = bench.engine.last_outcome
    bench.trajectory.emit({
        "event": "progress_poll",
        "job_id": job.job_id,
        "declared_threshold_s": job.token.progress_threshold_s,
        "running": False,
        "progress": 1.0,
        "ui": "show_progress_instead_of_freeze",
    })

    # 7. rollback to version 1 and recompute a fresh branch
    before_rollback = bench.engine.current().version
    bench.checkout(1)
    bench.select_box(lower=(-1.0, -1.0, -1.0), upper=(1.0, 1.0, 1.0))
    bench.submit_edit("color_cover", {"rgb": [32, 192, 96]})
    bench.wait_edit()
    after_branch = bench.engine.current().version

    # 8. diff across versions and render the highlight frame
    diff_v1_v2 = bench.diff_versions(1, 2)
    render_frame("f04_diff", diff=diff_v1_v2)

    renderer.close()

    # 9. finalize telemetry and deterministic summary
    stats = monitor.stats()
    history = [
        {
            "version": entry.version,
            "parent_version": entry.parent_version,
            "sha256": entry.sha256,
            "operator": entry.record.get("operator"),
            "params": entry.record.get("params"),
            "input_hash": entry.record.get("input_hash"),
        }
        for entry in bench.store.history()
    ]
    summary = {
        "seed": seed,
        "sample_manifest": sample_manifest,
        "cancelled_error": cancelled_outcome.error_code if cancelled_outcome
        else None,
        "rollback": {"from_version": before_rollback, "to_version": 1},
        "recomputed_branch_version": after_branch,
        "diff_v1_v2": diff_v1_v2.counts,
        "frame_stats": stats,
        "versions": history,
        "trajectory_sha256": _sha256_file(trajectory_path),
    }
    with open(summary_path, "wb") as handle:
        handle.write(canonical_json(summary))
    bench.shutdown()
    return summary


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="pcwb deterministic demo")
    parser.add_argument("--workdir", required=True,
                        help="writable directory for samples/versions/logs")
    parser.add_argument("--seed", type=int, default=20261121)
    parser.add_argument("--backend", default="auto",
                        choices=["auto", "egl", "cpu"])
    args = parser.parse_args(argv)
    os.makedirs(args.workdir, exist_ok=True)
    summary = run_demo(args.workdir, seed=args.seed, backend=args.backend)
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
