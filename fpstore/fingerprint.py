"""Deterministic WAV -> STFT peaks -> pair hashes pipeline."""

from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .audio import AudioData, merge_to_mono, read_wav
from .config import FingerprintConfig
from .errors import BizError, E_AUDIO_INVALID
from .util import content_hash, sha256_bytes


@dataclass(frozen=True)
class PeakPair:
    anchor_frame: int
    t1_frame: int
    t2_frame: int
    f1_bin: int
    f2_bin: int

    def as_dict(self) -> dict[str, int]:
        return {
            "anchor_frame": self.anchor_frame,
            "t1_frame": self.t1_frame,
            "t2_frame": self.t2_frame,
            "f1_bin": self.f1_bin,
            "f2_bin": self.f2_bin,
        }


@dataclass(frozen=True)
class HashHit:
    hash: str
    offset_ms: int
    offset_ticks: int
    delta_units: int
    t2_frame: int
    f1_bin: int
    f2_bin: int
    band1: int
    band2: int
    anchor_frame: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "hash": self.hash,
            "offset_ms": self.offset_ms,
            "offset_ticks": self.offset_ticks,
            "delta_units": self.delta_units,
            "t2_frame": self.t2_frame,
            "f1_bin": self.f1_bin,
            "f2_bin": self.f2_bin,
            "band1": self.band1,
            "band2": self.band2,
            "anchor_frame": self.anchor_frame,
        }


@dataclass(frozen=True)
class SegmentFingerprint:
    track_id: str
    segment_index: int
    start_frame: int
    end_frame: int
    start_ms: int
    end_ms: int
    frame_number: int
    peak_pairs: tuple[PeakPair, ...]
    hashes: tuple[HashHit, ...]
    segment_hash: str

    @property
    def key(self) -> str:
        return f"{self.track_id}:{self.segment_index}"


@dataclass(frozen=True)
class ExtractedFingerprint:
    track_id: str
    audio_path: str
    sample_rate: int
    channels: int
    channel_merge: str
    duration_ms: int
    hash_algo: str
    time_basis: dict[str, Any]
    config_hash: str
    segments: tuple[SegmentFingerprint, ...]

    @property
    def all_hashes(self) -> tuple[HashHit, ...]:
        return tuple(hit for segment in self.segments for hit in segment.hashes)


def _band_index(freq_hz: float, edges: Iterable[int]) -> int:
    index = 0
    for edge_index, edge in enumerate(edges):
        if freq_hz >= edge:
            index = edge_index
        else:
            break
    return index


