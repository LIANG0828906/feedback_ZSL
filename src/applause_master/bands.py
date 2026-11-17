"""Per-band microphone energy ratios for every detected applause event."""

from __future__ import annotations

import numpy as np
from scipy.signal import butter, sosfilt, sosfilt_zi

RATIO_TOLERANCE = 1e-6


def bandpass_sos(low_hz, high_hz, sample_rate, order=4):
    """Butterworth band-pass SOS for one declared frequency band."""
    nyquist = sample_rate / 2.0
    normalized = [max(1.0, low_hz) / nyquist, min(nyquist - 1.0, high_hz) / nyquist]
    return butter(order, normalized, btype="bandpass", output="sos")


def filter_signal(signal, sos):
    """Zero-initial-state band filtering (deterministic)."""
    zi = sosfilt_zi(sos)
    out, _ = sosfilt(sos, signal, zi=zi * signal[0])
    return out


def band_energies(mic_band_signals, start_sample, end_sample):
    """RMS^2 energy per microphone inside one event/band window."""
    energies = {}
    for name, band_signal in mic_band_signals.items():
        window = band_signal[start_sample:end_sample]
        energies[name] = float(np.dot(window, window))
    return energies


def compute_ratios(events, mic_names, band_defs, mic_signals, sample_rate, gate):
    """Compute energy ratios per event per band.

    Ratios depend only on physical energies, never on microphone ordering:
    names are sorted explicitly and every sum is normalized to 1.0.
    Microphones under the project energy gate are excluded from the mix; when
    every microphone is gated (e.g. a weak tail), equal weights are used and
    the fallback is recorded for review.
    """
    ordered_names = sorted(mic_names)
    band_sos = {band["name"]: bandpass_sos(band["low_hz"], band["high_hz"], sample_rate)
                for band in band_defs}
    filtered = {
        band_name: {name: filter_signal(mic_signals[name], sos) for name in ordered_names}
        for band_name, sos in band_sos.items()
    }

    ratio_table = []
    for event in events:
        start_sample = int(round(event["start"] * sample_rate))
        end_sample = int(round(event["end"] * sample_rate))
        entry = {"event_index": event["event_index"], "bands": []}
        for band in band_defs:
            energies = band_energies(filtered[band["name"]], start_sample, end_sample)
            strongest = max(energies.values()) if energies else 0.0
            gate_level = gate * max(strongest, 1e-30)
            participating = {
                name: energy for name, energy in energies.items()
                if energy >= gate_level
            }
            gated_names = sorted(name for name in ordered_names if name not in participating)
            fallback = not participating
            if fallback:
                participating = {name: 1.0 for name in ordered_names}
                gated_names = []
            total = sum(participating.values())
            ratios = {
                name: ((participating[name] / total) if name in participating else 0.0)
                for name in ordered_names
            }
            # Resolve float rounding so the documented sum is exactly 1.0.
            correction = 1.0 - sum(ratios.values())
            ratios[ordered_names[0]] += correction
            assert abs(sum(ratios.values()) - 1.0) <= RATIO_TOLERANCE
            entry["bands"].append({
                "band": band["name"],
                "fallback_equal_weights": fallback,
                "gated_microphones": gated_names,
                "energy": {name: energies[name] for name in ordered_names},
                "ratios": ratios,
            })
        ratio_table.append(entry)
    return ratio_table, filtered, band_sos
