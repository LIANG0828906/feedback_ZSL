"""WAV I/O and deterministic sample-domain helpers."""

from __future__ import annotations

import hashlib
import json
import os
import struct
import tempfile
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import BusinessError


@dataclass(frozen=True)
class WaveInfo:
    sample_rate: int
    channels: int
    bit_depth: int
    frames: int
    format_tag: int
    is_float: bool
    data_offset: int
    data_bytes: int


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str, chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def _parse_wav_header(path: str) -> WaveInfo:
    try:
        size = os.path.getsize(path)
        with open(path, "rb") as handle:
            header = handle.read(12)
            if len(header) != 12 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
                raise BusinessError("E_WAV_FORMAT", path, "not a RIFF/WAVE file")
            fmt: bytes | None = None
            data_offset = 0
            data_bytes = 0
            cursor = 12
            while cursor + 8 <= size:
                handle.seek(cursor)
                chunk_header = handle.read(8)
                if len(chunk_header) != 8:
                    break
                chunk_id = chunk_header[:4]
                chunk_size = struct.unpack("<I", chunk_header[4:])[0]
                payload_offset = cursor + 8
                if chunk_id == b"fmt ":
                    fmt = handle.read(min(chunk_size, 64))
                elif chunk_id == b"data":
                    data_offset = payload_offset
                    data_bytes = min(chunk_size, max(0, size - payload_offset))
                    break
                cursor = payload_offset + chunk_size + (chunk_size & 1)
    except OSError as exc:
        raise BusinessError("E_FILE_READ", path, str(exc)) from exc

    if fmt is None or len(fmt) < 16:
        raise BusinessError("E_WAV_CORRUPT", path, "missing or truncated fmt chunk")
    if data_offset <= 0:
        raise BusinessError("E_WAV_CORRUPT", path, "missing data chunk")

    format_tag, channels, sample_rate, _byte_rate, block_align, bits = struct.unpack(
        "<HHIIHH", fmt[:16]
    )
    if format_tag == 0xFFFE:
        if len(fmt) < 40:
            raise BusinessError("E_WAV_CORRUPT", path, "truncated WAVE_FORMAT_EXTENSIBLE header")
        format_tag = struct.unpack("<H", fmt[24:26])[0]
    if format_tag not in (1, 3):
        raise BusinessError(
            "E_WAV_FORMAT",
            path,
            f"unsupported WAVE format tag {format_tag}; PCM and IEEE float are supported",
        )
    if channels <= 0 or sample_rate <= 0 or bits <= 0 or block_align <= 0:
        raise BusinessError("E_WAV_CORRUPT", path, "invalid fmt chunk values")
    expected_align = channels * ((bits + 7) // 8)
    if block_align != expected_align:
        raise BusinessError(
            "E_WAV_CORRUPT",
            path,
            f"block alignment {block_align} != channels * bytes_per_sample {expected_align}",
        )
    if data_bytes % block_align:
        raise BusinessError("E_WAV_CORRUPT", path, "data chunk is not frame aligned")
    frames = data_bytes // block_align
    return WaveInfo(
        sample_rate=int(sample_rate),
        channels=int(channels),
        bit_depth=int(bits),
        frames=int(frames),
        format_tag=int(format_tag),
        is_float=format_tag == 3,
        data_offset=int(data_offset),
        data_bytes=int(data_bytes),
    )


def inspect_wav(path: str) -> WaveInfo:
    return _parse_wav_header(path)


def read_wav(path: str) -> tuple[np.ndarray, WaveInfo]:
    info = _parse_wav_header(path)
    bytes_per_sample = (info.bit_depth + 7) // 8
    if info.is_float and info.bit_depth not in (32, 64):
        raise BusinessError(
            "E_WAV_FORMAT", path, f"unsupported float bit depth {info.bit_depth}"
        )
    if not info.is_float and info.bit_depth not in (8, 16, 24, 32):
        raise BusinessError("E_WAV_FORMAT", path, f"unsupported PCM bit depth {info.bit_depth}")
    try:
        with open(path, "rb") as handle:
            handle.seek(info.data_offset)
            payload = handle.read(info.data_bytes)
    except OSError as exc:
        raise BusinessError("E_FILE_READ", path, str(exc)) from exc

    if info.is_float:
        dtype = "<f4" if info.bit_depth == 32 else "<f8"
        values = np.frombuffer(payload, dtype=dtype)
        data = values.astype(np.float64)
    elif info.bit_depth == 8:
        values = np.frombuffer(payload, dtype=np.uint8).astype(np.int16)
        data = (values - 128.0) / 128.0
    elif info.bit_depth == 16:
        data = np.frombuffer(payload, dtype="<i2").astype(np.float64) / 32768.0
    elif info.bit_depth == 24:
        raw = np.frombuffer(payload, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        values = raw[:, 0] | (raw[:, 1] << 8) | (raw[:, 2] << 16)
        values = (values ^ 0x800000) - 0x800000
        data = values.astype(np.float64) / 8388608.0
    else:
        data = np.frombuffer(payload, dtype="<i4").astype(np.float64) / 2147483648.0

    if info.channels > 1:
        data = data.reshape(-1, info.channels)
    else:
        data = data.reshape(-1, 1)
    return data, info


def wav_header(sample_rate: int, channels: int, bit_depth: int, frames: int) -> bytes:
    block_align = channels * ((bit_depth + 7) // 8)
    data_bytes = frames * block_align
    return struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        36 + data_bytes,
        b"WAVE",
        b"fmt ",
        16,
        1,
        channels,
        sample_rate,
        sample_rate * block_align,
        block_align,
        bit_depth,
        b"data",
        data_bytes,
    )


def pcm_encode(data: np.ndarray, bit_depth: int) -> bytes:
    values = np.asarray(data, dtype=np.float64)
    if values.ndim != 2:
        raise ValueError("audio data must have shape (frames, channels)")
    if bit_depth == 16:
        quantized = np.rint(np.clip(values, -1.0, 1.0 - 1.0 / 32768.0) * 32768.0)
        return quantized.astype("<i2").tobytes(order="C")
    if bit_depth == 24:
        quantized = np.rint(
            np.clip(values, -1.0, 1.0 - 1.0 / 8388608.0) * 8388608.0
        ).astype(np.int32)
        raw = quantized.astype("<i4").view(np.uint8).reshape(-1, 4)[:, :3]
        return raw.tobytes(order="C")
    if bit_depth == 32:
        quantized = np.rint(
            np.clip(values, -1.0, 1.0 - 1.0 / 2147483648.0) * 2147483648.0
        )
        return quantized.astype("<i4").tobytes(order="C")
    raise ValueError(f"unsupported PCM bit depth {bit_depth}")


def write_wav(path: str, data: np.ndarray, sample_rate: int, bit_depth: int) -> None:
    values = np.asarray(data, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    parent = os.path.dirname(os.path.abspath(path))
    if parent:
        os.makedirs(parent, exist_ok=True)
    payload = wav_header(sample_rate, values.shape[1], bit_depth, values.shape[0])
    payload += pcm_encode(values, bit_depth)
    atomic_write_bytes(path, payload)


def atomic_write_bytes(path: str, payload: bytes) -> None:
    target = os.path.abspath(path)
    parent = os.path.dirname(target)
    os.makedirs(parent, exist_ok=True)
    fd, temp_path = tempfile.mkstemp(prefix=".tmp-", dir=parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, target)
    finally:
        if os.path.exists(temp_path):
            os.unlink(temp_path)


def atomic_write_json(path: str, value: Any) -> None:
    payload = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        indent=2,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, payload)


def db_from_energy(value: float, floor_db: float = -240.0) -> float:
    if value <= 0.0:
        return floor_db
    return float(max(floor_db, 10.0 * np.log10(value)))


def db_from_amplitude(value: float, floor_db: float = -240.0) -> float:
    if value <= 0.0:
        return floor_db
    return float(max(floor_db, 20.0 * np.log10(value)))


def fractional_delay(signal: np.ndarray, delay_samples: float) -> np.ndarray:
    """Delay each channel while preserving the input frame count."""

    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if delay_samples <= 0:
        return values.copy()
    whole = int(np.floor(delay_samples))
    fraction = float(delay_samples - whole)
    output = np.zeros_like(values)
    if whole >= values.shape[0]:
        return output
    if whole == 0:
        output[0] = values[0] * (1.0 - fraction)
        output[1:] = values[1:] * (1.0 - fraction) + values[:-1] * fraction
    else:
        output[whole] = values[0] * (1.0 - fraction)
        output[whole + 1 :] = (
            values[1 : values.shape[0] - whole] * (1.0 - fraction)
            + values[: values.shape[0] - whole - 1] * fraction
        )
    return output


def _k_weighting_coefficients(sample_rate: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    # ITU-R BS.1770 K-weighting, derived for arbitrary sample rates.
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = np.tan(np.pi * f0 / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh ** 0.4996667741545416
    a0 = 1.0 + k / q + k * k
    b1 = np.array(
        [
            (vh + vb * k / q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    a1 = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0],
        dtype=np.float64,
    )

    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = np.tan(np.pi * f0 / sample_rate)
    a0 = 1.0 + k / q + k * k
    b2 = np.array([1.0 / a0, -2.0 / a0, 1.0 / a0], dtype=np.float64)
    a2 = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0],
        dtype=np.float64,
    )
    return b1, a1, b2, a2


def _apply_k_weighting(signal: np.ndarray, sample_rate: int) -> np.ndarray:
    values = np.asarray(signal, dtype=np.float64)
    frames = values.shape[0]
    if frames == 0:
        return values.copy()
    nfft = 1
    target = max(2, frames * 2)
    while nfft < target:
        nfft <<= 1
    frequencies = np.fft.rfftfreq(nfft, d=1.0 / sample_rate)
    z = np.exp(-2j * np.pi * frequencies / sample_rate)
    response = np.ones_like(z)
    for b, a in zip(
        _k_weighting_coefficients(sample_rate)[::2],
        _k_weighting_coefficients(sample_rate)[1::2],
    ):
        response *= (b[0] + b[1] * z + b[2] * z * z) / (
            a[0] + a[1] * z + a[2] * z * z
        )
    output = np.empty_like(values)
    for channel in range(values.shape[1]):
        spectrum = np.fft.rfft(values[:, channel], n=nfft)
        filtered = np.fft.irfft(spectrum * response, n=nfft)[:frames]
        output[:, channel] = filtered
    return output


def integrated_loudness_lufs(signal: np.ndarray, sample_rate: int) -> float:
    """Integrated loudness according to the BS.1770 gating procedure."""

    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    filtered = _apply_k_weighting(values, sample_rate)
    block = max(1, int(round(0.400 * sample_rate)))
    step = max(1, int(round(0.100 * sample_rate)))
    if filtered.shape[0] == 0:
        return float("-inf")
    if filtered.shape[0] < block:
        padded = np.zeros((block, filtered.shape[1]), dtype=np.float64)
        padded[: filtered.shape[0]] = filtered
        filtered = padded
    frame_count = 1 + (filtered.shape[0] - block) // step
    energies = np.empty(frame_count, dtype=np.float64)
    for index in range(frame_count):
        start = index * step
        window = filtered[start : start + block]
        energies[index] = np.mean(np.sum(window * window, axis=1))
    with np.errstate(divide="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(energies)
    absolute = block_loudness > -70.0
    if not np.any(absolute):
        return float("-inf")
    absolute_energy = np.mean(energies[absolute])
    relative_threshold = -0.691 + 10.0 * np.log10(absolute_energy) - 10.0
    gated = absolute & (block_loudness > relative_threshold)
    if not np.any(gated):
        return float("-inf")
    return float(-0.691 + 10.0 * np.log10(np.mean(energies[gated])))


def oversampled_peak(signal: np.ndarray, factor: int = 4) -> float:
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    frames = values.shape[0]
    if frames == 0:
        return 0.0
    nfft = frames * factor
    peak = 0.0
    for channel in range(values.shape[1]):
        spectrum = np.fft.rfft(values[:, channel])
        padded = np.zeros(nfft // 2 + 1, dtype=np.complex128)
        padded[: spectrum.shape[0]] = spectrum
        upsampled = np.fft.irfft(padded, n=nfft) * factor
        peak = max(peak, float(np.max(np.abs(upsampled))))
    return peak


def true_peak_dbtp(signal: np.ndarray, sample_rate: int, factor: int = 4) -> float:
    del sample_rate
    return db_from_amplitude(oversampled_peak(signal, factor))
