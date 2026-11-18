"""Reproducible local acceptance run for the deterministic renderer."""

from __future__ import annotations

import argparse
import json
import tempfile
from pathlib import Path

from aquarium_render.pipeline import run_pipeline
from aquarium_render.scene import build_sample_scene, load_scene, write_scene
from aquarium_render.verify import compare_run_directories


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--scene", type=Path, default=Path("scenes/sample_scene.json"))
    parser.add_argument("--out", type=Path, default=Path("artifacts/reference_run"))
    parser.add_argument(
        "--summary", type=Path, default=Path("artifacts/acceptance_summary.json")
    )
    parser.add_argument("--interrupt-after-blocks", type=int, default=9)
    args = parser.parse_args()

    if not args.scene.exists():
        write_scene(build_sample_scene(), args.scene)
    scene = load_scene(args.scene)
    reference = run_pipeline(scene, args.out)
    if reference.status != "complete":
        raise SystemExit(f"reference render failed: {reference.status}")

    committed = {"calls": 0}
    target_calls = args.interrupt_after_blocks * 2

    def stop_after_blocks() -> bool:
        committed["calls"] += 1
        return committed["calls"] >= target_calls

    with tempfile.TemporaryDirectory(prefix="aquarium-acceptance-") as temporary:
        resumed_dir = Path(temporary) / "resumed"
        interrupted = run_pipeline(
            scene,
            resumed_dir,
            interrupt_requested=stop_after_blocks,
        )
        if interrupted.status != "interrupted":
            raise SystemExit(f"expected an interrupt snapshot, got {interrupted.status}")
        resumed = run_pipeline(scene, resumed_dir)
        if resumed.status != "complete":
            raise SystemExit(f"resume failed: {resumed.status}")
        diffs = compare_run_directories(args.out, resumed_dir, scene)
        if diffs:
            print(json.dumps({"status": "error", "diffs": diffs}, indent=2, sort_keys=True))
            return 1
        committed_before_resume = len(interrupted.snapshot.completed)

    report = json.loads((args.out / "report.json").read_text(encoding="utf-8"))
    overlap = []
    for light in report["projections"]["lights"]:
        for fish in light["fish"]:
            if fish["receiver"]["status"] == "valid" and fish["water"]["status"] == "valid":
                overlap.append(
                    {
                        "light_id": light["light_id"],
                        "fish_id": fish["fish_id"],
                        "receiver_centroid": fish["receiver"]["centroid"],
                        "water_centroid": fish["water"]["centroid"],
                        "edge_width": fish["receiver"]["edge_width"],
                    }
                )
    summary = {
        "status": "ok",
        "scene": str(args.scene),
        "scene_sha256": scene.summary,
        "reference": str(args.out),
        "reference_completed_blocks": len(reference.snapshot.completed),
        "reference_angles": len(scene.camera.angles()),
        "resolution": [scene.render.width, scene.render.height],
        "block_size": [scene.render.block_width, scene.render.block_height],
        "interrupt_after_blocks": committed_before_resume,
        "resumed_completed_blocks": len(resumed.snapshot.completed),
        "resume_diff_count": 0,
        "two_light_projection_landings": overlap,
        "layer_contributions": report["layers"],
        "penumbra_monotonicity": report["penumbra"]["monotonicity_checks"],
        "shadow_maps": report["shadow_maps"],
        "angle_hashes": report["angles"],
    }
    args.summary.parent.mkdir(parents=True, exist_ok=True)
    args.summary.write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(json.dumps({"status": "ok", "summary": str(args.summary)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

