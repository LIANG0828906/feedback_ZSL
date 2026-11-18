from __future__ import annotations

import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf
from scipy import signal

from .manifest import REQUIRED_GROUPS, SUBTYPE_BY_BITS

TRACK_FILES = {
    "弦乐": ("01_strings.wav", "ref_01_strings.wav", "弦乐组"),
    "木管": ("02_woodwinds.wav", "ref_02_woodwinds.wav", "木管组"),
    "铜管": ("03_brass.wav", "ref_03_brass.wav", "铜管组"),
    "打击乐": ("04_percussion.wav", "ref_04_percussion.wav", "打击乐组"),
    "合唱": ("05_choir.wav", "ref_05_choir.wav", "合唱组"),
    "指挥提示": ("06_conductor_cue.wav", "ref_06_conductor_cue.wav", "指挥提示"),
}

DEFAULT_RATIOS = {
    "弦乐": 0.24,
    "木管": 0.19,
    "铜管": 0.18,
    "打击乐": 0.11,
    "合唱": 0.20,
    "指挥提示": 0.08,
}

DEFAULT_ANCHORS = {
    "弦乐": (-0.70, -1.10, 0.20),
    "木管": (0.18, 0.20, 0.35),
    "铜管": (0.70, 0.05, 0.45),
    "打击乐": (0.55, -1.45, 0.10),
    "合唱": (-0.18, 1.30, 0.55),
    "指挥提示": (0.00, -2.20, 0.80),
}

DEFAULT_ONSET_REFERENCES = {
    "弦乐": 0.220,
    "木管": 0.325,
    "铜管": 0.430,
    "打击乐": 0.535,
    "合唱": 0.640,
    "指挥提示": 0.745,
}


def _rng(seed: int, role_index: int, stream: int) -> np.random.Generator:
    return np.random.default_rng(np.random.SeedSequence([seed, role_index, stream]))


def _bandpass(data: np.ndarray, sample_rate: int, low: float, high: float) -> np.ndarray:
    nyquist = sample_rate / 2.0
    low = max(1.0, min(low, nyquist * 0.95))
    high = max(low + 1.0, min(high, nyquist * 0.98))
    sos = signal.butter(4, [low / nyquist, high / nyquist], btype="bandpass", output="sos")
    return signal.sosfiltfilt(sos, data)


def _window_gate(length: int, sample_rate: int, windows: list[tuple[float, float]], fade_seconds: float = 0.02) -> np.ndarray:
    gate = np.zeros(length, dtype=np.float64)
    fade = max(2, int(round(fade_seconds * sample_rate)))
    for start_seconds, end_seconds in windows:
        start = max(0, int(round(start_seconds * sample_rate)))
        end = min(length, int(round(end_seconds * sample_rate)))
        if end <= start:
            continue
        segment = np.ones(end - start, dtype=np.float64)
        ramp = min(fade, len(segment) // 2)
        if ramp > 1:
            curve = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, ramp))
            segment[:ramp] *= curve
            segment[-ramp:] *= curve[::-1]
        gate[start:end] = np.maximum(gate[start:end], segment)
    return gate


def _attack_marker(length: int, sample_rate: int, rng: np.random.Generator, scale: float = 0.20) -> np.ndarray:
    count = min(length, max(8, int(0.004 * sample_rate)))
    t = np.arange(count, dtype=np.float64) / sample_rate
    noise = rng.standard_normal(count) * np.exp(-t / 0.0012)
    noise[0] += 2.2
    maximum = float(np.max(np.abs(noise)))
    return noise * (scale / maximum)


def _harmonic_voice(
    time: np.ndarray,
    frequency: float,
    amplitudes: tuple[float, ...],
    vibrato_hz: float = 0.0,
    vibrato_depth: float = 0.0,
) -> np.ndarray:
    if vibrato_hz > 0 and vibrato_depth > 0:
        phase = 2.0 * np.pi * frequency * (
            time + (vibrato_depth / (2.0 * np.pi * vibrato_hz)) * np.sin(2.0 * np.pi * vibrato_hz * time)
        )
    else:
        phase = 2.0 * np.pi * frequency * time
    result = np.zeros_like(time)
    for harmonic, amplitude in enumerate(amplitudes, start=1):
        result += amplitude * np.sin(harmonic * phase + 0.11 * harmonic)
    return result


