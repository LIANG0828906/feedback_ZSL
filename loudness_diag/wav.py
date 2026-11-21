"""Strict 5.1 PCM WAV parsing with channel evidence."""

from __future__ import annotations

import hashlib
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import E_HEADER_INVALID, DiagnosticError

CHANNEL_LAYOUTS: dict[int, tuple[str, ...]] = {
    0x003F: ("FL", "FR", "FC", "LFE", "BL", "BR"),
    0x060F: ("FL", "FR", "FC", "LFE", "SL", "SR"),
}

CHANNEL_WEIGHTS: dict[str, float] = {
    "FL": 1.0,
    "FR": 1.0,
    "FC": 1.0,
    "LFE": 0.0,
    "BL": 1.41,
    "BR": 1.41,
    "SL": 1.41,
    "SR": 1.41,
}

_PCM_GUID_TAIL = bytes.fromhex("00001000800000aa00389b71")
_SCHEMA = "loudness-diag/wav/v1"


@dataclass(frozen=True)
class WavInfo:
    source_id: str
    sample_rate: int
    channels: int
    bit_depth: int
    valid_bits: int
    channel_mask: int
    channel_names: tuple[str, ...]
    frame_count: int
    data_bytes: int
    block_align: int
    byte_rate: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "source_id": self.source_id,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "bit_depth": self.bit_depth,
            "valid_bits": self.valid_bits,
            "channel_mask": self.channel_mask,
            "channel_names": list(self.channel_names),
            "frame_count": self.frame_count,
            "data_bytes": self.data_bytes,
            "block_align": self.block_align,
            "byte_rate": self.byte_rate,
        }


@dataclass(frozen=True)
class WavData:
    info: WavInfo
    samples: np.ndarray
    raw_data: bytes
    channel_raw: tuple[bytes, ...]
    channel_hashes: tuple[str, ...]
    content_hash: str
    channel_statistics: tuple[dict[str, Any], ...]

    @property
    def frame_count(self) -> int:
        return self.info.frame_count

    @property
    def channel_weights(self) -> np.ndarray:
        return np.asarray(
            [CHANNEL_WEIGHTS[name] for name in self.info.channel_names],
            dtype=np.float64,
        )

    def channel_bytes_for_window(self, start: int, end: int) -> tuple[bytes, ...]:
        if start < 0 or end < start or end > self.frame_count:
            raise ValueError("window is outside the sample range")
        bytes_per_sample = self.info.bit_depth // 8
        matrix = np.frombuffer(self.raw_data, dtype=np.uint8).reshape(
            self.frame_count, self.info.channels, bytes_per_sample
        )
        return tuple(
            matrix[start:end, channel, :].copy().tobytes()
            for channel in range(self.info.channels)
        )

    def evidence(self) -> dict[str, Any]:
        from .dsp import weighted_downmix

        downmix = weighted_downmix(self.samples)
        downmix_hash = hashlib.sha256(
            np.asarray(downmix, dtype="<f8").tobytes()
        ).hexdigest()
        downmix_peak = (
            float(np.max(np.abs(downmix))) if downmix.size else 0.0
        )
        return {
            **self.info.as_dict(),
            "content_hash": self.content_hash,
            "weighted_downmix": {
                "method": "sum(sqrt(BS1770_power_gain) * channel)",
                "weights": {
                    name: CHANNEL_WEIGHTS[name] for name in self.info.channel_names
                },
                "peak": downmix_peak,
                "content_hash": downmix_hash,
            },
            "channels": [
                {
                    "index": index,
                    "name": name,
                    "sample_count": stat["sample_count"],
                    "peak": stat["peak"],
                    "peak_raw": stat["peak_raw"],
                    "content_hash": stat["content_hash"],
                }
                for index, (name, stat) in enumerate(
                    zip(self.info.channel_names, self.channel_statistics)
                )
            ],
        }


def _header_error(message: str) -> DiagnosticError:
    return DiagnosticError(E_HEADER_INVALID, message)


