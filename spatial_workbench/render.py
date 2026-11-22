"""Deterministic parameter-driven convolution rendering."""

from __future__ import annotations

import hashlib
import math
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any, Mapping, Sequence

import numpy as np
from scipy.signal import fftconvolve, get_window

from .canonical import canonical_json
from .errors import ErrorCode, WorkbenchError
from .models import FrameParameters, FrameRenderStats, RenderResult


@dataclass(frozen=True)
class FrameContribution:
    frame_index: int
    start: int
    channels: tuple[str, ...]
    signal: np.ndarray
    stats: FrameRenderStats


def validate_frame_parameters(params: FrameParameters) -> None:
    def fail(message: str, **details: Any) -> None:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            message,
            stage="render",
            frame_index=params.frame_index,
            details=details,
        )

    if params.frame_index < 0 or params.dry_start < 0 or params.ir_start < 0:
        fail("frame indices and starts must be non-negative")
    if params.frame_length <= 0 or params.hop_length <= 0:
        fail("frame_length and hop_length must be positive")
    if params.sample_rate <= 0:
        fail("sample_rate must be positive")
    source_count = len(params.local_arrival_delays_samples)
    if source_count <= 0:
        fail("frame has no source channels")
    required_channels = (
        params.local_arrival_energy,
        params.direct_energy,
        params.early_reflection_energy,
        params.total_energy,
        params.energy_decay_coefficients,
    )
    if any(len(values) != source_count for values in required_channels):
        fail(
            "per-channel energy parameter lengths do not match source channels",
            source_channels=source_count,
        )
    if len(params.direct_delays_samples) != source_count:
        fail("direct delay length does not match source channels")
    if len(params.reflection_delays_samples) != source_count:
        fail("reflection delay count does not match source channels")
    if len(params.reflection_amplitudes) != source_count:
        fail("reflection amplitude count does not match source channels")
    if len(params.output_channels) == 0:
        fail("output_channels must be non-empty")
    if len(set(params.output_channels)) != len(params.output_channels):
        fail("output_channels must be unique")
    source_names = params.channel_map.get("source_channels")
    if not isinstance(source_names, list) or len(source_names) != source_count:
        fail("channel map source_channels does not match source channels")
    if not isinstance(params.channel_map.get("weights"), dict):
        fail("channel map weights must be an object")
    for channel_index in range(source_count):
        if not 0 <= params.local_arrival_delays_samples[channel_index] < params.frame_length:
            fail(
                "local arrival delay is outside the frame",
                channel_index=channel_index,
                delay=params.local_arrival_delays_samples[channel_index],
                frame_length=params.frame_length,
            )
        scale_values = [
            params.local_arrival_energy[channel_index],
            params.direct_energy[channel_index],
            params.early_reflection_energy[channel_index],
            params.total_energy[channel_index],
            params.energy_decay_coefficients[channel_index],
        ]
        if any(not math.isfinite(value) or value < 0.0 for value in scale_values):
            fail(
                "energy and decay parameters must be finite and non-negative",
                channel_index=channel_index,
            )
        delays = params.reflection_delays_samples[channel_index]
        amplitudes = params.reflection_amplitudes[channel_index]
        if len(delays) != len(amplitudes):
            fail(
                "reflection delay and amplitude lengths differ",
                channel_index=channel_index,
            )
        if any(
            not isinstance(delay, int) or not 0 <= delay < params.frame_length
            for delay in delays
        ):
            fail(
                "reflection delay is outside the frame",
                channel_index=channel_index,
            )
        if any(not math.isfinite(amplitude) for amplitude in amplitudes):
            fail(
                "reflection amplitude must be finite",
                channel_index=channel_index,
            )


def _tail_seed(params: FrameParameters, channel_index: int) -> int:
    material = canonical_json(
        {
            "frame": params.frame_index,
            "channel": channel_index,
            "frame_length": params.frame_length,
            "arrival": params.local_arrival_delays_samples[channel_index],
            "energy": params.local_arrival_energy[channel_index],
        }
    ).encode("utf-8")
    return int.from_bytes(hashlib.sha256(material).digest()[:8], "little")


