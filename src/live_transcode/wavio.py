from __future__ import annotations

import hashlib
import os
import struct
from pathlib import Path

import numpy as np

from .errors import wav_error
from .models import WavInfo

_PCM_FORMAT = 1
_EXTENSIBLE_FORMAT = 0xFFFE
_PCM_SUBFORMAT = bytes.fromhex("0100000000001000800000aa00389b71")


def _u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def _parse_fmt(chunk: bytes) -> dict[str, int]:
    if len(chunk) < 16:
        raise wav_error("fmt chunk is shorter than 16 bytes", "fmt.cbSize", size=len(chunk))
    fmt = {
        "format_tag": _u16(chunk, 0),
        "channels": _u16(chunk, 2),
        "sample_rate": _u32(chunk, 4),
        "byte_rate": _u32(chunk, 8),
        "block_align": _u16(chunk, 12),
        "bits_per_sample": _u16(chunk, 14),
    }
    if fmt["format_tag"] == _PCM_FORMAT:
        fmt["channel_mask"] = 0
        fmt["valid_bits"] = fmt["bits_per_sample"]
        return fmt
    if fmt["format_tag"] != _EXTENSIBLE_FORMAT:
        raise wav_error(
            "only integer PCM or WAVE_FORMAT_EXTENSIBLE PCM is accepted",
            "fmt.formatTag",
            value=fmt["format_tag"],
        )
    if len(chunk) < 40:
        raise wav_error("WAVE_FORMAT_EXTENSIBLE fmt chunk is too short", "fmt.cbSize", size=len(chunk))
    cb_size = _u16(chunk, 16)
    if cb_size < 22 or len(chunk) < 18 + cb_size:
        raise wav_error(
            "extensible fmt cbSize must declare the 22-byte extension",
            "fmt.cbSize",
            value=cb_size,
        )
    fmt["valid_bits"] = _u16(chunk, 18)
    fmt["channel_mask"] = _u32(chunk, 20)
    subformat = chunk[24:40]
    if subformat != _PCM_SUBFORMAT:
        raise wav_error(
            "extensible subformat is not integer PCM",
            "fmt.subFormat",
            value=subformat.hex(),
        )
    return fmt