def _read_chunks(blob: bytes, source_id: str) -> tuple[bytes, bytes]:
    if len(blob) < 12:
        raise _header_error(f"{source_id}: file is shorter than a RIFF header")
    if blob[0:4] != b"RIFF" or blob[8:12] != b"WAVE":
        raise _header_error(f"{source_id}: expected RIFF/WAVE container")

    riff_size = struct.unpack_from("<I", blob, 4)[0]
    if riff_size < 4 or riff_size + 8 > len(blob):
        raise _header_error(f"{source_id}: RIFF size is inconsistent with file length")
    if riff_size + 8 != len(blob):
        raise _header_error(f"{source_id}: trailing bytes make RIFF size ambiguous")

    position = 12
    fmt_data: bytes | None = None
    data_data: bytes | None = None
    while position + 8 <= len(blob):
        chunk_id = blob[position : position + 4]
        chunk_size = struct.unpack_from("<I", blob, position + 4)[0]
        chunk_start = position + 8
        chunk_end = chunk_start + chunk_size
        if chunk_end > len(blob):
            raise _header_error(
                f"{source_id}: chunk {chunk_id!r} extends beyond the file"
            )
        chunk = blob[chunk_start:chunk_end]
        if chunk_id == b"fmt ":
            if fmt_data is not None:
                raise _header_error(f"{source_id}: duplicate fmt chunk")
            fmt_data = chunk
        elif chunk_id == b"data":
            if data_data is not None:
                raise _header_error(f"{source_id}: duplicate data chunk")
            data_data = chunk
        position = chunk_end + (chunk_size & 1)

    if position != len(blob):
        raise _header_error(f"{source_id}: malformed chunk padding")
    if fmt_data is None:
        raise _header_error(f"{source_id}: missing fmt chunk")
    if data_data is None:
        raise _header_error(f"{source_id}: missing data chunk")
    return fmt_data, data_data