def _synthesize_filter(
    params: FrameParameters,
    channel_index: int,
) -> np.ndarray:
    length = params.frame_length
    if length <= 0:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "frame_length must be positive",
            stage="render",
            frame_index=params.frame_index,
        )
    if channel_index >= len(params.local_arrival_delays_samples):
        raise WorkbenchError(
            ErrorCode.E_CHAN,
            "frame parameters do not contain this channel",
            stage="render",
            frame_index=params.frame_index,
            channel_index=channel_index,
        )
    impulse = np.zeros(length, dtype=np.float64)
    arrival = int(np.clip(params.local_arrival_delays_samples[channel_index], 0, length - 1))
    arrival_energy = max(0.0, float(params.local_arrival_energy[channel_index]))
    impulse[arrival] += math.sqrt(arrival_energy)

    if channel_index < len(params.reflection_delays_samples):
        delays = params.reflection_delays_samples[channel_index]
        amplitudes = (
            params.reflection_amplitudes[channel_index]
            if channel_index < len(params.reflection_amplitudes)
            else []
        )
        for offset, delay in enumerate(delays):
            if not 0 <= int(delay) < length:
                continue
            if offset < len(amplitudes):
                amplitude = float(amplitudes[offset])
            elif channel_index < len(params.reflection_gains):
                amplitude = (
                    float(params.reflection_gains[channel_index][offset])
                    * math.sqrt(arrival_energy)
                )
            else:
                amplitude = 0.0
            impulse[int(delay)] += amplitude

    direct = max(0.0, float(params.direct_energy[channel_index]))
    early = max(0.0, float(params.early_reflection_energy[channel_index]))
    total = max(0.0, float(params.total_energy[channel_index]))
    tail_energy = max(0.0, total - direct - early)
    if tail_energy > 0.0 and arrival + 1 < length:
        tail_start = min(length - 1, arrival + 1)
        positions = np.arange(tail_start, length, dtype=np.float64)
        decay = max(
            0.0,
            float(params.energy_decay_coefficients[channel_index]),
        )
        envelope = np.exp(-decay * (positions - float(tail_start)))
        rng = np.random.Generator(np.random.PCG64(_tail_seed(params, channel_index)))
        noise = rng.standard_normal(positions.size)
        tail = envelope * noise
        current_energy = float(np.sum(np.square(tail, dtype=np.float64)))
        if current_energy > 1.0e-30:
            tail *= math.sqrt(tail_energy / current_energy)
            impulse[tail_start:] += tail
    return impulse


def _analysis_window(length: int, hop_length: int) -> np.ndarray:
    if hop_length >= length:
        return np.ones(length, dtype=np.float64)
    return get_window("hann", length, fftbins=True).astype(np.float64)


def render_frame(
    params: FrameParameters,
    dry: np.ndarray,
    *,
    clip_limit: float,
) -> FrameContribution:
    validate_frame_parameters(params)
    if not np.isfinite(clip_limit) or clip_limit <= 0.0:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "clip_limit must be positive and finite",
            stage="render",
            frame_index=params.frame_index,
        )
    dry = np.asarray(dry, dtype=np.float64)
    if dry.ndim != 1:
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            "dry signal must be mono",
            stage="render",
            frame_index=params.frame_index,
        )
    length = params.frame_length
    start = params.dry_start
    if start < 0:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "frame dry_start must be non-negative",
            stage="render",
            frame_index=params.frame_index,
        )
    segment = np.zeros(length, dtype=np.float64)
    available = max(0, min(length, dry.size - start))
    if available:
        segment[:available] = dry[start : start + available]
    segment *= _analysis_window(length, params.hop_length)

    source_map = params.channel_map.get("weights", {})
    output_channels = tuple(params.output_channels)
    if not output_channels:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "frame has no output channel mapping",
            stage="render",
            frame_index=params.frame_index,
        )
    source_count = len(params.local_arrival_delays_samples)
    filtered: list[np.ndarray] = []
    for channel_index in range(source_count):
        impulse = _synthesize_filter(params, channel_index)
        filtered.append(fftconvolve(segment, impulse, mode="full"))

    result = np.zeros((len(output_channels), filtered[0].size), dtype=np.float64)
    source_channels = tuple(params.channel_map.get("source_channels", []))
    for output_index, output_channel in enumerate(output_channels):
        raw_weights = source_map.get(output_channel)
        if not isinstance(raw_weights, Mapping):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "frame channel map is missing an output channel",
                stage="render",
                frame_index=params.frame_index,
                details={"output_channel": output_channel},
            )
        for source_name, weight in raw_weights.items():
            if source_name not in source_channels:
                raise WorkbenchError(
                    ErrorCode.E_CHAN,
                    f"channel map references an unknown source channel {source_name!r}",
                    stage="render",
                    frame_index=params.frame_index,
                )
            source_index = source_channels.index(source_name)
            result[output_index] += float(weight) * filtered[source_index]

    if result.size == 0:
        stats = FrameRenderStats(0.0, 0.0, 0.0)
    else:
        visible = max(0, min(result.shape[1], dry.size - start))
        stats = FrameRenderStats.from_signal(result[:, :visible])
    peak = float(np.max(np.abs(result))) if result.size else 0.0
    if peak > clip_limit:
        raise WorkbenchError(
            ErrorCode.E_CLIP,
            "rendered frame exceeds the clip limit",
            stage="render",
            frame_index=params.frame_index,
            details={"peak": peak, "clip_limit": clip_limit},
        )
    return FrameContribution(
        frame_index=params.frame_index,
        start=start,
        channels=output_channels,
        signal=result,
        stats=stats,
    )
