"""Minimal deterministic WAV reader/writer (PCM16, PCM24, IEEE float32).

Only mono and stereo are needed.  Samples are exchanged as float64 arrays in
the [-1, 1) range so the rest of the pipeline works in a single domain.
"""

import os
import struct

import numpy as np

from .errors import OperaError

PCM16 = 16
PCM24 = 24
FLOAT32 = 32


def _read_riff_header(fh):
    buf = fh.read(12)
    if len(buf) < 12 or buf[0:4] != b"RIFF" or buf[8:12] != b"WAVE":
        raise OperaError("not a RIFF/WAVE container", code="INVALID_WAV")
    fmt = None
    data = None
    while True:
        head = fh.read(8)
        if len(head) < 8:
            break
        chunk_id, size = struct.unpack("<4sI", head)
        body = fh.read(size)
        if len(body) < size:
            raise OperaError("truncated WAV chunk", code="INVALID_WAV")
        if size % 2 == 1:
            fh.read(1)
        if chunk_id == b"fmt ":
            fmt = body
        elif chunk_id == b"data":
            data = body
    if fmt is None or data is None:
        raise OperaError("WAV missing fmt/data chunk", code="INVALID_WAV")
    audio_format, channels, sample_rate, _, _, bits = struct.unpack(
        "<HHIIHH", fmt[:16]
    )
    return audio_format, channels, sample_rate, bits, data


def read_wav(path):
    """Return (samples float64 [n,ch], sample_rate, bit_depth, channels)."""
    with open(path, "rb") as fh:
        audio_format, channels, sample_rate, bits, data = _read_riff_header(fh)
    if audio_format == 1 and bits == 16:
        raw = np.frombuffer(data, dtype="<i2").astype(np.float64)
        samples = raw / 32768.0
    elif audio_format == 1 and bits == 24:
        raw = np.frombuffer(data, dtype=np.uint8).reshape(-1, 3)
        as32 = (
            raw[:, 0].astype(np.int32)
            | (raw[:, 1].astype(np.int32) << 8)
            | (raw[:, 2].astype(np.int32) << 16)
        )
        as32 = np.where(as32 & 0x800000, as32 - 0x1000000, as32)
        samples = as32.astype(np.float64) / 8388608.0
    elif audio_format == 3 and bits == 32:
        samples = np.frombuffer(data, dtype="<f4").astype(np.float64)
    else:
        raise OperaError(
            f"unsupported WAV format tag={audio_format} bits={bits}",
            code="INVALID_WAV",
        )
    if channels > 1:
        samples = samples.reshape(-1, channels)
    else:
        samples = samples.reshape(-1, 1)
    return samples, sample_rate, bits, channels


def _pcm16_bytes(samples):
    clipped = np.clip(samples, -1.0, 1.0 - 1.0 / 32768.0)
    q = np.round(clipped * 32768.0).astype("<i2")
    return q.tobytes()


def _pcm24_bytes(samples):
    clipped = np.clip(samples, -1.0, 1.0 - 1.0 / 8388608.0)
    q = np.round(clipped * 8388608.0).astype(np.int32).reshape(-1)
    out = np.empty((q.size, 3), dtype=np.uint8)
    q = q & 0xFFFFFF
    out[:, 0] = q & 0xFF
    out[:, 1] = (q >> 8) & 0xFF
    out[:, 2] = (q >> 16) & 0xFF
    return out.tobytes()


def _float32_bytes(samples):
    return samples.astype("<f4").tobytes()


def _riff_size(data_size, bits):
    if bits == FLOAT32:
        header_extra = 0
    else:
        header_extra = 0
    return 36 + data_size + header_extra


def write_wav(path, samples, sample_rate, bits=16):
    """Write a WAV file fully (used for the six source channels)."""
    if samples.ndim == 1:
        samples = samples[:, None]
    channels = samples.shape[1]
    if bits == PCM16:
        audio_format, payload = 1, _pcm16_bytes(samples)
    elif bits == PCM24:
        audio_format, payload = 1, _pcm24_bytes(samples)
    elif bits == FLOAT32:
        audio_format, payload = 3, _float32_bytes(samples)
    else:
        raise OperaError(f"unsupported bit depth {bits}", code="INVALID_CUE")
    block_align = channels * bits // 8
    byte_rate = sample_rate * block_align
    header = b"RIFF" + struct.pack(
        "<I4s4sIHHIIHH",
        _riff_size(len(payload), bits),
        b"WAVE",
        b"fmt ",
        16,
        audio_format,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bits,
    )
    with open(path, "wb") as fh:
        fh.write(header)
        fh.write(b"data")
        fh.write(struct.pack("<I", len(payload)))
        fh.write(payload)


def write_wav_header(path, sample_rate, channels, bits):
    """Create an empty master file with a placeholder data chunk."""
    block_align = channels * bits // 8
    byte_rate = sample_rate * block_align
    header = b"RIFF" + struct.pack(
        "<I4s4sIHHIIHH",
        0,
        b"WAVE",
        b"fmt ",
        16,
        3 if bits == FLOAT32 else 1,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        bits,
    )
    with open(path, "wb") as fh:
        fh.write(header)
        fh.write(b"data")
        fh.write(struct.pack("<I", 0))


def wav_data_offset(channels, bits):
    return 44


def finalize_wav(path):
    """Patch RIFF/data size fields after all blocks have been streamed."""
    size = os.path.getsize(path)
    data_size = size - 44
    with open(path, "r+b") as fh:
        fh.seek(4)
        fh.write(struct.pack("<I", size - 8))
        fh.seek(40)
        fh.write(struct.pack("<I", data_size))


def append_wav_block(path, samples, sample_rate, channels, bits, offset):
    """Append a stereo block at byte ``offset`` (data chunk start + n)."""
    if bits == PCM16:
        payload = _pcm16_bytes(samples)
    elif bits == FLOAT32:
        payload = _float32_bytes(samples)
    elif bits == PCM24:
        payload = _pcm24_bytes(samples)
    else:
        raise OperaError(f"unsupported bit depth {bits}", code="INVALID_CUE")
    with open(path, "r+b") as fh:
        fh.seek(offset)
        fh.write(payload)
    return len(payload)
