"""Deterministic acoustic signal processing."""

from __future__ import annotations

import math
from typing import Any, Callable, Dict, List, Mapping, Optional, Sequence, Tuple

import numpy as np

from .cancellation import CancelToken, checkpoint
from .codes import AcousticError, E_DECAY_NEGATIVE, E_IR_INVALID, E_SYNTHESIS_FAILED
from .util import hash_obj, stable_seed


def _db(value: float, floor: float = 1e-30) -> float:
    return float(10.0 * math.log10(max(float(value), floor)))


def detect_arrivals(
    samples: np.ndarray,
    *,
    threshold: float,
    reference_index: int,
) -> Tuple[np.ndarray, np.ndarray]:
    channel_count = int(samples.shape[1])
    arrivals = np.zeros(channel_count, dtype=np.int64)
    peaks = np.zeros(channel_count, dtype=np.float64)
    for channel_index in range(channel_count):
        channel = samples[:, channel_index]
        peak = float(np.max(np.abs(channel)))
        if not math.isfinite(peak) or peak <= 0.0:
            raise AcousticError(
                E_IR_INVALID,
                "IR channel is silent and cannot be aligned.",
                injection_point=f"dsp:direct_arrival:channel={channel_index}",
            )
        gate = threshold * peak
        candidates = np.flatnonzero(np.abs(channel) >= gate)
        if candidates.size == 0:
            raise AcousticError(
                E_IR_INVALID,
                "No direct arrival exceeds the configured threshold.",
                injection_point=f"dsp:direct_arrival:channel={channel_index}",
            )
        arrivals[channel_index] = int(candidates[0])
        peaks[channel_index] = peak
    if reference_index < 0 or reference_index >= channel_count:
        raise AcousticError(
            E_IR_INVALID,
            "Reference channel index is out of range.",
            injection_point="dsp:direct_arrival:reference",
        )
    return arrivals, peaks


def align_direct_sound(
    samples: np.ndarray,
    *,
    layout: Sequence[str],
    reference_channel: str,
    threshold: float,
    normalize_gain: bool,
) -> Dict[str, Any]:
    if samples.ndim != 2 or samples.shape[1] != len(layout):
        raise AcousticError(
            E_IR_INVALID,
            "IR shape does not match the 5.1 channel layout.",
            injection_point="dsp:align:shape",
        )
    reference_index = list(layout).index(reference_channel)
    arrivals, peaks = detect_arrivals(
        samples,
        threshold=threshold,
        reference_index=reference_index,
    )
    reference_arrival = int(arrivals[reference_index])
    shifts = arrivals - reference_arrival
    reference_peak = float(peaks[reference_index])
    gains = np.ones_like(peaks)
    if normalize_gain:
        gains = reference_peak / peaks

    aligned = np.zeros_like(samples, dtype=np.float64)
    frame_count = samples.shape[0]
    for channel_index, shift_value in enumerate(shifts):
        shift = int(shift_value)
        if abs(shift) >= frame_count:
            raise AcousticError(
                E_IR_INVALID,
                "Direct arrival shift exceeds the IR length.",
                injection_point=f"dsp:align:channel={channel_index}",
            )
        if shift == 0:
            aligned[:, channel_index] = samples[:, channel_index]
        elif shift > 0:
            aligned[: frame_count - shift, channel_index] = samples[shift:, channel_index]
        else:
            aligned[-shift:, channel_index] = samples[: frame_count + shift, channel_index]
        aligned[:, channel_index] *= gains[channel_index]

    if not np.isfinite(aligned).all():
        raise AcousticError(
            E_IR_INVALID,
            "Direct-sound alignment produced non-finite samples.",
            injection_point="dsp:align:result",
        )
    return {
        "aligned": aligned,
        "arrivals": arrivals,
        "gains": gains,
        "peaks": peaks,
        "reference_index": reference_index,
        "reference_arrival": reference_arrival,
        "shift_samples": shifts,
    }


def _band_mask(frequencies: np.ndarray, center: float) -> np.ndarray:
    low = center / math.sqrt(2.0)
    high = center * math.sqrt(2.0)
    return (frequencies >= low) & (frequencies < high)


