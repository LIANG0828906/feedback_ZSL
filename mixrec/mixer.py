"""Streaming multi-track mix engine over one unified output timeline."""

from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

import numpy as np

from .errors import E_MIX_INPUT, MixError, input_error
from .models import MixConfig, TrackSpec
from .resample import quantize_pcm, resample_output_segment, to_output_channels
from .wavio import WavData, read_wav


@dataclass
class LoadedTrack:
    spec: TrackSpec
    wav: WavData
    source_sha256: str
    path: Path

    @property
    def gain_linear(self) -> float:
        return math.pow(10.0, self.spec.gain_db / 20.0)


class Mixer:
    """A deterministic mixer that can render any output-time interval."""

    def __init__(self, config: MixConfig, *, sources: dict[str, LoadedTrack] | None = None):
        self.config = config
        self.sources: dict[str, LoadedTrack] = {}
        if sources is not None:
            for track in config.tracks:
                if track.id not in sources:
                    raise input_error(track.id, "missing loaded source")
                loaded = sources[track.id]
                loaded.spec = track
                self.sources[track.id] = loaded
            self._validate_sources()
        else:
            self._load_and_validate()
        self.config.set_total_from_sources(
            {track_id: (source.wav.frames, source.wav.sample_rate) for track_id, source in self.sources.items()}
        )

    def _load_and_validate(self) -> None:
        for track in self.config.tracks:
            path = self.config.track_path(track)
            wav = read_wav(path, track_id=track.id)
            self.sources[track.id] = LoadedTrack(
                spec=track,
                wav=wav,
                source_sha256=wav.source_sha256,
                path=path,
            )
        self._validate_sources()

    def _validate_sources(self) -> None:
        for track_id, source in self.sources.items():
            track = source.spec
            wav = source.wav
            if track.channels is not None and int(track.channels) != wav.channels:
                raise input_error(
                    track_id,
                    "declared channel layout does not match WAV",
                    declared=track.channels,
                    actual=wav.channels,
                )
            if track.sample_rate is not None and int(track.sample_rate) != wav.sample_rate:
                raise input_error(
                    track_id,
                    "declared sample rate does not match WAV",
                    declared=track.sample_rate,
                    actual=wav.sample_rate,
                )
            delay = track.delay_samples
            limit = (
                track.max_delay_samples
                if track.max_delay_samples is not None
                else self.config.max_delay_samples
            )
            if delay < 0 or delay > limit:
                raise input_error(
                    track_id,
                    "delay compensation is outside the declared range",
                    delay_samples=delay,
                    max_delay_samples=limit,
                )
            if wav.sample_rate not in (16000, 48000):
                raise input_error(
                    track_id,
                    "input sample rate must be 16000 or 48000",
                    sample_rate=wav.sample_rate,
                )

    @property
    def source_hashes(self) -> dict[str, str]:
        return {track_id: source.source_sha256 for track_id, source in self.sources.items()}

    def render(
        self,
        start_sample: int,
        end_sample: int,
        *,
        block_samples: int | None = None,
    ) -> np.ndarray:
        if start_sample < 0 or end_sample < start_sample:
            raise MixError(E_MIX_INPUT, "invalid mix interval", details={"start": start_sample, "end": end_sample})
        total = end_sample - start_sample
        if total == 0:
            return np.zeros((0, self.config.output_channels), dtype="<i2")
        block = int(block_samples or self.config.block_samples or total)
        block = max(1, min(block, total))
        pieces: list[np.ndarray] = []
        for offset in range(0, total, block):
            count = min(block, total - offset)
            pieces.append(self._render_block(start_sample + offset, count))
        if len(pieces) == 1:
            return pieces[0]
        return np.concatenate(pieces, axis=0)

    def _render_block(self, start_sample: int, count: int) -> np.ndarray:
        output_channels = self.config.output_channels
        accumulation = np.zeros((count, output_channels), dtype=np.float64)
        indices = np.arange(start_sample, start_sample + count, dtype=np.int64)
        for track in self.config.tracks:
            loaded = self.sources[track.id]
            values, _valid = resample_output_segment(
                loaded.wav.pcm,
                loaded.wav.sample_rate,
                self.config.output_rate,
                start_sample,
                count,
                track.delay_samples,
            )
            if values.shape[1] != output_channels:
                values = to_output_channels(values, output_channels)
            values *= loaded.gain_linear
            for mute_start, mute_end in track.effective_mute_intervals(self.config.output_rate):
                mask = (indices >= mute_start) & (indices < mute_end)
                if np.any(mask):
                    values[mask] = 0.0
            accumulation += values
        return quantize_pcm(accumulation)

    def iter_pcm(
        self,
        start_sample: int,
        end_sample: int,
        *,
        block_samples: int | None = None,
    ) -> Iterator[np.ndarray]:
        """Yield consecutive PCM blocks covering ``[start, end)``."""

        total = end_sample - start_sample
        block = max(1, int(block_samples or self.config.block_samples or total or 1))
        for offset in range(0, total, block):
            count = min(block, total - offset)
            yield self._render_block(start_sample + offset, count)

    def render_all(self, *, block_samples: int | None = None) -> Iterator[np.ndarray]:
        yield from self.iter_pcm(
            0,
            self.config.resolved_total_samples(),
            block_samples=block_samples,
        )


def estimate_task_memory_bytes(
    config: MixConfig,
    sources: dict[str, LoadedTrack],
    block_samples: int,
) -> int:
    """Estimate peak memory for one slice task at a given internal block size.

    The estimate includes the float64 mix accumulator, quantized PCM, source
    gather buffers and a small fixed amount of Python/WAV scratch space.  It is
    intentionally conservative and deterministic.
    """

    block = max(1, int(block_samples))
    output_elements = block * config.output_channels
    accumulator = output_elements * 8
    quantized = output_elements * 2
    encoded = output_elements * 2
    source_memory = 0
    for track in config.tracks:
        loaded = sources[track.id]
        source_frames = math.ceil(block * loaded.wav.sample_rate / config.output_rate) + 4
        source_memory += source_frames * loaded.wav.channels * 4
    scratch = output_elements * 4
    fixed = 2048 + 512 * len(config.tracks)
    estimate = accumulator + quantized + encoded + source_memory + scratch + fixed
    return int(math.ceil(estimate / 16.0) * 16)


@dataclass(frozen=True)
class TaskMemoryPlan:
    block_samples: int
    estimated_bytes: int
    attempts: tuple[tuple[int, int], ...]


def plan_task_memory(
    config: MixConfig,
    sources: dict[str, LoadedTrack],
    sample_count: int,
    *,
    budget_bytes: int | None = None,
    initial_block_samples: int | None = None,
) -> TaskMemoryPlan:
    budget = int(budget_bytes if budget_bytes is not None else config.per_slice_budget_bytes)
    start = int(initial_block_samples or config.block_samples or sample_count)
    start = max(1, min(start, max(1, sample_count)))
    minimum = min(128, start)
    attempts: list[tuple[int, int]] = []
    block = start
    while True:
        estimate = estimate_task_memory_bytes(config, sources, block)
        attempts.append((block, estimate))
        if estimate <= budget:
            return TaskMemoryPlan(block_samples=block, estimated_bytes=estimate, attempts=tuple(attempts))
        if block <= minimum:
            raise MixError(
                "E_BUDGET_EXCEEDED",
                "slice task exceeds per-slice memory budget after degradation",
                details={
                    "estimate": estimate,
                    "budget": budget,
                    "attempts": [{"block_samples": b, "estimated_bytes": e} for b, e in attempts],
                },
            )
        block = max(minimum, block // 2)
