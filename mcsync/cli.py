"""Command-line entry points for the synchronization workbench."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .atomic import atomic_write_bytes, atomic_write_json
from .errors import E_MARKER_MISSING, SyncError
from .incremental import IncrementalSyncEngine
from .models import (
    CompensationConfig,
    DiagnosisConfig,
    FitConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
)
from .pipeline import SyncPipeline, export_result
from .recovery import ChannelRange, RecoveryJournal, contiguous_ranges
from .resample import compare_interpolation_modes, resample_channel_range
from .synth import SyntheticConfig, generate_sample_bundle
from .utils import sha256_bytes
from .versioning import load_report, save_report
from .wavio import read_audio, write_multichannel_wav


def _split_ints(value: str | None) -> tuple[int, ...] | None:
    if value is None:
        return None
    return tuple(int(item) for item in value.split(",") if item.strip())


def _split_floats(value: str | None) -> tuple[float, ...] | None:
    if value is None:
        return None
    return tuple(float(item) for item in value.split(",") if item.strip())


def _parse_marker_lists(
    values: Sequence[str] | None,
) -> dict[int, list[int]]:
    output: dict[int, list[int]] = {}
    for value in values or ():
        for entry in value.split(","):
            channel_text, index_text = entry.split(":", 1)
            output.setdefault(int(channel_text), []).append(int(index_text))
    return output


def _config_from_args(args: argparse.Namespace, sample_rate: int) -> PipelineConfig:
    pulse_samples = int(round(args.marker_duration_seconds * sample_rate))
    marker = MarkerConfig(
        period_seconds=args.marker_period_seconds,
        pulse_duration_samples=max(8, pulse_samples),
        first_marker_sample=args.first_marker_sample,
        search_fraction=args.marker_search_fraction,
        min_confidence=args.marker_min_confidence,
        min_snr_db=args.marker_min_snr_db,
    )
    segmentation = SegmentationConfig(
        segment_length_samples=max(
            1, int(round(args.segment_seconds * sample_rate))
        ),
        delay_search_samples=args.delay_search_samples,
        refine_search_samples=args.refine_search_samples,
    )
    fit = FitConfig(
        residual_threshold_samples=args.residual_threshold,
        min_drift_ppm=args.min_drift_ppm,
        jump_threshold_samples=args.jump_threshold,
    )
    compensation = CompensationConfig(
        mode=args.mode,
        tolerance_samples=args.tolerance,
        sinc_half_taps=args.sinc_half_taps,
    )
    diagnosis = DiagnosisConfig(
        min_drift_ppm=args.min_drift_ppm,
        jump_threshold_samples=args.jump_threshold,
        residual_threshold_samples=args.residual_threshold,
    )
    return PipelineConfig(
        reference_channel=args.reference,
        active_channels=_split_ints(args.channels),
        marker=marker,
        segmentation=segmentation,
        fit=fit,
        compensation=compensation,
        diagnosis=diagnosis,
    )


def _write_summary(path: Path, value: dict[str, Any]) -> None:
    atomic_write_json(path, value)
    sys.stdout.write(
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    )


def _diagnostics_markdown(result: Any) -> str:
    report = result.report
    lines = [
        "# Synchronization diagnostics",
        "",
        f"- Parameter version: `{report.parameter_version}`",
        f"- Report content SHA-256: `{report.integrity.get('content_sha256', '')}`",
        f"- Tolerance: {report.compensation.tolerance_samples if report.compensation else 'n/a'} samples",
        "",
        "| Channel | Conclusion | Confidence | Drift ppm | Residual RMS | Residual max |",
        "|---:|---|---:|---:|---:|---:|",
    ]
    diagnoses = {item.channel: item for item in report.diagnoses}
    residual = {
        item.channel: item
        for item in (
            report.residual_summary.channels
            if report.residual_summary is not None
            else ()
        )
    }
    for fit in report.drift_fits:
        diagnosis = diagnoses.get(fit.channel)
        residual_channel = residual.get(fit.channel)
        lines.append(
            "| {channel} | {conclusion} | {confidence:.3f} | {ppm:.3f} | "
            "{rms:.3f} | {maximum:.3f} |".format(
                channel=fit.channel,
                conclusion=diagnosis.conclusion if diagnosis else "n/a",
                confidence=diagnosis.confidence if diagnosis else 0.0,
                ppm=fit.drift_ppm,
                rms=(
                    residual_channel.residual_rms_samples
                    if residual_channel
                    else 0.0
                ),
                maximum=(
                    residual_channel.residual_max_samples
                    if residual_channel
                    else 0.0
                ),
            )
        )
    if report.issues:
        lines.extend(["", "## Issues", ""])
        for issue in report.issues:
            lines.append(
                f"- `{issue.code}` channel={issue.channel} "
                f"marker={issue.marker_index} segment={issue.segment_index}: "
                f"{issue.message}"
            )
    return "\n".join(lines) + "\n"


def command_synth(args: argparse.Namespace) -> int:
    drift = _split_floats(args.drift_ppm)
    offsets = _split_floats(args.offset_samples)
    config = SyntheticConfig(
        channels=args.channels,
        sample_rate=args.sample_rate,
        duration_seconds=args.duration,
        marker_period_seconds=args.marker_period_seconds,
        marker_duration_seconds=args.marker_duration_seconds,
        seed=args.seed,
        drift_ppm=drift or SyntheticConfig.drift_ppm,
        initial_offset_samples=offsets or SyntheticConfig.initial_offset_samples,
        missing_markers=_parse_marker_lists(args.missing_marker),
        low_snr_markers=_parse_marker_lists(args.low_snr_marker),
    )
    _, paths = generate_sample_bundle(args.out, config)
    summary = dict(paths)
    summary["wav_sha256"] = sha256_bytes(Path(paths["wav"]).read_bytes())
    _write_summary(Path(args.out) / "manifest.json", summary)
    return 0


def command_analyze(args: argparse.Namespace) -> int:
    audio = read_audio(args.input)
    config = _config_from_args(args, audio.sample_rate)
    result = SyncPipeline(config).analyze(audio)
    if args.strict and any(issue.code == E_MARKER_MISSING for issue in result.issues):
        missing = [
            issue.to_dict()
            for issue in result.issues
            if issue.code == E_MARKER_MISSING
        ]
        sys.stderr.write(
            json.dumps(missing, ensure_ascii=False, sort_keys=True) + "\n"
        )
        return 2
    output = Path(args.out)
    output.mkdir(parents=True, exist_ok=True)
    versions = (
        ["v1", "v2", "v3"] if args.versions == "all" else [args.versions]
    )
    primary_wav = output / "compensated.wav"
    primary_report = output / "report_v3.json"
    exported = export_result(
        result,
        primary_wav,
        primary_report,
        target_version="v3",
    )
    report_paths = {"v3": str(primary_report)}
    losses: dict[str, list[str]] = {}
    for version in versions:
        path = output / f"report_{version}.json"
        if version == "v3":
            report_paths[version] = str(path)
            report_paths[version] = str(primary_report)
            continue
        downgraded = save_report(result.report, path, version)
        report_paths[version] = str(path)
        losses[version] = [
            field for issue in downgraded.losses for field in issue.fields
        ]
    markdown_path = output / "diagnostics.md"
    atomic_write_bytes(
        markdown_path, _diagnostics_markdown(result).encode("utf-8")
    )
    summary = {
        "schema_version": "analyze-summary-v1",
        "input": str(Path(args.input)),
        "parameter_version": result.parameter_version,
        "wav": str(primary_wav),
        "wav_sha256": exported.wav_sha256,
        "reports": report_paths,
        "diagnostics_markdown": str(markdown_path),
        "losses": losses,
        "max_abs_delay_samples": (
            result.residual_summary.max_abs_delay_samples
            if result.residual_summary
            else None
        ),
        "tolerance_samples": config.compensation.tolerance_samples,
        "issues": [issue.to_dict() for issue in result.issues],
        "diagnoses": [
            {
                "channel": diagnosis.channel,
                "conclusion": diagnosis.conclusion,
                "confidence": diagnosis.confidence,
            }
            for diagnosis in result.report.diagnoses
        ],
        "trace": list(result.trace),
    }
    _write_summary(output / "summary.json", summary)
    return 0


def command_convert(args: argparse.Namespace) -> int:
    report = load_report(args.input)
    result = save_report(report, args.output, args.to)
    summary = {
        "schema_version": "convert-summary-v1",
        "input": str(Path(args.input)),
        "output": str(Path(args.output)),
        "source_version": report.source_version,
        "target_version": args.to,
        "losses": [
            {"code": issue.code, "fields": issue.fields}
            for issue in result.losses
        ],
        "provenance": report.provenance,
    }
    sys.stdout.write(
        json.dumps(summary, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    )
    return 0


def command_compare_modes(args: argparse.Namespace) -> int:
    audio = read_audio(args.input)
    config = _config_from_args(args, audio.sample_rate)
    result = SyncPipeline(config).analyze(audio)
    comparison = compare_interpolation_modes(
        audio,
        result.report.drift_fits,
        reference_channel=config.reference_channel,
        marker_config=config.marker,
        tolerance_samples=config.compensation.tolerance_samples,
        sinc_half_taps=config.compensation.sinc_half_taps,
        channels=(
            config.active_channels
            if config.active_channels is not None
            else range(audio.channel_count)
        ),
    )
    output = Path(args.out)
    output.parent.mkdir(parents=True, exist_ok=True)
    comparison["parameter_version"] = result.parameter_version
    comparison["issues"] = [issue.to_dict() for issue in result.issues]
    atomic_write_json(output, comparison)
    sys.stdout.write(
        json.dumps(comparison, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    )
    return 0


def command_recover(args: argparse.Namespace) -> int:
    report = load_report(args.report)
    audio = read_audio(args.input)
    journal = RecoveryJournal(args.journal, report)
    fits = {fit.channel: fit for fit in report.drift_fits}
    output_length = (
        report.compensation.output_length
        if report.compensation is not None and report.compensation.output_length > 0
        else audio.frame_count
    )
    ranges = contiguous_ranges(
        range(audio.channel_count),
        output_length=output_length,
        chunk_length=int(round(args.chunk_seconds * audio.sample_rate)),
    )

    def compensate(channel_range: ChannelRange) -> np.ndarray:
        fit = fits.get(channel_range.channel)
        if fit is None:
            raise SyncError(
                "E_RECOVERY_INVALID",
                f"report has no fit for channel {channel_range.channel}",
                channel=channel_range.channel,
            )
        return resample_channel_range(
            audio.channel(channel_range.channel),
            fit,
            mode=report.compensation.mode,
            output_start=channel_range.start_sample,
            output_end=channel_range.end_sample,
            sinc_half_taps=report.compensation.sinc_half_taps,
        )

    trace_path = Path(args.out) / "recovery_trace.json"
    resumed = journal.resume(
        ranges,
        channels=range(audio.channel_count),
        output_length=output_length,
        compensate_range=compensate,
        trace_path=trace_path,
    )
    wav_path = Path(args.out) / "recovered_compensated.wav"
    wav_sha = write_multichannel_wav(
        wav_path, resumed.matrix, audio.sample_rate
    )
    summary = {
        "schema_version": "recovery-summary-v1",
        "wav": str(wav_path),
        "wav_sha256": wav_sha,
        "trace": str(trace_path),
        "last_sequence": resumed.state.last_sequence,
        "last_hash": resumed.state.last_hash,
        **resumed.to_dict(),
    }
    _write_summary(Path(args.out) / "recovery_summary.json", summary)
    return 0


def command_gui(_: argparse.Namespace) -> int:
    from .gui import main as gui_main

    return gui_main()


def _add_analysis_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--reference", type=int, default=0)
    parser.add_argument("--channels", help="comma-separated active channel indices")
    parser.add_argument("--marker-period-seconds", type=float, default=0.5)
    parser.add_argument("--marker-duration-seconds", type=float, default=0.002)
    parser.add_argument("--marker-search-fraction", type=float, default=0.35)
    parser.add_argument("--marker-min-confidence", type=float, default=0.35)
    parser.add_argument("--marker-min-snr-db", type=float, default=8.0)
    parser.add_argument("--first-marker-sample", type=float, default=None)
    parser.add_argument("--segment-seconds", type=float, default=1.0)
    parser.add_argument("--delay-search-samples", type=int, default=1024)
    parser.add_argument("--refine-search-samples", type=int, default=96)
    parser.add_argument("--residual-threshold", type=float, default=2.5)
    parser.add_argument("--min-drift-ppm", type=float, default=0.5)
    parser.add_argument("--jump-threshold", type=float, default=8.0)
    parser.add_argument(
        "--mode",
        choices=("linear", "windowed_sinc"),
        default="windowed_sinc",
    )
    parser.add_argument("--tolerance", type=float, default=2.0)
    parser.add_argument("--sinc-half-taps", type=int, default=16)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mcsync",
        description="Multi-channel clock-drift synchronization workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    synth = subparsers.add_parser("synth", help="generate deterministic sample WAV")
    synth.add_argument("--out", required=True)
    synth.add_argument("--channels", type=int, default=8)
    synth.add_argument("--sample-rate", type=int, default=48000)
    synth.add_argument("--duration", type=float, default=24.0)
    synth.add_argument("--marker-period-seconds", type=float, default=0.5)
    synth.add_argument("--marker-duration-seconds", type=float, default=0.002)
    synth.add_argument("--seed", type=int, default=20261122)
    synth.add_argument("--drift-ppm")
    synth.add_argument("--offset-samples")
    synth.add_argument(
        "--missing-marker",
        action="append",
        help="channel:marker_index; repeat or comma-separate",
    )
    synth.add_argument(
        "--low-snr-marker",
        action="append",
        help="channel:marker_index; repeat or comma-separate",
    )
    synth.set_defaults(handler=command_synth)

    analyze = subparsers.add_parser("analyze", help="run the full pipeline")
    analyze.add_argument("--input", required=True)
    analyze.add_argument("--out", required=True)
    analyze.add_argument("--versions", choices=("v1", "v2", "v3", "all"), default="all")
    analyze.add_argument("--strict", action="store_true")
    _add_analysis_options(analyze)
    analyze.set_defaults(handler=command_analyze)

    convert = subparsers.add_parser(
        "convert", help="upgrade or downgrade a report document"
    )
    convert.add_argument("--input", required=True)
    convert.add_argument("--to", choices=("v1", "v2", "v3"), required=True)
    convert.add_argument("--output", required=True)
    convert.set_defaults(handler=command_convert)

    compare = subparsers.add_parser(
        "compare-modes", help="compare linear and windowed-sinc compensation"
    )
    compare.add_argument("--input", required=True)
    compare.add_argument("--out", required=True)
    _add_analysis_options(compare)
    compare.set_defaults(handler=command_compare_modes)

    recover = subparsers.add_parser(
        "recover", help="resume compensation from a validated recovery journal"
    )
    recover.add_argument("--input", required=True)
    recover.add_argument("--report", required=True)
    recover.add_argument("--journal", required=True)
    recover.add_argument("--out", required=True)
    recover.add_argument("--chunk-seconds", type=float, default=1.0)
    recover.set_defaults(handler=command_recover)

    gui = subparsers.add_parser("gui", help="launch the PySide6 workbench")
    gui.set_defaults(handler=command_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except SyncError as exc:
        sys.stderr.write(
            json.dumps(exc.to_dict(), ensure_ascii=False, sort_keys=True) + "\n"
        )
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

