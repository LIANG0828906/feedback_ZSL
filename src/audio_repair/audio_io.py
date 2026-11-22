"""WAV decoding, immutable audio containers and atomic export."""

from __future__ import annotations

import hashlib
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path

import numpy as np
from scipy.io import wavfile

from .errors import decode_error


@dataclass(frozen=True)
class AudioData:
    samples: np.ndarray
    sample_rate: int
    source_sha256: str
    source_path: str = ""

    def __post_init__(self) -> None:
        samples = np.asarray(self.samples, dtype=np.float64)
        if samples.ndim == 1:
            samples = samples[:, None]
        if samples.ndim != 2:
            raise decode_error("decoded audio must be a 1-D or 2-D sample array")
        if samples.shape[0] <= 0:
            raise decode_error("decoded audio has no samples")
        if samples.shape[1] not in (1, 2):
            raise decode_error(
                "only mono and stereo WAV files are supported",
                channels=int(samples.shape[1]),
            )
        if not np.all(np.isfinite(samples)):
            raise decode_error("decoded audio contains NaN or infinite samples")
        samples.setflags(write=False)
        object.__setattr__(self, "samples", samples)

    @property
    def total_samples(self) -> int:
        return int(self.samples.shape[0])

    @property
    def channels(self) -> int:
        return int(self.samples.shape[1])

    @property
    def duration_seconds(self) -> float:
        return self.total_samples / float(self.sample_rate)

    def slice(self, start: int, end: int) -> np.ndarray:
        return np.array(self.samples[start:end], dtype=np.float64, copy=True)

    def identity_dict(self) -> dict[str, object]:
        return {
            "audio_sha256": self.source_sha256,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "total_samples": self.total_samples,
        }


def _hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _to_float64(data: np.ndarray, dtype: np.dtype) -> np.ndarray:
    if np.issubdtype(dtype, np.floating):
        return np.asarray(data, dtype=np.float64)
    if np.issubdtype(dtype, np.signedinteger):
        denominator = float(abs(np.iinfo(dtype).min))
        return np.asarray(data, dtype=np.float64) / denominator
    if np.issubdtype(dtype, np.unsignedinteger):
        center = float(2 ** (np.dtype(dtype).itemsize * 8 - 1))
        return (np.asarray(data, dtype=np.float64) - center) / center
    raise decode_error("unsupported WAV sample representation", dtype=str(dtype))


def load_audio(path: str | os.PathLike[str]) -> AudioData:
    source = Path(path)
    if not source.is_file():
        raise decode_error("audio file does not exist", path=str(source))
    try:
        file_hash = _hash_file(source)
        sample_rate, raw = wavfile.read(source, mmap=False)
    except Exception as exc:  # noqa: BLE001 - malformed files surface as E_DECODE
        raise decode_error(
            "audio file is damaged or cannot be decoded",
            path=str(source),
            reason=str(exc),
        ) from exc
    if raw.size == 0:
        raise decode_error("audio file contains no samples", path=str(source))
    if int(sample_rate) <= 0:
        raise decode_error(
            "audio file declares an invalid sample rate",
            path=str(source),
            sample_rate=int(sample_rate),
        )
    try:
        samples = _to_float64(raw, raw.dtype)
    except Exception as exc:
        if getattr(exc, "code", None) == "E_DECODE":
            raise
        raise decode_error(
            "audio samples cannot be converted", path=str(source), reason=str(exc)
        ) from exc
    return AudioData(
        samples=samples,
        sample_rate=int(sample_rate),
        source_sha256=file_hash,
        source_path=str(source),
    )


def _fsync_directory(directory: Path) -> None:
    if os.name == "nt":
        return
    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_wav_atomic(
    path: str | os.PathLike[str],
    samples: np.ndarray,
    sample_rate: int,
    *,
    sample_dtype: str = "float32",
) -> Path:
    """Write a WAV through fsync + replace so a partial file is never published."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    data = np.asarray(samples)
    if data.ndim == 1:
        data = data[:, None]
    if data.ndim != 2 or data.shape[1] not in (1, 2):
        raise decode_error("export requires mono or stereo samples", shape=list(data.shape))
    if data.shape[0] == 0:
        raise decode_error("export requires at least one sample")
    if not np.all(np.isfinite(data)):
        raise decode_error("export samples contain NaN or infinite values")
    encoded = np.asarray(data, dtype=np.dtype(sample_dtype))
    if data.shape[0] == 1 and encoded.shape[1] == 1:
        encoded = encoded[:, 0]

    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=str(target.parent)
    )
    os.close(fd)
    temp = Path(temp_name)
    try:
        wavfile.write(temp, int(sample_rate), encoded)
        with temp.open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(temp, target)
        _fsync_directory(target.parent)
    except Exception:
        try:
            temp.unlink(missing_ok=True)
        except OSError:
            pass
        raise
    return target