def inspect_wav(path: str | os.PathLike[str]) -> tuple[WavInfo, bytes]:
    wav_path = Path(path)
    try:
        raw = wav_path.read_bytes()
    except OSError as exc:
        raise wav_error(f"cannot read WAV file: {exc}", "file", path=str(wav_path)) from exc
    if len(raw) < 12 or raw[0:4] != b"RIFF" or raw[8:12] != b"WAVE":
        raise wav_error("missing RIFF/WAVE header", "riff")
    riff_size = _u32(raw, 4)
    if riff_size + 8 > len(raw):
        raise wav_error(
            "RIFF size exceeds file length",
            "riff.size",
            declared=riff_size + 8,
            actual=len(raw),
        )
    fmt_chunk: bytes | None = None
    data_chunk: bytes | None = None
    offset = 12
    end = min(len(raw), riff_size + 8)
    while offset + 8 <= end:
        chunk_id = raw[offset : offset + 4]
        chunk_size = _u32(raw, offset + 4)
        payload_start = offset + 8
        payload_end = payload_start + chunk_size
        if payload_end > end:
            raise wav_error(
                f"{chunk_id.decode('latin1')} chunk exceeds RIFF boundary",
                f"{chunk_id.decode('latin1')}.size",
                declared=chunk_size,
                available=end - payload_start,
            )
        payload = raw[payload_start:payload_end]
        if chunk_id == b"fmt ":
            if fmt_chunk is not None:
                raise wav_error("duplicate fmt chunk", "fmt")
            fmt_chunk = payload
        elif chunk_id == b"data":
            if data_chunk is not None:
                raise wav_error("duplicate data chunk", "data")
            data_chunk = payload
        offset = payload_end + (chunk_size & 1)
    if fmt_chunk is None:
        raise wav_error("missing fmt chunk", "fmt")
    if data_chunk is None:
        raise wav_error("missing data chunk", "data")

    fmt = _parse_fmt(fmt_chunk)
    channels = fmt["channels"]
    sample_rate = fmt["sample_rate"]
    bits = fmt["bits_per_sample"]
    valid_bits = fmt["valid_bits"]
    block_align = fmt["block_align"]
    channel_mask = fmt["channel_mask"]

    if channels not in (1, 6):
        raise wav_error("channel count must be mono or 5.1", "fmt.channels", value=channels)
    if sample_rate != 48000:
        raise wav_error("source sample rate must be 48000", "fmt.sampleRate", value=sample_rate)
    if bits not in (16, 24):
        raise wav_error("bits per sample must be 16 or 24", "fmt.bitsPerSample", value=bits)
    if valid_bits != bits:
        raise wav_error(
            "valid bits and container bits must agree for local PCM",
            "fmt.validBitsPerSample",
            value=valid_bits,
            bits=bits,
        )
    expected_align = channels * (bits // 8)
    if block_align != expected_align:
        raise wav_error(
            "block alignment contradicts channels and bit depth",
            "fmt.blockAlign",
            value=block_align,
            expected=expected_align,
        )
    expected_byte_rate = sample_rate * block_align
    if fmt["byte_rate"] != expected_byte_rate:
        raise wav_error(
            "byte rate contradicts sample rate and block alignment",
            "fmt.byteRate",
            value=fmt["byte_rate"],
            expected=expected_byte_rate,
        )
    if len(data_chunk) % block_align != 0:
        raise wav_error(
            "data length is not a whole number of sample frames",
            "data.size",
            value=len(data_chunk),
            block_align=block_align,
            remainder=len(data_chunk) % block_align,
        )

    if fmt["format_tag"] == _PCM_FORMAT:
        if channels == 1:
            channel_mask = 0x4
            layout = "mono"
        else:
            channel_mask = 0x3F
            layout = "5.1"
    elif channels == 1:
        if channel_mask != 0x4:
            raise wav_error(
                "mono channel mask must be FRONT_CENTER (0x4)",
                "fmt.dwChannelMask",
                value=f"0x{channel_mask:08x}",
                expected="0x00000004",
            )
        layout = "mono"
    else:
        if channel_mask != 0x3F:
            raise wav_error(
                "5.1 channel mask must be FL|FR|FC|LFE|BL|BR (0x3F)",
                "fmt.dwChannelMask",
                value=f"0x{channel_mask:08x}",
                expected="0x0000003f",
            )
        layout = "5.1"

    frames = len(data_chunk) // block_align
    file_hash = hashlib.sha256(raw).hexdigest()
    info = WavInfo(
        material_id=wav_path.stem,
        sample_rate=sample_rate,
        channels=channels,
        bits_per_sample=bits,
        channel_mask=channel_mask,
        layout=layout,
        frames=frames,
        data_bytes=len(data_chunk),
        format_tag=fmt["format_tag"],
        source_hash=file_hash,
        file_size=len(raw),
    )
    return info, data_chunk


def decode_pcm(data: bytes, channels: int, bits_per_sample: int) -> np.ndarray:
    if bits_per_sample == 16:
        values = np.frombuffer(data, dtype="<i2").astype(np.int32)
    elif bits_per_sample == 24:
        packed = np.frombuffer(data, dtype=np.uint8).reshape(-1, 3)
        values = (
            packed[:, 0].astype(np.int32)
            | (packed[:, 1].astype(np.int32) << 8)
            | (packed[:, 2].astype(np.int32) << 16)
        )
        values = np.where(values & 0x800000, values - 0x1000000, values)
    else:
        raise wav_error("unsupported PCM depth", "fmt.bitsPerSample", value=bits_per_sample)
    frames = values.reshape(-1, channels)
    scale = float(1 << (bits_per_sample - 1))
    return frames.astype(np.float64) / scale


def load_wav(path: str | os.PathLike[str]) -> tuple[WavInfo, np.ndarray]:
    info, data = inspect_wav(path)
    return info, decode_pcm(data, info.channels, info.bits_per_sample)

