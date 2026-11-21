"""Scripted deterministic editing demonstration."""

from __future__ import annotations

import shutil
import time
from pathlib import Path
from typing import Any

from .async_edit import AsyncEditController
from .cache import OperationCache
from .diff import diff_clouds
from .metrics import InteractionMetrics
from .probe import SnapshotProbeLoop
from .repository import PointCloudRepository
from .samplegen import generate_samples
from .selection import (
    Selection,
    select_sphere,
    select_world_rectangle,
)
from .trace import DeterministicTrace, RuntimeMetricsLog
from .util import pretty_json_bytes


def _run_async_edit(
    controller: AsyncEditController,
    trace: DeterministicTrace,
    metrics: InteractionMetrics,
    operator: str,
    params: dict[str, Any],
    selection: Selection,
) -> dict[str, Any]:
    base = controller.display_snapshot()
    trace.append(
        {
            "type": "selection",
            "operator": operator,
            "version": base.version,
            "version_hash": base.content_hash,
            "count": selection.count,
            "bounds": None if selection.bounds is None else selection.bounds.to_dict(),
        }
    )
    probe = SnapshotProbeLoop(controller, metrics, sleep_seconds=0.001)
    probe.start()
    started = time.perf_counter_ns()
    controller.submit(operator, params, selection)
    observed_progress: list[dict[str, Any]] = []
    while not controller.wait(0.02):
        status = controller.status()
        if status.show_progress:
            snapshot = {
                "phase": status.phase,
                "fraction": round(status.fraction, 4),
                "elapsed_ms": round(status.elapsed_ms, 3),
            }
            if (
                not observed_progress
                or observed_progress[-1]["phase"] != snapshot["phase"]
            ):
                observed_progress.append(snapshot)
    probe_report = probe.stop()
    duration_ms = (time.perf_counter_ns() - started) / 1_000_000.0
    metrics.record_edit_job(duration_ms)
    status = controller.status()
    if status.state != "succeeded" or status.result_version is None:
        raise RuntimeError(f"background edit failed: {status.to_dict()}")
    commit = controller.last_commit
    if commit is None:
        raise RuntimeError("background edit reported success without a commit")
    published = controller.display_snapshot()
    if (
        published.version != commit.version
        or published.content_hash != commit.output_hash
    ):
        raise RuntimeError("published snapshot does not match the committed version")
    trace.append(
        {
            "type": "commit",
            "version": commit.version,
            "operator": operator,
            "params": params,
            "input_version": base.version,
            "input_hash": base.content_hash,
            "output_hash": commit.output_hash,
            "point_count": commit.snapshot.count,
            "cache_hit": status.cache_hit,
        }
    )
    runtime_event = {
        "type": "edit_metric",
        "version": commit.version,
        "duration_ms": round(duration_ms, 6),
        "old_snapshot_isolated": probe_report.old_snapshot_observed_while_running,
        "progress_threshold_ms": controller.progress_threshold_ms,
        "progress_events": observed_progress,
        "probe": probe_report.to_dict(),
    }
    return runtime_event


