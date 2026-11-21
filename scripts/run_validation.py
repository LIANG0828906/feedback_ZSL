"""Run the local acceptance workflow and write a machine-readable report."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mcsync.atomic import atomic_write_json
from mcsync.errors import E_MARKER_MISSING
from mcsync.models import (
    CompensationConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
)
from mcsync.pipeline import SyncPipeline, export_result
from mcsync.recovery import (
    ChannelRange,
    RecoveryJournal,
    contiguous_ranges,
)
from mcsync.resample import (
    compare_interpolation_modes,
    resample_channel_range,
)
from mcsync.synth import (
    SyntheticConfig,
    generate_sample_bundle,
    generate_synthetic_audio,
)
from mcsync.versioning import load_report, save_report
from mcsync.wavio import read_multichannel_wav


def main() -> int:
    output = ROOT / "artifacts" / "validation"
    output.mkdir(parents=True, exist_ok=True)
    synthetic_config = SyntheticConfig(
        channels=8,
        duration_seconds=8.0,
        marker_period_seconds=0.5,
        seed=20261122,
    )
    _, paths = generate_sample_bundle(output / "sample", synthetic_config)
    audio = read_multichannel_wav(paths["wav"])
    config = PipelineConfig(
        marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
        segmentation=SegmentationConfig(segment_length_samples=48000),
        compensation=CompensationConfig(
            mode="linear", tolerance_samples=2.0
        ),
    )
    result = SyncPipeline(config).analyze(audio)
    mode_comparison = compare_interpolation_modes(
        audio,
        result.report.drift_fits,
        reference_channel=0,
        marker_config=config.marker,
        tolerance_samples=config.compensation.tolerance_samples,
    )
    exports = {}
    for version in ("v1", "v2", "v3"):
        exported = export_result(
            result,
            output / "exports" / f"compensated_{version}.wav",
            output / "exports" / f"report_{version}.json",
            target_version=version,
        )
        exports[version] = {
            "report_version": exported.report_version,
            "wav_sha256": exported.wav_sha256,
            "report_content_sha256": exported.report_content_sha256,
            "loss_fields": [
                field
                for issue in exported.losses
                for field in issue.fields
            ],
        }
    upgraded_v1 = load_report(output / "exports" / "report_v1.json")
    upgraded_v2 = load_report(output / "exports" / "report_v2.json")
    upgraded_v3 = load_report(output / "exports" / "report_v3.json")
    save_report(
        upgraded_v1,
        output / "exports" / "roundtrip_v3_from_v1.json",
        "v3",
    )

    recovery_root = output / "recovery"
    journal = RecoveryJournal(recovery_root, result.report)
    fits = {fit.channel: fit for fit in result.report.drift_fits}
    output_length = result.compensated_audio.frame_count
    ranges = contiguous_ranges(
        range(8),
        output_length=output_length,
        chunk_length=96000,
    )

    def compensate(channel_range: ChannelRange) -> np.ndarray:
        return resample_channel_range(
            audio.channel(channel_range.channel),
            fits[channel_range.channel],
            mode="linear",
            output_start=channel_range.start_sample,
            output_end=channel_range.end_sample,
        )

    for channel_range in ranges[:6]:
        journal.commit_range(channel_range, compensate(channel_range))
    restarted = RecoveryJournal(recovery_root, result.report)
    resumed = restarted.resume(
        ranges,
        channels=range(8),
        output_length=output_length,
        compensate_range=compensate,
        trace_path=output / "recovery" / "trace.json",
    )
    recovery_match = bool(
        np.array_equal(resumed.matrix, result.compensated_audio.samples)
    )

    first_export = export_result(
        result,
        output / "determinism_a" / "compensated.wav",
        output / "determinism_a" / "report_v3.json",
        target_version="v3",
    )
    second_result = SyncPipeline(config).analyze(audio)
    second_export = export_result(
        second_result,
        output / "determinism_b" / "compensated.wav",
        output / "determinism_b" / "report_v3.json",
        target_version="v3",
    )
    deterministic = (
        Path(first_export.wav_path).read_bytes()
        == Path(second_export.wav_path).read_bytes()
        and Path(first_export.report_path).read_bytes()
        == Path(second_export.report_path).read_bytes()
    )

    missing_sample = generate_synthetic_audio(
        SyntheticConfig(
            channels=2,
            duration_seconds=2.0,
            missing_markers={1: [2]},
        )
    )
    missing_result = SyncPipeline(
        PipelineConfig(
            active_channels=(0, 1),
            marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
            segmentation=SegmentationConfig(segment_length_samples=48000),
            compensation=CompensationConfig(mode="linear"),
        )
    ).analyze(missing_sample.audio)
    missing_issues = [
        issue.to_dict()
        for issue in missing_result.issues
        if issue.code == E_MARKER_MISSING
    ]

    event_sample = generate_synthetic_audio(
        SyntheticConfig(
            channels=2,
            duration_seconds=5.0,
            drift_ppm=(0.0, 0.0),
            initial_offset_samples=(0.0, 0.0),
            event_jumps={1: [{"time_s": 2.0, "jump_samples": 120.0}]},
        )
    )
    event_result = SyncPipeline(
        PipelineConfig(
            active_channels=(0, 1),
            marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
            segmentation=SegmentationConfig(segment_length_samples=48000),
            compensation=CompensationConfig(mode="linear"),
        )
    ).analyze(event_sample.audio)

    fits_truth = {
        str(fit.channel): {
            "estimated_drift_ppm": fit.drift_ppm,
            "known_drift_ppm": synthetic_config.drift_ppm[fit.channel],
            "error_ppm": fit.drift_ppm
            - synthetic_config.drift_ppm[fit.channel],
            "residual_rms_samples": fit.residual_rms,
        }
        for fit in result.report.drift_fits
    }
    summary = {
        "schema_version": "validation-report-v1",
        "sample": {
            "seed": synthetic_config.seed,
            "channels": audio.channel_count,
            "frames": audio.frame_count,
            "sample_rate": audio.sample_rate,
            "duration_seconds": audio.duration_seconds,
        },
        "markers": {
            "detected": len(result.marker_detection.markers),
            "expected_per_channel": result.marker_detection.channels[0].expected_count,
            "issues": [issue.to_dict() for issue in result.marker_detection.issues],
        },
        "drift_fit": {
            "channels": fits_truth,
            "maximum_abs_ppm_error": max(
                abs(item["error_ppm"]) for item in fits_truth.values()
            ),
        },
        "diagnosis": [
            {
                "channel": item.channel,
                "conclusion": item.conclusion,
                "confidence": item.confidence,
                "evidence_segments": list(item.evidence_segments),
                "ci_ppm": [item.ci_low_ppm, item.ci_high_ppm],
            }
            for item in result.report.diagnoses
        ],
        "compensation": {
            "mode": result.report.compensation.mode,
            "max_abs_delay_samples": result.residual_summary.max_abs_delay_samples,
            "rms_samples": result.residual_summary.rms_samples,
            "worst_channel": result.residual_summary.worst_channel,
            "tolerance_samples": result.report.compensation.tolerance_samples,
        },
        "interpolation_comparison": mode_comparison["comparison"],
        "exports": exports,
        "version_upgrade": {
            "v1_source": upgraded_v1.source_version,
            "v1_provenance": upgraded_v1.provenance,
            "v2_source": upgraded_v2.source_version,
            "v2_provenance": upgraded_v2.provenance,
            "v3_hash_valid": (
                upgraded_v3.content_hash()
                == upgraded_v3.integrity["content_sha256"]
            ),
        },
        "recovery": {
            "committed_before_restart": 6,
            "skipped_on_resume": len(resumed.skipped_committed_ranges),
            "written_on_resume": len(resumed.written_ranges),
            "last_sequence": resumed.state.last_sequence,
            "last_hash": resumed.state.last_hash,
            "matrix_matches_direct_compensation": recovery_match,
        },
        "determinism": {
            "wav_and_report_bytes_identical": deterministic,
        },
        "missing_marker": {
            "issues": missing_issues,
        },
        "event_jump": {
            "channel": event_result.report.diagnoses[1].channel,
            "conclusion": event_result.report.diagnoses[1].conclusion,
            "jump_segments": list(
                event_result.report.diagnoses[1].jump_segments
            ),
        },
    }
    atomic_write_json(output / "validation_summary.json", summary)
    sys.stdout.write(
        json.dumps(summary, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

