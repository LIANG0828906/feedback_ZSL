#!/usr/bin/env python3
"""Reproduce the main acceptance facts on local deterministic samples."""

from __future__ import annotations

import hashlib
import argparse
import json
import platform
import sys
import tempfile
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender import (  # noqa: E402
    E_LIGHT_DRIFT,
    E_MODE_INVALID,
    E_SNAPSHOT_INVALID,
    E_SNAPSHOT_STALE,
    E_TF_INVALID,
    TransferFunction,
    VolumeData,
    VolumeRenderer,
    VolRenderError,
    load_snapshot,
)


def caught_code(action) -> str:
    try:
        action()
        return "OK"
    except VolRenderError as exc:
        return exc.code


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=ROOT / "artifacts" / "verification.json")
    args = parser.parse_args(argv)
    artifacts = ROOT / "artifacts"
    artifacts.mkdir(exist_ok=True)
    cases = json.loads((ROOT / "cases" / "cases.json").read_text(encoding="utf-8"))
    points = next(
        case["points"]
        for case in cases["transfer_functions"]
        if case["name"] == "medical_default"
    )
    view = np.asarray(cases["views"][0]["matrix"], dtype=np.float64)
    volume = VolumeData.from_file(ROOT / "samples" / "density_64.vol")
    mask = VolumeData.from_file(ROOT / "samples" / "segmentation_64.vol")
    transfer = TransferFunction(points)
    renderer = VolumeRenderer(
        volume=volume,
        transfer=transfer,
        mode="float",
        view_matrix=view,
        step=1.0,
        image_size=(80, 80),
        error_threshold=1.0e-3,
    )
    first = renderer.render()
    second = renderer.render()
    drift = renderer.audit_lighting_drift(raise_on_fail=False)
    mode_report = renderer.set_precision("fixed")

    with tempfile.TemporaryDirectory() as tmp:
        snapshot_path = Path(tmp) / "float.volsnap.json"
        renderer.mode = "float"
        renderer._lut = transfer.build_lut("float")
        float_result = renderer.render()
        renderer.save_snapshot(snapshot_path, result=float_result)
        snapshot = load_snapshot(snapshot_path)
        renderer.set_step(2.0)
        renderer.restore_snapshot(snapshot)
        resumed = renderer.resume_render()
        snapshot_roundtrip = resumed.content_hash == float_result.content_hash

    fixture_codes = {}
    for case in cases["snapshot_cases"]:
        fixture_codes[case["name"]] = caught_code(
            lambda case=case: load_snapshot(ROOT / "cases" / case["file"])
        )

    report = {
        "platform": {
            "python": platform.python_version(),
            "numpy": np.__version__,
            "machine": platform.machine(),
            "platform": platform.platform(),
        },
        "samples": {
            "density_sha256": volume.file_sha256,
            "density_shape_xyz": list(volume.shape_xyz),
            "mask_sha256": mask.file_sha256,
            "mask_shape_xyz": list(mask.shape_xyz),
            "mask_labels": sorted(int(v) for v in np.unique(mask.raw)),
            "normalization_reference": volume.normalization_reference.as_dict(),
        },
        "determinism": {
            "byte_identical": bool(np.array_equal(first.rgba8, second.rgba8)),
            "content_hash": first.content_hash,
            "second_content_hash": second.content_hash,
            "state_hash": first.state_hash,
        },
        "float_render": first.as_dict(),
        "fixed_switch": {
            "mode": mode_report.mode,
            "comparison": mode_report.comparison.as_dict(),
            "result_content_hash": mode_report.result.content_hash,
            "result_error_summary": mode_report.result.error_summary.as_dict(),
        },
        "lighting_drift": drift.as_dict(),
        "snapshot_roundtrip": {
            "content_hash_matches_after_resume": snapshot_roundtrip,
            "fixture_codes": fixture_codes,
        },
        "error_codes": {
            "invalid_tf": caught_code(
                lambda: TransferFunction(
                    [(0.0, 0.0, 0, 0, 0), (0.8, 0.1, 0.5, 0.5, 0.5), (0.5, 0.2, 1, 1, 1)]
                )
            ),
            "invalid_mode": caught_code(
                lambda: VolumeRenderer(
                    volume=volume,
                    transfer=transfer,
                    mode="int8",
                )
            ),
            "lighting_drift": caught_code(
                lambda: renderer.audit_lighting_drift(tolerance=1.0e-12)
            ),
            "snapshot_invalid": fixture_codes["corrupt_content"],
            "snapshot_stale": fixture_codes["stale_view"],
        },
    }
    expected = {
        "invalid_tf": E_TF_INVALID,
        "invalid_mode": E_MODE_INVALID,
        "lighting_drift": E_LIGHT_DRIFT,
        "snapshot_invalid": E_SNAPSHOT_INVALID,
        "snapshot_stale": E_SNAPSHOT_STALE,
    }
    report["error_codes_match"] = {
        key: report["error_codes"][key] == code for key, code in expected.items()
    }
    report_path = args.output
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(report, indent=2, sort_keys=True))

    ok = (
        report["determinism"]["byte_identical"]
        and snapshot_roundtrip
        and drift.passed
        and all(report["error_codes_match"].values())
    )
    return 0 if ok else 5


if __name__ == "__main__":
    raise SystemExit(main())