def _strings(block: np.ndarray, sample_rate: int, rng: np.random.Generator) -> None:
    time = np.arange(len(block), dtype=np.float64) / sample_rate
    swell = 0.68 + 0.20 * np.sin(2.0 * np.pi * 0.11 * time) + 0.10 * np.sin(2.0 * np.pi * 0.037 * time + 0.6)
    voice_a = _harmonic_voice(time, 196.0, (1.0, 0.52, 0.31, 0.18, 0.12, 0.07), 5.1, 0.006)
    voice_b = _harmonic_voice(time, 293.66 * 1.0017, (0.56, 0.32, 0.19, 0.11, 0.06), 4.7, 0.005)
    voice_c = _harmonic_voice(time, 392.0 * 0.9991, (0.28, 0.18, 0.12, 0.06), 5.7, 0.004)
    bow_noise = _bandpass(rng.standard_normal(len(block)), sample_rate, 1800.0, 9000.0)
    bow_env = np.zeros_like(time)
    for onset in (0.0, 2.05, 4.10, 6.15):
        local = time - onset
        mask = (local >= 0) & (local < 0.11)
        bow_env[mask] = np.maximum(bow_env[mask], 0.10 * np.exp(-local[mask] / 0.018))
    attack = 1.0 - np.exp(-time / 0.028)
    block[:] = (0.55 * voice_a + 0.44 * voice_b + 0.25 * voice_c) * swell * attack
    block += bow_noise * bow_env


def _woodwinds(block: np.ndarray, sample_rate: int, rng: np.random.Generator) -> None:
    time = np.arange(len(block), dtype=np.float64) / sample_rate
    notes = (
        (0.00, 0.26, 523.25, 0.95),
        (0.29, 0.18, 659.25, 1.00),
        (0.49, 0.42, 587.33, 0.82),
        (0.93, 0.22, 783.99, 1.00),
        (1.18, 0.46, 698.46, 0.72),
        (1.68, 0.21, 880.00, 0.98),
        (1.92, 0.34, 783.99, 0.82),
        (2.30, 0.22, 659.25, 1.00),
        (2.55, 0.55, 587.33, 0.70),
        (3.15, 0.18, 523.25, 0.95),
        (3.36, 0.36, 698.46, 0.80),
        (3.74, 0.19, 783.99, 1.00),
        (3.96, 0.44, 659.25, 0.72),
        (4.45, 0.24, 880.00, 0.95),
        (4.72, 0.12, 987.77, 1.00),
        (4.86, 0.49, 783.99, 0.75),
        (5.39, 0.20, 698.46, 0.95),
        (5.62, 0.39, 659.25, 0.78),
        (6.04, 0.28, 587.33, 0.95),
        (6.35, 0.52, 523.25, 0.70),
        (6.91, 0.34, 783.99, 0.88),
        (7.28, 0.50, 659.25, 0.65),
    )
    out = np.zeros_like(time)
    breath = _bandpass(rng.standard_normal(len(block)), sample_rate, 900.0, 7000.0)
    for start, duration, frequency, gain in notes:
        begin = int(round(start * sample_rate))
        end = min(len(block), int(round((start + duration) * sample_rate)))
        if end <= begin:
            continue
        local_t = np.arange(end - begin, dtype=np.float64) / sample_rate
        # A short dip before the next note supplies the staccato/legato contrast.
        attack = 1.0 - np.exp(-local_t / 0.007)
        release = np.clip((duration - local_t) / 0.030, 0.0, 1.0)
        envelope = attack * np.minimum(1.0, release)
        phase = 2.0 * np.pi * frequency * local_t + 0.6 * np.sin(2.0 * np.pi * 4.9 * local_t)
        tone = np.sin(phase) + 0.20 * np.sin(2.0 * phase) + 0.08 * np.sin(3.0 * phase)
        out[begin:end] += gain * envelope * tone
        out[begin:end] += 0.025 * gain * envelope * breath[begin:end]
    block[:] = out