def band_energies(
    signal: np.ndarray,
    sample_rate: int,
    fft_size: int,
    bands_hz: Sequence[float],
) -> Tuple[List[float], np.ndarray]:
    segment = np.asarray(signal, dtype=np.float64).reshape(-1)
    if segment.size > fft_size:
        segment = segment[:fft_size]
    elif segment.size < fft_size:
        segment = np.pad(segment, (0, fft_size - segment.size))
    windowed = segment * np.hanning(fft_size)
    spectrum = np.fft.rfft(windowed)
    power = np.abs(spectrum) ** 2
    frequencies = np.fft.rfftfreq(fft_size, d=1.0 / sample_rate)
    energies: List[float] = []
    for center in bands_hz:
        mask = _band_mask(frequencies, float(center))
        energies.append(float(np.sum(power[mask])))
    return energies, frequencies


def extract_early_reflections(
    aligned: np.ndarray,
    *,
    sample_rate: int,
    layout: Sequence[str],
    arrivals: np.ndarray,
    reference_index: int,
    window: Mapping[str, Any],
    frequency: Mapping[str, Any],
    positions: Mapping[str, Any],
    cancel_token: Optional[CancelToken] = None,
) -> Dict[str, Any]:
    direct_end = int(arrivals[reference_index]) + int(
        round(float(window["direct_window_ms"]) * sample_rate / 1000.0)
    )
    early_end = int(arrivals[reference_index]) + int(
        round(float(window["early_window_ms"]) * sample_rate / 1000.0)
    )
    early_end = min(early_end, aligned.shape[0])
    direct_end = min(max(direct_end, 0), early_end)
    event_gate = float(window.get("event_threshold", 0.03))
    min_spacing = max(
        1,
        int(round(float(window.get("min_event_spacing_ms", 1.0)) * sample_rate / 1000.0)),
    )
    bands = [float(item) for item in frequency["bands_hz"]]
    fft_size = int(frequency["fft_size"])
    global_peak = float(np.max(np.abs(aligned))) if aligned.size else 0.0
    if global_peak <= 0.0:
        raise AcousticError(
            E_IR_INVALID,
            "Aligned IR is silent.",
            injection_point="dsp:early:aligned_ir",
        )
    gate = event_gate * global_peak
    events: List[Dict[str, Any]] = []
    for channel_index, channel_name in enumerate(layout):
        checkpoint(cancel_token, f"early:channel:{channel_name}")
        magnitude = np.abs(aligned[:, channel_index])
        above = magnitude >= gate
        candidates: List[int] = []
        start: Optional[int] = None
        for sample_index in range(direct_end, early_end):
            if above[sample_index] and start is None:
                start = sample_index
            elif not above[sample_index] and start is not None:
                segment = magnitude[start:sample_index]
                candidates.append(start + int(np.argmax(segment)))
                start = None
        if start is not None:
            segment = magnitude[start:early_end]
            if segment.size:
                candidates.append(start + int(np.argmax(segment)))

        filtered: List[int] = []
        for index in candidates:
            if not filtered or index - filtered[-1] >= min_spacing:
                filtered.append(index)
            elif magnitude[index] > magnitude[filtered[-1]]:
                filtered[-1] = index

        shift = int(arrivals[channel_index] - arrivals[reference_index])
        for event_index, sample_index in enumerate(filtered):
            half = fft_size // 2
            start_index = max(0, sample_index - half)
            end_index = min(aligned.shape[0], sample_index + half)
            segment = aligned[start_index:end_index, channel_index].copy()
            if segment.size < fft_size:
                left = min(half, segment.size)
                segment = np.pad(segment, (0, fft_size - segment.size))
            energies, _ = band_energies(segment, sample_rate, fft_size, bands)
            events.append(
                {
                    "order": len(events),
                    "channel": channel_name,
                    "channel_index": channel_index,
                    "event_index": event_index,
                    "arrival_sample": int(sample_index),
                    "source_sample_index": int(sample_index + shift),
                    "since_direct_ms": round(
                        (int(sample_index) - int(arrivals[reference_index]))
                        * 1000.0
                        / sample_rate,
                        9,
                    ),
                    "amplitude": float(aligned[sample_index, channel_index]),
                    "band_energy_db": [
                        round(_db(energy), 9) for energy in energies
                    ],
                }
            )
    return {
        "early_ir": aligned[:early_end].copy(),
        "events": events,
        "direct_end_sample": int(direct_end),
        "early_end_sample": int(early_end),
        "event_gate_absolute": float(gate),
        "positions": {
            "source": list(positions["source_position"]),
            "listener": list(positions["listener_position"]),
            "distance_m": round(float(positions["distance_m"]), 12),
        },
    }


