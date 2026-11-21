from __future__ import annotations

import math
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import E_WAV_INVALID, E_WAV_UNSUPPORTED, AppError
from .util import sha256_file

PCM = 0x0001
WAVE_FORMAT_EXTENSIBLE = 0xFFFE
PCM_SUBFORMAT_GUID = bytes.fromhex("0100000000001000800000aa00389b71")

CHANNEL_NAMES_BY_MASK: dict[int, tuple[str, ...]] = {
    0x4: ("FC",),
    0x3F: ("FL", "FR", "FC", "LFE", "BL", "BR"),
    0x60F: ("FL", "FR", "FC", "LFE", "SL", "SR"),
}

LAYOUT_BY_MASK: dict[int, str] = {
    0x4: "MONO",
    0x3F: "5.1",
    0x60F: "5.1_SIDE",
}

DEFAULT_LAYOUTS: dict[str, tuple[int, tuple[str, ...]]] = {
    "MONO": (0x4, ("FC",)),
    "5.1": (0x3F, ("FL", "FR", "FC", "LFE", "BL", "BR")),
    "5.1_SIDE": (0x60F, ("FL", "FR", "FC", "LFE", "SL", "SR")),
}


@dataclass(frozen=True)
class WavInfo:
    path: str
    sample_rate: int
    channels: int
    bits_per_sample: int
    channel_mask: int
    channel_layout: str
    channel_names: tuple[str, ...]
    channel_mask_source: str
    frames: int
    data_size: int
    block_align: int
    format_tag: int
    audio: np.ndarray
    file_sha256: str

    @property
    def duration_seconds(self) -> float:
        return self.frames / self.sample_rate

    @property
    def byte_size(self) -> int:
        return self.audio.nbytes

    def summary(self) -> dict[str, object]:
        return {
            "path": self.path,
            "file_sha256": self.file_sha256,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "bits_per_sample": self.bits_per_sample,
            "channel_mask": f"0x{self.channel_mask:04X}",
            "channel_layout": self.channel_layout,
            "channel_names": list(self.channel_names),
            "channel_mask_source": self.channel_mask_source,
            "frames": self.frames,
            "duration_seconds": self.duration_seconds,
            "data_size": self.data_size,
            "block_align": self.block_align,
        }


def _invalid(field: str, message: str) -> AppError:
    return AppError(E_WAV_INVALID, message, field=field)


def _unsupported(field: str, message: str) -> AppError:
    return AppError(E_WAV_UNSUPPORTED, message, field=field)


def _popcount(value: int) -> int:
    return value.bit_count()


def _decode_pcm(raw: bytes, bits: int, channels: int) -> np.ndarray:
    if bits == 16:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64)
        scale = 32768.0
    elif bits == 24:
        packed = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3)
        values = (
            packed[:, 0].astype(np.int32)
            | (packed[:, 1].astype(np.int32) << 8)
            | (packed[:, 2].astype(np.int32) << 16)
        )
        values = np.where(values & 0x800000, values - 0x1000000, values)
        values = values.astype(np.float64)
        scale = 8388608.0
    else:
        raise _unsupported("fmt.bits_per_sample", "only 16-bit and 24-bit PCM are supported")
    return (values / scale).reshape(-1, channels)


