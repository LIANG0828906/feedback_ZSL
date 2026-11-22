#!/usr/bin/env python3
"""Run repeatable incremental/checkpoint diagnostics and write a JSON report."""

from __future__ import annotations

import json
import platform
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from formation_workbench.engine import FormationEngine
from formation_workbench.generator import generate_scene
from formation_workbench.validation import load_scene


def _manifest_summary(manifest) -> dict:
    return {
        "direct_path_members": list(manifest.direct_path_members),
        "path_recomputed": list(manifest.path_recomputed),
        "smooth_recomputed": list(manifest.smooth_recomputed),
        "simulation_recomputed": list(manifest.simulation_recomputed),
        "reused_paths": list(manifest.reused_paths),
        "reused_simulations": list(manifest.reused_simulations),
        "invalidated_interval_count": len(manifest.invalidated_intervals),
        "reused_interval_count": len(manifest.reused_intervals),
        "status": manifest.status,
    }


def main() -> int:
    root = ROOT
    sample = root / "samples" / "formation_scene.json"
    if not sample.exists():
        sample.write_text(
            json.dumps(generate_scene(167), indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
    checkpoint = root / "artifacts" / "diagnostic_checkpoint.json"
    if checkpoint.exists():
        checkpoint.unlink()
    lock = checkpoint.with_suffix(checkpoint.suffix + ".lock")
    if lock.exists():
        lock.unlink()

    scene = load_scene(sample)
    engine = FormationEngine(scene, checkpoint_path=checkpoint)
    build = engine.build_all(parallel=True, save_checkpoint=True)
    first_compare = engine.compare_with_full(parallel=True)
    leaf_manifest = engine.apply_cell_cost((11, 7), 4)
    leaf_compare = engine.compare_with_full(parallel=True)
    root_manifest = engine.apply_cell_cost((13, 9), 8)
    root_compare = engine.compare_with_full(parallel=True)
    blockade_manifest = engine.apply_blockade("blk-crossing", True)
    blockade_compare = engine.compare_with_full(parallel=True)

    restored = FormationEngine(load_scene(sample), checkpoint_path=checkpoint)
    restore_attempt = restored.try_restore_checkpoint()

    export_path = root / "artifacts" / "trajectory_export.json"
    engine.export_json(export_path)
    report = {
        "generated_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "environment": {
            "python": sys.version.split()[0],
            "platform": platform.platform(),
            "executable": sys.executable,
        },
        "sample": str(sample.relative_to(root)),
        "sample_map_hash": scene.map.content_hash(),
        "build": {
            "status": build["status"],
            "complete": build["complete"],
            "elapsed_ms": build["elapsed_ms"],
            "commit_order": build["checkpoint"]["commit_order"],
            "checkpoint_digest": build["checkpoint"]["digest"],
        },
        "baseline_comparison": first_compare.to_dict(),
        "leaf_edit": {
            "cell": [11, 7],
            "new_cost": 4,
            "manifest": _manifest_summary(leaf_manifest),
            "comparison": leaf_compare.to_dict(),
        },
        "root_edit": {
            "cell": [13, 9],
            "new_cost": 8,
            "manifest": _manifest_summary(root_manifest),
            "comparison": root_compare.to_dict(),
        },
        "blockade_edit": {
            "blockade_id": "blk-crossing",
            "active": True,
            "manifest": _manifest_summary(blockade_manifest),
            "comparison": blockade_compare.to_dict(),
        },
        "checkpoint_restore_after_edit": {
            "restored": restore_attempt.restored,
            "reason": restore_attempt.reason,
            "full_rebuild_required": restore_attempt.full_rebuild_required,
        },
        "export": str(export_path.relative_to(root)),
    }
    destination = root / "artifacts" / "verification_report.json"
    destination.write_text(
        json.dumps(report, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(report, indent=2, ensure_ascii=False))
    return 0 if all(
        item["comparison"]["ok"]
        for item in (report["leaf_edit"], report["root_edit"], report["blockade_edit"])
    ) else 1


if __name__ == "__main__":
    raise SystemExit(main())
