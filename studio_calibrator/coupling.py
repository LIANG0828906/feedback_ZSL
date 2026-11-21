from __future__ import annotations

import math
from typing import Optional

import numpy as np

from .errors import E_BAND_UNRESOLVED, BusinessError
from .models import AlignmentResult, BandSpec, CouplingEstimate, ScenarioConfig
from .util import linear_to_db, optional_cancel


QUANTIZATION_STEP = 1.0 / 32768.0


def band_weights(
    frequencies: np.ndarray,
    band: BandSpec,
) -> np.ndarray:
    low = band.low_hz
    high = band.high_hz
    width = band.width_hz
    transition = min(band.transition_hz, width * 0.45)
    weights = np.zeros_like(frequencies, dtype=np.float64)
    lower_end = low + transition / 2.0
    upper_start = high - transition / 2.0
    lower = (frequencies >= low) & (frequencies < lower_end)
    plateau = (frequencies >= lower_end) & (frequencies <= upper_start)
    upper = (frequencies > upper_start) & (frequencies <= high)
    weights[lower] = 0.5 * (
        1.0
        - np.cos(
            math.pi
            * (frequencies[lower] - low)
            / max(lower_end - low, np.finfo(float).eps)
        )
    )
    weights[plateau] = 1.0
    weights[upper] = 0.5 * (
        1.0
        + np.cos(
            math.pi
            * (frequencies[upper] - upper_start)
            / max(high - upper_start, np.finfo(float).eps)
        )
    )
    return weights


def _valid_samples(valid_time_samples: int, band: BandSpec, sample_rate: int) -> int:
    nyquist = sample_rate / 2.0
    effective = valid_time_samples * band.width_hz / nyquist
    return max(0, int(round(effective)))


def _group_delay(
    transfer_bins: np.ndarray,
    frequencies: np.ndarray,
    weights: np.ndarray,
) -> float:
    active = weights > 0.05
    if int(np.count_nonzero(active)) < 3:
        return 0.0
    phase = np.unwrap(np.angle(transfer_bins[active]))
    omega = 2.0 * math.pi * frequencies[active]
    weight = weights[active]
    design = np.column_stack([omega, np.ones_like(omega)])
    weighted_design = design * np.sqrt(weight)[:, None]
    weighted_phase = phase * np.sqrt(weight)
    try:
        slope = np.linalg.lstsq(weighted_design, weighted_phase, rcond=None)[0][0]
    except np.linalg.LinAlgError:
        return 0.0
    delay = -float(slope)
    if not math.isfinite(delay):
        return 0.0
    return float(max(-1e4, min(1e4, delay)))


