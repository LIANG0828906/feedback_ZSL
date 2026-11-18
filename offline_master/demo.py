"""Deterministic sample project generator and command-line entry point."""

from __future__ import annotations

import argparse
import json
import os
import sys

from .codes import BusinessError
from .pipeline import render_project
from .synthesis import synthesize_project


def build_manifest() -> dict:
    return {
        "project": "turntable_offline_master_demo",
        "sample_rate": 48000,
        "bit_depth": 16,
        "channels": 1,
        "duration_s": 4.5,
        "frame_s": 0.1,
        "block_frames": 5,
        "stability_threshold_db": 0.5,
        "local_coordinate_bounds": {
            "x": [-1.0, 1.0],
            "y": [0.0, 2.0],
            "z": [-1.0, 1.0],
        },
        "turntable_angles_deg": list(range(0, 360, 10)),
        "lip_sync_points_s": [0.10, 0.82, 1.54, 2.26, 2.98, 3.70],
        "strong_section_s": [1.2, 3.6],
        "downmix": {
            "rule": "analytic_binaural_itd_ild",
            "output_channels": 2,
        },
        "mastering": {
            "integrated_lufs": -16.0,
            "true_peak_db": -1.0,
        },
        "tracks": [
            {
                "file": "stems/dialogue.wav",
                "material_type": "dialogue",
                "seed": 101,
                "anchor": [0.20, 1.40, 0.00],
                "start_offset_s": 0.030,
                "sync_ref_s": 0.10,
            },
            {
                "file": "stems/footstep.wav",
                "material_type": "footstep",
                "seed": 202,
                "anchor": [0.15, 0.10, 0.05],
                "start_offset_s": 0.050,
                "phase_group": "rhythm",
                "sync_ref_s": 0.82,
            },
            {
                "file": "stems/cloth.wav",
                "material_type": "cloth",
                "seed": 303,
                "anchor": [0.10, 1.20, 0.10],
                "start_offset_s": 0.050,
                "phase_group": "rhythm",
                "sync_ref_s": 0.82,
                "phase_inject": {
                    "polarity": -1,
                    "delay_samples": 0.0,
                },
            },
            {
                "file": "stems/ambient.wav",
                "material_type": "ambient",
                "seed": 404,
                "anchor": [0.00, 1.00, 0.00],
                "start_offset_s": 0.020,
            },
        ],
    }


def init_project(project_dir: str) -> str:
    os.makedirs(project_dir, exist_ok=True)
    manifest = build_manifest()
    manifest_path = os.path.join(project_dir, "manifest.json")
    with open(manifest_path, "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, sort_keys=True)
    synthesize_project(manifest, project_dir)
    return manifest_path


def build_invalid_manifest(project_dir: str) -> str:
    """Manifest exercising every validation warning category at once."""
    good = build_manifest()
    good["sample_rate"] = 22050  # mismatch against synthesized 48k WAVs
    good["turntable_angles_deg"] = [0, 0, 10]
    good["lip_sync_points_s"] = [0.5, 0.4]
    good["tracks"][0]["anchor"] = [5.0, 9.0, 5.0]
    good["tracks"][1]["file"] = good["tracks"][0]["file"]  # duplicate path
    good["tracks"][2]["material_type"] = "dialogue"  # duplicate type
    good["tracks"].append(
        {
            "file": "stems/missing.wav",
            "material_type": "unknown_type",
            "seed": 9,
            "anchor": [0.0, 1.0, 0.0],
            "start_offset_s": 0.0,
        }
    )
    path = os.path.join(project_dir, "manifest_invalid.json")
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(good, handle, indent=2, sort_keys=True)
    return path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Offline turntable mastering renderer")
    sub = parser.add_subparsers(dest="command", required=True)

    init_parser = sub.add_parser("init", help="create deterministic sample project")
    init_parser.add_argument("project_dir")

    render_parser = sub.add_parser("render", help="render a manifest to stereo master")
    render_parser.add_argument("manifest")
    render_parser.add_argument("--out", required=True)
    render_parser.add_argument("--block-sleep", type=float, default=0.0)

    invalid_parser = sub.add_parser(
        "invalid-demo", help="synthesize good stems then validate a broken manifest"
    )
    invalid_parser.add_argument("project_dir")

    args = parser.parse_args(argv)
    if args.command == "init":
        path = init_project(args.project_dir)
        print(f"initialized project with manifest: {path}")
        return 0
    if args.command == "render":
        try:
            report = render_project(args.manifest, args.out, block_sleep_s=args.block_sleep)
        except BusinessError as exc:
            print(json.dumps({"code": exc.code, "warnings": exc.warnings}, indent=2))
            return {"E_MANIFEST_INVALID": 2, "E_RESUME_MISMATCH": 3}.get(exc.code, 4)
        print(json.dumps(report, indent=2, sort_keys=True))
        return 0
    if args.command == "invalid-demo":
        init_project(args.project_dir)
        invalid_path = build_invalid_manifest(args.project_dir)
        try:
            render_project(invalid_path, os.path.join(args.project_dir, "bad_out"))
        except BusinessError as exc:
            print(json.dumps({"code": exc.code, "warnings": exc.warnings}, indent=2))
            return 2
        print("ERROR: invalid manifest was not rejected", file=sys.stderr)
        return 5
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
