"""RIFF/WAVE parsing, PCM decoding, deterministic writing, and downmix."""

from __future__ import annotations

import hashlib
import os
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .config import SUPPORTED_BITS
from .errors import ErrorCode, MeterError, error


SPEAKER_FRONT_LEFT = 0x0001
SPEAKER_FRONT_RIGHT = 0x0002
SPEAKER_FRONT_CENTER = 0x0004
SPEAKER_LOW_FREQUENCY = 0x0008
SPEAKER_BACK_LEFT = 0x0010
SPEAKER_BACK_RIGHT = 0x0020
SPEAKER_SIDE_LEFT = 0x0200
SPEAKER_SIDE_RIGHT = 0x0400


@dataclass(frozen=True)
class ChannelLayout:
    name: str
    mask: int
    labels: tuple[str, ...]

    @property
    def weights(self) -> tuple[float, ...]:
        labels = self.labels
        if len(labels) == 1:
            return (1.0,)
        result: list[float] = []
        for label in labels:
            if label == "LFE":
                result.append(0.0)
            elif label in {"BL", "BR", "SL", "SR"}:
                result.append(1.41)
            else:
                result.append(1.0)
        return tuple(result)


MASK_LAYOUTS: dict[int, ChannelLayout] = {
    0x0004: ChannelLayout("mono", 0x0004, ("FC",)),
    0x0003: ChannelLayout("stereo", 0x0003, ("FL", "FR")),
    0x0007: ChannelLayout("3.0", 0x0007, ("FL", "FR", "FC")),
    0x000B: ChannelLayout("2.1", 0x000B, ("FL", "FR", "LFE")),
    0x0033: ChannelLayout("quad", 0x0033, ("FL", "FR", "BL", "BR")),
    0x0037: ChannelLayout("5.0", 0x0037, ("FL", "FR", "FC", "BL", "BR")),
    0x0607: ChannelLayout("5.0-side", 0x0607, ("FL", "FR", "FC", "SL", "SR")),
    0x003F: ChannelLayout("5.1", 0x003F, ("FL", "FR", "FC", "LFE", "BL", "BR")),
}

COUNT_LAYOUTS: dict[int, ChannelLayout] = {
    1: MASK_LAYOUTS[0x0004],
    2: MASK_LAYOUTS[0x0003],
    3: MASK_LAYOUTS[0x0007],
    4: MASK_LAYOUTS[0x0033],
    5: MASK_LAYOUTS[0x0037],
    6: MASK_LAYOUTS[0x003F],
}


@dataclass(frozen=True)
class WavInfo:
    path: str
    channels: int
    sample_rate: int
    bits_per_sample: int
    bytes_per_sample: int
    block_align: int
    channel_mask: int
    layout: str
    labels: tuple[str, ...]
    num_frames: int
    data_offset: int
    data_size: int
    extensible: bool

    @property
    def duration_seconds(self) -> float:
        return self.num_frames / self.sample_rate

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "channels": self.channels,
            "sample_rate": self.sample_rate,
            "bits_per_sample": self.bits_per_sample,
            "channel_mask": self.channel_mask,
            "layout": self.layout,
            "labels": list(self.labels),
            "num_frames": self.num_frames,
            "data_size": self.data_size,
            "extensible": self.extensible,
        }


@dataclass(frozen=True)
class DownmixResult:
    """BS.1770-weighted channel signals.

    BS.1770 is a weighted power sum, not a destructive mono sum.  Keeping the
    weighted channel signals avoids phase cancellation while retaining the
    exact channel weights.  ``mono_preview`` is offered only for display.
    """

    signals: np.ndarray
    true_peak_signals: np.ndarray
    weights: tuple[float, ...]
    labels: tuple[str, ...]

    @property
    def mono_preview(self) -> np.ndarray:
        return self.signals.sum(axis=1, dtype=np.float64)


def _decode_channel_mask(
    channels: int,
    channel_mask: int,
    *,
    extensible: bool,
) -> ChannelLayout:
    if channel_mask:
        layout = MASK_LAYOUTS.get(channel_mask)
    else:
        layout = COUNT_LAYOUTS.get(channels)
    if layout is None or len(layout.labels) != channels:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "unsupported channel mask",
            {
                "channels": channels,
                "channel_mask": channel_mask,
                "extensible": extensible,
                "supported_masks": sorted(MASK_LAYOUTS),
            },
        )
    return layout


