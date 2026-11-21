from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .errors import ErrorCode, build_error
from .models import DegradeStage, SliceBoundary, TrackConfig


@dataclass(frozen=True)
class BudgetAttempt:
    stage: str
    estimated_bytes: int
    limit_bytes: int
    fits: bool
    components: dict[str, int]

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "estimated_bytes": self.estimated_bytes,
            "limit_bytes": self.limit_bytes,
            "fits": self.fits,
            "components": dict(self.components),
        }


@dataclass(frozen=True)
class BudgetDecision:
    stage: DegradeStage
    estimated_bytes: int
    attempts: tuple[BudgetAttempt, ...]
    components: dict[str, int]

    @property
    def degraded_stage_index(self) -> int:
        return next(index for index, attempt in enumerate(self.attempts) if attempt.fits)


def estimate_stage_memory(
    audio_channels: int,
    source_samples: int,
    output_samples: int,
    frame_count: int,
    stage: DegradeStage,
    quantization: int,
) -> tuple[int, dict[str, int]]:
    source_block = min(source_samples, stage.block_size)
    output_block = min(output_samples, stage.block_size)
    source_working = source_block * audio_channels * 8
    interpolation_working = output_block * audio_channels * 8
    temporary = int((source_working + interpolation_working) * stage.temp_factor + 0.999999)
    output_audio = output_samples * audio_channels * 2
    frame_records = int(frame_count * (128 + quantization) * stage.temp_factor + 0.999999)
    overhead = 64 * 1024
    components = {
        "source_working": source_working,
        "interpolation_working": interpolation_working,
        "temporary_buffers": temporary,
        "output_audio": output_audio,
        "frame_records": frame_records,
        "overhead": overhead,
    }
    return sum(components.values()), components


def choose_budget_stage(
    track: TrackConfig,
    boundary: SliceBoundary,
    frame_count: int,
    audio_channels: int,
) -> BudgetDecision:
    output_samples = boundary.output_samples_by_track[track.track_id]
    source_samples = boundary.end_source_sample - boundary.start_source_sample
    attempts: list[BudgetAttempt] = []
    for stage in track.degrade_stages:
        estimate, components = estimate_stage_memory(
            audio_channels=audio_channels,
            source_samples=source_samples,
            output_samples=output_samples,
            frame_count=frame_count,
            stage=stage,
            quantization=track.video_quantization,
        )
        fits = estimate <= track.memory_limit_bytes
        attempts.append(
            BudgetAttempt(
                stage=stage.name,
                estimated_bytes=estimate,
                limit_bytes=track.memory_limit_bytes,
                fits=fits,
                components=components,
            )
        )
        if fits:
            return BudgetDecision(
                stage=stage,
                estimated_bytes=estimate,
                attempts=tuple(attempts),
                components=components,
            )
    raise build_error(
        ErrorCode.BUDGET_EXCEEDED,
        "slice exceeds every declared memory degradation stage",
        track_id=track.track_id,
        slice_index=boundary.index,
        limit_bytes=track.memory_limit_bytes,
        attempts=[attempt.to_dict() for attempt in attempts],
    )