def _brass(block: np.ndarray, sample_rate: int, rng: np.random.Generator) -> None:
    time = np.arange(len(block), dtype=np.float64) / sample_rate
    pulses = (
        (0.00, 0.34, 116.54, 1.00),
        (0.37, 0.28, 155.56, 0.91),
        (0.68, 0.43, 174.61, 1.00),
        (1.16, 0.46, 174.61, 1.18),
        (1.66, 0.33, 233.08, 1.14),
        (2.03, 0.52, 196.00, 1.22),
        (2.62, 0.41, 261.63, 1.10),
        (3.11, 0.35, 174.61, 0.92),
        (3.50, 0.32, 196.00, 0.90),
        (3.86, 0.45, 233.08, 1.06),
        (4.35, 0.48, 174.61, 1.25),
        (4.88, 0.40, 196.00, 1.18),
        (5.33, 0.36, 233.08, 1.14),
        (5.74, 0.44, 261.63, 1.08),
        (6.23, 0.33, 196.00, 0.96),
        (6.60, 0.53, 174.61, 0.88),
        (7.18, 0.55, 233.08, 0.76),
    )
    output = np.zeros_like(time)
    broadband = _bandpass(
        rng.standard_normal(len(block)),
        sample_rate,
        180.0,
        12000.0,
    )
    for start, duration, frequency, gain in pulses:
        begin = int(round(start * sample_rate))
        end = min(len(block), int(round((start + duration) * sample_rate)))
        if end <= begin:
            continue
        local = np.arange(end - begin, dtype=np.float64) / sample_rate
        attack = 1.0 - np.exp(-local / 0.004)
        release = np.clip((duration - local) / 0.045, 0.0, 1.0)
        envelope = attack * np.minimum(1.0, release)
        phase = 2.0 * np.pi * frequency * local
        tone = (
            np.sin(phase)
            + 0.62 * np.sin(2.0 * phase)
            + 0.44 * np.sin(3.0 * phase)
            + 0.30 * np.sin(4.0 * phase)
            + 0.19 * np.sin(5.0 * phase)
        )
        impact_env = np.exp(-local / 0.030)
        output[begin:end] += gain * envelope * (0.42 * tone + 0.20 * broadband[begin:end])
        output[begin:end] += gain * 0.10 * impact_env * broadband[begin:end]
    block[:] = output


def _percussion(block: np.ndarray, sample_rate: int, rng: np.random.Generator) -> None:
    time = np.arange(len(block), dtype=np.float64) / sample_rate
    pulses = (
        (0.00, 58.27, 1.10),
        (0.73, 77.78, 0.62),
        (1.32, 58.27, 0.88),
        (1.98, 87.31, 0.58),
        (2.51, 58.27, 1.00),
        (3.06, 77.78, 0.70),
        (3.71, 87.31, 0.76),
        (4.12, 58.27, 0.86),
        (4.83, 77.78, 0.67),
        (5.42, 58.27, 1.05),
        (6.14, 87.31, 0.72),
        (6.72, 58.27, 0.82),
        (7.30, 77.78, 0.58),
    )
    output = np.zeros_like(time)
    noise = rng.standard_normal(len(block))
    for start, frequency, gain in pulses:
        begin = int(round(start * sample_rate))
        if begin >= len(block):
            continue
        end = min(len(block), begin + int(0.48 * sample_rate))
        if end <= begin:
            continue
        local = np.arange(end - begin, dtype=np.float64) / sample_rate
        envelope = (1.0 - np.exp(-local / 0.002)) * np.exp(-local / 0.16)
        pitch_drop = frequency * (1.0 + 0.13 * np.exp(-local / 0.035))
        phase = 2.0 * np.pi * np.cumsum(pitch_drop) / sample_rate
        body = np.sin(phase) + 0.34 * np.sin(1.59 * phase) + 0.16 * np.sin(2.71 * phase)
        impact = _bandpass(noise[begin:end], sample_rate, 1800.0, 8000.0)
        output[begin:end] += gain * envelope * (0.62 * body + 0.12 * impact)
    block[:] = output


