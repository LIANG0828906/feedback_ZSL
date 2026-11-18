from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf
from scipy import signal

from .errors import input_error
from .project import Project, load_project


SUBTYPE_BY_BITS = {
    16: "PCM_16",
    24: "PCM_24",
    32: "PCM_32",
    64: "DOUBLE",
}


def _sos(
    sample_rate: int,
    cutoff: float | tuple[float, float],
    btype: str,
    order: int = 4,
) -> np.ndarray:
    if isinstance(cutoff, tuple):
        normalized = [cutoff[0] / (sample_rate / 2), cutoff[1] / (sample_rate / 2)]
    else:
        normalized = cutoff / (sample_rate / 2)
    return signal.butter(order, normalized, btype=btype, output="sos")


def lowpass_deterministic(x: np.ndarray, sample_rate: int, cutoff: float) -> np.ndarray:
    if x.size < 16:
        return x.copy()
    sos = _sos(sample_rate, cutoff, "lowpass", order=4)
    padlen = min(x.size - 1, max(32, int(0.05 * sample_rate)))
    try:
        return signal.sosfiltfilt(sos, x, padlen=padlen)
    except ValueError:
        return signal.sosfilt(sos, x)


def bandpass_deterministic(
    x: np.ndarray,
    sample_rate: int,
    low: float,
    high: float,
    order: int = 4,
) -> np.ndarray:
    if x.size < 16:
        return x.copy()
    high = min(high, sample_rate * 0.49)
    sos = _sos(sample_rate, (low, high), "bandpass", order=order)
    padlen = min(x.size - 1, max(32, int(0.05 * sample_rate)))
    try:
        return signal.sosfiltfilt(sos, x, padlen=padlen)
    except ValueError:
        return signal.sosfilt(sos, x)