def run_demo(
    output_dir: str | Path,
    *,
    seed: int = 20261122,
    progress_threshold_ms: float = 50.0,
    clean: bool = True,
) -> dict[str, Any]:
    out = Path(output_dir)
    if clean and out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True, exist_ok=True)
    samples_dir = out / "samples"
    repo_dir = out / "repo"
    trace = DeterministicTrace(out / "trace.jsonl")
    runtime = RuntimeMetricsLog(out / "runtime_metrics.jsonl")
    metrics = InteractionMetrics(frame_budget_ms=16.667)

    sample_manifest = generate_samples(samples_dir, seed=seed)
    for record in sample_manifest["samples"]:
        trace.append(
            {
                "type": "sample",
                "name": record["name"],
                "file": record["file"],
                "count": record["count"],
                "sha256": record["sha256"],
                "bounds": record["bounds"],
            }
        )
    sample_path = samples_dir / "medium-hole.pcwb"
    from .format import read_point_cloud

    cloud = read_point_cloud(sample_path)
    repository = PointCloudRepository.create(
        repo_dir, cloud, source_name="medium-hole.pcwb"
    )
    cache = OperationCache(max_entries=8)
    controller = AsyncEditController(
        repository,
        cache=cache,
        progress_threshold_ms=progress_threshold_ms,
    )
    initial = controller.display_snapshot()
    trace.append(
        {
            "type": "commit",
            "version": initial.version,
            "operator": "import",
            "params": {"source": "medium-hole.pcwb"},
            "input_version": 0,
            "input_hash": "",
            "output_hash": initial.content_hash,
            "point_count": initial.count,
            "cache_hit": False,
        }
    )
    runtime_events: list[dict[str, Any]] = []

    sphere = select_sphere(
        controller.display_snapshot(),
        controller.display_snapshot().cloud.bounds.center,
        controller.display_snapshot().cloud.bounds.diagonal * 0.24,
    )
    runtime_events.append(
        _run_async_edit(
            controller,
            trace,
            metrics,
            "smooth",
            {
                "radius": round(
                    controller.display_snapshot().cloud.bounds.diagonal * 0.012, 6
                ),
                "iterations": 3,
            },
            sphere,
        )
    )

    snapshot = controller.display_snapshot()
    center = snapshot.cloud.bounds.center
    half = snapshot.cloud.bounds.diagonal * 0.16
    rectangle = select_world_rectangle(
        snapshot,
        (center[0] - half, center[1] - half, snapshot.cloud.bounds.minimum[2] - 1.0),
        (center[0] + half, center[1] + half, snapshot.cloud.bounds.maximum[2] + 1.0),
    )
    runtime_events.append(
        _run_async_edit(
            controller,
            trace,
            metrics,
            "downsample",
            {
                "mode": "voxel",
                "voxel_size": round(snapshot.cloud.bounds.diagonal * 0.018, 6),
            },
            rectangle,
        )
    )

    snapshot = controller.display_snapshot()
    sphere = select_sphere(
        snapshot, snapshot.cloud.bounds.center, snapshot.cloud.bounds.diagonal * 0.20
    )
    runtime_events.append(
        _run_async_edit(
            controller,
            trace,
            metrics,
            "color_overwrite",
            {"rgb": [238, 132, 32]},
            sphere,
        )
    )

    snapshot = controller.display_snapshot()
    rectangle = select_world_rectangle(
        snapshot,
        (
            snapshot.cloud.bounds.minimum[0],
            snapshot.cloud.bounds.minimum[1],
            snapshot.cloud.bounds.minimum[2] - 1.0,
        ),
        (
            snapshot.cloud.bounds.center[0],
            snapshot.cloud.bounds.maximum[1],
            snapshot.cloud.bounds.maximum[2] + 1.0,
        ),
    )
    runtime_events.append(
        _run_async_edit(
            controller,
            trace,
            metrics,
            "crop",
            {"mode": "remove"},
            rectangle,
        )
    )

    before_rollback = controller.display_snapshot()
    rolled_back = repository.checkout(2)
    controller.sync_current()
    trace.append(
        {
            "type": "rollback",
            "from_version": before_rollback.version,
            "to_version": rolled_back.version,
            "to_hash": rolled_back.content_hash,
        }
    )

    snapshot = controller.display_snapshot()
    sphere = select_sphere(
        snapshot, snapshot.cloud.bounds.center, snapshot.cloud.bounds.diagonal * 0.17
    )
    runtime_events.append(
        _run_async_edit(
            controller,
            trace,
            metrics,
            "color_overwrite",
            {"rgb": [40, 180, 240]},
            sphere,
        )
    )

    version_a = 1
    version_b = int(repository.manifest()["current_version"])
    diff = diff_clouds(
        repository.load_version(version_a).cloud,
        repository.load_version(version_b).cloud,
    )
    trace.append(
        {
            "type": "diff",
            "from_version": version_a,
            "to_version": version_b,
            "diff": diff.to_dict(),
        }
    )

    # Run a fixed number of display-only frames after editing.  Their measured
    # durations go to the runtime sidecar; the deterministic trace records only
    # the policy and sample count.
    final_probe = SnapshotProbeLoop(controller, metrics, sleep_seconds=0.0)
    final_probe.start()
    time.sleep(0.03)
    final_report = final_probe.stop()
    trace.append(
        {
            "type": "frame_policy",
            "budget_ms": 16.667,
            "renderer_reads": "one published snapshot per frame",
            "measured_samples_file": "runtime_metrics.jsonl",
            "deterministic_trace_excludes_wall_clock": True,
        }
    )
    runtime.append(
        {
            "type": "final_probe",
            "report": final_report.to_dict(),
        }
    )
    for event in runtime_events:
        runtime.append(event)

    manifest = repository.manifest()
    current = controller.display_snapshot()
    summary = {
        "schema": "pcwb-demo-summary-v1",
        "seed": seed,
        "current_version": manifest["current_version"],
        "max_version": manifest["max_version"],
        "current_hash": current.content_hash,
        "point_count": current.count,
        "versions": [
            {
                "version": entry["version"],
                "parent_version": entry["parent_version"],
                "operator": entry["operator"],
                "output_hash": entry["output_hash"],
                "output_count": entry["output_count"],
            }
            for entry in manifest["versions"]
        ],
        "diff": diff.to_dict(),
        "runtime_metrics_file": "runtime_metrics.jsonl",
    }
    (out / "summary.json").write_bytes(pretty_json_bytes(summary))
    trace.append(
        {
            "type": "summary",
            "current_version": current.version,
            "max_version": manifest["max_version"],
            "current_hash": current.content_hash,
            "point_count": current.count,
        }
    )
    controller.close()
    return summary
