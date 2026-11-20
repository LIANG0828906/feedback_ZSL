"""Minimal deterministic 16-bit PCM WAV reader and writer."""

from __future__ import annotations

import hashlib
import os
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import E_IO, E_MIX_INPUT, MixError, input_error


@dataclass
class WavData:
    sample_rate: int
    channels: int
    frames: int
    pcm: np.ndarray
    file_bytes: bytes

    @property
    def source_sha256(self) -> str:
        return hashlib.sha256(self.file_bytes).hexdigest()


def _read_riff_chunks(data: bytes, track_id: str) -> tuple[int, int, int, bytes]:
    if len(data) < 12 or data[0:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise input_error(track_id, "not a RIFF/WAVE file")
    offset = 12
    fmt: tuple[int, int, int, int, int, int] | None = None
    payload = None
    while offset + 8 <= len(data):
        chunk_id = data[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", data, offset + 4)[0]
        body_start = offset + 8
        body_end = body_start + chunk_size
        if body_end > len(data):
            raise input_error(track_id, "truncated WAV chunk")
        body = data[body_start:body_end]
        if chunk_id == b"fmt ":
            if chunk_size < 16:
                raise input_error(track_id, "invalid WAV fmt chunk")
            audio_format, channels, rate, _byte_rate, _block_align, bits = struct.unpack_from(
                "<HHIIHH", body, 0
            )
            fmt = (audio_format, channels, rate, _byte_rate, _block_align, bits)
        elif chunk_id == b"data":
            payload = body
        offset = body_end + (chunk_size & 1)
    if fmt is None or payload is None:
        raise input_error(track_id, "WAV must contain fmt and data chunks")
    audio_format, channels, rate, _byte_rate, block_align, bits = fmt
    if audio_format not in (1, 0xFFFE):
        raise input_error(track_id, "only PCM WAV input is supported", audio_format=audio_format)
    if bits != 16:
        raise input_error(track_id, "only 16-bit PCM input is supported", bit_depth=bits)
    if channels not in (1, 2):
        raise input_error(track_id, "WAV channels must be mono or stereo", channels=channels)
    if rate not in (16000, 48000):
        raise input_error(track_id, "WAV sample rate must be 16000 or 48000", sample_rate=rate)
    if block_align != channels * 2:
        raise input_error(track_id, "WAV block alignment is inconsistent", block_align=block_align)
    if len(payload) % block_align:
        raise input_error(track_id, "WAV data is not a whole number of frames")
    return rate, channels, len(payload) // block_align, payload


def read_wav(path: str | os.PathLike[str], *, track_id: str | None = None) -> WavData:
    label = track_id or str(path)
    try:
        data = Path(path).read_bytes()
    except OSError as exc:
        raise input_error(label, f"cannot read WAV: {exc}") from exc
    rate, channels, frames, payload = _read_riff_chunks(data, label)
    pcm = np.frombuffer(payload, dtype="<i2").reshape(frames, channels)
    return WavData(sample_rate=rate, channels=channels, frames=frames, pcm=pcm, file_bytes=data)


def pcm_bytes(pcm: np.ndarray) -> bytes:
    value = np.asarray(pcm, dtype="<i2")
    if not value.flags.c_contiguous:
        value = np.ascontiguousarray(value)
    return value.tobytes(order="C")


def content_sha256(pcm: np.ndarray) -> str:
    return hashlib.sha256(pcm_bytes(pcm)).hexdigest()


def wav_bytes(pcm: np.ndarray, sample_rate: int, channels: int) -> bytes:
    payload = pcm_bytes(pcm)
    if channels not in (1, 2):
        raise MixError(E_IO, "output WAV channels must be mono or stereo")
    block_align = channels * 2
    byte_rate = sample_rate * block_align
    header = struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        36 + len(payload),
        b"WAVE",
        b"fmt ",
        16,
        1,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        16,
        b"data",
        len(payload),
    )
    return header + payload


def _atomic_replace(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + f".tmp-{os.getpid()}")
    try:
        with temporary.open("wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise MixError(E_IO, f"cannot atomically write WAV: {exc}", details={"path": str(path)}) from exc


def atomic_write_wav(
    path: str | os.PathLike[str],
    pcm: np.ndarray,
    sample_rate: int,
    channels: int,
) -> tuple[str, str]:
    target = Path(path)
    encoded = wav_bytes(pcm, sample_rate, channels)
    _atomic_replace(target, encoded)
    return content_sha256(pcm), hashlib.sha256(encoded).hexdigest()


def sha256_file(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    try:
        with Path(path).open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError as exc:
        raise MixError(E_IO, f"cannot hash file: {exc}", details={"path": str(path)}) from exc
    return digest.hexdigest()