def _choir(block: np.ndarray, sample_rate: int, rng: np.random.Generator) -> None:
    time = np.arange(len(block), dtype=np.float64) / sample_rate
    f0 = 174.61 * (1.0 + 0.0025 * np.sin(2.0 * np.pi * 4.3 * time))
    phase = 2.0 * np.pi * np.cumsum(f0) / sample_rate
    vowel_mix = 0.5 + 0.5 * np.sin(2.0 * np.pi * 0.075 * time - 0.8)
    formants_a = ((650.0, 110.0, 1.0), (1080.0, 160.0, 0.72), (2500.0, 320.0, 0.38))
    formants_b = ((420.0, 90.0, 1.0), (1450.0, 180.0, 0.62), (2850.0, 360.0, 0.30))
    output = np.zeros_like(time)
    for harmonic in range(1, 21):
        frequency = harmonic * 174.61
        weight_a = sum(gain / (1.0 + ((frequency - center) / bandwidth) ** 2) for center, bandwidth, gain in formants_a)
        weight_b = sum(gain / (1.0 + ((frequency - center) / bandwidth) ** 2) for center, bandwidth, gain in formants_b)
        weight = (weight_a * (1.0 - vowel_mix) + weight_b * vowel_mix + 0.012) / harmonic**0.55
        output += weight * np.sin(harmonic * phase + 0.07 * harmonic)
    ensemble = (
        output
        + 0.42 * _harmonic_voice(time, 174.61 * 0.996, (1.0, 0.50, 0.24, 0.12), 4.1, 0.004)
        + 0.31 * _harmonic_voice(time, 174.61 * 1.004, (1.0, 0.45, 0.20, 0.10), 4.8, 0.003)
    )
    breath = _bandpass(rng.standard_normal(len(block)), sample_rate, 500.0, 4200.0)
    entrance = 1.0 - np.exp(-time / 0.070)
    dynamic = 0.78 + 0.15 * np.sin(2.0 * np.pi * 0.12 * time + 1.2)
    block[:] = entrance * dynamic * (ensemble + 0.025 * breath)


def _conductor_cue(block: np.ndarray, sample_rate: int, rng: np.random.Generator) -> None:
    time = np.arange(len(block), dtype=np.float64) / sample_rate
    output = np.zeros_like(time)
    clicks = (0.0, 0.50, 1.00, 1.50, 2.00, 2.50, 3.00, 3.50, 4.00, 4.50, 5.00, 5.50, 6.00, 6.50, 7.00, 7.50)
    click_noise = rng.standard_normal(len(block))
    for index, start in enumerate(clicks):
        begin = int(round(start * sample_rate))
        if begin >= len(block):
            continue
        end = min(len(block), begin + int(0.035 * sample_rate))
        if end <= begin:
            continue
        local = np.arange(end - begin, dtype=np.float64) / sample_rate
        envelope = np.exp(-local / 0.004)
        frequency = 1650.0 if index % 2 == 0 else 2200.0
        output[begin:end] += 0.16 * envelope * np.sin(2.0 * np.pi * frequency * local)
        output[begin:end] += 0.10 * envelope * click_noise[begin:end]
    commands = (
        (0.18, 0.24, 420.0, 980.0),
        (1.62, 0.30, 360.0, 830.0),
        (3.12, 0.22, 455.0, 1120.0),
        (4.58, 0.33, 330.0, 760.0),
        (6.12, 0.25, 410.0, 1010.0),
    )
    for start, duration, formant_a, formant_b in commands:
        begin = int(round(start * sample_rate))
        if begin >= len(block):
            continue
        end = min(len(block), int(round((start + duration) * sample_rate)))
        if end <= begin:
            continue
        local = np.arange(end - begin, dtype=np.float64) / sample_rate
        envelope = (1.0 - np.exp(-local / 0.012)) * np.clip((duration - local) / 0.035, 0.0, 1.0)
        carrier = np.sin(2.0 * np.pi * 126.0 * local)
        words = (
            np.sin(2.0 * np.pi * formant_a * local)
            + 0.55 * np.sin(2.0 * np.pi * formant_b * local)
            + 0.22 * np.sin(2.0 * np.pi * (formant_a + 460.0) * local)
        )
        gated_noise = _bandpass(
            click_noise[begin:end],
            sample_rate,
            180.0,
            3600.0,
        )
        output[begin:end] += 0.17 * envelope * carrier * words
        output[begin:end] += 0.012 * envelope * gated_noise
    block[:] = output