def parse_wav(path: str | Path) -> WavInfo:
    path_str = str(path)
    try:
        file_size = os.path.getsize(path)
        with open(path, "rb") as handle:
            header = handle.read(12)
            if len(header) != 12:
                raise ValueError("truncated RIFF header")
            riff, riff_size, wave = struct.unpack("<4sI4s", header)
            if riff != b"RIFF" or wave != b"WAVE":
                raise ValueError("not a RIFF/WAVE file")
            if riff_size + 8 > file_size:
                raise ValueError("RIFF size exceeds file size")
            fmt_data: bytes | None = None
            data_offset = -1
            data_size = -1
            while True:
                chunk_header = handle.read(8)
                if not chunk_header:
                    break
                if len(chunk_header) != 8:
                    raise ValueError("truncated chunk header")
                chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
                payload_start = handle.tell()
                if payload_start + chunk_size > file_size:
                    raise ValueError("chunk exceeds file size")
                if chunk_id == b"fmt ":
                    fmt_data = handle.read(chunk_size)
                elif chunk_id == b"data":
                    data_offset = payload_start
                    data_size = chunk_size
                    handle.seek(chunk_size, os.SEEK_CUR)
                else:
                    handle.seek(chunk_size, os.SEEK_CUR)
                if chunk_size & 1:
                    handle.seek(1, os.SEEK_CUR)
            if fmt_data is None or data_offset < 0:
                raise ValueError("missing fmt or data chunk")
    except FileNotFoundError:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "WAV file does not exist",
            {"path": path_str},
        ) from None
    except (OSError, ValueError, struct.error) as exc:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "invalid WAV file",
            {"path": path_str, "error": str(exc)},
        ) from None

    if len(fmt_data) < 16:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "fmt chunk is too short",
            {"path": path_str},
        )
    (
        audio_format,
        channels,
        sample_rate,
        _byte_rate,
        block_align,
        bits_per_sample,
    ) = struct.unpack("<HHIIHH", fmt_data[:16])
    extensible = audio_format == 0xFFFE
    channel_mask = 0
    if extensible:
        if len(fmt_data) < 40:
            raise error(
                ErrorCode.E_AUDIO_FORMAT,
                "extensible fmt chunk is too short",
                {"path": path_str},
            )
        cb_size, valid_bits, channel_mask = struct.unpack("<HHI", fmt_data[16:24])
        subformat = fmt_data[24:40]
        if cb_size < 22 or subformat[:2] != b"\x01\x00":
            raise error(
                ErrorCode.E_AUDIO_FORMAT,
                "only integer PCM WAVE_FORMAT_EXTENSIBLE is supported",
                {"path": path_str, "subformat": subformat.hex()},
            )
        if valid_bits not in (0, bits_per_sample):
            raise error(
                ErrorCode.E_CHANNEL_UNSUPPORTED,
                "container and valid bits differ",
                {"bits_per_sample": bits_per_sample, "valid_bits": valid_bits},
            )
    elif audio_format != 1:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "only integer PCM is supported",
            {"path": path_str, "audio_format": audio_format},
        )

    if sample_rate not in (48_000,):
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "only 48 kHz audio is supported",
            {"path": path_str, "sample_rate": sample_rate},
        )
    if bits_per_sample not in SUPPORTED_BITS:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "only 16-bit and 24-bit PCM are supported",
            {"path": path_str, "bits_per_sample": bits_per_sample},
        )
    if channels < 1 or channels > 6:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "only mono through 5.1 layouts are supported",
            {"path": path_str, "channels": channels},
        )
    bytes_per_sample = bits_per_sample // 8
    expected_align = channels * bytes_per_sample
    if block_align != expected_align:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "invalid block alignment",
            {
                "path": path_str,
                "block_align": block_align,
                "expected": expected_align,
            },
        )
    if data_size % block_align:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "data chunk is not frame aligned",
            {"path": path_str, "data_size": data_size, "block_align": block_align},
        )
    layout = _decode_channel_mask(
        channels,
        channel_mask,
        extensible=extensible,
    )
    return WavInfo(
        path=path_str,
        channels=channels,
        sample_rate=sample_rate,
        bits_per_sample=bits_per_sample,
        bytes_per_sample=bytes_per_sample,
        block_align=block_align,
        channel_mask=channel_mask or layout.mask,
        layout=layout.name,
        labels=layout.labels,
        num_frames=data_size // block_align,
        data_offset=data_offset,
        data_size=data_size,
        extensible=extensible,
    )


