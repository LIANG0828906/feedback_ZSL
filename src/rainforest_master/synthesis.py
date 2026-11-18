from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .manifest import Manifest, TrackSpec, load_manifest
from .wavio import write_float32_wav


def _fft_fir_same(x: np.ndarray, impulse: np.ndarray, block_frames: int = 262_144) -> np.ndarray:
    """Zero-phase-aligned FIR convolution for finite arrays using overlap-add."""
    x = np.asarray(x, dtype=np.float32)
    impulse = np.asarray(impulse, dtype=np.float64)
    length = x.shape[0]
    taps = impulse.shape[0]
    output = np.empty(length, dtype=np.float32)
    if taps == 0:
        output[:] = x
        return output
    carry = np.zeros(taps - 1, dtype=np.float64)
    spectrum = None
    nfft = 0
    for start in range(0, length, block_frames):
        end = min(length, start + block_frames)
        chunk = x[start:end].astype(np.float64)
        needed = chunk.shape[0] + taps - 1
        new_nfft = 1 << (needed - 1).bit_length()
        if spectrum is None or new_nfft != nfft:
            nfft = new_nfft
            spectrum = np.fft.rfft(impulse, n=nfft)
        convolved = np.fft.irfft(np.fft.rfft(chunk, n=nfft) * spectrum, n=nfft)[:needed]
        overlap = min(carry.size, convolved.size)
        if overlap:
            convolved[:overlap] += carry[:overlap]
        take = min(chunk.shape[0], convolved.size)
        output[start : start + take] = convolved[:take]
        carry = convolved[take:].copy()
    return output


def _bandpass_impulse(sample_rate: int, low_hz: float, high_hz: float, taps: int = 1023) -> np.ndarray:
    nyquist = sample_rate / 2.0
    low = max(1.0, min(low_hz, nyquist - 1.0))
    high = max(low + 1.0, min(high_hz, nyquist - 1.0))
    n = np.arange(taps, dtype=np.float64)
    center = (taps - 1) / 2.0
    h = (
        2.0 * high / sample_rate * np.sinc(2.0 * high / sample_rate * (n - center))
        - 2.0 * low / sample_rate * np.sinc(2.0 * low / sample_rate * (n - center))
    )
    h *= np.hamming(taps)
    peak = float(np.max(np.abs(np.fft.rfft(h, n=1 << 16))))
    if peak > 0.0:
        h /= peak
    return h


def _event_rng(seed: int, track: TrackSpec, event_index: int) -> np.random.Generator:
    offset = int(track.synthesis.get("seed_offset", 0))
    return np.random.Generator(np.random.PCG64((seed * 2_000_003 + offset * 97 + event_index) & 0xFFFFFFFFFFFFFFFF))


def _add(out: np.ndarray, start: int, wave: np.ndarray) -> None:
    if start >= out.shape[0] or wave.shape[0] == 0:
        return
    if start < 0:
        wave = wave[-start:]
        start = 0
    end = min(out.shape[0], start + wave.shape[0])
    out[start:end] += wave[: end - start].astype(np.float32)


def _drop_wave(seed: int, track: TrackSpec, event_index: int, sample_rate: int) -> np.ndarray:
    rng = _event_rng(seed, track, event_index)
    duration = rng.uniform(0.045, 0.085)
    length = max(8, int(duration * sample_rate))
    u = np.arange(length, dtype=np.float64) / length
    attack = np.minimum(1.0, u / 0.045)
    decay = np.exp(-u * rng.uniform(6.5, 10.0))
    noise = np.diff(rng.standard_normal(length + 1))
    noise /= max(1e-9, float(np.max(np.abs(noise))))
    tone_hz = float(track.synthesis.get("tone_hz", 3900.0)) * rng.uniform(0.78, 1.3)
    tone = np.sin(2.0 * np.pi * tone_hz * np.arange(length) / sample_rate + rng.uniform(0.0, 2.0 * np.pi))
    return (0.76 * noise + 0.30 * tone) * attack * decay


