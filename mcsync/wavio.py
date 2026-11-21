"""16-bit PCM WAV input and deterministic atomic output."""

from __future__ import annotations

import io
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .atomic import atomic_write_bytes
from .errors import E_WAV_INVALID, SyncError
from .utils import sha256_array


@dataclass
class AudioData:
    samples: np.ndarray
    sample_rate: int
    channel_names: tuple[str, ...]
    source_path: str | None = None
    original_lengths: tuple[int, ...] | None = None

    def __post_init__(self) -> None:
        array = np.asarray(self.samples, dtype=np.float64)
        if array.ndim != 2:
            raise SyncError(
                E_WAV_INVALID, "audio samples must have shape (channels, frames)"
            )
        if self.sample_rate <= 0:
            raise SyncError(E_WAV_INVALID, "sample rate must be positive")
        if array.shape[0] == 0:
            raise SyncError(E_WAV_INVALID, "audio must contain at least one channel")
        if len(self.channel_names) != array.shape[0]:
            raise SyncError(
                E_WAV_INVALID, "channel name count must match channel count"
            )
        self.samples = array

    @property
    def channel_count(self) -> int:
        return int(self.samples.shape[0])

    @property
    def frame_count(self) -> int:
        return int(self.samples.shape[1])

    @property
    def duration_seconds(self) -> float:
        return self.frame_count / float(self.sample_rate)

    def channel(self, index: int) -> np.ndarray:
        if index < 0 or index >= self.channel_count:
            raise SyncError(
                E_WAV_INVALID,
                f"channel index out of range: {index}",
                channel=index,
            )
        return self.samples[index]

    def subset(self, channels: tuple[int, ...]) -> "AudioData":
        names = tuple(self.channel_names[index] for index in channels)
        return AudioData(
            samples=self.samples[list(channels), :],
            sample_rate=self.sample_rate,
            channel_names=names,
            source_path=self.source_path,
            original_lengths=self.original_lengths,
        )

    def content_hash(self) -> str:
        return sha256_array(self.samples)

    def with_samples(
        self,
        samples: np.ndarray,
        *,
        channel_names: tuple[str, ...] | None = None,
    ) -> "AudioData":
        return AudioData(
            samples=samples,
            sample_rate=self.sample_rate,
            channel_names=channel_names or self.channel_names,
            source_path=None,
            original_lengths=None,
        )


def read_multichannel_wav(path: str | Path) -> AudioData:
    source = Path(path)
    try:
        with wave.open(str(source), "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            frame_count = handle.getnframes()
            compression = handle.getcomptype()
            raw = handle.readframes(frame_count)
    except (OSError, wave.Error) as exc:
        raise SyncError(
            E_WAV_INVALID, f"cannot read WAV file: {source}", details={"error": str(exc)}
        ) from exc
    if compression != "NONE":
        raise SyncError(
            E_WAV_INVALID,
            f"unsupported WAV compression: {compression}",
            details={"path": str(source)},
        )
    if sample_width != 2:
        raise SyncError(
            E_WAV_INVALID,
            f"only 16-bit PCM is supported, got {sample_width * 8}-bit",
            details={"path": str(source)},
        )
    expected_bytes = frame_count * channels * 2
    if len(raw) != expected_bytes:
        raise SyncError(
            E_WAV_INVALID,
            "WAV payload length does not match header",
            details={
                "path": str(source),
                "expected_bytes": expected_bytes,
                "actual_bytes": len(raw),
            },
        )
    pcm = np.frombuffer(raw, dtype="<i2")
    if pcm.size == 0:
        interleaved = np.zeros((0, channels), dtype=np.float64)
    else:
        interleaved = pcm.reshape(-1, channels).astype(np.float64) / 32768.0
    samples = np.ascontiguousarray(interleaved.T)
    names = tuple(f"channel_{index}" for index in range(channels))
    return AudioData(
        samples=samples,
        sample_rate=sample_rate,
        channel_names=names,
        source_path=str(source),
    )


def read_channel_directory(path: str | Path) -> AudioData:
    directory = Path(path)
    files = sorted(directory.glob("*.wav"))
    if not files:
        raise SyncError(
            E_WAV_INVALID, f"no WAV files found in channel directory: {directory}"
        )
    channels: list[np.ndarray] = []
    lengths: list[int] = []
    sample_rate: int | None = None
    names: list[str] = []
    for index, file_path in enumerate(files):
        audio = read_multichannel_wav(file_path)
        if audio.channel_count != 1:
            raise SyncError(
                E_WAV_INVALID,
                "channel directory entries must be mono WAV files",
                channel=index,
                details={"path": str(file_path)},
            )
        if sample_rate is None:
            sample_rate = audio.sample_rate
        elif audio.sample_rate != sample_rate:
            raise SyncError(
                E_WAV_INVALID,
                "channel WAV files have inconsistent sample rates",
                details={
                    "expected": sample_rate,
                    "actual": audio.sample_rate,
                    "path": str(file_path),
                },
            )
        channels.append(audio.samples[0])
        lengths.append(audio.frame_count)
        names.append(file_path.stem)
    assert sample_rate is not None
    maximum = max(lengths)
    padded = np.zeros((len(channels), maximum), dtype=np.float64)
    for index, channel in enumerate(channels):
        padded[index, : channel.size] = channel
    return AudioData(
        samples=padded,
        sample_rate=sample_rate,
        channel_names=tuple(names),
        source_path=str(directory),
        original_lengths=tuple(lengths),
    )


def read_audio(path: str | Path) -> AudioData:
    source = Path(path)
    if source.is_dir():
        return read_channel_directory(source)
    return read_multichannel_wav(source)


def pcm16_bytes(samples: np.ndarray) -> bytes:
    array = np.asarray(samples, dtype=np.float64)
    if array.ndim != 2:
        raise SyncError(
            E_WAV_INVALID, "output samples must have shape (channels, frames)"
        )
    clipped = np.clip(array, -1.0, 1.0)
    scaled = np.rint(clipped * 32768.0)
    scaled = np.clip(scaled, -32768, 32767).astype("<i2")
    return np.ascontiguousarray(scaled.T).tobytes(order="C")


def encode_wav_bytes(samples: np.ndarray, sample_rate: int) -> bytes:
    array = np.asarray(samples)
    if array.ndim != 2:
        raise SyncError(
            E_WAV_INVALID, "output samples must have shape (channels, frames)"
        )
    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as handle:
        handle.setnchannels(int(array.shape[0]))
        handle.setsampwidth(2)
        handle.setframerate(int(sample_rate))
        handle.writeframes(pcm16_bytes(array))
    return buffer.getvalue()


def write_multichannel_wav(
    path: str | Path,
    samples: np.ndarray,
    sample_rate: int,
) -> str:
    data = encode_wav_bytes(samples, sample_rate)
    atomic_write_bytes(path, data)
    from .utils import sha256_bytes

    return sha256_bytes(data)