def estimate_band(
    alignment: AlignmentResult,
    band: BandSpec,
    config: ScenarioConfig,
    cancel_event: Optional[object] = None,
) -> CouplingEstimate:
    optional_cancel(cancel_event)
    valid_time_samples = max(1, alignment.valid_samples)
    reference = alignment.aligned_reference[:valid_time_samples]
    component = alignment.aligned_component[:valid_time_samples]
    frequencies = np.fft.rfftfreq(reference.size, d=1.0 / alignment.sample_rate)
    reference_spectrum = np.fft.rfft(reference)
    component_spectrum = np.fft.rfft(component)
    weights = band_weights(frequencies, band)
    active = weights > 1e-6
    active_bins = int(np.count_nonzero(active))
    effective_samples = _valid_samples(valid_time_samples, band, alignment.sample_rate)
    if (
        effective_samples < config.min_band_samples
        or active_bins < 3
    ):
        raise BusinessError(
            E_BAND_UNRESOLVED,
            "effective samples are insufficient for band coupling estimation",
            source=alignment.source,
            receiver=alignment.receiver,
            band=band.id,
            details={
                "valid_samples": effective_samples,
                "active_bins": active_bins,
                "minimum_samples": config.min_band_samples,
                "band_low_hz": band.low_hz,
                "band_high_hz": band.high_hz,
            },
        )

    reference_power = float(
        np.sum(weights[active] * np.abs(reference_spectrum[active]) ** 2)
    )
    component_power = float(
        np.sum(weights[active] * np.abs(component_spectrum[active]) ** 2)
    )
    if (
        reference_power <= 1e-20
        or component_power <= 1e-20
        or not math.isfinite(reference_power)
        or not math.isfinite(component_power)
    ):
        raise BusinessError(
            E_BAND_UNRESOLVED,
            "band contains no valid energy",
            source=alignment.source,
            receiver=alignment.receiver,
            band=band.id,
            details={
                "valid_samples": effective_samples,
                "reference_power": reference_power,
                "component_power": component_power,
            },
        )

    denominator = np.sum(
        weights[active] * np.abs(reference_spectrum[active]) ** 2
    )
    numerator = np.sum(
        weights[active]
        * np.conj(reference_spectrum[active])
        * component_spectrum[active]
    )
    if abs(denominator) <= 1e-30:
        raise BusinessError(
            E_BAND_UNRESOLVED,
            "band reference energy is numerically unresolved",
            source=alignment.source,
            receiver=alignment.receiver,
            band=band.id,
            details={"valid_samples": effective_samples},
        )
    transfer = numerator / denominator
    magnitude = abs(complex(transfer))
    strength_db = linear_to_db(max(magnitude, 1e-12))
    phase_deg = float(math.degrees(math.atan2(transfer.imag, transfer.real)))

    transfer_bins = component_spectrum * np.conj(reference_spectrum) / (
        np.abs(reference_spectrum) ** 2 + 1e-30
    )
    modelled = transfer * reference_spectrum
    residual_power = float(
        np.sum(weights[active] * np.abs(component_spectrum[active] - modelled[active]) ** 2)
    )
    coherence = max(0.0, min(0.999999, 1.0 - residual_power / component_power))
    component_rms = float(np.sqrt(np.mean(np.square(component))))
    quantization_relative = QUANTIZATION_STEP / (
        math.sqrt(12.0) * max(component_rms, 1e-12)
    )
    transition_fraction = min(0.9, band.transition_hz / max(band.width_hz, 1e-12))
    leakage_fraction = 0.5 * transition_fraction
    phase_sigma_rad = math.sqrt(
        (1.0 - coherence)
        / (2.0 * max(coherence, 1e-6) * max(effective_samples, 1))
    ) + math.radians(band.transition_hz / max(band.width_hz, 1e-12))
    amplitude_sigma_relative = math.sqrt(
        phase_sigma_rad**2
        + quantization_relative**2
        + leakage_fraction**2
    )
    amplitude_margin_db = 20.0 / math.log(10.0) * amplitude_sigma_relative
    phase_margin_deg = math.degrees(phase_sigma_rad)
    group_delay = _group_delay(transfer_bins, frequencies, weights)
    return CouplingEstimate(
        source=alignment.source,
        receiver=alignment.receiver,
        band_id=band.id,
        low_hz=band.low_hz,
        high_hz=band.high_hz,
        strength_db=float(strength_db),
        strength_ci_db=(
            float(strength_db - amplitude_margin_db),
            float(strength_db + amplitude_margin_db),
        ),
        phase_deg=phase_deg,
        phase_ci_deg=(
            float(phase_deg - phase_margin_deg),
            float(phase_deg + phase_margin_deg),
        ),
        coherence=float(coherence),
        group_delay_samples=group_delay,
        valid_samples=effective_samples,
        active_bins=active_bins,
        quantization_relative=float(quantization_relative),
        leakage_fraction=float(leakage_fraction),
        transition_fraction=float(transition_fraction),
        estimator_version=config.estimator_version,
    )


def estimate_all(
    alignments: dict[str, AlignmentResult],
    config: ScenarioConfig,
    cancel_event: Optional[object] = None,
) -> dict[tuple[str, str, str], CouplingEstimate]:
    estimates: dict[tuple[str, str, str], CouplingEstimate] = {}
    for pair_key in sorted(alignments):
        alignment = alignments[pair_key]
        for band in config.bands:
            optional_cancel(cancel_event)
            estimate = estimate_band(
                alignment, band, config, cancel_event=cancel_event
            )
            estimates[(estimate.source, estimate.receiver, band.id)] = estimate
    return estimates