def _sabine_decay(
    material: Mapping[str, Any],
    bands: Sequence[float],
) -> List[float]:
    volume = max(float(material["volume_m3"]), 1e-9)
    surface_area = max(float(material["surface_area_m2"]), 1e-9)
    result: List[float] = []
    for alpha in material["absorption"]:
        alpha_eff = min(max(float(alpha), 1e-4), 0.999)
        value = 0.161 * volume / (surface_area * alpha_eff)
        result.append(min(max(value, 0.05), 8.0))
    return result


def model_decay(
    early_ir: np.ndarray,
    *,
    sample_rate: int,
    reference_sample: int,
    early_end_sample: int,
    frequency: Mapping[str, Any],
    material: Mapping[str, Any],
    decay_config: Mapping[str, Any],
    source_position: Sequence[float],
    listener_position: Sequence[float],
    cancel_token: Optional[CancelToken] = None,
) -> Dict[str, Any]:
    bands = [float(item) for item in frequency["bands_hz"]]
    fft_size = int(frequency["fft_size"])
    hop = max(1, fft_size // 2)
    frame_starts = list(
        range(max(0, reference_sample), max(reference_sample + 1, early_end_sample), hop)
    )
    if not frame_starts:
        frame_starts = [max(0, reference_sample)]
    frequencies = np.fft.rfftfreq(fft_size, d=1.0 / sample_rate)
    masks = [_band_mask(frequencies, center) for center in bands]
    band_frames: List[List[float]] = [[] for _ in bands]
    frame_times: List[float] = []
    window = np.hanning(fft_size)
    for frame_number, start in enumerate(frame_starts):
        checkpoint(cancel_token, f"decay:frame:{frame_number}")
        end = min(early_ir.shape[0], start + fft_size)
        segment = early_ir[start:end, :]
        if segment.shape[0] < fft_size:
            segment = np.pad(segment, ((0, fft_size - segment.shape[0]), (0, 0)))
        power_sum = np.zeros(fft_size // 2 + 1, dtype=np.float64)
        for channel_index in range(segment.shape[1]):
            spectrum = np.fft.rfft(segment[:, channel_index] * window)
            power_sum += np.abs(spectrum) ** 2
        power_sum /= max(1, segment.shape[1])
        frame_times.append((start - reference_sample) / sample_rate)
        for band_index, mask in enumerate(masks):
            band_frames[band_index].append(float(np.sum(power_sum[mask])))

    times = np.asarray(frame_times, dtype=np.float64)
    fitted_t60: List[float] = []
    slopes: List[float] = []
    band_energy_db = np.zeros((len(frame_times), len(bands)), dtype=np.float64)
    schroeder_db = np.zeros((len(frame_times), len(bands)), dtype=np.float64)
    for band_index, energies in enumerate(band_frames):
        energy_array = np.asarray(energies, dtype=np.float64)
        band_energy_db[:, band_index] = 10.0 * np.log10(np.maximum(energy_array, 1e-30))
        cumulative = np.cumsum(energy_array[::-1])[::-1]
        normalized = cumulative / max(cumulative[0], 1e-30)
        schroeder_db[:, band_index] = 10.0 * np.log10(np.maximum(normalized, 1e-12))
        finite = np.isfinite(band_energy_db[:, band_index])
        if np.count_nonzero(finite) >= 2:
            slope = float(np.polyfit(times[finite], band_energy_db[finite, band_index], 1)[0])
        else:
            slope = -60.0
        slopes.append(slope)
        if math.isfinite(slope) and slope < -1e-9:
            fitted_t60.append(float(np.clip(-60.0 / slope, 0.05, 8.0)))
        else:
            fitted_t60.append(0.0)

    sabine = _sabine_decay(material, bands)
    distance = float(
        np.linalg.norm(
            np.asarray(listener_position, dtype=np.float64)
            - np.asarray(source_position, dtype=np.float64)
        )
    )
    distance_factor = 1.0 + min(0.15, 0.01 * distance)
    override = decay_config.get("decay_time_override")
    min_decay = float(decay_config.get("min_decay_time_sec", 0.05))
    decay_times: List[float] = []
    for band_index, fit_value in enumerate(fitted_t60):
        if override is not None:
            value = float(override)
        elif fit_value > 0.0:
            value = 0.5 * fit_value + 0.5 * sabine[band_index] * distance_factor
        else:
            value = sabine[band_index] * distance_factor
        value = max(value, min_decay)
        if not math.isfinite(value) or value <= 0.0:
            raise AcousticError(
                E_DECAY_NEGATIVE,
                "Decay model produced a non-positive or non-finite decay time.",
                injection_point=f"dsp:decay:band={band_index}",
                details={"band_hz": bands[band_index], "decay_time_sec": value},
            )
        decay_times.append(float(value))
    if override is not None:
        configured = float(override)
        if not math.isfinite(configured) or configured <= 0.0:
            raise AcousticError(
                E_DECAY_NEGATIVE,
                "Configured decay time is negative or non-finite.",
                injection_point="dsp:decay:override",
                details={"decay_time_override": configured},
            )
    return {
        "frame_times_sec": times,
        "band_energy_db": band_energy_db,
        "schroeder_db": schroeder_db,
        "decay_times_sec": np.asarray(decay_times, dtype=np.float64),
        "fit_slopes_db_per_sec": np.asarray(slopes, dtype=np.float64),
        "bands_hz": bands,
        "material": dict(material),
        "distance_m": distance,
        "override_used": override is not None,
    }


def synthesize_late_reverb(
    *,
    decay_times: np.ndarray,
    bands_hz: Sequence[float],
    sample_rate: int,
    tail_ms: float,
    early_ir: np.ndarray,
    early_end_sample: int,
    seed: int,
    material: Mapping[str, Any],
    source_position: Sequence[float],
    listener_position: Sequence[float],
    cancel_token: Optional[CancelToken] = None,
) -> Dict[str, Any]:
    tail_samples = max(1, int(round(float(tail_ms) * sample_rate / 1000.0)))
    channel_count = int(early_ir.shape[1])
    time = np.arange(tail_samples, dtype=np.float64) / sample_rate
    frequencies = np.fft.rfftfreq(tail_samples, d=1.0 / sample_rate)
    distance = float(
        np.linalg.norm(
            np.asarray(listener_position, dtype=np.float64)
            - np.asarray(source_position, dtype=np.float64)
        )
    )
    distance_gain = 1.0 / (1.0 + distance / 12.0)
    reflection_gain = float(material.get("reflection_gain", 0.8))
    early_part = early_ir[: max(1, min(early_end_sample, early_ir.shape[0])), :]
    early_rms = float(np.sqrt(np.mean(early_part**2))) if early_part.size else 0.01
    target_rms = max(early_rms * 0.22 * distance_gain * reflection_gain, 1e-6)
    late = np.zeros((tail_samples, channel_count), dtype=np.float64)
    for channel_index in range(channel_count):
        checkpoint(cancel_token, f"reverb:channel:{channel_index}")
        channel_seed = stable_seed(f"{seed}:{channel_index}")
        rng = np.random.Generator(np.random.PCG64(channel_seed))
        noise = rng.standard_normal(tail_samples)
        spectrum = np.fft.rfft(noise)
        channel_signal = np.zeros(tail_samples, dtype=np.float64)
        for band_index, center in enumerate(bands_hz):
            mask = _band_mask(frequencies, float(center))
            if not np.any(mask):
                continue
            filtered = np.fft.irfft(spectrum * mask, n=tail_samples)
            decay_time = max(float(decay_times[band_index]), 0.01)
            envelope = np.exp(-math.log(1000.0) * time / decay_time)
            channel_signal += filtered * envelope / math.sqrt(len(bands_hz))
        current_rms = float(np.sqrt(np.mean(channel_signal**2)))
        if not math.isfinite(current_rms) or current_rms <= 0.0:
            raise AcousticError(
                E_SYNTHESIS_FAILED,
                "Late reverb synthesis failed for a channel.",
                injection_point=f"dsp:reverb:channel={channel_index}",
            )
        channel_signal *= target_rms / current_rms
        late[:, channel_index] = channel_signal
    if not np.isfinite(late).all():
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Late reverb tail contains non-finite samples.",
            injection_point="dsp:reverb:result",
        )
    return {"late_tail": late, "tail_samples": tail_samples}


def synthesize_spatial_response(
    early_ir: np.ndarray,
    late_tail: np.ndarray,
    *,
    early_end_sample: int,
    layout: Sequence[str],
    align_arrivals: np.ndarray,
    cancel_token: Optional[CancelToken] = None,
) -> Dict[str, Any]:
    if early_ir.ndim != 2 or late_tail.ndim != 2:
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Early and late responses must be two-dimensional.",
            injection_point="dsp:spatial:shape",
        )
    if early_ir.shape[1] != len(layout) or late_tail.shape[1] != len(layout):
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "A channel was lost while assembling the spatial response.",
            injection_point="dsp:spatial:channels",
            details={"layout": list(layout)},
        )
    parts = []
    for channel_index in range(len(layout)):
        checkpoint(cancel_token, f"spatial:channel:{channel_index}")
        early_part = early_ir[:early_end_sample, channel_index]
        late_part = late_tail[:, channel_index]
        parts.append(np.concatenate([early_part, late_part]))
    lengths = {part.size for part in parts}
    if len(lengths) != 1:
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Channel responses have inconsistent lengths.",
            injection_point="dsp:spatial:length",
        )
    spatial = np.column_stack(parts)
    if not np.isfinite(spatial).all():
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Spatial response contains non-finite samples.",
            injection_point="dsp:spatial:result",
        )
    reference = int(align_arrivals[0])
    alignment_error = [int(value) - reference for value in align_arrivals]
    return {
        "spatial_ir": spatial,
        "early_end_sample": int(early_end_sample),
        "alignment_error_samples": alignment_error,
    }


