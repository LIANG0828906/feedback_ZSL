"""Generate samples, run verification, export the dashboard, and record results."""

from __future__ import annotations

import json
import platform
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from transparent_pipeline.comparison import compare_results  # noqa: E402
from transparent_pipeline.engine import RenderEngine  # noqa: E402
from transparent_pipeline.opengl_probe import probe_opengl  # noqa: E402
from transparent_pipeline.renderer import RenderConfig  # noqa: E402
from transparent_pipeline.sample_generator import generate_sample  # noqa: E402
from transparent_pipeline.scene import Scene, load_truth  # noqa: E402
from transparent_pipeline.ui import export_static_dashboard  # noqa: E402
from transparent_pipeline.verify import verify_three_paths, verify_truth  # noqa: E402


def _run_pytest() -> dict:
    completed = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-o", "addopts="],
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )
    return {
        "returncode": completed.returncode,
        "summary": (completed.stdout + completed.stderr).strip().splitlines()[-1],
        "stdout": completed.stdout,
        "stderr": completed.stderr,
    }


def main() -> int:
    started = time.time()
    artifacts = ROOT / "artifacts"
    artifacts.mkdir(parents=True, exist_ok=True)
    sample_dir = ROOT / "samples"
    manifest = generate_sample(sample_dir, seed=20261123)
    scene = Scene.load(sample_dir / "scene.json")
    truth = load_truth(sample_dir / "truth.json", scene)
    config = RenderConfig(step_size=0.05)

    truth_report = verify_truth(scene, truth, config)
    consistency = verify_three_paths(
        scene,
        config,
        work_directory=ROOT / "runtime" / "validation-consistency",
    )

    incremental_dir = ROOT / "runtime" / "validation-incremental"
    incremental_engine = RenderEngine(scene, config, checkpoint_directory=incremental_dir)
    initial = incremental_engine.render(mode="full")
    invalidation = incremental_engine.edit_media(
        "glass_sphere",
        {"absorption": [0.061, 0.051, 0.041]},
    )
    invalidated_blocks = sorted({entry["block_id"] for entry in invalidation})
    incremental = incremental_engine.render(mode="incremental")
    full_reference = RenderEngine(
        incremental_engine.scene.copy(),
        config,
        checkpoint_directory=None,
    ).render(mode="full")
    incremental_comparison = compare_results(incremental, full_reference)

    reduced_preview_config = RenderConfig(step_size=0.05, preview_step_size=0.025)
    reduced_full = RenderEngine(scene, config, checkpoint_directory=None).render(mode="full")
    reduced_preview = RenderEngine(
        scene,
        reduced_preview_config,
        checkpoint_directory=None,
    ).render(mode="preview")
    reduced_preview_comparison = compare_results(reduced_preview, reduced_full)

    export_dir = ROOT / "artifacts" / "dashboard_export"
    if export_dir.exists():
        import shutil

        shutil.rmtree(export_dir)
    export_engine = RenderEngine(scene, config, checkpoint_directory=None)
    export_outcome = export_engine.render(mode="full")
    exported = export_outcome.export(export_dir, include_arrays=True)
    exported.update(export_static_dashboard(export_engine, export_outcome, export_dir))

    pytest_report = _run_pytest()
    report = {
        "ok": (
            truth_report["ok"]
            and consistency["ok"]
            and incremental_comparison.equal
            and pytest_report["returncode"] == 0
        ),
        "generated_at_unix": time.time(),
        "elapsed_seconds": time.time() - started,
        "environment": {
            "python": sys.version,
            "platform": platform.platform(),
            "opengl": probe_opengl(),
        },
        "sample": manifest,
        "sample_validation": {
            "scene_hash_matches_truth": truth["scene_hash"] == scene.content_hash,
            "wavelengths": scene.band_count,
            "media": len(scene.media),
            "lights": len(scene.lights),
            "camera_samples": len(scene.camera["trajectory"]),
        },
        "analytic_truth": truth_report,
        "three_path_consistency": consistency,
        "incremental_invalidation": {
            "entries": invalidation,
            "invalidated_blocks": invalidated_blocks,
            "invalidated_count": len(invalidated_blocks),
            "total_blocks": len(initial.block_windows),
            "did_not_rebuild_full_screen": len(invalidated_blocks) < len(initial.block_windows),
            "matches_fresh_full_recompute": incremental_comparison.equal,
            "comparison": incremental_comparison.to_dict(),
        },
        "reduced_preview": {
            "actual_step": reduced_preview.actual_step,
            "error_bound": reduced_preview.preview_error_bound,
            "exact_parity_with_full_step": reduced_preview_comparison.equal,
            "comparison": reduced_preview_comparison.to_dict(),
        },
        "ui_export": {
            "files": exported,
            "all_files_exist": all(Path(path).exists() for path in exported.values()),
        },
        "pytest": pytest_report,
    }
    output = artifacts / "validation_report.json"
    output.write_text(json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps({"ok": report["ok"], "report": str(output), "pytest": pytest_report["summary"]}, indent=2))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