SYNTHESIZERS = {
    "弦乐": _strings,
    "木管": _woodwinds,
    "铜管": _brass,
    "打击乐": _percussion,
    "合唱": _choir,
    "指挥提示": _conductor_cue,
}


def _build_clean_bases(
    sample_rate: int,
    duration: float,
    source_onsets: dict[str, float],
    seed: int,
) -> dict[str, np.ndarray]:
    length = int(round(duration * sample_rate))
    result: dict[str, np.ndarray] = {}
    for role_index, role in enumerate(REQUIRED_GROUPS):
        start = max(0, int(round(source_onsets[role] * sample_rate)))
        available = length - start
        if available < sample_rate // 4:
            raise ValueError(f"{role} 的起音时刻过早，剩余时长不足")
        block = np.zeros(available, dtype=np.float64)
        rng = _rng(seed, role_index, 101)
        SYNTHESIZERS[role](block, sample_rate, rng)
        marker_rng = _rng(seed, role_index, 211)
        marker = _attack_marker(available, sample_rate, marker_rng, scale=0.23)
        block[: len(marker)] += marker
        peak = float(np.max(np.abs(block)))
        if peak > 0:
            block *= 0.32 / peak
        full = np.zeros(length, dtype=np.float64)
        full[start:] = block
        result[role] = full
    return result


def _make_target_and_reference(
    role: str,
    clean: np.ndarray,
    bases: dict[str, np.ndarray],
    relations: list[dict[str, Any]],
    sample_rate: int,
) -> tuple[np.ndarray, np.ndarray]:
    target = clean.copy()
    reference = clean.copy()
    for relation in relations:
        if relation["target_group"] != role:
            continue
        source = bases[relation["source_group"]]
        low, high = relation["band_hz"]
        filtered = _bandpass(source, sample_rate, float(low), float(high))
        windows = [tuple(pair) for pair in relation["window_seconds"]]
        gate = _window_gate(len(source), sample_rate, windows, fade_seconds=0.015)
        gated = filtered * gate
        source_energy = float(np.sum(source[gate > 0.999] ** 2))
        source_rms = math.sqrt(source_energy / max(1, int(np.count_nonzero(gate > 0.999))))
        injection_rms = source_rms * 10.0 ** (float(relation["gain_db"]) / 20.0)
        current_rms = math.sqrt(float(np.mean(gated[gate > 0.999] ** 2))) if np.any(gate > 0.999) else 0.0
        if current_rms > 0:
            target += gated * (injection_rms / current_rms)
    peak = max(1e-12, float(np.max(np.abs(target))), float(np.max(np.abs(reference))))
    scale = min(1.0, 0.88 / peak)
    return target * scale, reference * scale


