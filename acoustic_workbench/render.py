from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np

from .errors import E_RENDER_FAIL, fail
from .hashing import sha256_array, sha256_json
from .model import Scene
from .wavio import WavData, encode_wav_bytes, read_wav


def _convolve_channel(source: np.ndarray, response: np.ndarray) -> np.ndarray:
    length = source.size + response.size - 1
    fft_size = 1 << max(0, (length - 1).bit_length())
    spectrum = np.fft.rfft(source, fft_size) * np.fft.rfft(response, fft_size)
    return np.fft.irfft(spectrum, fft_size)[:length]


def render_field(
    coefficients: np.ndarray,
    test_signal: WavData,
    scene: Scene,
    field_version: str,
    dependencies: list[dict[str, Any]],
) -> tuple[bytes, dict[str, Any]]:
    if test_signal.sample_rate != scene.sample_rate:
        fail(
            E_RENDER_FAIL,
            "test signal sample rate does not match the acoustic field",
            sample_rate=test_signal.sample_rate,
        )
    if coefficients.ndim != 2 or coefficients.shape[0] != 6:
        fail(E_RENDER_FAIL, "field coefficients are not a 5.1 response matrix")
    if test_signal.channels not in (1, 6):
        fail(
            E_RENDER_FAIL,
            "deterministic test signal must be mono or 5.1",
            channels=test_signal.channels,
        )
    max_seconds = float(scene.render.get("max_seconds", 3.0))
    max_samples = max(1, int(round(max_seconds * scene.sample_rate)))
    rendered_channels: list[np.ndarray] = []
    for channel_index in range(6):
        source = test_signal.samples[
            0 if test_signal.channels == 1 else channel_index
        ]
        convolved = _convolve_channel(source.astype(np.float64), coefficients[channel_index])
        rendered_channels.append(convolved[:max_samples])
    rendered = np.vstack(rendered_channels)
    if not np.all(np.isfinite(rendered)):
        fail(E_RENDER_FAIL, "convolution produced non-finite audio")
    peak = float(np.max(np.abs(rendered))) if rendered.size else 0.0
    target_peak = min(0.99, max(0.05, float(scene.render.get("normalize_peak", 0.89))))
    if peak > target_peak:
        rendered *= target_peak / peak
    wav_bytes = encode_wav_bytes(rendered, scene.sample_rate)
    metadata = {
        "point_id": "",
        "field_version": field_version,
        "dependencies": dependencies,
        "coefficient_hash": sha256_array(coefficients),
        "test_signal_sha256": sha256_json(
            {
                "samples": sha256_array(test_signal.samples),
                "sample_rate": test_signal.sample_rate,
                "channels": test_signal.channels,
            }
        ),
        "sample_rate": scene.sample_rate,
        "channel_layout": list(scene.channel_layout),
        "frames": int(rendered.shape[1]),
        "peak": float(np.max(np.abs(rendered))) if rendered.size else 0.0,
        "rms": float(np.sqrt(np.mean(rendered**2))) if rendered.size else 0.0,
    }
    return wav_bytes, metadata


def load_render_signal(path: str | Path) -> WavData:
    return read_wav(path)

