"""Multichannel WAV reading and weighted downmix.

Supports 48 kHz, 16-bit / 24-bit PCM, 5.1 channel layout. Header
contradictions, missing channels or bit-depth mismatches raise
E_WAV_INVALID.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass

import numpy as np

from .errors import WavInvalidError

EXPECTED_CHANNELS = 6
EXPECTED_RATE = 48000
SUPPORTED_BITS = (16, 24)

# WAVEFORMATEXTENSIBLE channel-mask bits (5.1: FL FR FC LFE BL BR or SL SR).
MASK_51_BACK = 0x003F  # FL|FR|FC|LFE|BL|BR
MASK_51_SIDE = 0x060F  # FL|FR|FC|LFE|SL|SR

WAVE_FORMAT_PCM = 0x0001
WAVE_FORMAT_EXTENSIBLE = 0xFFFE
KSDATAFORMAT_SUBTYPE_PCM = bytes(
    [0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
     0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71]
)


@dataclass(frozen=True)
class WavData:
    samples: np.ndarray  # float64, shape (n_frames, 6), full scale = 1.0
    sample_rate: int
    bits_per_sample: int
    channel_mask: int | None

    @property
    def n_frames(self) -> int:
        return self.samples.shape[0]


def _fail(msg: str) -> None:
    raise WavInvalidError(msg)


def read_wav(path: str) -> WavData:
    raw = open(path, "rb").read()
    if len(raw) < 12 or raw[0:4] != b"RIFF" or raw[8:12] != b"WAVE":
        _fail("not a RIFF/WAVE file")
    riff_size = struct.unpack_from("<I", raw, 4)[0]
    if riff_size + 8 > len(raw):
        _fail("RIFF size exceeds file length")

    fmt = None
    data = None
    pos = 12
    while pos + 8 <= len(raw):
        cid = raw[pos:pos + 4]
        csize = struct.unpack_from("<I", raw, pos + 4)[0]
        body = raw[pos + 8: pos + 8 + csize]
        if len(body) < csize:
            _fail("truncated chunk")
        if cid == b"fmt ":
            fmt = body
        elif cid == b"data":
            data = body
        pos += 8 + csize + (csize & 1)

    if fmt is None or data is None:
        _fail("missing fmt or data chunk")
    if len(fmt) < 16:
        _fail("fmt chunk too small")

    (fmt_tag, channels, rate, byte_rate, block_align, bits) = struct.unpack_from(
        "<HHIIHH", fmt, 0
    )
    channel_mask = None
    if fmt_tag == WAVE_FORMAT_EXTENSIBLE:
        if len(fmt) < 40:
            _fail("extensible fmt chunk too small")
        cb_size = struct.unpack_from("<H", fmt, 16)[0]
        if cb_size < 22:
            _fail("extensible fmt cbSize invalid")
        valid_bits = struct.unpack_from("<H", fmt, 18)[0]
        channel_mask = struct.unpack_from("<I", fmt, 20)[0]
        sub_format = fmt[24:40]
        if sub_format != KSDATAFORMAT_SUBTYPE_PCM:
            _fail("extensible sub-format is not PCM")
        if valid_bits != bits:
            _fail("valid bits differ from container bits")
    elif fmt_tag != WAVE_FORMAT_PCM:
        _fail(f"unsupported format tag {fmt_tag}")

    if rate != EXPECTED_RATE:
        _fail(f"sample rate {rate} != {EXPECTED_RATE}")
    if channels != EXPECTED_CHANNELS:
        _fail(f"channel count {channels} != {EXPECTED_CHANNELS} (5.1 required)")
    if bits not in SUPPORTED_BITS:
        _fail(f"bit depth {bits} not in {SUPPORTED_BITS}")

    expected_align = channels * (bits // 8)
    if block_align != expected_align:
        _fail("block align inconsistent with channels/bit depth")
    if byte_rate != rate * expected_align:
        _fail("byte rate inconsistent with header fields")
    if len(data) == 0 or len(data) % block_align != 0:
        _fail("data length not a whole number of frames")

    if channel_mask is not None and channel_mask not in (MASK_51_BACK, MASK_51_SIDE):
        _fail(f"channel mask 0x{channel_mask:x} is not a 5.1 layout")

    n_frames = len(data) // block_align
    if bits == 16:
        pcm = np.frombuffer(data, dtype="<i2").astype(np.float64) / 32768.0
    else:
        triples = np.frombuffer(data, dtype=np.uint8).reshape(n_frames, channels, 3)
        vals = (
            triples[:, :, 0].astype(np.int32)
            | (triples[:, :, 1].astype(np.int32) << 8)
            | (triples[:, :, 2].astype(np.int32) << 16)
        )
        vals = np.where(vals & 0x800000, vals - 0x1000000, vals)
        pcm = vals.astype(np.float64) / 8388608.0

    samples = pcm.reshape(n_frames, channels)
    return WavData(samples=samples, sample_rate=rate, bits_per_sample=bits,
                   channel_mask=channel_mask)


def downmix(samples: np.ndarray, weights) -> np.ndarray:
    """Weighted 5.1 -> mono downmix. Weights are per-channel gains."""
    w = np.asarray(weights, dtype=np.float64)
    if samples.ndim != 2 or samples.shape[1] != len(w):
        _fail("downmix input shape does not match channel weights")
    return samples @ w