class _OutputAccumulator:
    def __init__(self, sample_count: int) -> None:
        self.sample_count = sample_count
        self.channels: list[str] = []
        self.buffers: dict[str, np.ndarray] = {}

    def add(self, contribution: FrameContribution) -> None:
        for channel_index, channel in enumerate(contribution.channels):
            if channel not in self.buffers:
                self.channels.append(channel)
                self.buffers[channel] = np.zeros(self.sample_count, dtype=np.float64)
            buffer = self.buffers[channel]
            available = min(contribution.signal.shape[1], self.sample_count - contribution.start)
            if available > 0:
                buffer[
                    contribution.start : contribution.start + available
                ] += contribution.signal[channel_index, :available]

    def output(self) -> np.ndarray:
        if not self.channels:
            return np.zeros((0, self.sample_count), dtype=np.float64)
        return np.stack([self.buffers[channel] for channel in self.channels], axis=0)


class RenderEngine:
    """Render persisted parameters into a fixed-length multichannel result."""

    def __init__(self, *, clip_limit: float = 1.0) -> None:
        if clip_limit <= 0.0 or not math.isfinite(clip_limit):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "clip_limit must be positive and finite",
                stage="render",
            )
        self.clip_limit = float(clip_limit)

    def render_contribution(
        self,
        params: FrameParameters,
        dry: np.ndarray,
    ) -> FrameContribution:
        return render_frame(params, dry, clip_limit=self.clip_limit)

    def render_all(
        self,
        frames: Sequence[FrameParameters],
        dry: np.ndarray,
        *,
        path: str,
        max_workers: int = 1,
        checkpoint_generation: int = 0,
    ) -> RenderResult:
        dry = np.asarray(dry, dtype=np.float64)
        if dry.ndim != 1 or dry.size == 0:
            raise WorkbenchError(
                ErrorCode.E_DECODE,
                "dry signal must be a non-empty mono array",
                stage="render",
            )
        ordered = sorted(frames, key=lambda item: item.frame_index)
        indices = [frame.frame_index for frame in ordered]
        if len(set(indices)) != len(indices):
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "duplicate frame indices cannot be rendered",
                stage="render",
            )
        if not ordered:
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "no frames are available for rendering",
                stage="render",
            )
        accumulator = _OutputAccumulator(int(dry.size))
        stats: dict[int, FrameRenderStats] = {}
        contributions: dict[int, FrameContribution] = {}
        workers = max(1, min(int(max_workers), len(ordered)))
        if workers == 1:
            for params in ordered:
                contributions[params.frame_index] = self.render_contribution(params, dry)
        else:
            with ThreadPoolExecutor(max_workers=workers) as executor:
                futures = {
                    params.frame_index: executor.submit(
                        self.render_contribution,
                        params,
                        dry,
                    )
                    for params in ordered
                }
                for frame_index in indices:
                    contributions[frame_index] = futures[frame_index].result()
        for frame_index in indices:
            contribution = contributions[frame_index]
            accumulator.add(contribution)
            stats[frame_index] = contribution.stats
        output = accumulator.output()
        if output.shape[1] != dry.size:
            raise WorkbenchError(
                ErrorCode.E_FRAME,
                "rendered sample count does not equal dry duration",
                stage="render",
                details={
                    "rendered_samples": int(output.shape[1]),
                    "dry_samples": int(dry.size),
                },
            )
        if output.size:
            peak = float(np.max(np.abs(output)))
            if peak > self.clip_limit:
                raise WorkbenchError(
                    ErrorCode.E_CLIP,
                    "overlap-add output exceeds the clip limit",
                    stage="render",
                    details={"peak": peak, "clip_limit": self.clip_limit},
                )
        return RenderResult(
            output=np.ascontiguousarray(output, dtype=np.float64),
            channels=tuple(accumulator.channels),
            sample_rate=int(ordered[0].sample_rate),
            frames=tuple(ordered),
            render_stats=stats,
            path=path,
            checkpoint_generation=checkpoint_generation,
        )