def _write_wav(path: Path, data: np.ndarray, sample_rate: int, bit_depth: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    subtype = SUBTYPE_BY_BITS[bit_depth]
    temporary = path.with_name(path.name + ".tmp")
    sf.write(str(temporary), np.asarray(data, dtype=np.float32), sample_rate, format="WAV", subtype=subtype)
    os.replace(temporary, path)


def _atomic_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    payload = json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    with temporary.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def create_demo_project(
    output_dir: str | Path,
    *,
    seed: int = 24701,
    duration_seconds: float = 8.0,
    sample_rate: int = 48000,
    bit_depth: int = 24,
    block_frames: int = 16384,
    force: bool = False,
) -> Path:
    root = Path(output_dir).resolve()
    manifest_path = root / "manifest.json"
    audio_dir = root / "audio"
    if manifest_path.exists() and not force:
        from .errors import BusinessError, Issue

        raise BusinessError(
            Issue(
                "E_OUTPUT_EXISTS",
                f"输出目录已包含清单: {manifest_path}",
                channel="manifest",
                details={"path": str(manifest_path), "hint": "使用 --force 覆盖"},
            )
        )
    if sample_rate < 8000:
        raise ValueError("sample_rate 必须不小于 8000")
    if bit_depth not in SUBTYPE_BY_BITS:
        raise ValueError("bit_depth 仅支持 16/24/32")
    root.mkdir(parents=True, exist_ok=True)

    jitter_rng = np.random.default_rng(np.random.SeedSequence([seed, 7781]))
    source_onsets: dict[str, float] = {}
    for role in REQUIRED_GROUPS:
        jitter = float(jitter_rng.uniform(-0.036, 0.036))
        source_onsets[role] = round(DEFAULT_ONSET_REFERENCES[role] + jitter, 6)

    bases = _build_clean_bases(sample_rate, duration_seconds, source_onsets, seed)
    windows = [[1.15, 2.80], [4.20, 5.55]]
    masking = [
        {
            "source_group": "铜管",
            "target_group": "木管",
            "gain_db": -7.0,
            "band_hz": [180.0, 9000.0],
            "window_seconds": windows,
        },
        {
            "source_group": "合唱",
            "target_group": "弦乐",
            "gain_db": -8.0,
            "band_hz": [260.0, 3400.0],
            "window_seconds": windows,
        },
    ]
    tracks: list[dict[str, Any]] = []
    for role in REQUIRED_GROUPS:
        stem_name, reference_name, display_name = TRACK_FILES[role]
        target, reference = _make_target_and_reference(role, bases[role], bases, masking, sample_rate)
        stem_path = audio_dir / stem_name
        reference_path = audio_dir / reference_name
        _write_wav(stem_path, target, sample_rate, bit_depth)
        _write_wav(reference_path, reference, sample_rate, bit_depth)
        anchor = DEFAULT_ANCHORS[role]
        tracks.append(
            {
                "name": display_name,
                "group": role,
                "path": stem_path.relative_to(root).as_posix(),
                "reference_path": reference_path.relative_to(root).as_posix(),
                "anchor": {"x": anchor[0], "y": anchor[1], "z": anchor[2]},
                "target_energy_ratio": DEFAULT_RATIOS[role],
                "onset_reference_seconds": DEFAULT_ONSET_REFERENCES[role],
                "source_onset_seconds": source_onsets[role],
                "channels": 1,
            }
        )

    manifest = {
        "schema_version": 1,
        "seed": seed,
        "sample_rate": sample_rate,
        "bit_depth": bit_depth,
        "channels": 1,
        "duration_seconds": duration_seconds,
        "block_frames": block_frames,
        "synthesis": {
            "profile": "symphony_rehearsal_v1",
            "generator": "deterministic_numpy_scipy_v1",
        },
        "master": {
            "target_loudness_lufs": -18.0,
            "true_peak_max_dbtp": -1.0,
        },
        "unmasking": {
            "window_seconds": windows,
            "residual_threshold_db": -24.0,
            "min_attenuation_db": 12.0,
        },
        "masking": masking,
        "tracks": tracks,
    }
    _atomic_json(manifest_path, manifest)
    return manifest_path