def decode_pcm_bytes(
    payload: bytes,
    *,
    bits_per_sample: int,
    channels: int,
) -> np.ndarray:
    if bits_per_sample == 16:
        values = np.frombuffer(payload, dtype="<i2").astype(np.float64)
        values /= 32768.0
    elif bits_per_sample == 24:
        raw = np.frombuffer(payload, dtype=np.uint8).reshape(-1, 3)
        values = (
            raw[:, 0].astype(np.int32)
            | (raw[:, 1].astype(np.int32) << 8)
            | (raw[:, 2].astype(np.int32) << 16)
        )
        values = np.where(values & 0x800000, values - 0x1000000, values)
        values = values.astype(np.float64) / 8388608.0
    else:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "unsupported PCM bit depth",
            {"bits_per_sample": bits_per_sample},
        )
    if channels < 1 or values.size % channels:
        raise error(
            ErrorCode.E_AUDIO_FORMAT,
            "PCM payload is not channel aligned",
            {"samples": int(values.size), "channels": channels},
        )
    return values.reshape(-1, channels)


class WaveAsset:
    """Random-access local WAV reader with no retained file handle."""

    def __init__(self, path: str | Path):
        self.path = Path(path)
        self.info = parse_wav(self.path)

    @property
    def frames(self) -> int:
        return self.info.num_frames

    def read_frames(self, start: int, end: int) -> np.ndarray:
        if start < 0 or end < start or end > self.info.num_frames:
            raise error(
                ErrorCode.E_BLOCK_NOT_READY,
                "frame range is outside the asset",
                {
                    "start": start,
                    "end": end,
                    "num_frames": self.info.num_frames,
                },
            )
        if start == end:
            return np.empty((0, self.info.channels), dtype=np.float64)
        offset = self.info.data_offset + start * self.info.block_align
        byte_count = (end - start) * self.info.block_align
        with open(self.path, "rb") as handle:
            handle.seek(offset)
            payload = handle.read(byte_count)
        if len(payload) != byte_count:
            raise error(
                ErrorCode.E_AUDIO_FORMAT,
                "short read from WAV data chunk",
                {"path": str(self.path), "expected": byte_count, "actual": len(payload)},
            )
        return decode_pcm_bytes(
            payload,
            bits_per_sample=self.info.bits_per_sample,
            channels=self.info.channels,
        )

    def read_raw(self, start: int, end: int) -> bytes:
        if start < 0 or end < start or end > self.info.num_frames:
            raise error(
                ErrorCode.E_BLOCK_NOT_READY,
                "frame range is outside the asset",
                {"start": start, "end": end},
            )
        offset = self.info.data_offset + start * self.info.block_align
        byte_count = (end - start) * self.info.block_align
        with open(self.path, "rb") as handle:
            handle.seek(offset)
            return handle.read(byte_count)

    def content_hash(self, start: int, end: int) -> str:
        digest = hashlib.sha256()
        digest.update(self.read_raw(start, end))
        return digest.hexdigest()

    def read_all(self) -> np.ndarray:
        return self.read_frames(0, self.info.num_frames)

    def close(self) -> None:
        # Reads are intentionally short-lived; this method exists so callers can
        # drop a handle in a uniform pause/seek/restart path.
        pass


def downmix(
    samples: np.ndarray,
    info: WavInfo,
    *,
    channel_weights: Sequence[float] | None = None,
    true_peak_include_lfe: bool = True,
) -> DownmixResult:
    if samples.ndim != 2 or samples.shape[1] != info.channels:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "sample channel count does not match WAV metadata",
            {"shape": list(samples.shape), "channels": info.channels},
        )
    if channel_weights is None:
        weights = ChannelLayout(
            info.layout,
            info.channel_mask,
            info.labels,
        ).weights
    else:
        weights = tuple(float(x) for x in channel_weights)
    if len(weights) != info.channels:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "channel weight count does not match layout",
            {
                "channel_weights": list(weights),
                "channels": info.channels,
                "layout": info.layout,
            },
        )
    if any((not np.isfinite(w)) or w < 0 for w in weights):
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "channel weights must be finite and non-negative",
            {"channel_weights": list(weights)},
        )
    gains = np.sqrt(np.asarray(weights, dtype=np.float64))
    weighted = np.asarray(samples, dtype=np.float64) * gains[None, :]
    true_peak_gains = gains.copy()
    if true_peak_include_lfe:
        for index, label in enumerate(info.labels):
            if label == "LFE" and true_peak_gains[index] == 0.0:
                true_peak_gains[index] = 1.0
    return DownmixResult(
        signals=weighted,
        true_peak_signals=np.asarray(samples, dtype=np.float64)
        * true_peak_gains[None, :],
        weights=weights,
        labels=info.labels,
    )


