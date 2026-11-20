"""Unified internal representation of a fingerprint, independent of the
on-disk format version (v1/v2/v3)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional

# Canonical extraction parameter set. Frame length is derived from the sample
# rate so the *physical* window stays constant across sampling rates; band
# edges are given in Hz and converted to FFT bins per sample rate.
FRAME_MS = 46.4
HOP_RATIO = 4
TIME_QUANT_MS = 32.0  # unified time quantization baseline
BAND_EDGES_HZ = (300.0, 500.0, 800.0, 1200.0, 2000.0, 3500.0)
TARGET_ZONE_FRAMES = 24
FAN_OUT = 4
EXTRACTOR_ID = "fp-extract/1.0"


def frame_length_for(sample_rate: int) -> int:
    """Power-of-two FFT size covering ~FRAME_MS at the given sample rate."""
    import math

    target = sample_rate * FRAME_MS / 1000.0
    n = 1 << round(math.log2(target))
    return max(256, min(8192, n))


def hop_length_for(sample_rate: int) -> int:
    return frame_length_for(sample_rate) // HOP_RATIO


def band_edges_bins(sample_rate: int) -> list[int]:
    """Convert Hz band edges to FFT bin indices for this sample rate."""
    nfft = frame_length_for(sample_rate)
    nyquist = sample_rate / 2.0
    edges = [0]
    for hz in BAND_EDGES_HZ:
        edges.append(min(nfft // 2, round(hz / nyquist * (nfft // 2))))
    edges.append(nfft // 2)
    out = []
    for e in edges:
        if not out or e > out[-1]:
            out.append(e)
    return out


@dataclass
class PeakPair:
    anchor_frame: int
    freq1: int  # FFT bin of anchor peak
    freq2: int  # FFT bin of paired peak
    delta_frames: int

    def hash(self) -> int:
        h = (self.freq1 & 0x3FF) << 22
        h |= (self.freq2 & 0x3FF) << 12
        h |= self.delta_frames & 0xFFF
        return h


@dataclass
class Fingerprint:
    """Version-neutral in-memory fingerprint."""

    sample_id: str
    sample_rate: int
    channels: int
    duration_ms: float
    frame_length: int
    hop_length: int
    time_quant_ms: float = TIME_QUANT_MS
    peak_pairs: list[PeakPair] = field(default_factory=list)
    # list of (hash, quantized time offset in ms)
    hashes: list[tuple[int, float]] = field(default_factory=list)
    bands_hz: tuple = BAND_EDGES_HZ
    channel_merge: bool = False
    compression: Optional[dict] = None
    quant_bits: Optional[int] = None
    provenance: dict = field(default_factory=dict)
    legacy: dict = field(default_factory=dict)
    source_version: int = 3

    def quantize_time(self, frame: int) -> float:
        t_ms = frame * self.hop_length * 1000.0 / self.sample_rate
        q = self.time_quant_ms
        return round(round(t_ms / q) * q, 6)