def read_wav(path: str | Path) -> WavInfo:
    source = Path(path)
    try:
        blob = source.read_bytes()
    except OSError as exc:
        raise _invalid("path", f"cannot read WAV file: {exc}") from exc
    if len(blob) < 12:
        raise _invalid("RIFF", "file is shorter than a RIFF header")
    if blob[0:4] != b"RIFF":
        raise _invalid("RIFF", "missing RIFF signature")
    if blob[8:12] != b"WAVE":
        raise _invalid("WAVE", "RIFF form is not WAVE")
    riff_size = struct.unpack_from("<I", blob, 4)[0]
    if riff_size + 8 != len(blob):
        raise _invalid(
            "RIFF.size",
            f"RIFF length {riff_size + 8} does not match file length {len(blob)}",
        )

    fmt_chunk: bytes | None = None
    data_chunk: bytes | None = None
    offset = 12
    while offset + 8 <= len(blob):
        chunk_id = blob[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", blob, offset + 4)[0]
        body_start = offset + 8
        body_end = body_start + chunk_size
        if body_end > len(blob):
            raise _invalid(
                f"chunk.{chunk_id.decode('ascii', 'replace')}",
                "chunk length exceeds the file boundary",
            )
        body = blob[body_start:body_end]
        if chunk_id == b"fmt ":
            if fmt_chunk is not None:
                raise _invalid("fmt", "duplicate fmt chunk")
            fmt_chunk = body
        elif chunk_id == b"data":
            if data_chunk is not None:
                raise _invalid("data", "multiple data chunks are not supported")
            data_chunk = body
        offset = body_end + (chunk_size & 1)

    if fmt_chunk is None:
        raise _invalid("fmt", "missing fmt chunk")
    if data_chunk is None:
        raise _invalid("data", "missing data chunk")
    if len(fmt_chunk) < 16:
        raise _invalid("fmt.cbSize", "fmt chunk is too short")

    (
        format_tag,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bits_per_sample,
    ) = struct.unpack_from("<HHIIHH", fmt_chunk, 0)

    if sample_rate != 48000:
        raise _unsupported("fmt.sample_rate", "only 48 kHz WAV files are supported")
    if channels not in (1, 6):
        raise _unsupported("fmt.channels", "only mono and 5.1 WAV files are supported")
    if bits_per_sample not in (16, 24):
        raise _unsupported("fmt.bits_per_sample", "only 16-bit and 24-bit PCM are supported")

    bytes_per_sample = bits_per_sample // 8
    expected_align = channels * bytes_per_sample
    if block_align != expected_align:
        raise _invalid(
            "fmt.block_align",
            f"block alignment {block_align} != channels*bytes_per_sample {expected_align}",
        )
    if byte_rate != sample_rate * block_align:
        raise _invalid(
            "fmt.byte_rate",
            f"byte rate {byte_rate} != sample_rate*block_align {sample_rate * block_align}",
        )

    channel_mask = 0
    mask_source = "inferred_from_channel_count"
    if format_tag == PCM:
        if channels == 1:
            channel_mask = 0x4
        else:
            channel_mask = 0x3F
    elif format_tag == WAVE_FORMAT_EXTENSIBLE:
        if len(fmt_chunk) < 40:
            raise _invalid("fmt.cbSize", "WAVE_FORMAT_EXTENSIBLE fmt chunk is too short")
        extension_size, valid_bits, channel_mask = struct.unpack_from("<HHI", fmt_chunk, 16)
        if extension_size < 22:
            raise _invalid("fmt.cbSize", "extensible format extension must be at least 22 bytes")
        if valid_bits not in (0, bits_per_sample):
            raise _invalid(
                "fmt.wValidBitsPerSample",
                f"valid bits {valid_bits} do not match container bits {bits_per_sample}",
            )
        subformat = fmt_chunk[24:40]
        if subformat != PCM_SUBFORMAT_GUID:
            raise _unsupported("fmt.SubFormat", "extensible subformat is not integer PCM")
        if channel_mask == 0:
            raise _invalid("fmt.dwChannelMask", "extensible PCM must declare a channel mask")
        mask_source = "wav_header"
    else:
        raise _unsupported("fmt.format_tag", "only integer PCM is supported")

    names = CHANNEL_NAMES_BY_MASK.get(channel_mask)
    if names is None:
        raise _invalid(
            "fmt.dwChannelMask",
            f"unsupported channel mask 0x{channel_mask:04X}",
        )
    if len(names) != channels:
        raise _invalid(
            "fmt.dwChannelMask",
            f"mask popcount {_popcount(channel_mask)} != channel count {channels}",
        )
    if channels == 1 and channel_mask != 0x4:
        raise _invalid("fmt.dwChannelMask", "mono channel mask must identify front center")
    if channels == 6 and channel_mask not in (0x3F, 0x60F):
        raise _invalid("fmt.dwChannelMask", "5.1 channel mask must be 0x003F or 0x060F")

    if len(data_chunk) % block_align != 0:
        raise _invalid(
            "data.size",
            f"data length {len(data_chunk)} is not divisible by block alignment {block_align}",
        )
    frames = len(data_chunk) // block_align
    if frames <= 0:
        raise _invalid("data.size", "WAV file contains no sample frames")
    if frames * block_align != len(data_chunk):
        raise _invalid("data.size", "data length does not match frame count")

    audio = _decode_pcm(data_chunk, bits_per_sample, channels)
    if audio.shape != (frames, channels):
        raise _invalid("data.size", "decoded sample shape does not match the header")
    if not np.isfinite(audio).all():
        raise _invalid("data", "decoded PCM contains non-finite values")

    return WavInfo(
        path=str(source),
        sample_rate=sample_rate,
        channels=channels,
        bits_per_sample=bits_per_sample,
        channel_mask=channel_mask,
        channel_layout=LAYOUT_BY_MASK[channel_mask],
        channel_names=names,
        channel_mask_source=mask_source,
        frames=frames,
        data_size=len(data_chunk),
        block_align=block_align,
        format_tag=format_tag,
        audio=audio,
        file_sha256=sha256_file(source),
    )


def estimated_analysis_memory(wav: WavInfo, copies: int = 5) -> int:
    """Conservative deterministic estimate used by the task scheduler."""
    working = wav.audio.nbytes * copies
    fft_padding = (1 << math.ceil(math.log2(max(2, wav.frames)))) * 2 * 8
    return int(working + fft_padding * wav.channels + 128 * 1024)

