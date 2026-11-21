"""Minimal robust 16-bit PCM RIFF/WAVE reader and writer."""

from __future__ import annotations

import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import CalibrationError, E_WAV_INVALID

PCM_GUID = bytes.fromhex("0100000000001000800000aa00389b71")
FIVE_ONE_CHANNEL_MASK = 0x003F
CHANNEL_LAYOUTS: dict[int, tuple[str, ...]] = {
    1: ("M",),
    2: ("L", "R"),
    6: ("FL", "FR", "FC", "LFE", "BL", "BR"),
}


@dataclass(frozen=True)
class WavData:
    sample_rate: int
    channels: np.ndarray
    channel_names: tuple[str, ...]
    source_path: str | None = None
    channel_mask: int = 0
    blocks: int | None = None

    def __post_init__(self) -> None:
        channels = np.asarray(self.channels, dtype=np.float64)
        if channels.ndim != 2:
            raise ValueError("channels must be a 2-D [channel, sample] array")
        if channels.shape[0] != len(self.channel_names):
            raise ValueError("channel_names length does not match channel count")
        channels.setflags(write=False)
        object.__setattr__(self, "channels", channels)

    @property
    def channel_count(self) -> int:
        return int(self.channels.shape[0])

    @property
    def sample_count(self) -> int:
        return int(self.channels.shape[1])

    @property
    def layout_name(self) -> str:
        if self.channel_count == 1:
            return "mono"
        if self.channel_count == 6:
            return "5.1"
        return f"{self.channel_count}ch"


def _invalid(message: str, **details: object) -> CalibrationError:
    return CalibrationError(E_WAV_INVALID, message, dict(details))


def _channel_names(channel_count: int, channel_mask: int) -> tuple[str, ...]:
    if channel_count == 6 and channel_mask in (0, FIVE_ONE_CHANNEL_MASK):
        return CHANNEL_LAYOUTS[6]
    if channel_count in CHANNEL_LAYOUTS:
        return CHANNEL_LAYOUTS[channel_count]
    return tuple(f"CH{index + 1}" for index in range(channel_count))


def read_wav_16(path: str | Path) -> WavData:
    path = Path(path)
    try:
        payload = path.read_bytes()
    except OSError as exc:
        raise _invalid("cannot read WAV file", path=str(path), error=str(exc)) from exc
    if len(payload) < 44 or payload[0:4] != b"RIFF" or payload[8:12] != b"WAVE":
        raise _invalid("not a RIFF/WAVE file", path=str(path))

    fmt_chunk: bytes | None = None
    data_chunk: bytes | None = None
    offset = 12
    while offset + 8 <= len(payload):
        chunk_id = payload[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", payload, offset + 4)[0]
        body_start = offset + 8
        body_end = body_start + chunk_size
        if body_end > len(payload):
            raise _invalid("truncated WAV chunk", chunk=str(chunk_id, "ascii", "replace"))
        body = payload[body_start:body_end]
        if chunk_id == b"fmt ":
            fmt_chunk = body
        elif chunk_id == b"data":
            data_chunk = body
        offset = body_end + (chunk_size & 1)

    if fmt_chunk is None or len(fmt_chunk) < 16:
        raise _invalid("missing fmt chunk")
    if data_chunk is None:
        raise _invalid("missing data chunk")

    audio_format, channel_count, sample_rate, _byte_rate, block_align, bits = struct.unpack_from(
        "<HHIIHH", fmt_chunk, 0
    )
    channel_mask = 0
    valid_bits = bits
    if audio_format == 0xFFFE:
        if len(fmt_chunk) < 40:
            raise _invalid("truncated WAVE_FORMAT_EXTENSIBLE fmt chunk")
        extension_size, valid_bits, channel_mask = struct.unpack_from("<HHI", fmt_chunk, 16)
        if extension_size < 22:
            raise _invalid("invalid WAVE_FORMAT_EXTENSIBLE extension size")
        if fmt_chunk[24:40] != PCM_GUID:
            raise _invalid("only linear PCM WAV is supported", format="0xFFFE")
        audio_format = 1

    if audio_format != 1:
        raise _invalid("only 16-bit linear PCM WAV is supported", audio_format=audio_format)
    if bits != 16 or valid_bits != 16:
        raise _invalid("only 16-bit PCM is supported", bits=bits, valid_bits=valid_bits)
    if sample_rate != 48000:
        raise _invalid("sample rate must be 48000 Hz", sample_rate=sample_rate)
    if channel_count not in (1, 6):
        raise _invalid("only mono or 5.1 channel layouts are supported", channels=channel_count)
    if block_align != channel_count * 2:
        raise _invalid("invalid WAV block alignment", block_align=block_align, channels=channel_count)
    if len(data_chunk) % block_align:
        raise _invalid("data chunk is not aligned to complete sample frames")

    raw = np.frombuffer(data_chunk, dtype="<i2")
    matrix = raw.reshape(-1, channel_count).T.astype(np.float64) / 32768.0
    matrix.setflags(write=False)
    names = _channel_names(channel_count, channel_mask)
    return WavData(
        sample_rate=sample_rate,
        channels=matrix,
        channel_names=names,
        source_path=str(path.resolve()),
        channel_mask=channel_mask,
        blocks=None,
    )


def write_wav_16(path: str | Path, channels: np.ndarray, sample_rate: int = 48000) -> Path:
    matrix = np.asarray(channels, dtype=np.float64)
    if matrix.ndim == 1:
        matrix = matrix.reshape(1, -1)
    if matrix.ndim != 2 or matrix.shape[0] not in (1, 6):
        raise _invalid("only mono or 5.1 channel arrays can be written", shape=list(matrix.shape))
    if sample_rate != 48000:
        raise _invalid("sample rate must be 48000 Hz", sample_rate=sample_rate)
    channel_count = int(matrix.shape[0])
    data = np.clip(np.rint(matrix.T * 32768.0), -32768, 32767).astype("<i2").tobytes()
    if channel_count == 6:
        fmt = struct.pack(
            "<HHIIHHHHI",
            0xFFFE,
            6,
            48000,
            48000 * 12,
            12,
            16,
            22,
            16,
            FIVE_ONE_CHANNEL_MASK,
        ) + PCM_GUID
    else:
        fmt = struct.pack("<HHIIHH", 1, channel_count, sample_rate, sample_rate * channel_count * 2, 2, 16)
    chunks = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    if len(fmt) & 1:
        chunks += b"\x00"
    chunks += b"data" + struct.pack("<I", len(data)) + data
    if len(data) & 1:
        chunks += b"\x00"
    riff_size = 4 + len(chunks)
    payload = b"RIFF" + struct.pack("<I", riff_size) + b"WAVE" + chunks
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_bytes(payload)
    temporary.replace(path)
    return path
