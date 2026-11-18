"""Deterministic synthetic Peking-opera percussion sources."""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .dsp import reflection_decay_envelope
from .errors import canonical_json_bytes
from .manifest import validate_manifest_document

INSTRUMENT_PROFILES: dict[str, dict[str, Any]] = {
    "板鼓": {
        "length_ms": 12.0,
        "frequencies": [1750.0, 2600.0, 3450.0],
        "decays": [0.0028, 0.0038, 0.0020],
        "noise_decay": 0.0018,
        "noise_mix": 0.58,
        "amplitude": 0.72,
    },
    "大锣": {
        "length_ms": 720.0,
        "frequencies": [112.0, 169.0, 243.0, 331.0],
        "decays": [0.46, 0.34, 0.24, 0.16],
        "noise_decay": 0.018,
        "noise_mix": 0.19,
        "amplitude": 0.62,
    },
    "小锣": {
        "length_ms": 20.0,
        "frequencies": [760.0, 1140.0, 1690.0, 2290.0],
        "decays": [0.070, 0.052, 0.036, 0.025],
        "noise_decay": 0.004,
        "noise_mix": 0.24,
        "amplitude": 0.60,
    },
    "铙钹": {
        "length_ms": 450.0,
        "frequencies": [431.0, 691.0, 1127.0, 1679.0, 2381.0],
        "decays": [0.31, 0.24, 0.16, 0.11, 0.075],
        "noise_decay": 0.070,
        "noise_mix": 0.76,
        "amplitude": 0.58,
    },
    "堂鼓": {
        "length_ms": 30.0,
        "frequencies": [72.0, 108.0, 155.0],
        "decays": [0.230, 0.165, 0.105],
        "noise_decay": 0.018,
        "noise_mix": 0.28,
        "amplitude": 0.80,
    },
}


DEMO_CUES = [
    0.55,
    0.90,
    1.25,
    1.65,
    2.05,
    2.45,
    2.90,
    3.35,
    3.85,
    4.35,
    4.90,
    5.45,
    6.05,
    6.65,
    7.25,
]


def make_demo_manifest() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "sample_rate": 48000,
        "bit_depth": 24,
        "duration_seconds": 8.80,
        "block_frames": 4096,
        "target_loudness_lufs": -16.0,
        "true_peak_limit_dbtp": -1.0,
        "channel_search_ms": {"before": 120.0, "after": 120.0},
        "channels": [
            {
                "name": "bangu",
                "instrument": "板鼓",
                "file": "audio/bangu.wav",
                "seed": 1103,
                "start_offset_samples": 731,
                "fallback_alignment_samples": -731,
                "cues": list(DEMO_CUES),
                "mix_weights": [0.95, 0.88],
                "early_reflection": {
                    "enabled": True,
                    "delay_ms": 28.5,
                    "gain_db": -7.5,
                    "window_ms": [20.0, 45.0],
                },
            },
            {
                "name": "daluo",
                "instrument": "大锣",
                "file": "audio/daluo.wav",
                "seed": 2207,
                "start_offset_samples": 1489,
                "fallback_alignment_samples": -1489,
                "cues": list(DEMO_CUES),
                "mix_weights": [0.82, 0.76],
                "early_reflection": {"enabled": False},
            },
            {
                "name": "xiaoluo",
                "instrument": "小锣",
                "file": "audio/xiaoluo.wav",
                "seed": 3301,
                "start_offset_samples": 2213,
                "fallback_alignment_samples": -2213,
                "cues": list(DEMO_CUES),
                "mix_weights": [0.70, 0.84],
                "early_reflection": {
                    "enabled": True,
                    "delay_ms": 44.0,
                    "gain_db": -9.0,
                    "window_ms": [30.0, 70.0],
                },
            },
            {
                "name": "naobo",
                "instrument": "铙钹",
                "file": "audio/naobo.wav",
                "seed": 4409,
                "start_offset_samples": 2939,
                "fallback_alignment_samples": -2939,
                "cues": list(DEMO_CUES),
                "mix_weights": [0.76, 0.72],
                "early_reflection": {"enabled": False},
            },
            {
                "name": "tanggu",
                "instrument": "堂鼓",
                "file": "audio/tanggu.wav",
                "seed": 5501,
                "start_offset_samples": 347,
                "fallback_alignment_samples": -347,
                "cues": list(DEMO_CUES),
                "mix_weights": [0.98, 0.94],
                "early_reflection": {
                    "enabled": True,
                    "delay_ms": 58.0,
                    "gain_db": -8.5,
                    "window_ms": [38.0, 100.0],
                },
            },
        ],
    }


def _profile(instrument: str) -> dict[str, Any]:
    if instrument in INSTRUMENT_PROFILES:
        return INSTRUMENT_PROFILES[instrument]
    return {
        "length_ms": 80.0,
        "frequencies": [340.0, 710.0, 1290.0],
        "decays": [0.06, 0.045, 0.03],
        "noise_decay": 0.008,
        "noise_mix": 0.5,
        "amplitude": 0.65,
    }


