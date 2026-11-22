from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Any

import numpy as np

from .models import ResampleRatio, TierParams, WavInfo, canonical_json_bytes


@dataclass(frozen=True)
class RenderedSlice:
    index: int
    out_start: int
    out_end: int
    src_start: int
    src_end: int
    phase_num: int
    phase_den: int
    phase_remainder_num: int
    end_phase_num: int
    pcm_bytes: bytes
    content_hash: str
    peak_abs: int
    per_channel_peaks: tuple[int, ...]
    silent: bool

    @property
    def sample_count(self) -> int:
        return self.out_end - self.out_start

    def meta_core(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "out_start": self.out_start,
            "out_end": self.out_end,
            "src_start": self.src_start,
            "src_end": self.src_end,
            "sample_count": self.sample_count,
            "phase_num": self.phase_num,
            "phase_den": self.phase_den,
            "phase_remainder_num": self.phase_remainder_num,
            "end_phase_num": self.end_phase_num,
            "peak_abs": self.peak_abs,
            "per_channel_peaks": list(self.per_channel_peaks),
            "silent": self.silent,
            "content_hash": self.content_hash,
        }


def quantize_pcm(samples: np.ndarray, bits: int) -> tuple[bytes, np.ndarray]:
    if samples.ndim != 2:
        raise ValueError("samples must be frames x channels")
    scale = 1 << (bits - 1)
    scaled = np.rint(np.asarray(samples, dtype=np.float64) * scale)
    integers = np.clip(scaled, -scale, scale - 1).astype(np.int32)
    if bits == 16:
        pcm = integers.astype("<i2").tobytes(order="C")
    elif bits == 24:
        raw = integers.astype("<i4").view(np.uint8).reshape(-1, 4)
        pcm = raw[:, :3].tobytes(order="C")
    else:
        raise ValueError(f"unsupported quantization depth {bits}")
    return pcm, integers


def pcm_to_integers(pcm: bytes, channels: int, bits: int) -> np.ndarray:
    if bits == 16:
        values = np.frombuffer(pcm, dtype="<i2").astype(np.int32)
    elif bits == 24:
        packed = np.frombuffer(pcm, dtype=np.uint8).reshape(-1, 3)
        values = (
            packed[:, 0].astype(np.int32)
            | (packed[:, 1].astype(np.int32) << 8)
            | (packed[:, 2].astype(np.int32) << 16)
        )
        values = np.where(values & 0x800000, values - 0x1000000, values)
    else:
        raise ValueError(f"unsupported PCM depth {bits}")
    return values.reshape(-1, channels)


def _kernel_matrix(
    centers: np.ndarray,
    fractions: np.ndarray,
    filter_length: int,
    cutoff: float,
) -> tuple[np.ndarray, np.ndarray]:
    half = filter_length // 2
    relative = np.arange(filter_length, dtype=np.int64) - (half - 1)
    source_indices = centers[:, None] + relative[None, :]
    distance = fractions[:, None] - relative[None, :].astype(np.float64)
    window = (
        0.42
        + 0.5 * np.cos(np.pi * distance / half)
        + 0.08 * np.cos(2.0 * np.pi * distance / half)
    )
    weights = cutoff * np.sinc(cutoff * distance) * window
    weights /= np.sum(weights, axis=1, keepdims=True)
    return source_indices, weights


def render_output_range(
    source: np.ndarray,
    info: WavInfo,
    tier: TierParams,
    out_start: int,
    out_end: int,
    total_output_samples: int | None = None,
) -> RenderedSlice:
    if out_start < 0 or out_end <= out_start:
        raise ValueError("invalid output range")
    total = (
        total_output_samples
        if total_output_samples is not None
        else ResampleRatio.build(info.sample_rate, tier.target_rate).output_length(info.frames)
    )
    if out_end > total:
        raise ValueError("output range exceeds tier output length")

    ratio = ResampleRatio.build(info.sample_rate, tier.target_rate)
    output_indices = np.arange(out_start, out_end, dtype=np.int64)
    position_numerators = output_indices * ratio.step_num
    centers = position_numerators // ratio.step_den
    fractions = (position_numerators % ratio.step_den).astype(np.float64) / ratio.step_den
    cutoff = min(1.0, tier.target_rate / info.sample_rate) * 0.97
    source_indices, weights = _kernel_matrix(
        centers, fractions, tier.filter_length, cutoff
    )
    in_bounds = (source_indices >= 0) & (source_indices < info.frames)
    safe_indices = np.clip(source_indices, 0, info.frames - 1)
    gathered = source[safe_indices, :]
    gathered = np.where(in_bounds[:, :, None], gathered, 0.0)
    output = np.einsum("ml,mlc->mc", weights, gathered, optimize=True)

    pcm, integers = quantize_pcm(output, tier.quant_bits)
    content_hash = hashlib.sha256(pcm).hexdigest()
    channel_peaks = np.max(np.abs(integers), axis=0).astype(np.int64)
    peak_abs = int(channel_peaks.max(initial=0))
    silent = bool(peak_abs == 0)

    valid_indices = source_indices[in_bounds]
    if valid_indices.size:
        src_start = int(valid_indices.min())
        src_end = int(valid_indices.max()) + 1
    else:
        src_start = min(info.frames, out_start)
        src_end = src_start
    phase_num = ratio.phase(out_start)
    end_phase_num = ratio.phase(out_end)
    remainder = ratio.phase_remainder(out_start, tier.filter_length)

    return RenderedSlice(
        index=out_start // tier.slice_samples,
        out_start=out_start,
        out_end=out_end,
        src_start=src_start,
        src_end=src_end,
        phase_num=phase_num,
        phase_den=ratio.step_den,
        phase_remainder_num=remainder,
        end_phase_num=end_phase_num,
        pcm_bytes=pcm,
        content_hash=content_hash,
        peak_abs=peak_abs,
        per_channel_peaks=tuple(int(v) for v in channel_peaks),
        silent=silent,
    )


def render_slice(
    source: np.ndarray,
    info: WavInfo,
    tier: TierParams,
    index: int,
    total_output_samples: int | None = None,
) -> RenderedSlice:
    if index < 0:
        raise ValueError("slice index must be non-negative")
    total = (
        total_output_samples
        if total_output_samples is not None
        else ResampleRatio.build(info.sample_rate, tier.target_rate).output_length(info.frames)
    )
    out_start = index * tier.slice_samples
    if out_start >= total:
        raise IndexError("slice index beyond output length")
    out_end = min(out_start + tier.slice_samples, total)
    return render_output_range(source, info, tier, out_start, out_end, total)


def slice_chain_hash(previous: str, rendered: RenderedSlice) -> str:
    payload = {
        "previous": previous,
        "meta": rendered.meta_core(),
    }
    return hashlib.sha256(canonical_json_bytes(payload)).hexdigest()