def _parse_format(fmt_data: bytes, source_id: str) -> tuple[int, int, int, int, int]:
    if len(fmt_data) < 16:
        raise _header_error(f"{source_id}: fmt chunk is too short")
    (
        audio_format,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bit_depth,
    ) = struct.unpack_from("<HHIIHH", fmt_data, 0)

    if audio_format == 1:
        raise _header_error(
            f"{source_id}: 5.1 PCM requires WAVE_FORMAT_EXTENSIBLE "
            "so the channel mask can be validated"
        )
    if audio_format != 0xFFFE:
        raise _header_error(f"{source_id}: unsupported WAVE format tag {audio_format}")
    if len(fmt_data) < 40:
        raise _header_error(f"{source_id}: extensible fmt chunk is too short")

    extension_size, valid_bits, channel_mask = struct.unpack_from("<HHI", fmt_data, 16)
    if extension_size < 22 or len(fmt_data) < 18 + extension_size:
        raise _header_error(f"{source_id}: invalid extensible fmt payload length")
    guid = fmt_data[24:40]
    if guid[:4] != b"\x01\x00\x00\x00" or guid[4:] != _PCM_GUID_TAIL:
        raise _header_error(f"{source_id}: extensible subformat is not PCM")
    if channels != 6:
        raise _header_error(
            f"{source_id}: expected 6 channels, found {channels}"
        )
    if sample_rate != 48000:
        raise _header_error(
            f"{source_id}: expected 48000 Hz, found {sample_rate}"
        )
    if bit_depth not in (16, 24):
        raise _header_error(
            f"{source_id}: unsupported PCM bit depth {bit_depth}"
        )
    if valid_bits != bit_depth:
        raise _header_error(
            f"{source_id}: valid bits {valid_bits} disagree with container bits {bit_depth}"
        )
    expected_align = channels * (bit_depth // 8)
    if block_align != expected_align:
        raise _header_error(
            f"{source_id}: block alignment {block_align} != {expected_align}"
        )
    expected_rate = sample_rate * block_align
    if byte_rate != expected_rate:
        raise _header_error(
            f"{source_id}: byte rate {byte_rate} != {expected_rate}"
        )
    if channel_mask not in CHANNEL_LAYOUTS:
        raise _header_error(
            f"{source_id}: channel mask 0x{channel_mask:04x} is not a supported 5.1 layout"
        )
    if channel_mask.bit_count() != 6:
        raise _header_error(
            f"{source_id}: channel mask 0x{channel_mask:04x} does not describe 6 channels"
        )
    return bit_depth, valid_bits, channel_mask, block_align, channels


def _decode_pcm(data: bytes, bit_depth: int, channels: int, frames: int) -> np.ndarray:
    if bit_depth == 16:
        integers = np.frombuffer(data, dtype="<i2").reshape(frames, channels)
        return integers.astype(np.float64) / 32768.0

    packed = np.frombuffer(data, dtype=np.uint8).reshape(frames, channels, 3)
    unsigned = (
        packed[:, :, 0].astype(np.int32)
        | (packed[:, :, 1].astype(np.int32) << 8)
        | (packed[:, :, 2].astype(np.int32) << 16)
    )
    signed = np.where(unsigned & 0x800000, unsigned - 0x1000000, unsigned)
    return signed.astype(np.float64) / 8388608.0


def _channel_raw_from_data(
    data: bytes, bit_depth: int, channels: int, frames: int
) -> tuple[bytes, ...]:
    bytes_per_sample = bit_depth // 8
    matrix = np.frombuffer(data, dtype=np.uint8).reshape(
        frames, channels, bytes_per_sample
    )
    return tuple(matrix[:, channel, :].copy().tobytes() for channel in range(channels))


def _asset_content_hash(channel_raw: tuple[bytes, ...]) -> str:
    digest = hashlib.sha256()
    digest.update(_SCHEMA.encode("ascii"))
    digest.update(b"\0")
    digest.update(struct.pack("<I", len(channel_raw)))
    for index, raw in enumerate(channel_raw):
        digest.update(struct.pack("<II", index, len(raw)))
        digest.update(raw)
    return digest.hexdigest()


def block_content_hash(channel_raw: tuple[bytes, ...]) -> str:
    """Hash raw, channel-major bytes for one observation window."""

    digest = hashlib.sha256()
    digest.update(b"loudness-diag/block/v1\0")
    digest.update(struct.pack("<I", len(channel_raw)))
    for index, raw in enumerate(channel_raw):
        digest.update(struct.pack("<II", index, len(raw)))
        digest.update(raw)
    return digest.hexdigest()


def read_wav_bytes(blob: bytes, *, source_id: str = "memory.wav") -> WavData:
    fmt_data, data = _read_chunks(blob, source_id)
    bit_depth, valid_bits, channel_mask, block_align, channels = _parse_format(
        fmt_data, source_id
    )
    if len(data) == 0:
        raise _header_error(f"{source_id}: data chunk is empty")
    if len(data) % block_align != 0:
        raise _header_error(
            f"{source_id}: data length {len(data)} is not a whole number of frames"
        )

    frames = len(data) // block_align
    channel_names = CHANNEL_LAYOUTS[channel_mask]
    samples = _decode_pcm(data, bit_depth, channels, frames)
    channel_raw = _channel_raw_from_data(data, bit_depth, channels, frames)
    channel_hashes = tuple(hashlib.sha256(raw).hexdigest() for raw in channel_raw)
    scale = float(1 << (bit_depth - 1))

    statistics: list[dict[str, Any]] = []
    for channel, raw in enumerate(channel_raw):
        peak = float(np.max(np.abs(samples[:, channel]))) if frames else 0.0
        statistics.append(
            {
                "sample_count": frames,
                "peak": peak,
                "peak_raw": round(peak * scale),
                "content_hash": channel_hashes[channel],
                "raw_bytes": len(raw),
            }
        )

    info = WavInfo(
        source_id=source_id,
        sample_rate=48000,
        channels=channels,
        bit_depth=bit_depth,
        valid_bits=valid_bits,
        channel_mask=channel_mask,
        channel_names=channel_names,
        frame_count=frames,
        data_bytes=len(data),
        block_align=block_align,
        byte_rate=48000 * block_align,
    )
    return WavData(
        info=info,
        samples=samples,
        raw_data=data,
        channel_raw=channel_raw,
        channel_hashes=channel_hashes,
        content_hash=_asset_content_hash(channel_raw),
        channel_statistics=tuple(statistics),
    )


def read_wav(path: str | Path, *, source_id: str | None = None) -> WavData:
    path = Path(path)
    return read_wav_bytes(
        path.read_bytes(),
        source_id=source_id if source_id is not None else path.name,
    )