def make_hit(
    instrument: str,
    seed: int,
    event_index: int,
    sample_rate: int,
) -> np.ndarray:
    profile = _profile(instrument)
    rng = np.random.default_rng(
        (int(seed) + (event_index + 1) * 0x9E3779B1) & 0xFFFFFFFFFFFFFFFF
    )
    count = max(4, round(profile["length_ms"] * sample_rate / 1000.0))
    time = np.arange(count, dtype=np.float64) / float(sample_rate)
    output = np.zeros(count, dtype=np.float64)
    for frequency, decay in zip(profile["frequencies"], profile["decays"]):
        phase = float(rng.uniform(0.0, 2.0 * math.pi))
        output += (
            np.sin(2.0 * math.pi * frequency * time + phase)
            * np.exp(-time / decay)
            / math.sqrt(len(profile["frequencies"]))
        )
    noise = rng.standard_normal(count) * np.exp(
        -time / float(profile["noise_decay"])
    )
    output = (
        (1.0 - float(profile["noise_mix"])) * output
        + float(profile["noise_mix"]) * noise
    )
    # A deterministic broadband attack sample makes the physical onset
    # unambiguous without placing any energy before it.
    output[0] += 0.95 * (1.0 if rng.random() >= 0.5 else -1.0)
    if count > 1:
        output[1] += 0.38 * (1.0 if rng.random() >= 0.5 else -1.0)
    output /= max(1.0e-12, float(np.max(np.abs(output))))
    output *= float(profile["amplitude"]) * float(rng.uniform(0.94, 1.0))

    fade_length = min(count // 8, max(2, int(0.001 * sample_rate)))
    if fade_length > 1:
        fade = 0.5 - 0.5 * np.cos(
            np.linspace(0.0, math.pi, fade_length, dtype=np.float64)
        )
        output[-fade_length:] *= fade
    return output


def synthesize_channel(
    channel: dict[str, Any],
    sample_rate: int,
    total_frames: int,
) -> np.ndarray:
    csr = int(sample_rate)
    output = np.zeros(total_frames, dtype=np.float64)
    instrument = str(channel["instrument"])
    seed = int(channel["seed"])
    start_offset = int(channel["start_offset_samples"])
    reflection = channel.get("early_reflection", {})
    reflection_enabled = bool(reflection.get("enabled", False))
    delay = round(float(reflection.get("delay_ms", 0.0)) * csr / 1000.0)
    gain = float(10.0 ** (float(reflection.get("gain_db", -160.0)) / 20.0))
    reflection_window = reflection.get("window_ms", [0.0, 0.0])
    window_start = round(float(reflection_window[0]) * csr / 1000.0)
    window_end = round(float(reflection_window[1]) * csr / 1000.0)

    for event_index, cue in enumerate(channel["cues"]):
        hit = make_hit(instrument, seed, event_index, csr)
        start = round(float(cue) * csr) + start_offset
        end = start + hit.size
        if start < 0 or end > total_frames:
            raise ValueError(
                f"channel {channel['name']} event {event_index} exceeds duration"
            )
        output[start:end] += hit
        if reflection_enabled:
            envelope = reflection_decay_envelope(
                hit.size, window_start, window_end
            )
            reflection_start = start + delay
            reflection_end = reflection_start + hit.size
            if reflection_end > total_frames:
                raise ValueError(
                    f"channel {channel['name']} reflection {event_index} "
                    "exceeds duration"
                )
            output[reflection_start:reflection_end] += gain * hit * envelope
    return output


def generate_channels(
    document: dict[str, Any],
    manifest_path: str | Path,
) -> list[dict[str, Any]]:
    errors = validate_manifest_document(document)
    if errors:
        raise ValueError("manifest validation failed: " + errors[0]["code"])
    base_dir = Path(manifest_path).resolve().parent
    sample_rate = int(document["sample_rate"])
    bit_depth = int(document["bit_depth"])
    subtype = f"PCM_{bit_depth}"
    total_frames = round(float(document["duration_seconds"]) * sample_rate)
    results: list[dict[str, Any]] = []
    for channel in document["channels"]:
        signal = synthesize_channel(channel, sample_rate, total_frames)
        destination = Path(channel["file"])
        if not destination.is_absolute():
            destination = base_dir / destination
        destination.parent.mkdir(parents=True, exist_ok=True)
        sf.write(
            str(destination),
            signal,
            sample_rate,
            subtype=subtype,
            format="WAV",
        )
        results.append(
            {
                "name": channel["name"],
                "file": Path(channel["file"]).as_posix(),
                "frames": total_frames,
                "sha256": _sha256(destination),
            }
        )
    return results


def _sha256(path: Path) -> str:
    import hashlib

    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_demo(directory: str | Path) -> tuple[Path, Path]:
    root = Path(directory)
    root.mkdir(parents=True, exist_ok=True)
    manifest = make_demo_manifest()
    manifest_path = root / "manifest.json"
    manifest_path.write_bytes(
        json.dumps(manifest, ensure_ascii=False, indent=2).encode("utf-8") + b"\n"
    )
    generate_channels(manifest, manifest_path)
    return manifest_path, root / "master.wav"


def demo_manifest_bytes() -> bytes:
    return canonical_json_bytes(make_demo_manifest())