def _fade_envelope(length: int, sample_rate: int, attack_s: float, release_s: float) -> np.ndarray:
    env = np.ones(length, dtype=np.float64)
    attack = min(length // 2, max(1, int(attack_s * sample_rate)))
    release = min(length - attack, max(1, int(release_s * sample_rate)))
    env[:attack] *= np.linspace(0.0, 1.0, attack, endpoint=False)
    if release > 0:
        env[-release:] *= np.linspace(1.0, 0.0, release)
    return env


def _normalize_peak(x: np.ndarray, peak_db: float) -> np.ndarray:
    peak = float(np.max(np.abs(x))) if x.size else 0.0
    if peak <= 1.0e-15:
        return x
    return x * (10.0 ** (peak_db / 20.0) / peak)


def _actor_voice(
    sample_rate: int,
    length: int,
    rng: np.random.Generator,
    index: int,
) -> np.ndarray:
    output = np.zeros(length, dtype=np.float64)
    cursor = int((0.18 + 0.045 * index + rng.uniform(0.0, 0.08)) * sample_rate)
    base_f0 = 132.0 + 21.0 * index + rng.uniform(-4.0, 4.0)
    phrase_index = 0
    while cursor < length - int(0.35 * sample_rate):
        plosive_len = int(rng.uniform(0.025, 0.06) * sample_rate)
        plosive_len = min(plosive_len, length - cursor)
        if plosive_len > 8:
            noise = rng.normal(0.0, 1.0, plosive_len)
            noise = bandpass_deterministic(noise, sample_rate, 180.0, 6500.0)
            envelope = np.exp(-np.linspace(0.0, 8.0, plosive_len))
            output[cursor : cursor + plosive_len] += (
                noise * envelope * rng.uniform(0.20, 0.42)
            )
        cursor += plosive_len + int(0.012 * sample_rate)

        vowel_len = int(rng.uniform(0.30, 0.68) * sample_rate)
        vowel_len = min(vowel_len, length - cursor)
        if vowel_len > 16:
            t = np.arange(vowel_len, dtype=np.float64) / sample_rate
            f0 = base_f0 + 4.0 * np.sin(2.0 * np.pi * 0.65 * t + phrase_index * 0.7)
            phase = 2.0 * np.pi * np.cumsum(f0) / sample_rate
            vowel = np.zeros(vowel_len, dtype=np.float64)
            formants = (
                (420.0 + 75.0 * (index % 3), 130.0),
                (980.0 + 95.0 * (index % 2), 190.0),
                (2350.0 + 120.0 * (index % 4), 320.0),
            )
            for harmonic in range(1, 20):
                frequency = base_f0 * harmonic
                if frequency > sample_rate * 0.45:
                    break
                formant_gain = 1.0
                for center, width in formants:
                    formant_gain += 1.15 * np.exp(-0.5 * ((frequency - center) / width) ** 2)
                amplitude = formant_gain / (harmonic ** 1.28)
                vowel += amplitude * np.sin(harmonic * phase + 0.31 * harmonic + index)
            breath = bandpass_deterministic(
                rng.normal(0.0, 1.0, vowel_len), sample_rate, 900.0, 7000.0
            )
            envelope = _fade_envelope(vowel_len, sample_rate, 0.025, 0.055)
            vowel = vowel + 0.035 * breath
            output[cursor : cursor + vowel_len] += vowel * envelope * rng.uniform(0.65, 0.95)
        cursor += vowel_len

        pause_len = int(rng.uniform(0.26, 0.52) * sample_rate)
        cursor += pause_len
        phrase_index += 1

    output = bandpass_deterministic(output, sample_rate, 145.0, 9000.0, order=3)
    return _normalize_peak(output, -17.5 - 0.65 * index)


def _environment_voice(
    sample_rate: int,
    length: int,
    rng: np.random.Generator,
    index: int,
) -> np.ndarray:
    t = np.arange(length, dtype=np.float64) / sample_rate
    white = rng.normal(0.0, 1.0, length)
    air = bandpass_deterministic(white, sample_rate, 90.0, 12000.0)
    rumble = bandpass_deterministic(
        rng.normal(0.0, 1.0, length), sample_rate, 28.0, 160.0
    )
    ventilation = (
        np.sin(2.0 * np.pi * (47.0 + index * 5.0) * t + 0.3 * index)
        + 0.45 * np.sin(2.0 * np.pi * (83.0 + index * 3.0) * t + 0.7)
    )
    modulation = 1.0 + 0.13 * np.sin(2.0 * np.pi * (0.13 + 0.02 * index) * t)
    mixed = (0.75 * air + 0.10 * rumble + 0.025 * ventilation) * modulation
    return _normalize_peak(mixed, -25.0 - index)


def _audience_voice(
    sample_rate: int,
    length: int,
    rng: np.random.Generator,
) -> np.ndarray:
    t = np.arange(length, dtype=np.float64) / sample_rate
    base = bandpass_deterministic(
        rng.normal(0.0, 1.0, length), sample_rate, 180.0, 5200.0
    )
    syllable_modulation = 0.42 + 0.58 * (
        0.5 + 0.5 * np.sin(2.0 * np.pi * 3.7 * t + 0.4)
    )
    slow = 0.75 + 0.25 * np.sin(2.0 * np.pi * 0.21 * t + 1.1)
    bursts = np.zeros(length, dtype=np.float64)
    burst_count = max(3, int(length / sample_rate * 1.4))
    for _ in range(burst_count):
        start = int(rng.uniform(0.0, max(0.01, length / sample_rate - 0.18)) * sample_rate)
        burst_len = min(length - start, int(rng.uniform(0.07, 0.18) * sample_rate))
        if burst_len <= 0:
            continue
        burst = bandpass_deterministic(
            rng.normal(0.0, 1.0, burst_len), sample_rate, 240.0, 3800.0
        )
        bursts[start : start + burst_len] += burst * _fade_envelope(
            burst_len, sample_rate, 0.018, 0.035
        )
    mixed = 0.65 * base * syllable_modulation * slow + 0.75 * bursts
    return _normalize_peak(mixed, -27.0)


def _reference_voice(
    sample_rate: int,
    length: int,
    rng: np.random.Generator,
    cutoff: float,
) -> np.ndarray:
    t = np.arange(length, dtype=np.float64) / sample_rate
    noise = bandpass_deterministic(
        rng.normal(0.0, 1.0, length), sample_rate, 24.0, cutoff
    )
    hum = (
        np.sin(2.0 * np.pi * 37.0 * t + 0.2)
        + 0.45 * np.sin(2.0 * np.pi * 53.0 * t + 1.4)
        + 0.18 * np.sin(2.0 * np.pi * 71.0 * t + 0.8)
    )
    return _normalize_peak(noise + 0.08 * hum, -12.5)


def _generated_waveform(
    project: Project,
    track_index: int,
    source_type: str,
    rng: np.random.Generator,
    length: int,
) -> np.ndarray:
    if source_type == "actor":
        actor_index = sum(
            1 for item in project.tracks[:track_index] if item.source_type == "actor"
        )
        return _actor_voice(project.sample_rate, length, rng, actor_index)
    if source_type == "environment":
        environment_index = sum(
            1 for item in project.tracks[:track_index] if item.source_type == "environment"
        )
        return _environment_voice(project.sample_rate, length, rng, environment_index)
    if source_type == "audience":
        return _audience_voice(project.sample_rate, length, rng)
    if source_type == "speaker_reference":
        return _reference_voice(
            project.sample_rate, length, rng, project.leakage_lowpass_hz
        )
    raise input_error("E_SOURCE_TYPE", project.tracks[track_index].name, "unsupported source")


def _subtype_for_track(project: Project, track_index: int) -> str:
    bits = project.tracks[track_index].bit_depth
    if bits is None:
        bits = 24
    if bits not in SUBTYPE_BY_BITS:
        raise input_error(
            "E_BIT_DEPTH",
            project.tracks[track_index].name,
            f"cannot generate {bits}-bit audio",
        )
    return SUBTYPE_BY_BITS[bits]


def _write_deterministic(path: Path, data: np.ndarray, sample_rate: int, subtype: str) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.{path.suffix}.tmp")
    sf.write(
        str(temp),
        data,
        sample_rate,
        subtype=subtype,
        format="WAV",
        endian="LITTLE",
    )
    temp.replace(path)
    return hashlib.sha256(path.read_bytes()).hexdigest()


def generate_project_audio(project_path: str | Path) -> dict[str, Any]:
    """Generate all eight declared WAV files deterministically.

    The project's declared file paths are authoritative. Existing files are replaced
    only after a complete new file has been written.
    """

    project = load_project(project_path, validate_audio=False)
    length = int(round(project.duration_s * project.sample_rate))
    if length <= 0:
        raise input_error("E_PROJECT_SCHEMA", "project", "duration must produce at least one sample")

    reference_track = project.track_map[project.reference_channel]
    reference_rng = np.random.default_rng(
        np.random.SeedSequence([project.seed, 0x53504B52])
    )
    reference = _reference_voice(
        project.sample_rate,
        length,
        reference_rng,
        project.leakage_lowpass_hz,
    )
    ref_band = lowpass_deterministic(
        reference, project.sample_rate, project.leakage_lowpass_hz
    )

    waveforms: dict[str, np.ndarray] = {}
    for track in project.tracks:
        if track.name == reference_track.name:
            waveforms[track.name] = reference.copy()
            continue
        rng = np.random.default_rng(
            np.random.SeedSequence([project.seed, 0x54484541, track.index])
        )
        waveform = _generated_waveform(
            project, track.index, track.source_type, rng, length
        )
        if track.name in project.leakage_injection_channels:
            gain_db = project.leakage_per_channel_gain_db[track.name]
            delay_ms = float(
                project.raw.get("leakage", {}).get(
                    "injection_delay_ms",
                    project.raw.get("leakage", {}).get("delay_ms", 8.0 + 3.0 * track.index),
                )
            )
            delay = int(round(delay_ms * project.sample_rate / 1000.0))
            delayed = np.zeros(length, dtype=np.float64)
            if 0 <= delay < length:
                delayed[delay:] = ref_band[: length - delay]
            waveform = waveform + (10.0 ** (gain_db / 20.0)) * delayed
        peak = float(np.max(np.abs(waveform))) if waveform.size else 0.0
        if peak > 0.985:
            waveform *= 0.985 / peak
        waveforms[track.name] = waveform

    outputs: list[dict[str, Any]] = []
    for track in project.tracks:
        subtype = _subtype_for_track(project, track.index)
        waveform = waveforms[track.name]
        if track.channels > 1:
            waveform = np.repeat(waveform[:, None], track.channels, axis=1)
        digest = _write_deterministic(
            track.path,
            waveform,
            project.sample_rate,
            subtype,
        )
        outputs.append(
            {
                "name": track.name,
                "path": track.path_display,
                "source_type": track.source_type,
                "sample_rate": project.sample_rate,
                "channels": track.channels,
                "bit_depth": track.bit_depth,
                "sha256": digest,
            }
        )

    return {
        "status": "ok",
        "operation": "generate",
        "project_id": project.project_id,
        "seed": project.seed,
        "sample_rate": project.sample_rate,
        "frames": length,
        "leakage": {
            "reference_channel": project.reference_channel,
            "injection_channels": list(project.leakage_injection_channels),
            "gain_db": {
                name: project.leakage_per_channel_gain_db[name]
                for name in project.leakage_injection_channels
            },
        },
        "tracks": outputs,
    }


def dumps_stable(payload: Any) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2) + "\n"