def _time_parts(frame: int, audio: AudioData, config: FingerprintConfig) -> tuple[int, int]:
    """Return (display_ms, ticks); ticks are frames or ms quanta by config."""
    if config.time_quant_bits is None:
        display_ms = int((frame * config.hop_length * 1000) // audio.sample_rate)
        return display_ms, frame
    quantum = 1 << config.time_quant_bits
    display_ms = int((frame * config.hop_length * 1000) // (audio.sample_rate * quantum) * quantum)
    return display_ms, display_ms // quantum


def make_pair_hash(
    *,
    hash_algo: str,
    f1_bin: int,
    f2_bin: int,
    delta_units: int,
    band1: int,
    band2: int,
    time_quant_bits: int | None,
    band_edges: Iterable[int] | None,
) -> str:
    if hash_algo == "v1-pair-raw":
        material = f"v1-pair-raw|{f1_bin}|{f2_bin}|{delta_units}"
    elif hash_algo == "v2-pairs-quant":
        layout = ",".join(str(edge) for edge in (band_edges or ()))
        material = (
            f"v2-pairs-quant|{f1_bin}|{f2_bin}|{delta_units}|"
            f"{band1}|{band2}|{time_quant_bits}|{layout}"
        )
    else:
        raise BizError(
            E_AUDIO_INVALID,
            "unknown pair hash algorithm",
            {"field": "hash_algo", "value": hash_algo},
        )
    return sha256_bytes(material.encode("utf-8"))[:20]


def segment_content_hash(
    peak_pairs: Iterable[PeakPair],
    hashes: Iterable[HashHit],
) -> str:
    value = {
        "peak_pairs": [pair.as_dict() for pair in peak_pairs],
        "hashes": [hit.as_dict() for hit in hashes],
    }
    return content_hash(value)


def _stft_log_magnitude(signal: np.ndarray, frame_size: int, hop_length: int) -> np.ndarray:
    if signal.size < frame_size:
        padded = np.zeros(frame_size, dtype=np.float64)
        padded[: signal.size] = signal
        signal = padded
    window = 0.5 - 0.5 * np.cos(2.0 * np.pi * np.arange(frame_size, dtype=np.float64) / frame_size)
    frames = np.lib.stride_tricks.sliding_window_view(signal, frame_size)[::hop_length]
    spectrum = np.abs(np.fft.rfft(frames * window, axis=1))
    return np.log1p(spectrum)


def _frame_peaks(log_mag: np.ndarray, peak_count: int, threshold: float) -> list[list[int]]:
    if log_mag.shape[1] < 3:
        return [[] for _ in range(log_mag.shape[0])]
    interior = log_mag[:, 1:-1]
    left = log_mag[:, :-2]
    right = log_mag[:, 2:]
    gate = float(np.median(log_mag)) + threshold * float(np.std(log_mag))
    mask = (interior > left) & (interior >= right) & (interior >= gate)
    peaks: list[list[int]] = []
    for row_index in range(log_mag.shape[0]):
        positions = np.flatnonzero(mask[row_index]) + 1
        if positions.size > peak_count:
            ranked = sorted(
                (int(position) for position in positions),
                key=lambda position: (-float(log_mag[row_index, position]), position),
            )
            positions = np.asarray(sorted(ranked[:peak_count]))
        peaks.append([int(position) for position in positions])
    return peaks


def extract_fingerprint(
    audio: AudioData | str | Path,
    config: FingerprintConfig,
    *,
    track_id: str | None = None,
) -> ExtractedFingerprint:
    config.validate()
    if isinstance(audio, (str, Path)):
        audio_data = read_wav(Path(audio))
    else:
        audio_data = audio
    if track_id is None:
        track_id = Path(audio_data.path).stem
    if not track_id:
        raise BizError(E_AUDIO_INVALID, "track_id is empty", {"field": "track_id"})

    signal = merge_to_mono(audio_data, config.channel_merge)
    log_mag = _stft_log_magnitude(signal, config.frame_size, config.hop_length)
    peaks_by_frame = _frame_peaks(log_mag, config.peak_count, config.peak_threshold)
    edges = tuple(config.band_edges_hz)
    nyquist = audio_data.sample_rate / 2.0
    bin_hz = audio_data.sample_rate / config.frame_size

    pairs: list[PeakPair] = []
    hashes: list[HashHit] = []
    seen: set[tuple[int, int, int, int]] = set()
    frame_count = log_mag.shape[0]
    for anchor_frame in range(frame_count):
        anchor_peaks = peaks_by_frame[anchor_frame]
        if not anchor_peaks:
            continue
        for f1_bin in anchor_peaks:
            candidates: list[tuple[float, int, int, int]] = []
            for delta in range(1, config.pair_max_delta_frames + 1):
                target_frame = anchor_frame + delta
                if target_frame >= frame_count:
                    break
                for f2_bin in peaks_by_frame[target_frame]:
                    score = float(log_mag[anchor_frame, f1_bin] + log_mag[target_frame, f2_bin])
                    candidates.append((score, target_frame, f2_bin, delta))
            if not candidates:
                continue
            candidates.sort(key=lambda row: (-row[0], row[1], row[2]))
            for _, target_frame, f2_bin, delta in candidates[: config.pairs_per_peak]:
                dedupe_key = (anchor_frame, target_frame, f1_bin, f2_bin)
                if dedupe_key in seen:
                    continue
                seen.add(dedupe_key)
                pair = PeakPair(anchor_frame, anchor_frame, target_frame, f1_bin, f2_bin)
                pairs.append(pair)

                display_ms, offset_ticks = _time_parts(anchor_frame, audio_data, config)
                if config.time_quant_bits is None:
                    delta_units = delta
                    band1 = -1 if config.version == 1 else _band_index(f1_bin * bin_hz, edges)
                    band2 = -1 if config.version == 1 else _band_index(f2_bin * bin_hz, edges)
                else:
                    quantum = 1 << config.time_quant_bits
                    t1_ms = (anchor_frame * config.hop_length * 1000) // audio_data.sample_rate
                    t2_ms = (target_frame * config.hop_length * 1000) // audio_data.sample_rate
                    delta_units = t2_ms // quantum - t1_ms // quantum
                    band1 = _band_index(f1_bin * bin_hz, edges)
                    band2 = _band_index(f2_bin * bin_hz, edges)
                digest = make_pair_hash(
                    hash_algo=config.hash_algo,
                    f1_bin=f1_bin,
                    f2_bin=f2_bin,
                    delta_units=delta_units,
                    band1=band1,
                    band2=band2,
                    time_quant_bits=config.time_quant_bits,
                    band_edges=edges,
                )
                hashes.append(
                    HashHit(
                        hash=digest,
                        offset_ms=display_ms,
                        offset_ticks=offset_ticks,
                        delta_units=delta_units,
                        t2_frame=target_frame,
                        f1_bin=f1_bin,
                        f2_bin=f2_bin,
                        band1=band1,
                        band2=band2,
                        anchor_frame=anchor_frame,
                    )
                )

    segment_count = max(1, math.ceil(audio_data.duration_ms / config.segment_ms))
    segment_pairs: list[list[PeakPair]] = [[] for _ in range(segment_count)]
    segment_hashes: list[list[HashHit]] = [[] for _ in range(segment_count)]
    for pair in pairs:
        exact_ms = pair.anchor_frame * config.hop_length * 1000 / audio_data.sample_rate
        index = min(segment_count - 1, int(exact_ms // config.segment_ms))
        segment_pairs[index].append(pair)
    for hit in hashes:
        exact_ms = hit.anchor_frame * config.hop_length * 1000 / audio_data.sample_rate
        index = min(segment_count - 1, int(exact_ms // config.segment_ms))
        segment_hashes[index].append(hit)

    segments: list[SegmentFingerprint] = []
    for index in range(segment_count):
        start_ms = index * config.segment_ms
        end_ms = min(audio_data.duration_ms, (index + 1) * config.segment_ms)
        start_frame = int(math.ceil(start_ms * audio_data.sample_rate / (1000 * config.hop_length)))
        end_frame = int(math.ceil(end_ms * audio_data.sample_rate / (1000 * config.hop_length)))
        ordered_pairs = tuple(
            sorted(
                segment_pairs[index],
                key=lambda pair: (pair.anchor_frame, pair.f1_bin, pair.t2_frame, pair.f2_bin),
            )
        )
        ordered_hashes = tuple(
            sorted(
                segment_hashes[index],
                key=lambda hit: (hit.offset_ticks, hit.hash, hit.f1_bin, hit.f2_bin),
            )
        )
        segments.append(
            SegmentFingerprint(
                track_id=track_id,
                segment_index=index,
                start_frame=start_frame,
                end_frame=end_frame,
                start_ms=start_ms,
                end_ms=end_ms,
                frame_number=start_frame,
                peak_pairs=ordered_pairs,
                hashes=ordered_hashes,
                segment_hash=segment_content_hash(ordered_pairs, ordered_hashes),
            )
        )

    time_basis = dict(config.time_basis)
    if time_basis.get("kind") == "frame":
        time_basis["sample_rate"] = audio_data.sample_rate
        time_basis["hop_length"] = config.hop_length
    return ExtractedFingerprint(
        track_id=track_id,
        audio_path=audio_data.path,
        sample_rate=audio_data.sample_rate,
        channels=audio_data.channels,
        channel_merge=config.channel_merge,
        duration_ms=audio_data.duration_ms,
        hash_algo=config.hash_algo,
        time_basis=time_basis,
        config_hash=config.config_hash,
        segments=tuple(segments),
    )


def _pair_key(pair: PeakPair) -> tuple[int, int, int, int]:
    return (pair.t1_frame, pair.t2_frame, pair.f1_bin, pair.f2_bin)


def compare_extractions(
    audio: AudioData | str | Path,
    old_config: FingerprintConfig,
    new_config: FingerprintConfig,
    *,
    track_id: str | None = None,
) -> dict[str, Any]:
    old = extract_fingerprint(audio, old_config, track_id=track_id)
    new = extract_fingerprint(audio, new_config, track_id=track_id)
    old_map = {
        _pair_key(pair): hit
        for segment in old.segments
        for pair in segment.peak_pairs
        for hit in segment.hashes
        if hit.anchor_frame == pair.t1_frame
        and hit.t2_frame == pair.t2_frame
        and hit.f1_bin == pair.f1_bin
        and hit.f2_bin == pair.f2_bin
    }
    new_map = {
        _pair_key(pair): hit
        for segment in new.segments
        for pair in segment.peak_pairs
        for hit in segment.hashes
        if hit.anchor_frame == pair.t1_frame
        and hit.t2_frame == pair.t2_frame
        and hit.f1_bin == pair.f1_bin
        and hit.f2_bin == pair.f2_bin
    }

    shared = sorted(set(old_map).intersection(new_map))
    details: list[dict[str, Any]] = []
    changed = 0
    changed_hash = 0
    max_abs = 0
    total_abs = 0
    for key in shared:
        old_hit = old_map[key]
        new_hit = new_map[key]
        offset_diff = new_hit.offset_ms - old_hit.offset_ms
        hash_changed = old_hit.hash != new_hit.hash
        changed += int(offset_diff != 0)
        changed_hash += int(hash_changed)
        max_abs = max(max_abs, abs(offset_diff))
        total_abs += abs(offset_diff)
        if offset_diff != 0 or hash_changed:
            details.append(
                {
                    "t1_frame": key[0],
                    "t2_frame": key[1],
                    "f1_bin": key[2],
                    "f2_bin": key[3],
                    "old_offset_ms": old_hit.offset_ms,
                    "new_offset_ms": new_hit.offset_ms,
                    "offset_diff_ms": offset_diff,
                    "old_hash": old_hit.hash,
                    "new_hash": new_hit.hash,
                }
            )
    return {
        "track_id": old.track_id,
        "old_config_hash": old.config_hash,
        "new_config_hash": new.config_hash,
        "old_hash_algo": old.hash_algo,
        "new_hash_algo": new.hash_algo,
        "compared_pairs": len(shared),
        "unmatched_old_pairs": len(old_map) - len(shared),
        "unmatched_new_pairs": len(new_map) - len(shared),
        "offset_changed_pairs": changed,
        "hash_changed_pairs": changed_hash,
        "max_abs_offset_ms": max_abs,
        "mean_abs_offset_ms": round(total_abs / len(shared), 6) if shared else 0.0,
        "affected": bool(
            changed
            or changed_hash
            or len(old_map) != len(new_map)
            or len(old_map) != len(shared)
            or len(new_map) != len(shared)
        ),
        "details": details,
    }
