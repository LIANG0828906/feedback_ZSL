from __future__ import annotations

import math
from typing import Any

import numpy as np

from .errors import E_BAND_INVALID, fail
from .hashing import stable_seed
from .model import Scene


def _band_noise(
    length: int,
    sample_rate: int,
    center_hz: float,
    seed: int,
    fraction: float = 1.0,
) -> np.ndarray:
    rng = np.random.Generator(np.random.PCG64(seed))
    noise = rng.standard_normal(length)
    spectrum = np.fft.rfft(noise)
    frequencies = np.fft.rfftfreq(length, d=1.0 / sample_rate)
    ratio = 2.0 ** (1.0 / (2.0 * max(1.0, fraction)))
    low = center_hz / ratio
    high = min(sample_rate / 2.0 - 1e-9, center_hz * ratio)
    mask = (frequencies >= low) & (frequencies < high)
    spectrum[~mask] = 0.0
    shaped = np.fft.irfft(spectrum, n=length)
    rms = float(np.sqrt(np.mean(shaped**2)))
    if rms <= 1e-15:
        return np.zeros(length, dtype=np.float64)
    return shaped / rms


def synthesize_late_reverb(
    decay: dict[str, Any],
    scene: Scene,
    listener_id: str,
    decay_output_hash: str,
) -> np.ndarray:
    tail_ms = float(scene.late.get("tail_ms", 800.0))
    if not math.isfinite(tail_ms) or tail_ms <= 0.0:
        fail(E_BAND_INVALID, "late tail_ms must be positive and finite")
    early_end_sample = int(
        round(float(scene.early["end_ms"]) * scene.sample_rate / 1000.0)
    )
    tail_samples = max(1, int(round(tail_ms * scene.sample_rate / 1000.0)))
    total_samples = early_end_sample + tail_samples
    output = np.zeros(
        (len(scene.channel_layout), total_samples),
        dtype=np.float64,
    )
    centers = [float(value) for value in decay["band_centers_hz"]]
    base_seed = int(scene.late.get("seed", 20261120))
    fraction = float(scene.bands.get("fraction", 1.0))
    t = np.arange(tail_samples, dtype=np.float64) / scene.sample_rate
    for channel_index, channel_name in enumerate(scene.channel_layout):
        channel_curves = decay["channels"][channel_name]
        energies = np.asarray(
            [max(0.0, float(curve["initial_energy"])) for curve in channel_curves],
            dtype=np.float64,
        )
        maximum = float(np.max(energies)) if energies.size else 0.0
        if maximum <= 0.0:
            continue
        for band_index, center in enumerate(centers):
            t60 = float(channel_curves[band_index]["t60_s"])
            level = 0.18 * math.sqrt(energies[band_index] / maximum)
            noise = _band_noise(
                tail_samples,
                scene.sample_rate,
                center,
                stable_seed(
                    "late",
                    listener_id,
                    channel_name,
                    band_index,
                    decay_output_hash,
                    base_seed,
                ),
                fraction,
            )
            envelope = np.power(10.0, -3.0 * t / max(t60, 1e-6))
            output[channel_index, early_end_sample:] += (
                level * noise * envelope
            )
    if not np.all(np.isfinite(output)):
        fail(E_BAND_INVALID, "late reverberation synthesis produced non-finite data")
    return np.ascontiguousarray(output)

