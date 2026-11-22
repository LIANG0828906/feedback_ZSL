"""Spectral band-delta fingerprinting with nested quantisation.

Per frame:

1. Hann-windowed FFT magnitude spectrum.
2. Log-spaced triangular aggregation into ``bands`` energy bins.
3. Delta vector ``d[i] = band[i+1] - band[i]`` (spectral-shape feature,
   robust to global gain changes introduced by lossy codecs).
4. Nested threshold quantisation of every delta:

   * 1 bit  -> sign code, 2 levels
   * 2 bits  -> sign + strong/weak, 4 levels
   * 4 bits  -> sign + 3 nested magnitude thresholds, 8 levels

The codes are nested, so for the same delta the 2-bit code always begins
with the 1-bit code and the 4-bit code begins with the 2-bit code.  Tightening
quantisation therefore can only *remove* hash collisions, never create them:
the cross-format match rate is provably non-increasing in ``quant_bits``.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .config import Config
from .errors import ParameterError

# Nested magnitude thresholds, expressed as fractions of the frame's
# robust delta scale (median |d| + floor).  level 2 uses t1, level 4 t1..t3.
THRESHOLDS = (0.35, 1.15, 2.4)
FLOOR = 1e-10


@dataclass(frozen=True)
class FrameFingerprint:
    index: int
    time_s: float
    hash_hex: str


@dataclass
class FingerprintTrack:
    file_id: str
    frames: list[FrameFingerprint]
    sample_count: int
    duration_s: float

    def hash_array(self) -> np.ndarray:
        return np.array([int(f.hash_hex, 16) for f in frames], dtype=np.int64)


def _band_edges(bands: int, nfft: int) -> np.ndarray:
    """Log-spaced FFT-bin edges, always including bin 1 and the Nyquist bin."""
    nyq = nfft // 2
    lo, hi = np.log(1.0), np.log(max(2.0, float(nyq)))
    edges = np.exp(np.linspace(lo, hi, bands + 1))
    edges = np.unique(np.round(edges).astype(int))
    if edges.size < bands + 1:  # extremely small FFT fallback
        edges = np.linspace(1, nyq, bands + 1).astype(int)
    edges[0] = 1
    edges[-1] = nyq
    return edges


def band_energies(spectrum: np.ndarray, edges: np.ndarray) -> np.ndarray:
    """Triangular/trapezoid aggregation of magnitude into log-spaced bands."""
    mag = np.abs(spectrum)
    bands = edges.size - 1
    out = np.zeros(bands, dtype=np.float64)
    for i in range(bands):
        lo, hi = edges[i], edges[i + 1]
        if hi <= lo:
            hi = lo + 1
        seg = mag[lo:hi]
        if seg.size == 0:
            continue
        # Linear ramp weights inside the band for smoother boundaries.
        weights = np.linspace(0.5, 1.0, seg.size, dtype=np.float64)
        out[i] = float(np.sqrt(np.mean((seg * weights) ** 2)))
    return out


def quantize_delta(delta: float, scale: float, quant_bits: int) -> int:
    """Nested quantisation of one delta value (see module docstring)."""
    s = delta / max(scale, FLOOR)
    if quant_bits == 1:
        return 1 if s >= 0 else 0
    t1, t2, t3 = THRESHOLDS
    if quant_bits == 2:
        if s >= t1:
            return 0b11
        if s >= 0:
            return 0b10
        if s > -t1:
            return 0b01
        return 0b00
    if quant_bits == 4:
        if s >= t3:
            return 0b111
        if s >= t2:
            return 0b110
        if s >= t1:
            return 0b101
        if s >= 0:
            return 0b100
        if s > -t1:
            return 0b011
        if s > -t2:
            return 0b010
        if s > -t3:
            return 0b001
        return 0b000
    raise ParameterError("unsupported quant_bits",
                         details={"quant_bits": quant_bits})


def _hash_width(quant_bits: int, deltas: int) -> int:
    """Bytes needed so every frame hash occupies a fixed-width hex string."""
    bits = deltas * (1 if quant_bits == 1 else (2 if quant_bits == 2 else 3))
    return (bits + 7) // 8


def code_bit_count(quant_bits: int, bands: int) -> int:
    """Number of information bits actually used by a frame code."""
    per_delta = 1 if quant_bits == 1 else (2 if quant_bits == 2 else 3)
    return per_delta * (bands - 1)


def code_value(frame: FrameFingerprint) -> int:
    return int(frame.hash_hex, 16)


def coarse_key(value: int, info_bits: int, prefix_bits: int) -> int:
    """Leading ``prefix_bits`` of the code as an inverted-index key."""
    return value >> max(0, info_bits - prefix_bits)


def hamming_similarity(value_a: int, value_b: int, info_bits: int) -> float:
    """Fraction of identical information bits (1.0 = identical code)."""
    if info_bits <= 0:
        return 0.0
    return 1.0 - (value_a ^ value_b).bit_count() / info_bits


# Per-level Hamming acceptance thresholds (fraction of differing bits).
HAMMING_THRESHOLD = {1: 0.20, 2: 0.25, 4: 0.30}
# Coarse prefix width in bits per level.
PREFIX_BITS = {1: 7, 2: 8, 4: 10}


def extract(samples: np.ndarray, cfg: Config, *, file_id: str) -> FingerprintTrack:
    """Extract the full frame-hash sequence from mono/multi-channel float audio.

    ``samples`` shape: ``(channels, n)`` float32 in [-1, 1].
    """
    cfg.validate()
    if samples.ndim != 2 or samples.shape[0] < 1:
        raise ParameterError("samples must have shape (channels, n)")
    mono = samples.mean(axis=0) if samples.shape[0] > 1 else samples[0]
    mono = mono.astype(np.float64, copy=False)

    nfft = cfg.frame_samples()
    hop = cfg.hop_samples()
    if mono.size < nfft:
        return FingerprintTrack(file_id=file_id, frames=[],
                                sample_count=mono.size,
                                duration_s=mono.size / cfg.sample_rate)

    window = np.hanning(nfft).astype(np.float64)
    edges = _band_edges(cfg.bands, nfft)
    width = _hash_width(cfg.quant_bits, cfg.bands - 1)

    starts = np.arange(0, mono.size - nfft + 1, hop)
    frames: list[FrameFingerprint] = []
    for idx, start in enumerate(starts):
        frame = mono[start:start + nfft] * window
        spectrum = np.fft.rfft(frame, n=nfft)
        energies = band_energies(spectrum, edges)
        deltas = np.diff(energies)
        scale = float(np.median(np.abs(deltas)) + FLOOR)
        value = 0
        for d in deltas:
            code = quantize_delta(float(d), scale, cfg.quant_bits)
            if cfg.quant_bits == 1:
                value = (value << 1) | code
            elif cfg.quant_bits == 2:
                value = (value << 2) | code
            else:
                value = (value << 3) | code
        frames.append(
            FrameFingerprint(
                index=idx,
                time_s=start / cfg.sample_rate,
                hash_hex=value.to_bytes(width, "big").hex(),
            )
        )
    return FingerprintTrack(
        file_id=file_id,
        frames=frames,
        sample_count=mono.size,
        duration_s=mono.size / cfg.sample_rate,
    )


def frame_match_rate(track_a: FingerprintTrack, track_b: FingerprintTrack) -> float:
    """Fraction of identically indexed frames with an *identical* hash.

    Frames are aligned by index (fixed hop); the shorter track defines the
    comparison window.  This strict metric is non-increasing as quantisation
    tightens because the codes are nested.
    """
    n = min(len(track_a.frames), len(track_b.frames))
    if n == 0:
        return 0.0
    ha = [f.hash_hex for f in track_a.frames[:n]]
    hb = [f.hash_hex for f in track_b.frames[:n]]
    return sum(a == b for a, b in zip(ha, hb)) / n


def hamming_match_rate(track_a: FingerprintTrack, track_b: FingerprintTrack,
                       cfg: Config) -> float:
    """Robust cross-format metric: mean bit-level similarity across frames."""
    n = min(len(track_a.frames), len(track_b.frames))
    if n == 0:
        return 0.0
    info_bits = code_bit_count(cfg.quant_bits, cfg.bands)
    total = 0.0
    for fa, fb in zip(track_a.frames[:n], track_b.frames[:n]):
        total += hamming_similarity(
            code_value(fa), code_value(fb), info_bits
        )
    return total / n
