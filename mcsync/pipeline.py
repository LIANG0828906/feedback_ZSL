"""End-to-end synchronization pipeline and atomic result export."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .atomic import atomic_write_json
from .delays import estimate_all_segment_delays
from .diagnostics import diagnose_all_channels
from .errors import (
    E_ALIGNMENT_FAILED,
    E_CHANNEL_INVALID,
    E_MARKER_MISSING,
    E_RESIDUAL_THRESHOLD,
    E_VERSION_STALE,
    Issue,
    SyncError,
    deduplicate_issues,
)
from .fitting import fit_all_channels
from .markers import MarkerDetectionSet, detect_all_markers
from .models import (
    ChannelLayout,
    ChannelSpec,
    CompensationParams,
    PipelineConfig,
    ResidualSummary,
    SyncReport,
)
from .resample import compensate_audio
from .utils import sha256_json, short_hash
from .versioning import downgrade_report
from .wavio import AudioData, write_multichannel_wav


@dataclass
class ProcessingResult:
    source_audio: AudioData
    compensated_audio: AudioData | None
    report: SyncReport
    marker_detection: MarkerDetectionSet
    residual_summary: ResidualSummary | None
    issues: tuple[Issue, ...]
    parameter_version: str
    trace: tuple[dict[str, Any], ...] = ()


@dataclass
class ExportResult:
    wav_path: str
    report_path: str
    report_version: str
    wav_sha256: str
    report_content_sha256: str
    losses: tuple[Issue, ...] = ()
    document: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "wav_path": self.wav_path,
            "report_path": self.report_path,
            "report_version": self.report_version,
            "wav_sha256": self.wav_sha256,
            "report_content_sha256": self.report_content_sha256,
            "losses": [issue.to_dict() for issue in self.losses],
        }


def _active_channels(
    audio: AudioData,
    config: PipelineConfig,
) -> tuple[int, ...]:
    if config.active_channels is None:
        channels = tuple(range(audio.channel_count))
    else:
        channels = tuple(sorted(set(int(value) for value in config.active_channels)))
    invalid = [
        channel
        for channel in set(channels) | {config.reference_channel}
        if channel < 0 or channel >= audio.channel_count
    ]
    if invalid:
        raise SyncError(
            E_CHANNEL_INVALID,
            f"channel indices outside input audio: {sorted(invalid)}",
            details={"channel_count": audio.channel_count},
        )
    if config.reference_channel not in channels:
        channels = (config.reference_channel,) + channels
    return tuple(sorted(channels))


def _parameter_version(
    audio: AudioData,
    config: PipelineConfig,
    fits: Iterable[Any],
) -> str:
    return short_hash(
        {
            "input_audio_sha256": audio.content_hash(),
            "reference_channel": config.reference_channel,
            "active_channels": config.active_channels,
            "segmentation": config.segmentation.to_dict(),
            "fitting": config.fit.to_dict(),
            "compensation": {
                "mode": config.compensation.mode,
                "tolerance_samples": config.compensation.tolerance_samples,
                "sinc_half_taps": config.compensation.sinc_half_taps,
            },
            "fits": [
                {
                    "channel": fit.channel,
                    "initial_offset_samples": fit.initial_offset_samples,
                    "slope": fit.slope,
                }
                for fit in fits
            ],
        },
        length=24,
    )


def _layout(
    audio: AudioData,
    channels: Iterable[int],
    reference_channel: int,
) -> ChannelLayout:
    return ChannelLayout(
        channels=tuple(
            ChannelSpec(
                index=channel,
                name=audio.channel_names[channel],
                sample_rate=audio.sample_rate,
            )
            for channel in channels
        ),
        reference_channel=reference_channel,
        sample_rate=audio.sample_rate,
    )


def _residual_threshold_issues(fits: Iterable[Any]) -> list[Issue]:
    issues: list[Issue] = []
    for fit in fits:
        if fit.exceeded_threshold:
            issues.append(
                Issue(
                    code=E_RESIDUAL_THRESHOLD,
                    message=(
                        "drift-fit residual exceeds the declared threshold"
                    ),
                    channel=fit.channel,
                    segment_index=fit.max_residual_segment,
                    details={
                        "residual_max_samples": fit.residual_max,
                        "residual_threshold_samples": fit.residual_threshold_samples,
                    },
                )
            )
    return issues


class SyncPipeline:
    def __init__(self, config: PipelineConfig | None = None) -> None:
        self.config = (config or PipelineConfig()).clone()
        self.config.validate()

    def analyze(self, audio: AudioData) -> ProcessingResult:
        config = self.config
        channels = _active_channels(audio, config)
        trace: list[dict[str, Any]] = []
        marker_detection = detect_all_markers(
            audio,
            config.marker,
            reference_channel=config.reference_channel,
            channels=channels,
        )
        trace.append(
            {
                "stage": "markers",
                "channels": list(channels),
                "detected": len(marker_detection.markers),
                "issues": len(marker_detection.issues),
            }
        )
        marker_map = {
            channel: marker_detection.markers_for(channel)
            for channel in channels
        }
        segments = estimate_all_segment_delays(
            audio,
            reference_channel=config.reference_channel,
            marker_map=marker_map,
            config=config.segmentation,
            channels=channels,
        )
        trace.append(
            {
                "stage": "segments",
                "segment_length_samples": config.segmentation.segment_length_samples,
                "count": len(segments),
                "content_sha256": sha256_json(
                    [segment.to_dict() for segment in segments]
                ),
            }
        )
        fits = fit_all_channels(
            segments,
            channels=channels,
            reference_channel=config.reference_channel,
            sample_rate=audio.sample_rate,
            config=config.fit,
        )
        trace.append(
            {
                "stage": "fit",
                "count": len(fits),
                "content_sha256": sha256_json(
                    [fit.to_dict() for fit in fits]
                ),
            }
        )
        diagnoses, diagnosis_issues = diagnose_all_channels(
            segments, fits, config.diagnosis
        )
        parameter_version = _parameter_version(audio, config, fits)
        issues: list[Issue] = list(marker_detection.issues)
        issues.extend(diagnosis_issues)
        issues.extend(_residual_threshold_issues(fits))
        residual_summary: ResidualSummary | None = None
        compensated: AudioData | None = None
        compensation: CompensationParams | None = None
        if segments and fits:
            compensated, residual_summary, _, residual_issues = compensate_audio(
                audio,
                fits,
                mode=config.compensation.mode,
                reference_channel=config.reference_channel,
                marker_config=config.marker,
                tolerance_samples=config.compensation.tolerance_samples,
                sinc_half_taps=config.compensation.sinc_half_taps,
                channels=channels,
            )
            issues.extend(residual_issues)
            compensation = CompensationParams(
                mode=config.compensation.mode,
                parameter_version=parameter_version,
                tolerance_samples=config.compensation.tolerance_samples,
                sinc_half_taps=config.compensation.sinc_half_taps,
                output_length=compensated.frame_count,
            )
            trace.append(
                {
                    "stage": "compensation",
                    "mode": config.compensation.mode,
                    "output_length": compensated.frame_count,
                    "max_abs_delay_samples": (
                        residual_summary.max_abs_delay_samples
                        if residual_summary is not None
                        else None
                    ),
                }
            )
            if residual_summary is not None and (
                residual_summary.max_abs_delay_samples
                > config.compensation.tolerance_samples
            ):
                issues.append(
                    Issue(
                        code=E_ALIGNMENT_FAILED,
                        message=(
                            "compensated marker residuals exceed the declared "
                            "tolerance"
                        ),
                        channel=residual_summary.worst_channel,
                        details={
                            "max_abs_delay_samples": residual_summary.max_abs_delay_samples,
                            "tolerance_samples": config.compensation.tolerance_samples,
                        },
                    )
                )
        else:
            trace.append(
                {
                    "stage": "compensation",
                    "skipped": True,
                    "reason": E_MARKER_MISSING,
                }
            )
        issues = list(deduplicate_issues(issues))
        report = SyncReport(
            sample_rate=audio.sample_rate,
            reference_channel=config.reference_channel,
            channel_layout=_layout(audio, channels, config.reference_channel),
            markers=marker_detection.markers,
            segment_delays=segments,
            drift_fits=fits,
            diagnoses=diagnoses,
            compensation=compensation,
            residual_summary=residual_summary,
            segmentation=config.segmentation,
            fitting=config.fit,
            parameter_version=parameter_version,
            schema_version="v3",
            source_version="v3",
            kind="sync_diagnostic_report",
            issues=tuple(issues),
            provenance={
                "markers": "pipeline:detected",
                "segment_delays": "pipeline:segmented_normalized_xcorr",
                "drift_fits": "pipeline:first_order_robust_fit",
                "diagnoses": "pipeline:continuity_and_jump_analysis",
                "compensation": "pipeline:fractional_delay_resampling",
                "residual_summary": "pipeline:post_compensation_marker_detection",
            },
            integrity={},
        )
        report.with_integrity()
        report.integrity["input_audio_sha256"] = audio.content_hash()
        trace.append(
            {
                "stage": "report",
                "parameter_version": parameter_version,
                "content_sha256": report.integrity["content_sha256"],
                "issues": len(issues),
            }
        )
        return ProcessingResult(
            source_audio=audio,
            compensated_audio=compensated,
            report=report,
            marker_detection=marker_detection,
            residual_summary=residual_summary,
            issues=tuple(issues),
            parameter_version=parameter_version,
            trace=tuple(trace),
        )


def validate_parameter_versions(result: ProcessingResult) -> str:
    report = result.report
    if report.compensation is None:
        raise SyncError(
            E_VERSION_STALE,
            "compensation parameters are missing from the report",
            fields=["compensation"],
        )
    report_version = report.parameter_version
    compensation_version = report.compensation.parameter_version
    if not report_version or report_version != compensation_version:
        raise SyncError(
            E_VERSION_STALE,
            "report parameter version and compensation parameter version differ",
            fields=["parameter_version", "compensation.parameter_version"],
            details={
                "report_parameter_version": report_version,
                "compensation_parameter_version": compensation_version,
            },
        )
    if result.parameter_version != report_version:
        raise SyncError(
            E_VERSION_STALE,
            "published result version and report parameter version differ",
            fields=["parameter_version"],
            details={
                "result_parameter_version": result.parameter_version,
                "report_parameter_version": report_version,
            },
        )
    return report_version


def export_result(
    result: ProcessingResult,
    wav_path: str | Path,
    report_path: str | Path,
    *,
    target_version: str = "v3",
) -> ExportResult:
    parameter_version = validate_parameter_versions(result)
    if result.compensated_audio is None:
        raise SyncError(
            E_VERSION_STALE,
            "no compensated audio is available for export",
            fields=["compensated_audio"],
        )
    report = result.report
    report.integrity = {}
    report.with_integrity()
    report.integrity["input_audio_sha256"] = result.source_audio.content_hash()
    wav_sha = write_multichannel_wav(
        wav_path,
        result.compensated_audio.samples,
        result.compensated_audio.sample_rate,
    )
    report.integrity["wav_sha256"] = wav_sha
    report.integrity["parameter_version"] = parameter_version
    downgraded = downgrade_report(report, target_version)
    atomic_write_json(report_path, downgraded.document)
    return ExportResult(
        wav_path=str(Path(wav_path)),
        report_path=str(Path(report_path)),
        report_version=target_version,
        wav_sha256=wav_sha,
        report_content_sha256=report.integrity["content_sha256"],
        losses=downgraded.losses,
        document=downgraded.document,
    )

