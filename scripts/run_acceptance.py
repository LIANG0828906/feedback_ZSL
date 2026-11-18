from __future__ import annotations

import json
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from bamboo_render.compare import compare_outputs
from bamboo_render.engine import run_render
from bamboo_render.io_utils import sha256_file, write_json
from bamboo_render.sample_scene import write_sample_scene
from bamboo_render.scene import load_scene


def main() -> int:
    artifacts = ROOT / "artifacts" / "acceptance"
    artifacts.mkdir(parents=True, exist_ok=True)
    scene_file = artifacts / "scene.json"
    write_sample_scene(scene_file)
    loaded = load_scene(scene_file)
    full_dir = artifacts / "full"
    resumed_dir = artifacts / "resumed"
    for directory in (full_dir, resumed_dir):
        if directory.exists():
            shutil.rmtree(directory)

    full = run_render(loaded, full_dir)
    interrupted = run_render(loaded, resumed_dir, interrupt_after=7)
    resumed = run_render(loaded, resumed_dir)
    compare_ok, compare_report = compare_outputs(
        full_dir,
        resumed_dir,
        artifacts / "compare.json",
    )

    projection = json.loads(
        (full_dir / "projection_report.json").read_text(encoding="utf-8")
    )
    run_report = json.loads(
        (full_dir / "run_report.json").read_text(encoding="utf-8")
    )
    recovery = json.loads(
        (resumed_dir / "recovery_report.json").read_text(encoding="utf-8")
    )
    summary = {
        "schema_version": 1,
        "ok": compare_ok
        and full.status == "complete"
        and interrupted.status == "interrupted"
        and resumed.status == "complete",
        "scene_digest": loaded.scene_digest,
        "full_render": {
            "status": full.status,
            "blocks_completed": full.completed_blocks,
            "blocks_recomputed": full.recomputed_blocks,
        },
        "interrupted_render": {
            "status": interrupted.status,
            "blocks_completed": interrupted.completed_blocks,
            "recovery_exit_code": 0,
        },
        "resumed_render": {
            "status": resumed.status,
            "blocks_reused": resumed.reused_blocks,
            "blocks_recomputed": resumed.recomputed_blocks,
            "recovery_status": recovery["status"],
        },
        "compare": {
            "ok": compare_ok,
            "difference_count": compare_report["difference_count"],
            "pixel_difference_count": compare_report["pixel_difference_count"],
        },
        "image_checks": run_report["image_checks"],
        "layer_energy": [
            {
                "layer": layer["layer"],
                "transmission_share": layer["transmission_share"],
                "reflection_share": layer["reflection_share"],
                "scatter_share": layer["scatter_share"],
                "caustic_share": layer["caustic_share"],
            }
            for layer in run_report["layers"]
        ],
        "projection": {
            "sampling_density": projection["sampling_density"],
            "sample_count": projection["sample_count"],
            "edge_widths": [
                {
                    "pair": pair["pair"],
                    "gap": pair["gap"],
                    "edge_width_world": pair["edge_width_world"],
                    "edge_width_pixels": pair["edge_width_pixels"],
                }
                for pair in projection["pairs"]
            ],
            "grayscale_sha256": projection["grayscale_sha256"],
        },
        "required_equal_files": {
            "rgba": sha256_file(full_dir / "rgba.npy")
            == sha256_file(resumed_dir / "rgba.npy"),
            "ownership": sha256_file(full_dir / "ownership.npy")
            == sha256_file(resumed_dir / "ownership.npy"),
            "layer_premultiplied": sha256_file(full_dir / "layer_premultiplied.npy")
            == sha256_file(resumed_dir / "layer_premultiplied.npy"),
            "projection_points": sha256_file(full_dir / "projection_points.json")
            == sha256_file(resumed_dir / "projection_points.json"),
            "snapshot": sha256_file(full_dir / "snapshot.json")
            == sha256_file(resumed_dir / "snapshot.json"),
            "run_report": sha256_file(full_dir / "run_report.json")
            == sha256_file(resumed_dir / "run_report.json"),
        },
        "outputs": {
            "scene": "scene.json",
            "rgba": "full/rgba.png",
            "ownership": "full/ownership.png",
            "projection_combined": "full/projection/projection_combined.png",
            "full_report": "full/run_report.json",
            "projection_report": "full/projection_report.json",
            "recovery_report": "resumed/recovery_report.json",
            "compare_report": "compare.json",
        },
    }
    summary["ok"] = bool(summary["ok"] and all(summary["required_equal_files"].values()))
    write_json(artifacts / "summary.json", summary)
    print(json.dumps(summary, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    return 0 if summary["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
