"""Strict local 16-bit PCM WAV metadata and waveform helpers."""

from __future__ import annotations

import array
import sys
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

from .errors import LCFTError


@dataclass(frozen=True)
class WavInfo:
    path: str
    sample_rate: int
    channels: int
    sample_width: int
    total_samples: int

    @property
    def duration_ticks(self) -> int:
        return self.total_samples

    def to_dict(self) -> dict[str, int | str]:
        return {
            "path": self.path,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "sample_width": self.sample_width,
            "total_samples": self.total_samples,
        }


def inspect_wav(path: str | Path) -> WavInfo:
    target = Path(path)
    try:
        with wave.open(str(target), "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            total_samples = handle.getnframes()
            compression = handle.getcomptype()
    except (OSError, EOFError, wave.Error) as exc:
        raise LCFTError(
            "E_TIMEBASE_INVALID",
            "cannot read PCM WAV header",
            field="audio",
            details={"path": str(target), "reason": str(exc)},
        ) from exc

    if compression != "NONE":
        raise LCFTError(
            "E_TIMEBASE_INVALID",
            "compressed WAV is not supported",
            field="audio.compression",
            details={"compression": compression},
        )
    if sample_rate != 48_000:
        raise LCFTError(
            "E_TIMEBASE_INVALID",
            "audio sample rate must be 48000 Hz",
            field="audio.sample_rate",
            details={"actual": sample_rate, "expected": 48_000},
        )
    if sample_width != 2:
        raise LCFTError(
            "E_TIMEBASE_INVALID",
            "audio must be 16-bit PCM",
            field="audio.bits_per_sample",
            details={"actual_bits": sample_width * 8, "expected_bits": 16},
        )
    if channels not in (1, 6):
        raise LCFTError(
            "E_TIMEBASE_INVALID",
            "audio must be mono or 5.1",
            field="audio.channels",
            details={"actual": channels, "allowed": [1, 6]},
        )
    if total_samples <= 0:
        raise LCFTError(
            "E_TIMEBASE_INVALID",
            "audio contains no sample frames",
            field="audio.total_samples",
            details={"actual": total_samples},
        )
    return WavInfo(
        path=str(target),
        sample_rate=sample_rate,
        channels=channels,
        sample_width=sample_width,
        total_samples=total_samples,
    )


def iter_channel_peaks(
    path: str | Path,
    buckets: int = 1024,
    *,
    max_buckets: int = 8192,
) -> list[dict[str, int]]:
    """Return deterministic peak/RMS buckets without loading the entire file."""

    if buckets < 1 or buckets > max_buckets:
        raise ValueError(f"buckets must be in [1, {max_buckets}]")
    info = inspect_wav(path)
    bucket_size = max(1, (info.total_samples + buckets - 1) // buckets)
    result: list[dict[str, int]] = []
    with wave.open(str(path), "rb") as handle:
        remaining = info.total_samples
        while remaining:
            count = min(bucket_size, remaining)
            raw = handle.readframes(count)
            samples = array.array("h")
            samples.frombytes(raw)
            if sys.byteorder != "little":
                samples.byteswap()
            count_channels = info.channels
            peak = 0
            energy = 0
            for index, sample in enumerate(samples):
                if index % count_channels == 0 or count_channels == 1:
                    magnitude = abs(int(sample))
                    peak = max(peak, magnitude)
                    energy += magnitude * magnitude
            divisor = max(1, len(samples) // count_channels)
            result.append(
                {
                    "peak": peak,
                    "rms": int((energy // divisor) ** 0.5),
                }
            )
            remaining -= count
    return result


def iter_pcm_chunks(path: str | Path, frames_per_chunk: int = 65_536) -> Iterator[bytes]:
    with wave.open(str(path), "rb") as handle:
        while True:
            data = handle.readframes(frames_per_chunk)
            if not data:
                return
            yield data