def _bearing_weight(channel: str, azimuth_degrees: float) -> float:
    angles = {
        "FL": -30.0,
        "FR": 30.0,
        "FC": 0.0,
        "LFE": 0.0,
        "BL": -110.0,
        "BR": 110.0,
    }
    delta = math.radians(angles[channel] - azimuth_degrees)
    return float((math.cos(delta) + 1.0) * 0.5)


def compute_field_coefficients(
    spatial_ir: np.ndarray,
    *,
    sample_rate: int,
    layout: Sequence[str],
    decay_times: np.ndarray,
    bands_hz: Sequence[float],
    arrivals: np.ndarray,
    early_end_sample: int,
    source_position: Sequence[float],
    listener_position: Sequence[float],
    fft_size: int,
    cancel_token: Optional[CancelToken] = None,
) -> Dict[str, Any]:
    source = np.asarray(source_position, dtype=np.float64)
    listener = np.asarray(listener_position, dtype=np.float64)
    direction = listener - source
    azimuth = math.degrees(math.atan2(float(direction[1]), float(direction[0])))
    coefficients: Dict[str, Any] = {}
    for channel_index, channel in enumerate(layout):
        checkpoint(cancel_token, f"field:channel:{channel}")
        signal = spatial_ir[:, channel_index]
        energies, _ = band_energies(signal, sample_rate, fft_size, bands_hz)
        rms = float(np.sqrt(np.mean(signal**2)))
        peak = float(np.max(np.abs(signal)))
        early = signal[:early_end_sample]
        late = signal[early_end_sample:]
        early_energy = float(np.sum(early**2))
        late_energy = float(np.sum(late**2))
        ratio = 10.0 * math.log10(max(early_energy, 1e-30) / max(late_energy, 1e-30))
        coefficients[channel] = {
            "channel_index": channel_index,
            "gain_rms_db": round(20.0 * math.log10(max(rms, 1e-12)), 9),
            "peak_dbfs": round(20.0 * math.log10(max(peak, 1e-12)), 9),
            "delay_samples": int(arrivals[channel_index] - arrivals[0]),
            "arrival_time_ms": round(
                float(arrivals[channel_index]) * 1000.0 / sample_rate, 9
            ),
            "early_to_late_ratio_db": round(float(ratio), 9),
            "band_energy_db": [round(_db(value), 9) for value in energies],
            "spatial_weight": round(_bearing_weight(channel, azimuth), 9),
            "decay_time_sec": round(
                float(
                    len(decay_times)
                    / max(float(np.sum(1.0 / np.maximum(decay_times, 1e-9))), 1e-9)
                ),
                9,
            ),
        }
    return {
        "coefficients": coefficients,
        "filters": np.asarray(spatial_ir, dtype=np.float32),
        "azimuth_degrees": azimuth,
    }


