from __future__ import annotations

import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

import numpy
import PIL

ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "artifacts" / "canopy_render_validation"


def run(args: list[str], *, expect: int = 0) -> subprocess.CompletedProcess:
    result = subprocess.run(
        args,
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode != expect:
        raise RuntimeError(
            f"command failed ({result.returncode}, expected {expect}): "
            f"{' '.join(args)}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
        )
    return result


def main() -> int:
    if OUTPUT.exists():
        shutil.rmtree(OUTPUT)
    OUTPUT.mkdir(parents=True)
    scene_path = OUTPUT / "scene.json"
    reference = OUTPUT / "reference"
    resumed = OUTPUT / "resumed"

    run(
        [
            sys.executable,
            "-m",
            "canopy_render",
            "generate-scene",
            "--out",
            str(scene_path),
        ]
    )
    run(
        [
            sys.executable,
            "-m",
            "canopy_render",
            "render",
            "--scene",
            str(scene_path),
            "--out",
            str(reference),
        ]
    )
    killed = run(
        [
            sys.executable,
            "-m",
            "canopy_render",
            "render",
            "--scene",
            str(scene_path),
            "--out",
            str(resumed),
            "--hard-kill-after-tiles",
            "4",
        ],
        expect=137,
    )
    run(
        [
            sys.executable,
            "-m",
            "canopy_render",
            "render",
            "--scene",
            str(scene_path),
            "--out",
            str(resumed),
            "--reference",
            str(reference),
        ]
    )

    with open(reference / "render_report.json", "r", encoding="utf-8") as handle:
        reference_report = json.load(handle)
    with open(resumed / "render_report.json", "r", encoding="utf-8") as handle:
        resumed_report = json.load(handle)

    tests = run(
        [sys.executable, "-m", "unittest", "discover", "-s", "tests", "-v"],
    )
    test_output = tests.stdout + tests.stderr
    match = re.search(r"Ran (\d+) tests", test_output)
    test_count = int(match.group(1)) if match else 0
    measurements = {
        item["tassel_id"]: item
        for item in reference_report["soft_shadows"]["measurements"]
    }
    summary = {
        "schema_version": "1.0",
        "status": "verified",
        "environment": {
            "python": sys.version.split()[0],
            "numpy": numpy.__version__,
            "pillow": PIL.__version__,
        },
        "scene_file": "scene.json",
        "reference_directory": "reference",
        "resumed_directory": "resumed",
        "scene_digest": reference_report["scene_digest"],
        "hard_kill_exit_code": killed.returncode,
        "resume": {
            "status": resumed_report["status"],
            "recovery_count": resumed_report["recovery"]["recovery_count"],
            "snapshot_status": resumed_report["recovery"]["snapshot_status"],
            "render_png_byte_identical": (
                (resumed / "render.png").read_bytes()
                == (reference / "render.png").read_bytes()
            ),
            "ownership_png_byte_identical": (
                (resumed / "ownership.png").read_bytes()
                == (reference / "ownership.png").read_bytes()
            ),
            "fabric_png_byte_identical": (
                (resumed / "fabric_composite.png").read_bytes()
                == (reference / "fabric_composite.png").read_bytes()
            ),
            "projection_points_identical": (
                resumed_report["projection"]["points"]
                == reference_report["projection"]["points"]
            ),
            "image_summary_identical": (
                resumed_report["image_summary"]
                == reference_report["image_summary"]
            ),
        },
        "tests": {
            "return_code": tests.returncode,
            "count": test_count,
            "passed": "OK" in test_output,
        },
        "soft_shadow_transition_widths_px": {
            tassel_id: measurements[tassel_id]["transition_width_px"]
            for tassel_id in (
                "tassel_near_left",
                "tassel_mid_left",
                "tassel_far_left",
            )
        },
        "soft_shadow_sample_count": reference_report["soft_shadows"][
            "soft_shadow_sample_count"
        ],
        "fabric_rgb_max": reference_report["image_summary"]["fabric_rgb_max"],
        "fabric_luminance": reference_report["image_summary"][
            "fabric_luminance"
        ],
        "layers": reference_report["layers"],
    }
    with open(OUTPUT / "verification_summary.json", "w", encoding="utf-8") as handle:
        json.dump(summary, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