def _pcm_payload(samples: np.ndarray, bits_per_sample: int) -> bytes:
    samples = np.asarray(samples, dtype=np.float64)
    if bits_per_sample == 16:
        scale = 32768.0
        values = np.rint(samples * scale).astype(np.int64)
        values = np.clip(values, -32768, 32767).astype("<i2")
        return values.tobytes(order="C")
    if bits_per_sample == 24:
        scale = 8388608.0
        values = np.rint(samples * scale).astype(np.int64)
        values = np.clip(values, -8388608, 8388607)
        unsigned = (values.reshape(-1) & 0xFFFFFF).astype(np.uint32)
        packed = np.empty(values.size * 3, dtype=np.uint8)
        packed[0::3] = (unsigned & 0xFF).astype(np.uint8)
        packed[1::3] = ((unsigned >> 8) & 0xFF).astype(np.uint8)
        packed[2::3] = ((unsigned >> 16) & 0xFF).astype(np.uint8)
        return packed.tobytes(order="C")
    raise error(
        ErrorCode.E_AUDIO_FORMAT,
        "only 16-bit and 24-bit PCM can be written",
        {"bits_per_sample": bits_per_sample},
    )


def write_wav(
    path: str | Path,
    samples: np.ndarray,
    *,
    sample_rate: int,
    bits_per_sample: int,
    channel_mask: int,
    labels: Sequence[str] | None = None,
) -> WavInfo:
    """Write a deterministic WAVE_FORMAT_EXTENSIBLE PCM file."""
    path = Path(path)
    samples = np.asarray(samples, dtype=np.float64)
    if samples.ndim != 2:
        raise error(ErrorCode.E_AUDIO_FORMAT, "samples must be a 2-D array")
    channels = int(samples.shape[1])
    if sample_rate != 48_000:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "sample writer only supports 48 kHz",
            {"sample_rate": sample_rate},
        )
    layout = MASK_LAYOUTS.get(channel_mask)
    if layout is None or len(layout.labels) != channels:
        raise error(
            ErrorCode.E_CHANNEL_UNSUPPORTED,
            "unsupported channel mask for writer",
            {"channels": channels, "channel_mask": channel_mask},
        )
    if labels is not None and tuple(labels) != layout.labels:
        raise error(
            ErrorCode.E_CONFIG_INVALID,
            "labels do not match channel mask",
            {"labels": list(labels), "expected": list(layout.labels)},
        )
    bytes_per_sample = bits_per_sample // 8
    block_align = channels * bytes_per_sample
    payload = _pcm_payload(samples, bits_per_sample)
    guid = bytes.fromhex("0100000000001000800000aa00389b71")
    fmt = struct.pack(
        "<HHIIHHHHI",
        0xFFFE,
        channels,
        sample_rate,
        sample_rate * block_align,
        block_align,
        bits_per_sample,
        22,
        bits_per_sample,
        channel_mask,
    ) + guid
    chunks = bytearray()
    chunks.extend(b"fmt ")
    chunks.extend(struct.pack("<I", len(fmt)))
    chunks.extend(fmt)
    if len(fmt) & 1:
        chunks.append(0)
    chunks.extend(b"data")
    chunks.extend(struct.pack("<I", len(payload)))
    chunks.extend(payload)
    if len(payload) & 1:
        chunks.append(0)
    riff_size = 4 + len(chunks)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as handle:
        handle.write(struct.pack("<4sI4s", b"RIFF", riff_size, b"WAVE"))
        handle.write(chunks)
        handle.flush()
        os.fsync(handle.fileno())
    return parse_wav(path)