def _insect_wave(seed: int, track: TrackSpec, event_index: int, sample_rate: int) -> np.ndarray:
    rng = _event_rng(seed, track, event_index)
    duration = rng.uniform(0.060, 0.095)
    length = max(8, int(duration * sample_rate))
    u = np.arange(length, dtype=np.float64) / length
    envelope = np.sin(np.pi * u) ** 2
    carrier = float(track.synthesis.get("carrier_hz", 4350.0)) * rng.uniform(0.94, 1.06)
    phase = 2.0 * np.pi * carrier * np.arange(length) / sample_rate + rng.uniform(0.0, 2.0 * np.pi)
    tremolo = 0.72 + 0.28 * np.sin(2.0 * np.pi * rng.uniform(28.0, 42.0) * np.arange(length) / sample_rate)
    return 0.52 * np.sin(phase) * tremolo * envelope


def _bird_wave(seed: int, track: TrackSpec, event_index: int, sample_rate: int) -> np.ndarray:
    rng = _event_rng(seed, track, event_index)
    duration = rng.uniform(0.17, 0.31)
    length = max(16, int(duration * sample_rate))
    u = np.arange(length, dtype=np.float64) / length
    envelope = np.sin(np.pi * u) ** 2
    base = float(track.synthesis.get("base_hz", 2450.0)) * rng.uniform(0.72, 1.28)
    sweep = rng.uniform(-0.32, 0.42)
    fm_depth = rng.uniform(0.05, 0.16)
    fm_rate = rng.uniform(2.0, 5.5)
    phase = 2.0 * np.pi * np.cumsum(base * (1.0 + sweep * u + fm_depth * np.sin(2.0 * np.pi * fm_rate * u))) / sample_rate
    phase += rng.uniform(0.0, 2.0 * np.pi)
    wave = np.sin(phase) + 0.38 * np.sin(2.03 * phase) + 0.16 * np.sin(3.07 * phase)
    return 0.30 * wave * envelope


def _thunder_wave(seed: int, track: TrackSpec, event_index: int, sample_rate: int) -> np.ndarray:
    rng = _event_rng(seed, track, event_index)
    duration = rng.uniform(1.7, 3.2)
    length = max(64, int(duration * sample_rate))
    u = np.arange(length, dtype=np.float64) / length
    attack = np.minimum(1.0, u / 0.12)
    envelope = attack * np.exp(-u * rng.uniform(2.1, 3.5))
    white = rng.standard_normal(length)
    smooth = np.convolve(white, np.ones(128, dtype=np.float64) / 128.0, mode="same")
    smooth /= max(1e-9, float(np.max(np.abs(smooth))))
    rumble_hz = float(track.synthesis.get("rumble_hz", 43.0)) * rng.uniform(0.82, 1.2)
    rumble = np.sin(2.0 * np.pi * rumble_hz * np.arange(length) / sample_rate)
    return (0.72 * smooth + 0.55 * rumble) * envelope


def _footstep_wave(seed: int, track: TrackSpec, event_index: int, sample_rate: int) -> np.ndarray:
    rng = _event_rng(seed, track, event_index)
    pair_seconds = float(track.synthesis.get("pair_seconds", 0.16))
    total = pair_seconds + 0.20
    length = max(16, int(total * sample_rate))
    output = np.zeros(length, dtype=np.float64)
    for pair_index in range(2):
        start = int(pair_index * pair_seconds * sample_rate)
        burst_len = max(12, int(rng.uniform(0.08, 0.14) * sample_rate))
        u = np.arange(burst_len, dtype=np.float64) / burst_len
        env = np.minimum(1.0, u / 0.025) * np.exp(-u * rng.uniform(5.0, 8.5))
        noise = np.diff(rng.standard_normal(burst_len + 1))
        noise /= max(1e-9, float(np.max(np.abs(noise))))
        plop_hz = rng.uniform(700.0, 1350.0)
        plop = np.sin(2.0 * np.pi * plop_hz * np.arange(burst_len) / sample_rate)
        burst = (0.78 * noise + 0.26 * plop) * env
        end = min(length, start + burst_len)
        output[start:end] += burst[: end - start]
    return output * 0.60


