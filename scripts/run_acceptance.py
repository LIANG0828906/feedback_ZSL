#!/usr/bin/env python3
"""Reproducible acceptance run using only local synthetic material."""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from audio_sync.models import SyncConfig  # noqa: E402
from audio_sync.pipeline import run_pipeline  # noqa: E402
from audio_sync.store import ResultStore  # noqa: E402
from audio_sync.synthesizer import synthesize_dataset  # noqa: E402
from audio_sync.wavio import WavReader, hash_file, write_json_atomic  # noqa: E402


def _paths(snapshot):
    return {
        "version": snapshot.version,
        "content_hash": snapshot.content_hash,
        "aligned_wav": snapshot.payload["outputs"]["aligned_wav"],
        "heatmap_npz": snapshot.payload["outputs"]["residual_heatmap_npz"],
        "summary_json": snapshot.payload["outputs"]["run_summary_json"],
    }


def _check(condition: bool, label: str, failures: list[str], details=None) -> dict:
    item = {
        "check": label,
        "passed": bool(condition),
        "details": details,
    }
    if not condition:
        failures.append(label)
    return item


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default=str(ROOT / "artifacts" / "acceptance"))
    parser.add_argument("--duration", type=float, default=24.0)
    parser.add_argument("--seed", type=int, default=20261122)
    args = parser.parse_args(argv)

    root = Path(args.out).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    sample = synthesize_dataset(
        root / "samples" / f"linear-{stamp}",
        duration_seconds=args.duration,
        channels=8,
        seed=args.seed,
        missing_markers={6: [13]},
    )
    sample_payload = json.loads(Path(sample["markers"]).read_text(encoding="utf-8"))
    truth = sample_payload["ground_truth"]
    store = ResultStore(root)
    failures: list[str] = []
    checks: list[dict] = []

    linear = store.publish(
        run_pipeline(
            sample["wav"],
            sample["markers"],
            root,
            SyncConfig(segment_seconds=2.0, interpolation="linear"),
            run_id=f"linear-{stamp}",
        )
    )
    linear_payload = linear.payload
    segments = linear_payload["strategy"]["segment_count"]
    missing = [
        error
        for error in linear_payload["errors"]
        if error["code"] == "E_MARKER_MISSING"
    ]
    checks.append(
        _check(
            any(
                error["details"].get("channel") == 6
                and error["details"].get("marker_index") == 13
                for error in missing
            ),
            "missing marker is reported with channel and marker index",
            failures,
            missing,
        )
    )
    checks.append(
        _check(
            linear_payload["strategy"]["selected"] == "segmented",
            "8-channel auto strategy selects segmented estimation",
            failures,
            linear_payload["strategy"],
        )
    )
    linear_models = {
        item["channel"]: item for item in linear_payload["drift"]["channels"]
    }
    ppm_errors = {
        index: abs(linear_models[index]["drift_ppm"] - truth["drift_ppm"][index])
        for index in range(1, 8)
    }
    checks.append(
        _check(
            all(model["model_kind"] == "linear_drift" for model in linear_models.values() if model["channel"] != 0)
            and max(ppm_errors.values()) < 1.0,
            "first-order drift is classified as linear and matches injected ppm",
            failures,
            {"ppm_errors": ppm_errors, "models": linear_models},
        )
    )
    checks.append(
        _check(
            linear_payload["compensation"]["tolerance_ok"]
            and linear_payload["diagnostics"]["measured_worst_residual_samples"]
            <= linear_payload["compensation"]["tolerance_samples"],
            "linear interpolation compensation remains inside tolerance",
            failures,
            linear_payload["diagnostics"],
        )
    )

    warm = store.publish(
        run_pipeline(
            sample["wav"],
            sample["markers"],
            root,
            SyncConfig(segment_seconds=2.0, interpolation="linear"),
            previous_payload=linear_payload,
            run_id=f"linear-warm-{stamp}",
        )
    )
    warm_payload = warm.payload
    warm_stats = warm_payload["cache"]["stats"]
    checks.append(
        _check(
            warm_stats["marker_hits"] == 8
            and warm_stats["delay_hits"] == 8 * segments
            and warm_stats["fit_hits"] == 8
            and warm_stats["segment_hits"] == 8 * segments,
            "unchanged second run reuses markers, delays, fits and every compensated segment",
            failures,
            warm_stats,
        )
    )
    same_hashes = all(
        left["segment_hashes"] == right["segment_hashes"]
        for left, right in zip(
            linear_payload["compensation"]["channels"],
            warm_payload["compensation"]["channels"],
        )
    )
    checks.append(
        _check(
            same_hashes,
            "cached segment content hashes are identical after reuse",
            failures,
        )
    )

    sinc = store.publish(
        run_pipeline(
            sample["wav"],
            sample["markers"],
            root,
            SyncConfig(segment_seconds=2.0, interpolation="windowed_sinc"),
            previous_payload=warm_payload,
            run_id=f"sinc-{stamp}",
        )
    )
    sinc_payload = sinc.payload
    comparison = sinc_payload["compensation"]["interpolation_comparison"]
    checks.append(
        _check(
            sinc_payload["compensation"]["tolerance_ok"]
            and comparison["available"]
            and comparison["previous_mode"] == "linear"
            and comparison["current_mode"] == "windowed_sinc",
            "windowed-sinc mode emits a two-mode residual comparison",
            failures,
            comparison,
        )
    )

    full = store.publish(
        run_pipeline(
            sample["wav"],
            sample["markers"],
            root,
            SyncConfig(strategy="full", interpolation="linear"),
            previous_payload=sinc_payload,
            run_id=f"full-{stamp}",
        )
    )
    full_payload = full.payload
    strategy_invariants = {
        "same_total_samples": (
            full_payload["compensation"]["total_samples"]
            == linear_payload["compensation"]["total_samples"]
        ),
        "same_marker_signature": (
            full_payload["marker_detection"]["marker_description"]["signature"]
            == linear_payload["marker_detection"]["marker_description"]["signature"]
        ),
        "full_worst_residual": full_payload["diagnostics"][
            "measured_worst_residual_samples"
        ],
        "segmented_worst_residual": linear_payload["diagnostics"][
            "measured_worst_residual_samples"
        ],
    }
    checks.append(
        _check(
            strategy_invariants["same_total_samples"]
            and strategy_invariants["same_marker_signature"]
            and strategy_invariants["full_worst_residual"]
            > strategy_invariants["segmented_worst_residual"],
            "full and segmented strategies preserve material and are numerically comparable",
            failures,
            strategy_invariants,
        )
    )

    step_sample = synthesize_dataset(
        root / "samples" / f"step-{stamp}",
        duration_seconds=16.0,
        channels=4,
        seed=args.seed + 1,
        step_samples={1: 12.0},
    )
    step = store.publish(
        run_pipeline(
            step_sample["wav"],
            step_sample["markers"],
            root,
            SyncConfig(strategy="segmented", segment_seconds=2.0),
            run_id=f"step-{stamp}",
        )
    )
    step_payload = step.payload
    step_models = {
        item["channel"]: item for item in step_payload["drift"]["channels"]
    }
    checks.append(
        _check(
            step_models[1]["model_kind"] == "step_jump"
            and abs(step_models[1]["step_samples"] - 12.0) < 1.0
            and step_models[2]["model_kind"] == "linear_drift"
            and step_payload["compensation"]["tolerance_ok"],
            "step jump is separated from continuous linear drift",
            failures,
            {
                "channel_1": step_models[1],
                "channel_2": step_models[2],
                "diagnostics": step_payload["diagnostics"],
            },
        )
    )

    with WavReader(linear_payload["outputs"]["aligned_wav"]) as aligned:
        aligned_frames = aligned.frames
        aligned_channels = aligned.channels
    with np.load(
        linear_payload["outputs"]["residual_heatmap_npz"],
        allow_pickle=False,
    ) as heatmap:
        heat_shape = list(heatmap["after_rms"].shape)
    checks.append(
        _check(
            aligned_frames == linear_payload["input"]["frames"]
            and aligned_channels == 8
            and heat_shape == [8, segments],
            "aligned multichannel WAV and residual heatmap dimensions are stable",
            failures,
            {"frames": aligned_frames, "channels": aligned_channels, "heatmap": heat_shape},
        )
    )

    result = {
        "status": "PASS" if not failures else "FAIL",
        "failed_checks": failures,
        "checks": checks,
        "sample_manifest": sample,
        "step_sample_manifest": step_sample,
        "reports": {
            "linear": _paths(linear),
            "linear_warm": _paths(warm),
            "windowed_sinc": _paths(sinc),
            "full": _paths(full),
            "step": _paths(step),
        },
        "metrics": {
            "linear_worst_residual_samples": linear_payload["diagnostics"][
                "measured_worst_residual_samples"
            ],
            "sinc_worst_residual_samples": sinc_payload["diagnostics"][
                "measured_worst_residual_samples"
            ],
            "full_worst_residual_samples": full_payload["diagnostics"][
                "measured_worst_residual_samples"
            ],
            "linear_aligned_sha256": hash_file(
                linear_payload["outputs"]["aligned_wav"]
            ),
            "sinc_aligned_sha256": hash_file(sinc_payload["outputs"]["aligned_wav"]),
            "cache_warm_run": warm_stats,
            "strategy": linear_payload["strategy"],
            "step_model": step_models[1],
        },
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
    }
    report_path = root / "acceptance_report.json"
    write_json_atomic(report_path, result)
    print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    print(f"\nacceptance report: {report_path}")
    return 0 if not failures else 1


if __name__ == "__main__":
    sys.exit(main())
