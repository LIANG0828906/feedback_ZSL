"""Deterministic local acceptance suite for the complete render pipeline."""

from __future__ import annotations

import json
import shutil
import tempfile
from pathlib import Path
from typing import Any

import numpy as np

from .chunks import build_chunk_table
from .config import RenderConfig
from .errors import E_BUDGET_EXCEEDED, E_RECOVERY_INVALID, DustRenderError
from .incremental import IncrementalRenderer
from .samplegen import generate_sample_set
from .scene import Scene
from .scheduler import RenderScheduler
from .snapshot import load_snapshot, save_snapshot
from .util import atomic_write_json, sha256_file


def run_acceptance(
    outdir: str | Path,
    *,
    seed: int = 20261121,
    reuse_samples: bool = False,
) -> dict[str, Any]:
    root = Path(outdir)
    root.mkdir(parents=True, exist_ok=True)
    traces = root / "traces"
    traces.mkdir(parents=True, exist_ok=True)
    samples_dir = root / "samples"
    if reuse_samples and (samples_dir / "manifest.json").exists():
        manifest = json.loads((samples_dir / "manifest.json").read_text(encoding="utf-8"))
    else:
        manifest = generate_sample_set(samples_dir, seed=seed)
    checks: list[dict[str, Any]] = []

    smoke_path = samples_dir / "smoke_small.dust"
    nested_path = samples_dir / "nested_medium.dust"
    triple_path = samples_dir / "triple_wide.dust"
    smoke = Scene.load(smoke_path)
    nested = Scene.load(nested_path)
    triple = Scene.load(triple_path)

    checks.append(
        {
            "name": "deterministic_sample_generation",
            "status": "passed"
            if all(sha256_file(samples_dir / sample["file"]) == sample["source_sha256"] for sample in manifest["samples"])
            else "failed",
            "samples": [
                {
                    "name": sample["name"],
                    "resolution": [sample["width"], sample["height"]],
                    "media": sample["medium_count"],
                    "density": sample["density_distribution"],
                    "source_sha256": sample["source_sha256"],
                    "baseline_image_sha256": sample["baseline"]["image_sha256"],
                }
                for sample in manifest["samples"]
            ],
        }
    )

    seam_config = RenderConfig.from_dict(
        {
            "chunk_width": 12,
            "chunk_height": 9,
            "parallelism": 3,
            "task_budget": 100_000,
            "budget_policy": "degrade",
            "base_step": 0.12,
            "min_step": 0.04,
            "max_step": 0.8,
            "density_adapt_gain": 1.25,
            "phase": 0.5,
            "shadow_samples": 2,
            "shadow_distance": 8.0,
            "precision": "float",
            "compare_precision": True,
            "seam_validation": True,
            "seam_tolerance": 2e-5,
        }
    )
    seam_first = RenderScheduler().render(smoke, seam_config)
    seam_second = RenderScheduler().render(smoke, seam_config)
    seam_first.write(root / "render-seam")
    seam_first.trace.write(traces / "01-seam-first.jsonl")
    seam_second.trace.write(traces / "01-seam-second.jsonl")
    summary_first = seam_first.summary()
    summary_second = seam_second.summary()
    deterministic = (
        seam_first.image.tobytes() == seam_second.image.tobytes()
        and summary_first["image_sha256"] == summary_second["image_sha256"]
        and summary_first["summary_sha256"] == summary_second["summary_sha256"]
    )
    precision = seam_first.precision_compare or {}
    checks.append(
        {
            "name": "seam_continuity_and_determinism",
            "status": "passed"
            if seam_first.error is None
            and seam_first.seam.get("violation_count") == 0
            and deterministic
            and precision.get("pixels_over_upper_bound") == 0
            else "failed",
            "chunks": len(seam_first.chunks),
            "seam_violations": seam_first.seam.get("violation_count"),
            "max_scattering_error": seam_first.seam.get("max_scattering_abs_error"),
            "max_transmittance_error": seam_first.seam.get("max_transmittance_abs_error"),
            "deterministic_image_sha256": summary_first["image_sha256"],
            "deterministic_summary_sha256": summary_first["summary_sha256"],
            "fixed_point_max_abs": precision.get("max_abs"),
            "fixed_point_upper_bound": precision.get("upper_bound"),
        }
    )
    checks.append(
        {
            "name": "fixed_point_error_summary",
            "status": "passed"
            if precision
            and precision.get("max_abs", 1.0) <= precision.get("upper_bound", 0.0)
            and precision.get("pixels_over_upper_bound") == 0
            else "failed",
            "reference_precision": precision.get("reference_precision"),
            "alternate_precision": precision.get("alternate_precision"),
            "max_abs": precision.get("max_abs"),
            "mean_abs": precision.get("mean_abs"),
            "p95_abs": precision.get("p95_abs"),
            "rms": precision.get("rms"),
            "scatter_max_abs": precision.get("scatter_max_abs"),
            "transmittance_max_abs": precision.get("transmittance_max_abs"),
            "upper_bound": precision.get("upper_bound"),
            "pixels_over_upper_bound": precision.get("pixels_over_upper_bound"),
            "error_map_sha256": precision.get("error_map_sha256"),
        }
    )

    degrade_config = RenderConfig.from_dict(
        {
            "chunk_width": 16,
            "chunk_height": 16,
            "parallelism": 2,
            "task_budget": 12_000,
            "budget_policy": "degrade",
            "base_step": 0.08,
            "min_step": 0.02,
            "max_step": 2.0,
            "density_adapt_gain": 1.25,
            "phase": 0.5,
            "shadow_samples": 3,
            "shadow_distance": 8.0,
            "precision": "float",
            "compare_precision": False,
            "seam_validation": False,
        }
    )
    degrade = RenderScheduler().render(nested, degrade_config)
    degrade.write(root / "render-degrade")
    degrade.trace.write(traces / "02-budget-degrade.jsonl")
    all_within = all(
        result.samples <= degrade_config.task_budget for result in degrade.results.values()
    )
    checks.append(
        {
            "name": "budget_degradation",
            "status": "passed"
            if degrade.error is None and degrade.degraded and all_within
            else "failed",
            "samples": degrade.summary()["samples"]["total"],
            "step_base": degrade_config.base_step,
            "step_effective": degrade.effective_step,
            "step_scale": degrade.effective_step / degrade_config.base_step,
            "waiting_tasks": len(degrade.initial_waiting),
            "seam": "not_run",
        }
    )

    reject_config = RenderConfig.from_dict(
        {
            "chunk_width": 8,
            "chunk_height": 8,
            "parallelism": 4,
            "task_budget": 3_000,
            "budget_policy": "reject",
            "base_step": 0.06,
            "min_step": 0.02,
            "max_step": 1.0,
            "density_adapt_gain": 1.25,
            "phase": 0.5,
            "shadow_samples": 2,
            "shadow_distance": 8.0,
            "precision": "float",
            "compare_precision": False,
            "seam_validation": False,
        }
    )
    rejected = RenderScheduler().render(triple, reject_config)
    rejected.write(root / "budget-reject")
    rejected.trace.write(traces / "03-budget-reject.jsonl")
    reject_error = rejected.error
    if reject_error is not None:
        atomic_write_json(root / "budget-reject" / "error.json", reject_error.to_dict())
    checks.append(
        {
            "name": "budget_rejection_preserves_partial_results",
            "status": "passed"
            if reject_error is not None
            and reject_error.code == E_BUDGET_EXCEEDED
            and rejected.rejected
            and rejected.results
            else "failed",
            "code": reject_error.code if reject_error else None,
            "completed_chunks": sorted(rejected.results, key=lambda value: int(value[1:])),
            "rejected_chunks": sorted(rejected.rejected, key=lambda value: int(value[1:])),
            "partial_samples": rejected.summary()["samples"]["total"],
        }
    )

    incremental_config = RenderConfig.from_dict(
        {
            "chunk_width": 6,
            "chunk_height": 6,
            "parallelism": 4,
            "task_budget": 100_000,
            "budget_policy": "degrade",
            "base_step": 0.12,
            "min_step": 0.04,
            "max_step": 0.8,
            "density_adapt_gain": 1.25,
            "phase": 0.5,
            "shadow_samples": 2,
            "shadow_distance": 8.0,
            "precision": "float",
            "compare_precision": False,
            "seam_validation": False,
        }
    )
    changed = _perturb_density_block(smoke, (0, 0, 0))
    incremental_renderer = IncrementalRenderer()
    base_run = incremental_renderer.render_initial(smoke, incremental_config)
    old_version = incremental_renderer.preview.version
    incremental = incremental_renderer.render_update(base_run, changed, incremental_config)
    incremental.run.write(root / "render-incremental")
    incremental.run.trace.write(traces / "04-incremental-update.jsonl")
    base_run.write(root / "render-incremental-base")
    base_run.trace.write(traces / "04-incremental-base.jsonl")
    full_reference = RenderScheduler().render(changed, incremental_config)
    full_reference.write(root / "render-incremental-fresh")
    full_reference.trace.write(traces / "04-incremental-fresh.jsonl")
    same_image = np.array_equal(incremental.run.image, full_reference.image)
    checks.append(
        {
            "name": "incremental_rerender_and_atomic_publish",
            "status": "passed"
            if incremental.run.error is None
            and same_image
            and incremental.reused
            and incremental.affected
            and old_version == 1
            and incremental_renderer.preview.version == 2
            else "failed",
            "affected_chunks": incremental.affected,
            "affected_count": len(incremental.affected),
            "reused_chunks": incremental.reused,
            "reused_count": len(incremental.reused),
            "incremental_image_sha256": incremental.run.summary()["image_sha256"],
            "full_render_image_sha256": full_reference.summary()["image_sha256"],
            "published_version": incremental_renderer.preview.version,
        }
    )

    snapshot_config = RenderConfig.from_dict(
        {
            "chunk_width": 12,
            "chunk_height": 9,
            "parallelism": 2,
            "task_budget": 1_000,
            "budget_policy": "degrade",
            "base_step": 0.12,
            "min_step": 0.04,
            "max_step": 0.5,
            "density_adapt_gain": 1.25,
            "phase": 0.5,
            "shadow_samples": 3,
            "shadow_distance": 8.0,
            "precision": "float",
            "compare_precision": False,
            "seam_validation": False,
        }
    )
    snapshot_dir = root / "snapshot"
    snapshot_dir.mkdir(parents=True, exist_ok=True)
    snapshot_path = snapshot_dir / "render-snapshot.json"
    snapshot_chunks = build_chunk_table(24, 18, 12, 9)
    partial_scheduler = RenderScheduler()

    def progress(results, statuses, step) -> None:
        save_snapshot(
            snapshot_path,
            smoke,
            snapshot_config,
            snapshot_chunks,
            results,
            statuses,
            effective_step=step,
        )
        if len(results) >= 2:
            partial_scheduler.request_cancel()

    partial = partial_scheduler.render(smoke, snapshot_config, on_progress=progress)
    partial.trace.write(traces / "05-snapshot-partial.jsonl")
    loaded = load_snapshot(snapshot_path, smoke, snapshot_config)
    resumed = RenderScheduler().render(
        smoke,
        snapshot_config,
        preloaded=loaded.results,
        preloaded_status="recovered",
    )
    resumed.write(snapshot_dir / "resumed")
    resumed.trace.write(traces / "05-snapshot-resume.jsonl")
    snapshot_full = RenderScheduler().render(smoke, snapshot_config)
    snapshot_full.write(snapshot_dir / "full-reference")
    info = {
        "completed_indices": loaded.completed_indices,
        "reused": len(loaded.results),
        "snapshot_hash": loaded.snapshot_hash,
        "hash_chain": loaded.document["hash_chain"],
        "resumed_image_sha256": resumed.summary()["image_sha256"],
        "full_image_sha256": snapshot_full.summary()["image_sha256"],
    }
    atomic_write_json(snapshot_dir / "snapshot-info.json", info)
    corrupt_code = None
    with tempfile.TemporaryDirectory(prefix="dustrender-recovery-") as temp:
        corrupt = Path(temp) / "corrupt.json"
        document = json.loads(snapshot_path.read_text(encoding="utf-8"))
        document["hash_chain"][0] = "00" * 32
        corrupt.write_text(json.dumps(document), encoding="utf-8")
        try:
            load_snapshot(corrupt, smoke, snapshot_config)
        except DustRenderError as exc:
            corrupt_code = exc.code
    checks.append(
        {
            "name": "atomic_snapshot_resume",
            "status": "passed"
            if partial.error is not None
            and partial.error.code == "E_CANCELLED"
            and loaded.completed_indices == [0, 1]
            and resumed.error is None
            and np.array_equal(resumed.image, snapshot_full.image)
            and corrupt_code == E_RECOVERY_INVALID
            else "failed",
            "completed_before_resume": loaded.completed_indices,
            "reused": len(loaded.results),
            "snapshot_hash": loaded.snapshot_hash,
            "resumed_image_sha256": resumed.summary()["image_sha256"],
            "full_image_sha256": snapshot_full.summary()["image_sha256"],
            "corrupt_snapshot_code": corrupt_code,
        }
    )

    try:
        import OpenGL

        opengl_version = OpenGL.__version__
    except Exception:
        opengl_version = "unavailable"
    import platform

    report = {
        "mode": "local-offline",
        "network_access": False,
        "seed": seed,
        "ok": all(check["status"] == "passed" for check in checks),
        "dependencies": {
            "python": platform.python_version(),
            "numpy": np.__version__,
            "PyOpenGL": opengl_version,
        },
        "sample_manifest": manifest,
        "checks": checks,
        "artifacts": {
            "samples_manifest": str(samples_dir / "manifest.json"),
            "seam_render_summary": str(root / "render-seam" / "summary.json"),
            "degrade_render_summary": str(root / "render-degrade" / "summary.json"),
            "incremental_summary": str(root / "render-incremental" / "summary.json"),
            "snapshot_info": str(snapshot_dir / "snapshot-info.json"),
            "traces": str(traces),
            "images": str(root / "images"),
        },
    }
    images = root / "images"
    images.mkdir(parents=True, exist_ok=True)
    for name in ("render-seam", "render-degrade", "render-incremental", "budget-reject"):
        source = root / name / "image.ppm"
        if source.exists():
            shutil.copyfile(source, images / f"{name}.ppm")
    atomic_write_json(root / "report.json", report)
    return report


def _perturb_density_block(scene: Scene, index: tuple[int, int, int]) -> Scene:
    field = scene.density_fields["main"]
    block = next(item for item in field.blocks if item.index == index)
    values = np.frombuffer(
        scene.payload,
        dtype="<f4",
        count=block.count,
        offset=block.offset,
    ).reshape(block.shape).copy()
    values = np.clip(values * 0.9 + 0.05, 0.0, 1.0).astype("<f4")
    return scene.with_density_block("main", index, values)
