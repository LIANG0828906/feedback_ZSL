from __future__ import annotations

import io
import math
import wave
from dataclasses import dataclass

import numpy as np

from .models import SliceBoundary, SourceAudio, TrackConfig


@dataclass(frozen=True)
class AudioSlice:
    data: bytes
    sha256: str
    start_output_sample: int
    end_output_sample: int
    source_sample_count: int
    output_sample_count: int


def output_sample_bounds(boundary: SliceBoundary, track: TrackConfig, ticks_per_second: int) -> tuple[int, int]:
    start = boundary.start_ticks * track.resample_rate // ticks_per_second
    end = boundary.end_ticks * track.resample_rate // ticks_per_second
    return start, end


def deterministic_resample_slice(
    audio: SourceAudio,
    boundary: SliceBoundary,
    track: TrackConfig,
    ticks_per_second: int,
    block_size: int,
) -> AudioSlice:
    source_start = boundary.start_source_sample
    source_end = boundary.end_source_sample
    source = audio.samples[source_start:source_end]
    if not 0 < source.shape[0]:
        raise ValueError("source slice is empty")

    out_start, out_end = output_sample_bounds(boundary, track, ticks_per_second)
    output_count = out_end - out_start
    expected = boundary.output_samples_by_track[track.track_id]
    if output_count != expected:
        raise ValueError(f"output sample count mismatch: {output_count} != {expected}")

    output = np.empty((output_count, audio.channels), dtype="<i2")
    gain = np.float64(10.0 ** (track.gain_db / 20.0))
    chunk = max(1, int(block_size))
    source_count = source.shape[0]
    for chunk_start in range(0, output_count, chunk):
        chunk_end = min(output_count, chunk_start + chunk)
        absolute_output = np.arange(out_start + chunk_start, out_start + chunk_end, dtype=np.int64)
        numerator = absolute_output * audio.sample_rate
        left_index = numerator // track.resample_rate
        remainder = numerator % track.resample_rate
        fraction = (remainder.astype(np.float64) / float(track.resample_rate))[:, None]
        local_left = left_index - source_start
        local_right = np.minimum(local_left + 1, source_count - 1)
        left = source[local_left].astype(np.float64)
        right = source[local_right].astype(np.float64)
        interpolated = left + (right - left) * fraction
        scaled = np.rint(interpolated * gain)
        output[chunk_start:chunk_end] = np.clip(scaled, -32768, 32767).astype("<i2")

    pcm = output.astype("<i2", copy=False).tobytes()
    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as handle:
        handle.setnchannels(audio.channels)
        handle.setsampwidth(2)
        handle.setframerate(track.resample_rate)
        handle.setcomptype("NONE", "not compressed")
        handle.writeframes(pcm)
    data = buffer.getvalue()
    import hashlib

    return AudioSlice(
        data=data,
        sha256=hashlib.sha256(data).hexdigest(),
        start_output_sample=out_start,
        end_output_sample=out_end,
        source_sample_count=source_end - source_start,
        output_sample_count=output_count,
    )


def output_duration_ms(sample_count: int, sample_rate: int) -> float:
    return sample_count * 1000.0 / sample_rate if sample_rate else math.inf

