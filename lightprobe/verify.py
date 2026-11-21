from __future__ import annotations

import json
import shutil
import tempfile
import time
from pathlib import Path
from typing import Any

import numpy as np

from .bake import Baker, BakeResult
from .bake_types import BakeConfig
from .scene import load_scene
from .sh import evaluate_irradiance
from .snapshot import load_snapshot
from .util import atomic_write_json


def compare_to_baseline(
    result: BakeResult,
    baseline_path: str | Path,
    scene_id: str,
) -> dict[str, Any]:
    baseline = json.loads(Path(baseline_path).read_text(encoding="utf-8"))
    level = baseline["levels"][scene_id]
    normals = np.asarray(baseline["truth"]["normals"], dtype=np.float64)
    per_probe: list[float] = []
    relative: list[float] = []
    missing: list[str] = []
    absolute_error_sum = 0.0
    truth_energy = 0.0
    for item in level["probes"]:
        probe_id = item["probe_id"]
        if probe_id not in result.state.records:
            missing.append(probe_id)
            continue
        record = result.state.records[probe_id]
        estimate = evaluate_irradiance(
            record.coefficients, normals, result.state.config.sh_order
        )
        truth = np.asarray(item["irradiance"], dtype=np.float64)
        difference = estimate - truth
        per_probe.append(float(np.sqrt(np.mean(difference * difference))))
        relative.append(
            float(np.linalg.norm(difference)) / max(float(np.linalg.norm(truth)), 1.0e-9)
        )
        absolute_error_sum += float(np.sum(difference * difference))
        truth_energy += float(np.sum(truth * truth))
    count = max(1, len(per_probe))
    return {
        "scene_id": scene_id,
        "probe_count": len(level["probes"]),
        "compared_probe_count": len(per_probe),
        "missing_probe_count": len(missing),
        "sampling": "all baseline probes, all six axis-aligned normals",
        "truth_source": baseline["truth"],
        "rmse_absolute": float(np.sqrt(absolute_error_sum / (count * normals.shape[0] * 3))),
        "mean_probe_rmse": float(np.mean(per_probe)),
        "max_probe_rmse": float(np.max(per_probe)),
        "rms_relative": float(np.sqrt(absolute_error_sum / max(truth_energy, 1.0e-12))),
        "mean_probe_relative": float(np.mean(relative)),
        "max_probe_relative": float(np.max(relative)),
    }


def verify_two_levels(
    samples_dir: str | Path,
    *,
    output_path: str | Path | None = None,
) -> dict[str, Any]:
    samples = Path(samples_dir)
    suite = json.loads((samples / "suite.json").read_text(encoding="utf-8"))
    baseline_path = samples / "baseline_irradiance.json"
    baker = Baker()
    report: dict[str, Any] = {
        "ok": True,
        "suite": suite,
        "levels": {},
        "started_at": time.time(),
    }
    for scene_id in suite["levels"]:
        scene = load_scene(samples / f"{scene_id}.json")
        config = BakeConfig.from_scene(scene)
        started = time.monotonic()
        initial = baker.bake(scene, config)
        elapsed = time.monotonic() - started
        baseline_compare = compare_to_baseline(initial, baseline_path, scene_id)

        if scene_id == "street_corner_room":
            lamp = scene.occluder("street_lamp")
            assert lamp is not None
            center = list(lamp.center)
            moved = scene.with_updates(
                {
                    "occluders": {
                        "street_lamp": {
                            "center": [center[0] - 0.75, center[1] + 0.25, center[2] + 0.5]
                        }
                    }
                }
            )
        else:
            tree = scene.occluder("tree_c")
            assert tree is not None
            center = list(tree.center)
            moved = scene.with_updates(
                {
                    "occluders": {
                        "tree_c": {
                            "center": [center[0] + 1.5, center[1], center[2] - 1.0]
                        }
                    }
                }
            )
        moved_config = BakeConfig.from_scene(moved)
        incremental = baker.bake(moved, moved_config, base_state=initial.state)

        with tempfile.TemporaryDirectory(prefix=f"lp-verify-{scene_id}-") as temporary:
            root = Path(temporary)
            snapshot = root / "snapshot"
            resumed_snapshot = root / "resumed"
            snap_result = baker.bake(
                scene,
                config,
                snapshot_dir=snapshot,
                target_version=3,
                run_id=f"verify-{scene_id}",
            )
            recovered = load_snapshot(snapshot)
            resumed = baker.bake(
                scene,
                config,
                recovered=recovered,
                snapshot_dir=resumed_snapshot,
                target_version=3,
                run_id=f"verify-{scene_id}",
            )
            recovery_consistent = (
                resumed.state.content_hash() == snap_result.state.content_hash()
            )
            recovery_reused = resumed.report.reused_count
            recovery_recomputed = resumed.report.recomputed_count

        report["levels"][scene_id] = {
            "probe_count": initial.report.probe_count,
            "strategy": initial.report.strategy,
            "strategy_audit": initial.report.strategy_audit,
            "baseline": baseline_compare,
            "initial_elapsed_seconds": elapsed,
            "incremental": {
                "changed_factors": incremental.report.influence["changed_factors"],
                "seed_count": incremental.report.influence["seed_count"],
                "affected_count": incremental.report.influence["affected_count"],
                "recomputed_count": incremental.report.recomputed_count,
                "reused_count": incremental.report.reused_count,
                "max_coefficient_deviation": incremental.report.max_coefficient_deviation,
                "max_recomputed_truncation_error": incremental.report.max_recomputed_truncation_error,
                "conservative": incremental.report.influence["conservative"],
            },
            "snapshot_recovery": {
                "chunks": recovered.chunk_count,
                "committed_probes": len(recovered.records),
                "reused_count": recovery_reused,
                "recomputed_count": recovery_recomputed,
                "content_hash_equal": recovery_consistent,
            },
        }
    report["finished_at"] = time.time()
    report["ok"] = all(
        level["snapshot_recovery"]["content_hash_equal"]
        and level["incremental"]["reused_count"] > 0
        for level in report["levels"].values()
    )
    if output_path is not None:
        atomic_write_json(Path(output_path), report)
    return report


def copy_committed_prefix(snapshot_dir: str | Path, target_dir: str | Path, chunks: int) -> None:
    """Test helper: preserve metadata plus an atomic committed chunk prefix."""
    source = Path(snapshot_dir)
    target = Path(target_dir)
    target.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source / "meta.json", target / "meta.json")
    for index in range(chunks):
        shutil.copy2(
            source / f"chunk_{index:06d}.json",
            target / f"chunk_{index:06d}.json",
        )

