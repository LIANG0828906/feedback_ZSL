#!/usr/bin/env python3
"""Generate the sample scene and run the complete offline acceptance flow."""

from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from shadowbox.checkpoint import atomic_write_json
from shadowbox.diff import compare_arrays, load_raw_output
from shadowbox.pipeline import render_to_directory
from shadowbox.scene import generate_scene, load_scene, write_scene
from shadowbox.verify import verify_output


def main() -> int:
    output_root = ROOT / "artifacts" / "demo"
    output_root.mkdir(parents=True, exist_ok=True)
    scene_path = output_root / "scene.json"
    write_scene(
        scene_path,
        generate_scene(
            width=320,
            height=256,
            block_size=(64, 64),
            samples_per_pixel=2,
        ),
    )
    scene = load_scene(scene_path)

    uninterrupted = output_root / "uninterrupted"
    uninterrupted_result = render_to_directory(scene, uninterrupted, fresh=True)
    if uninterrupted_result.exit_code != 0:
        raise RuntimeError("uninterrupted render failed")
    uninterrupted_rgba, uninterrupted_depth = load_raw_output(
        uninterrupted, scene.camera.width, scene.camera.height
    )

    resumed = output_root / "resumed"
    interrupted_result = render_to_directory(
        scene,
        resumed,
        fresh=True,
        interrupt_after_blocks=7,
    )
    if not interrupted_result.interrupted:
        raise RuntimeError("expected a recoverable interruption")
    resumed_result = render_to_directory(
        scene,
        resumed,
        reference_directory=uninterrupted,
    )
    if resumed_result.exit_code != 0:
        raise RuntimeError("resumed render did not match the uninterrupted baseline")

    resumed_rgba, resumed_depth = load_raw_output(
        resumed, scene.camera.width, scene.camera.height
    )
    comparison = compare_arrays(
        uninterrupted_rgba,
        resumed_rgba,
        uninterrupted_depth,
        resumed_depth,
        scene,
    )
    verification = verify_output(scene, resumed)
    report = json.loads((resumed / "report.json").read_text(encoding="utf-8"))
    summary = {
        "format": "shadowbox-demo-summary-v1",
        "scene_sha256": scene.digest,
        "interrupted_after_blocks": 7,
        "uninterrupted_image_sha256": report["summary"]["image_rgba_sha256"],
        "uninterrupted_depth_sha256": report["summary"]["optical_depth_sha256"],
        "resume_recovery_count": report["summary"]["recovery_count"],
        "pixel_diff_equal": comparison["equal"],
        "pixel_diff_count": comparison["mismatch_pixels"],
        "stream_verify_ok": verification["ok"],
        "report_mismatches": verification["report_mismatches"],
        "layer_stats": [
            {
                "layer_index": layer["layer_index"],
                "visible_fragment_samples": layer["visible_fragment_samples"],
                "average_transmittance": layer["average_transmittance"],
                "hole_count": layer["hole_count"],
                "soft_shadow_samples": layer["soft_shadow_samples"],
            }
            for layer in report["layers"]
        ],
        "penumbra_widths_mm": [
            probe["half_penumbra_width_mm"]
            for probe in report["penumbra_probes"]
        ],
    }
    atomic_write_json(output_root / "acceptance_summary.json", summary)
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0 if comparison["equal"] and verification["ok"] else 4


if __name__ == "__main__":
    raise SystemExit(main())