def deterministic_test_signal(
    sample_rate: int,
    duration_sec: float,
    seed: int,
) -> np.ndarray:
    sample_count = max(1, int(round(duration_sec * sample_rate)))
    time = np.arange(sample_count, dtype=np.float64) / sample_rate
    rng = np.random.Generator(np.random.PCG64(stable_seed(f"render-test:{seed}")))
    noise = rng.standard_normal(sample_count)
    kernel = np.ones(31, dtype=np.float64) / 31.0
    smooth_noise = np.convolve(noise, kernel, mode="same")
    signal = (
        0.42 * np.sin(2.0 * math.pi * 220.0 * time)
        + 0.23 * np.sin(2.0 * math.pi * 440.0 * time + 0.37)
        + 0.11 * np.sin(2.0 * math.pi * 880.0 * time + 1.11)
        + 0.08 * smooth_noise
    )
    fade_samples = min(sample_count // 20, int(0.01 * sample_rate))
    if fade_samples > 1:
        fade = np.linspace(0.0, 1.0, fade_samples, endpoint=False)
        signal[:fade_samples] *= fade
        signal[-fade_samples:] *= fade[::-1]
    peak = float(np.max(np.abs(signal)))
    if peak > 0:
        signal *= 0.7 / peak
    return signal


def convolve_same(signal: np.ndarray, impulse: np.ndarray) -> np.ndarray:
    source = np.asarray(signal, dtype=np.float64)
    ir = np.asarray(impulse, dtype=np.float64)
    length = source.size + ir.size - 1
    fft_size = 1 << (length - 1).bit_length()
    spectrum = np.fft.rfft(source, fft_size) * np.fft.rfft(ir, fft_size)
    return np.fft.irfft(spectrum, fft_size)[: source.size]


def render_channels(
    filters: np.ndarray,
    *,
    sample_rate: int,
    layout: Sequence[str],
    render_config: Mapping[str, Any],
    cancel_token: Optional[CancelToken] = None,
) -> Dict[str, Any]:
    if filters.ndim != 2 or filters.shape[1] != len(layout):
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Filter bank does not match the 5.1 channel layout.",
            injection_point="render:filters:channels",
        )
    duration = float(render_config.get("duration_sec", 2.0))
    seed = int(render_config.get("signal_seed", 2026))
    target_dbfs = float(render_config.get("target_dbfs", -3.0))
    source = deterministic_test_signal(sample_rate, duration, seed)
    rendered = np.zeros((source.size, len(layout)), dtype=np.float64)
    for channel_index, channel in enumerate(layout):
        checkpoint(cancel_token, f"render:channel:{channel}")
        rendered[:, channel_index] = convolve_same(source, filters[:, channel_index])
    peak = float(np.max(np.abs(rendered))) if rendered.size else 0.0
    if not math.isfinite(peak) or peak <= 0.0:
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Rendered channels are silent or non-finite.",
            injection_point="render:result",
        )
    target = 10.0 ** (target_dbfs / 20.0)
    rendered *= target / peak
    if not np.isfinite(rendered).all():
        raise AcousticError(
            E_SYNTHESIS_FAILED,
            "Rendered channels contain non-finite samples.",
            injection_point="render:result:finite",
        )
    return {
        "samples": rendered,
        "source_signal_hash": hash_obj([round(float(value), 15) for value in source[:: max(1, source.size // 257)]]),
        "peak_dbfs": round(20.0 * math.log10(max(float(np.max(np.abs(rendered))), 1e-12)), 9),
    }

