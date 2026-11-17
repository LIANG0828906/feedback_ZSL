from __future__ import annotations

import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    bit_depth: int
    channels: int
    frames: int
    format_tag: int


@dataclass(frozen=True)
class WavData:
    samples: np.ndarray
    info: WavInfo


class WavFormatError(ValueError):
    pass


def read_wav(path: str | Path) -> WavData:
    source = Path(path)
    payload = source.read_bytes()
    if len(payload) < 12 or payload[:4] != b"RIFF" or payload[8:12] != b"WAVE":
        raise WavFormatError("not a RIFF/WAVE file")

    cursor = 12
    fmt: bytes | None = None
    data: bytes | None = None
    while cursor + 8 <= len(payload):
        chunk_id = payload[cursor : cursor + 4]
        chunk_size = struct.unpack_from("<I", payload, cursor + 4)[0]
        chunk_start = cursor + 8
        chunk_end = chunk_start + chunk_size
        if chunk_end > len(payload):
            raise WavFormatError(f"truncated {chunk_id!r} chunk")
        if chunk_id == b"fmt ":
            fmt = payload[chunk_start:chunk_end]
        elif chunk_id == b"data":
            data = payload[chunk_start:chunk_end]
        cursor = chunk_end + (chunk_size & 1)

    if fmt is None or len(fmt) < 16:
        raise WavFormatError("missing fmt chunk")
    if data is None:
        raise WavFormatError("missing data chunk")

    format_tag, channels, sample_rate, _, block_align, bit_depth = struct.unpack_from(
        "<HHIIHH", fmt, 0
    )
    if format_tag == 0xFFFE:
        if len(fmt) < 40:
            raise WavFormatError("truncated WAVE_FORMAT_EXTENSIBLE header")
        sub_format = fmt[24:40]
        format_tag = struct.unpack_from("<H", sub_format, 0)[0]
    if format_tag != 1:
        raise WavFormatError(f"unsupported WAV format tag {format_tag}")
    if channels < 1:
        raise WavFormatError("channel count must be positive")
    if bit_depth not in (16, 24, 32):
        raise WavFormatError(f"unsupported bit depth {bit_depth}")
    bytes_per_sample = bit_depth // 8
    expected_align = channels * bytes_per_sample
    if block_align != expected_align:
        raise WavFormatError("invalid block alignment")
    if len(data) % expected_align:
        raise WavFormatError("data chunk length is not sample aligned")

    raw = np.frombuffer(data, dtype=np.uint8)
    if bit_depth == 16:
        values = raw.view("<i2").astype(np.float64)
        samples = values / 32768.0
    elif bit_depth == 24:
        triplets = raw.reshape(-1, 3).astype(np.int32)
        values = triplets[:, 0] | (triplets[:, 1] << 8) | (triplets[:, 2] << 16)
        values = np.where(values & 0x800000, values - 0x1000000, values)
        samples = values.astype(np.float64) / 8388608.0
    else:
        values = raw.view("<i4").astype(np.float64)
        samples = values / 2147483648.0

    frames = samples.size // channels
    samples = samples.reshape(frames, channels)
    info = WavInfo(
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        channels=channels,
        frames=frames,
        format_tag=format_tag,
    )
    return WavData(samples=samples, info=info)


def quantize_pcm(samples: np.ndarray, bit_depth: int) -> np.ndarray:
    if bit_depth not in (16, 24, 32):
        raise WavFormatError(f"unsupported bit depth {bit_depth}")
    scale = float(1 << (bit_depth - 1))
    lower = -int(scale)
    upper = int(scale) - 1
    values = np.rint(np.asarray(samples, dtype=np.float64) * scale)
    values = np.clip(values, lower, upper).astype(np.int32)
    return values


def pcm_bytes(values: np.ndarray, bit_depth: int) -> bytes:
    values = np.asarray(values, dtype=np.int32)
    if bit_depth == 16:
        return values.astype("<i2").tobytes(order="C")
    if bit_depth == 24:
        flat = values.reshape(-1)
        unsigned = flat & 0xFFFFFF
        encoded = np.empty((unsigned.size, 3), dtype=np.uint8)
        encoded[:, 0] = unsigned & 0xFF
        encoded[:, 1] = (unsigned >> 8) & 0xFF
        encoded[:, 2] = (unsigned >> 16) & 0xFF
        return encoded.tobytes(order="C")
    if bit_depth == 32:
        return values.astype("<i4").tobytes(order="C")
    raise WavFormatError(f"unsupported bit depth {bit_depth}")


def write_wav(
    path: str | Path,
    samples: np.ndarray,
    sample_rate: int,
    bit_depth: int,
) -> None:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.ndim != 2:
        raise WavFormatError("samples must be 1-D or 2-D")
    channels = values.shape[1]
    byte_depth = bit_depth // 8
    data = pcm_bytes(quantize_pcm(values, bit_depth), bit_depth)
    block_align = channels * byte_depth
    byte_rate = sample_rate * block_align
    header = wav_header(
        frames=values.shape[0],
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        channels=channels,
    )
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(header + data)


def wav_header(
    frames: int,
    sample_rate: int,
    bit_depth: int,
    channels: int,
) -> bytes:
    byte_depth = bit_depth // 8
    block_align = channels * byte_depth
    byte_rate = sample_rate * block_align
    data_size = int(frames) * block_align
    riff_size = 36 + data_size
    return b"".join(
        (
            b"RIFF",
            struct.pack("<I", riff_size),
            b"WAVE",
            b"fmt ",
            struct.pack("<I", 16),
            struct.pack(
                "<HHIIHH",
                1,
                channels,
                sample_rate,
                byte_rate,
                block_align,
                bit_depth,
            ),
            b"data",
            struct.pack("<I", data_size),
        )
    )


def pcm_frame_bytes(values: np.ndarray) -> int:
    return int(values.shape[1]) * 3

