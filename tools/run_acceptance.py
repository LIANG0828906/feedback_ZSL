#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mastering.engine import master_project
from mastering.fixture import generate_fixture
from mastering.limiter import true_peak_dbtp
from mastering.loudness import integrated_loudness
from mastering.wav import read_wav, read_wav_header


def _normalized_report(report: dict[str, Any]) -> dict[str, Any]:
    normalized = json.loads(json.dumps(report, sort_keys=True))
    normalized["wav"].pop("path", None)
    normalized["project"].pop("manifest", None)
    normalized["project"].pop("automation", None)
    normalized["project"].pop("output_config", None)
    return normalized


def main() -> int:
    parser = argparse.ArgumentParser(description="Run deterministic mastering acceptance")
    parser.add_argument("--workdir", default=str(ROOT / "outputs" / "acceptance"))
    parser.add_argument("--duration", type=float, default=180.0)
    parser.add_argument("--sample-rate", type=int, default=48000)
    parser.add_argument("--format", default="pcm_s24")
    parser.add_argument("--target-lufs", type=float, default=-14.0)
    parser.add_argument("--true-peak-dbtp", type=float, default=-1.0)
    parser.add_argument(
        "--summary",
        default=str(ROOT / "artifacts" / "acceptance_summary.json"),
    )
    parser.add_argument("--force-fixture", action="store_true")
    parser.add_argument("--no-repeat", action="store_true")
    args = parser.parse_args()

    workdir = Path(args.workdir).resolve()
    fixture_dir = workdir / "fixture"
    manifest = fixture_dir / "manifest.json"
    if args.force_fixture or not manifest.is_file():
        if fixture_dir.exists():
            shutil.rmtree(fixture_dir)
        generate_fixture(
            fixture_dir,
            duration_seconds=args.duration,
            output_sample_rate=args.sample_rate,
            output_format=args.format,
            target_lufs=args.target_lufs,
            true_peak_dbtp=args.true_peak_dbtp,
        )

    workdir.mkdir(parents=True, exist_ok=True)
    master_path = workdir / "master.wav"
    report_path = workdir / "report.json"
    report = master_project(
        manifest,
        fixture_dir / "automation.json",
        fixture_dir / "output_config.json",
        master_path,
        report_path,
    )

    repeat_master = workdir / "master_repeat.wav"
    repeat_report_path = workdir / "report_repeat.json"
    repeat_report: dict[str, Any] | None = None
    if not args.no_repeat:
        repeat_report = master_project(
            manifest,
            fixture_dir / "automation.json",
            fixture_dir / "output_config.json",
            repeat_master,
            repeat_report_path,
        )

    header = read_wav_header(master_path)
    decoded = read_wav(master_path).samples
    decoded_dbtp, _ = true_peak_dbtp(decoded, 4)
    decoded_loudness = integrated_loudness(decoded, args.sample_rate).integrated_lufs
    expected_frames = int(round(args.duration * args.sample_rate))
    tracks = report["tracks"]
    drifting = [track for track in tracks if track["drift"]["enabled"]]
    sync_ok = all(
        track["sync"]["detected"]
        and track["sync"]["aligned"]
        and track["sync"]["alignment_residual_samples"] <= 1
        for track in tracks
    )
    drift_ok = all(
        track["drift"]["verification"]["within_two_output_samples"]
        for track in drifting
    )
    header_ok = (
        header.sample_rate == args.sample_rate
        and header.frame_count == expected_frames
        and header.channels == report["project"]["channels"]
        and header.bits_per_sample == report["wav"]["bits_per_sample"]
        and header.data_bytes
        == expected_frames
        * header.channels
        * (header.bits_per_sample // 8)
    )
    loudness_deviation = abs(
        report["loudness"]["calibration"]["deviation_lu"]
    ) if report["loudness"]["calibration"] else None
    final_loudness_deviation = abs(decoded_loudness - args.target_lufs)
    deterministic = True
    if repeat_report is not None:
        deterministic = (
            repeat_master.read_bytes() == master_path.read_bytes()
            and _normalized_report(repeat_report) == _normalized_report(report)
        )

    lead_vocal = next(track for track in tracks if track["id"] == "lead_vocal")
    automation_ok = (
        sum(track["automation"]["sharp_segments_smoothed"] for track in tracks) >= 5
        and lead_vocal["automation"]["sharp_segments_smoothed"] == 2
    )
    checks = {
        "project_succeeded": bool(report["success"]),
        "eleven_tracks_present": len(tracks) == 11,
        "output_frames_exact": header.frame_count == expected_frames,
        "wav_header_self_consistent": header_ok,
        "all_sync_aligned_within_one_sample": sync_ok,
        "all_drift_anchors_within_two_samples": drift_ok,
        "sharp_automation_segments_smoothed": automation_ok,
        "loudness_calibration_within_half_lu": (
            loudness_deviation is not None and loudness_deviation <= 0.5
        ),
        "final_loudness_within_half_lu": final_loudness_deviation <= 0.5,
        "final_true_peak_within_ceiling": decoded_dbtp <= args.true_peak_dbtp,
        "limiter_triggered": report["limiter"]["triggered_frames"] > 0,
        "repeat_run_byte_identical": deterministic,
    }
    summary = {
        "success": all(checks.values()),
        "checks": checks,
        "inputs": {
            "fixture": str(fixture_dir),
            "duration_seconds": args.duration,
            "track_count": len(tracks),
            "source_rates_hz": sorted(
                {track["actual_header"]["sample_rate"] for track in tracks}
            ),
            "source_depths": sorted(
                {track["actual_header"]["bits_per_sample"] for track in tracks}
            ),
        },
        "output": {
            "master": str(master_path),
            "report": str(report_path),
            "format": report["project"]["format"],
            "sample_rate": header.sample_rate,
            "channels": header.channels,
            "bits_per_sample": header.bits_per_sample,
            "frames": header.frame_count,
            "duration_seconds": header.frame_count / header.sample_rate,
            "data_bytes": header.data_bytes,
            "sha256": report["wav"]["sha256"],
        },
        "measurements": {
            "sync_max_residual_samples": max(
                track["sync"]["alignment_residual_samples"] for track in tracks
            ),
            "drift_anchor_max_error_samples": max(
                track["drift"]["verification"]["max_mapping_error_output_samples"]
                for track in drifting
            ),
            "smoothed_segment_count": sum(
                track["automation"]["sharp_segments_smoothed"] for track in tracks
            ),
            "total_smoothing_seconds": round(
                sum(
                    track["automation"]["total_smoothing_seconds"] for track in tracks
                ),
                9,
            ),
            "loudness_target_lufs": args.target_lufs,
            "pre_limiter_loudness_deviation_lu": loudness_deviation,
            "final_loudness_lufs": round(decoded_loudness, 9),
            "final_loudness_deviation_lu": round(final_loudness_deviation, 9),
            "limiter_triggered_frames": report["limiter"]["triggered_frames"],
            "limiter_max_attenuation_db": report["limiter"]["max_attenuation_db"],
            "final_true_peak_dbtp": round(decoded_dbtp, 9),
            "true_peak_ceiling_dbtp": args.true_peak_dbtp,
        },
        "report_verification": report["verification"],
    }
    if repeat_report is not None:
        summary["determinism"] = {
            "second_master": str(repeat_master),
            "second_report": str(repeat_report_path),
            "byte_identical": deterministic,
        }
        repeat_master.unlink()
        repeat_report_path.unlink()

    summary_path = Path(args.summary).resolve()
    summary_path.parent.mkdir(parents=True, exist_ok=True)
    summary_path.write_text(
        json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
    return 0 if summary["success"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