def synthesize_track(manifest: Manifest, track: TrackSpec) -> np.ndarray:
    sample_rate = manifest.sample_rate
    length = manifest.source_frames
    kind = str(track.synthesis.get("kind", ""))
    seed = manifest.seed
    output = np.zeros(length, dtype=np.float32)

    if kind == "rain_curtain":
        rng = np.random.Generator(np.random.PCG64(seed * 2_000_003 + int(track.synthesis.get("seed_offset", 0))))
        white = rng.standard_normal(length, dtype=np.float32)
        impulse = _bandpass_impulse(
            sample_rate,
            float(track.synthesis.get("low_hz", 45.0)),
            float(track.synthesis.get("high_hz", 15_500.0)),
            taps=511,
        )
        output = _fft_fir_same(white, impulse)
    elif kind == "stream":
        rng = np.random.Generator(np.random.PCG64(seed * 2_000_003 + int(track.synthesis.get("seed_offset", 0))))
        white = rng.standard_normal(length, dtype=np.float32)
        impulse = _bandpass_impulse(
            sample_rate,
            float(track.synthesis.get("low_hz", 420.0)),
            float(track.synthesis.get("high_hz", 1750.0)),
            taps=1023,
        )
        output = _fft_fir_same(white, impulse)
        t = np.arange(length, dtype=np.float64) / sample_rate
        modulation_hz = float(track.synthesis.get("modulation_hz", 0.071))
        modulation_depth = float(track.synthesis.get("modulation_depth", 0.23))
        modulation = 1.0 + modulation_depth * np.sin(2.0 * np.pi * modulation_hz * t + 0.4)
        output *= modulation.astype(np.float32)
    else:
        base_gain = float(track.synthesis.get("base_gain", 0.5))
        for event_index, seconds in enumerate(track.transient_reference_times):
            start = int(round(seconds * sample_rate))
            if kind == "rain_drops":
                wave = _drop_wave(seed, track, event_index, sample_rate)
            elif kind == "insects":
                wave = _insect_wave(seed, track, event_index, sample_rate)
            elif kind == "birds":
                wave = _bird_wave(seed, track, event_index, sample_rate)
            elif kind == "thunder":
                wave = _thunder_wave(seed, track, event_index, sample_rate)
            elif kind == "footsteps":
                wave = _footstep_wave(seed, track, event_index, sample_rate)
            else:
                raise ValueError(f"unknown synthesis kind: {kind}")
            _add(output, start, wave * base_gain)

    peak_target = float(track.synthesis.get("peak", 0.8))
    peak = float(np.max(np.abs(output))) if output.size else 0.0
    if peak > 0.0:
        output *= peak_target / peak
    return np.clip(output, -0.999_999, 0.999_999).astype(np.float32)


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def synthesize_project(manifest_path: str | Path) -> dict[str, Any]:
    manifest = load_manifest(manifest_path, verify_audio=False)
    if manifest.source_frames <= manifest.frames:
        raise ValueError("the source track length must exceed the loop length")
    results: list[dict[str, Any]] = []
    for track in manifest.tracks:
        audio = synthesize_track(manifest, track)
        track.path.parent.mkdir(parents=True, exist_ok=True)
        write_float32_wav(track.path, audio, manifest.sample_rate)
        results.append(
            {
                "channel": track.name,
                "scene": track.scene_name,
                "path": str(track.path.relative_to(manifest.source_path.parent)),
                "frames": int(audio.shape[0]),
                "sample_rate": manifest.sample_rate,
                "bit_depth": 32,
                "sha256": _sha256_file(track.path),
                "reference_start_samples": [
                    int(round(seconds * manifest.sample_rate))
                    for seconds in track.transient_reference_times
                ],
            }
        )
    return {
        "code": "SYNTHESIS_OK",
        "seed": manifest.seed,
        "source_frames": manifest.source_frames,
        "loop_frames": manifest.frames,
        "crossfade_frames": manifest.crossfade_frames,
        "tracks": results,
    }


def write_json_atomic(path: str | Path, payload: Any) -> Path:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    temp = output.with_suffix(output.suffix + ".tmp")
    temp.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    with temp.open("rb") as handle:
        os.fsync(handle.fileno())
    os.replace(temp, output)
    try:
        directory_fd = os.open(output.parent, os.O_RDONLY)
    except OSError:
        return output
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)
    return output
