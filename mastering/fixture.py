from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

import numpy as np

from .wav import write_wav_atomic


TRACK_BLUEPRINTS: tuple[dict[str, Any], ...] = (
    {
        "id": "kick",
        "name": "Kick",
        "file": "tracks/kick_48k_24.wav",
        "sample_rate": 48000,
        "bit_depth": 24,
        "channels": 1,
        "pan": 0.0,
        "kind": "kick",
        "drift_end_seconds": 0.025,
        "sync_jitter_samples": 0,
    },
    {
        "id": "snare",
        "name": "Snare",
        "file": "tracks/snare_48k_24.wav",
        "sample_rate": 48000,
        "bit_depth": 24,
        "channels": 1,
        "pan": 0.08,
        "kind": "snare",
        "drift_end_seconds": 0.018,
        "sync_jitter_samples": 0,
    },
    {
        "id": "overheads",
        "name": "Drum Overheads",
        "file": "tracks/overheads_96k_24.wav",
        "sample_rate": 96000,
        "bit_depth": 24,
        "channels": 2,
        "pan": 0.0,
        "kind": "overheads",
        "drift_end_seconds": -0.012,
        "sync_jitter_samples": 0,
    },
    {
        "id": "bass",
        "name": "Bass DI",
        "file": "tracks/bass_44k1_24.wav",
        "sample_rate": 44100,
        "bit_depth": 24,
        "channels": 1,
        "pan": 0.0,
        "kind": "bass",
        "drift_end_seconds": -0.020,
        "sync_jitter_samples": 0,
    },
    {
        "id": "guitar_l",
        "name": "Guitar Left",
        "file": "tracks/guitar_l_44k1_16.wav",
        "sample_rate": 44100,
        "bit_depth": 16,
        "channels": 1,
        "pan": -0.45,
        "kind": "guitar_l",
        "sync_jitter_samples": 0,
    },
    {
        "id": "guitar_r",
        "name": "Guitar Right",
        "file": "tracks/guitar_r_48k_16.wav",
        "sample_rate": 48000,
        "bit_depth": 16,
        "channels": 1,
        "pan": 0.45,
        "kind": "guitar_r",
        "sync_jitter_samples": 0,
    },
    {
        "id": "keys",
        "name": "Keys",
        "file": "tracks/keys_96k_24.wav",
        "sample_rate": 96000,
        "bit_depth": 24,
        "channels": 2,
        "pan": 0.0,
        "kind": "keys",
        "sync_jitter_samples": 0,
    },
    {
        "id": "lead_vocal",
        "name": "Lead Vocal",
        "file": "tracks/lead_vocal_48k_24.wav",
        "sample_rate": 48000,
        "bit_depth": 24,
        "channels": 1,
        "pan": 0.0,
        "kind": "lead_vocal",
        "sync_jitter_samples": 0,
    },
    {
        "id": "backing_vocal",
        "name": "Backing Vocal",
        "file": "tracks/backing_vocal_44k1_16.wav",
        "sample_rate": 44100,
        "bit_depth": 16,
        "channels": 2,
        "pan": 0.0,
        "kind": "backing_vocal",
        "sync_jitter_samples": 0,
    },
    {
        "id": "room",
        "name": "Room Mics",
        "file": "tracks/room_96k_16.wav",
        "sample_rate": 96000,
        "bit_depth": 16,
        "channels": 2,
        "pan": 0.0,
        "kind": "room",
        "drift_end_seconds": -0.008,
        "sync_jitter_samples": 0,
    },
    {
        "id": "click",
        "name": "Reference Click",
        "file": "tracks/click_44k1_16.wav",
        "sample_rate": 44100,
        "bit_depth": 16,
        "channels": 1,
        "pan": 0.0,
        "kind": "click",
        "sync_jitter_samples": 0,
    },
)


def _beat_phase(t: np.ndarray, bpm: float) -> np.ndarray:
    return np.mod(t * bpm / 60.0, 1.0)


def _base_signal(kind: str, t: np.ndarray, seed: int, channels: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    noise = rng.standard_normal((t.shape[0], channels))
    phase = _beat_phase(t, 112.0)
    beat_index = np.floor(t * 112.0 / 60.0).astype(np.int64)
    kick_env = np.exp(-phase * 22.0)
    snare_phase = np.mod(beat_index + 1, 4) < 2
    snare_env = np.where(snare_phase, np.exp(-phase * 16.0), 0.0)
    note_frequency = 55.0 * 2.0 ** ((beat_index % 8) // 2 / 12.0)
    vocal_note = 196.0 * 2.0 ** ((beat_index % 12) // 3 / 12.0)
    slow = 0.5 + 0.5 * np.sin(2.0 * np.pi * 0.035 * t)

    if kind == "kick":
        signal = 0.62 * np.sin(2.0 * np.pi * 57.0 * t) * kick_env
        signal += 0.08 * noise[:, 0] * np.exp(-phase * 55.0)
    elif kind == "snare":
        signal = 0.34 * noise[:, 0] * snare_env
        signal += 0.14 * np.sin(2.0 * np.pi * 185.0 * t) * snare_env
    elif kind == "overheads":
        cymbal = 0.14 * noise * (
            0.35 + 0.65 * np.exp(-phase * 5.0)
        )[:, None]
        body = 0.10 * np.column_stack(
            [
                np.sin(2.0 * np.pi * (87.0 + 0.7 * c) * t) * kick_env
                for c in range(channels)
            ]
        )
        signal = cymbal + body
    elif kind == "bass":
        signal = 0.30 * np.sin(2.0 * np.pi * note_frequency * t)
        signal *= 0.75 + 0.25 * np.exp(-phase * 3.0)
        signal += 0.025 * np.sin(2.0 * np.pi * 110.0 * t)
        signal = signal[:, None]
    elif kind in ("guitar_l", "guitar_r"):
        detune = -0.15 if kind == "guitar_l" else 0.15
        signal = 0.13 * np.sin(2.0 * np.pi * (147.0 + detune) * t)
        signal += 0.07 * np.sin(2.0 * np.pi * (220.0 + detune) * t + 0.3)
        signal *= 0.55 + 0.45 * slow
        signal = signal[:, None]
    elif kind == "keys":
        left = 0.11 * np.sin(2.0 * np.pi * 261.63 * t) * slow
        right = 0.11 * np.sin(2.0 * np.pi * 329.63 * t + 0.2) * (1.0 - 0.5 * slow)
        signal = np.column_stack((left, right))
    elif kind == "lead_vocal":
        vibrato = 1.0 + 0.004 * np.sin(2.0 * np.pi * 5.2 * t)
        signal = 0.20 * np.sin(2.0 * np.pi * vocal_note * vibrato * t)
        signal *= 0.45 + 0.55 * slow
        signal = signal[:, None]
    elif kind == "backing_vocal":
        left = 0.10 * np.sin(2.0 * np.pi * vocal_note * 1.25 * t + 0.1)
        right = 0.10 * np.sin(2.0 * np.pi * vocal_note * 1.5 * t + 0.5)
        signal = np.column_stack((left, right))
    elif kind == "room":
        signal = 0.06 * noise
        signal += 0.035 * np.column_stack(
            [
                np.sin(2.0 * np.pi * (58.0 + 0.1 * c) * t) * kick_env
                for c in range(channels)
            ]
        )
    elif kind == "click":
        accent = np.where(beat_index % 4 == 0, 1.0, 0.55)
        signal = 0.045 * accent * np.sin(2.0 * np.pi * 1000.0 * t) * np.exp(
            -phase * 70.0
        )
        signal = signal[:, None]
    else:
        raise ValueError(f"unknown fixture kind: {kind}")
    if signal.ndim == 1:
        signal = signal[:, None]
    if signal.shape[1] != channels:
        raise RuntimeError("fixture signal channel count does not match blueprint")
    return signal


def _sync_pulse(
    frames: int,
    sample_rate: int,
    start_sample: int,
    channels: int,
) -> np.ndarray:
    pulse = np.zeros((frames, channels), dtype=np.float64)
    length = max(8, min(int(round(0.004 * sample_rate)), frames - start_sample))
    n = np.arange(length, dtype=np.float64)
    envelope = np.exp(-n / max(1.0, 0.0012 * sample_rate))
    waveform = 0.36 * np.cos(2.0 * np.pi * 1700.0 * n / sample_rate) * envelope
    waveform[0] = 0.36
    for channel in range(channels):
        pulse[start_sample : start_sample + length, channel] += (
            waveform * (1.0 if channel == 0 else 0.93)
        )
    return pulse


def _anchor_source_sample(
    target_seconds: float,
    duration_seconds: float,
    sample_rate: int,
    end_drift_seconds: float,
) -> int:
    offset = end_drift_seconds * (target_seconds / duration_seconds) ** 2
    return int(round(target_seconds * sample_rate + offset * sample_rate))


def _automation(duration: float) -> dict[str, Any]:
    end = round(float(duration), 9)
    midpoint = duration * 0.5
    curves: dict[str, Any] = {}
    for blueprint in TRACK_BLUEPRINTS:
        curves[blueprint["id"]] = {
            "nodes": [
                [0.0, 0.0],
                [end, 0.0],
            ]
        }
    curves["kick"] = {
        "nodes": [
            [0.0, 0.0],
            [max(0.5, duration * 0.18), 0.0],
            [max(0.51, duration * 0.18 + 0.012), -12.0],
            [max(0.8, duration * 0.20), -12.0],
            [max(1.1, duration * 0.22), 0.0],
            [end, 0.0],
        ]
    }
    curves["bass"] = {
        "nodes": [
            [0.0, -2.0],
            [midpoint - 0.01, -2.0],
            [midpoint + 0.005, 5.0],
            [midpoint + 0.35, 0.0],
            [end, 0.0],
        ]
    }
    curves["lead_vocal"] = {
        "nodes": [
            [0.0, -4.0],
            [duration * 0.22, -4.0],
            [duration * 0.22 + 0.008, 4.0],
            [duration * 0.36, 0.0],
            [duration * 0.68, 0.0],
            [duration * 0.68 + 0.006, -10.0],
            [duration * 0.76, -10.0],
            [duration * 0.80, -1.0],
            [end, -1.0],
        ]
    }
    curves["keys"] = {
        "nodes": [
            [0.0, -6.0],
            [duration * 0.45, -6.0],
            [duration * 0.45 + 0.015, 3.0],
            [duration * 0.52, 1.0],
            [end, 0.0],
        ]
    }
    return {"tracks": curves}


def _output_config(
    output_sample_rate: int,
    output_format: str,
    target_lufs: float | None,
    true_peak_dbtp: float,
    duration_seconds: float | None,
) -> dict[str, Any]:
    output: dict[str, Any] = {
        "sample_rate": output_sample_rate,
        "channels": 2,
        "format": output_format,
        "true_peak_dbtp": true_peak_dbtp,
    }
    if target_lufs is not None:
        output["target_lufs"] = target_lufs
    if duration_seconds is not None:
        output["duration_seconds"] = duration_seconds
    return {
        "output": output,
        "automation": {
            "gain_min_db": -96.0,
            "gain_max_db": 24.0,
            "smooth_max_interval_seconds": 0.03,
            "smooth_min_gain_delta_db": 6.0,
            "smooth_duration_seconds": 0.24,
        },
        "limiter": {
            "lookahead_ms": 2.0,
            "release_ms": 45.0,
            "max_iterations": 8,
            "safety_margin_db": 0.12,
        },
        "true_peak": {"oversample": 4},
    }


def generate_fixture(
    destination: str | Path,
    *,
    duration_seconds: float = 180.0,
    output_sample_rate: int = 48000,
    output_format: str = "pcm_s24",
    target_lufs: float | None = -14.0,
    true_peak_dbtp: float = -1.0,
    with_drift: bool = True,
) -> dict[str, Path]:
    """Generate the complete deterministic 11-track acceptance fixture."""

    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    (destination / "tracks").mkdir(exist_ok=True)
    sync_target_seconds = 0.02
    tick_rate = 96000.0
    manifest_tracks: list[dict[str, Any]] = []

    for seed, blueprint in enumerate(TRACK_BLUEPRINTS, start=1):
        sample_rate = int(blueprint["sample_rate"])
        channels = int(blueprint["channels"])
        end_drift = float(blueprint.get("drift_end_seconds", 0.0)) if with_drift else 0.0
        source_duration = duration_seconds + max(0.05, abs(end_drift) + 0.05)
        frames = int(round(source_duration * sample_rate))
        t = np.arange(frames, dtype=np.float64) / sample_rate
        signal = _base_signal(str(blueprint["kind"]), t, seed * 1009, channels)
        preamble = int(round(0.05 * sample_rate))
        if preamble < frames:
            shifted = np.zeros_like(signal)
            shifted[preamble:] = signal[: frames - preamble]
            signal = shifted
            fade = min(int(round(0.01 * sample_rate)), frames - preamble)
            if fade > 0:
                signal[preamble : preamble + fade] *= np.linspace(
                    0.0, 1.0, fade, endpoint=False
                )[:, None]

        target_samples = np.asarray(
            [
                0.0,
                duration_seconds * 0.25,
                duration_seconds * 0.5,
                duration_seconds * 0.75,
                duration_seconds,
            ],
            dtype=np.float64,
        )
        anchor_source = [
            _anchor_source_sample(
                float(target), duration_seconds, sample_rate, end_drift
            )
            for target in target_samples
        ]
        for index, source_sample in enumerate(anchor_source):
            if index > 0 and 0 <= source_sample < frames:
                marker = 0.32
                signal[source_sample] += marker
                if source_sample + 1 < frames:
                    signal[source_sample + 1] -= marker * 0.65

        # Put sync positions through the inverse of the declared anchor map at
        # 20 ms, then add a small deliberate jitter.
        anchor_times = target_samples.tolist()
        anchor_values = [float(value) for value in anchor_source]
        source_at_sync = float(
            np.interp(sync_target_seconds, anchor_times, anchor_values)
        )
        pulse_start = int(
            round(source_at_sync + int(blueprint["sync_jitter_samples"]))
        )
        pulse_start = max(1, min(frames - 8, pulse_start))
        signal += _sync_pulse(frames, sample_rate, pulse_start, channels)
        signal = np.clip(signal, -0.98, 0.98)

        track_path = destination / str(blueprint["file"])
        track_path.parent.mkdir(parents=True, exist_ok=True)
        write_wav_atomic(
            track_path,
            signal,
            sample_rate,
            "pcm",
            int(blueprint["bit_depth"]),
        )
        manifest_track: dict[str, Any] = {
            "id": blueprint["id"],
            "name": blueprint["name"],
            "file": blueprint["file"],
            "sample_rate": sample_rate,
            "bit_depth": blueprint["bit_depth"],
            "channels": channels,
            "pan": blueprint["pan"],
            "sync": {
                "search_start_sample": 0,
                "search_end_sample": int(round(0.045 * sample_rate)),
                "threshold_ratio": 0.20,
                "min_peak": 1e-5,
                "target_time_seconds": sync_target_seconds,
            },
        }
        if end_drift != 0.0:
            manifest_track["drift"] = {
                "tick_rate": tick_rate,
                "anchors": [
                    {
                        "source_sample": int(source_sample),
                        "target_tick": int(round(target * tick_rate)),
                    }
                    for target, source_sample in zip(target_samples, anchor_source)
                ],
            }
        manifest_tracks.append(manifest_track)

    manifest = {
        "project": {
            "name": "Deterministic Eleven Track Session",
            "duration_seconds": duration_seconds,
            "tick_rate": tick_rate,
            "sync_target_seconds": sync_target_seconds,
        },
        "tracks": manifest_tracks,
    }
    config = _output_config(
        output_sample_rate,
        output_format,
        target_lufs,
        true_peak_dbtp,
        duration_seconds,
    )
    manifest_path = destination / "manifest.json"
    automation_path = destination / "automation.json"
    config_path = destination / "output_config.json"
    for path, payload in (
        (manifest_path, manifest),
        (automation_path, _automation(duration_seconds)),
        (config_path, config),
    ):
        with path.open("w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
            handle.write("\n")
    return {
        "directory": destination,
        "manifest": manifest_path,
        "automation": automation_path,
        "config": config_path,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic multitrack fixtures")
    parser.add_argument("--output", required=True, help="fixture destination directory")
    parser.add_argument("--duration", type=float, default=180.0)
    parser.add_argument("--sample-rate", type=int, default=48000)
    parser.add_argument("--format", default="pcm_s24")
    parser.add_argument("--target-lufs", type=float, default=-14.0)
    parser.add_argument("--true-peak-dbtp", type=float, default=-1.0)
    parser.add_argument("--no-drift", action="store_true")
    args = parser.parse_args()
    result = generate_fixture(
        args.output,
        duration_seconds=args.duration,
        output_sample_rate=args.sample_rate,
        output_format=args.format,
        target_lufs=args.target_lufs,
        true_peak_dbtp=args.true_peak_dbtp,
        with_drift=not args.no_drift,
    )
    print(json.dumps({key: str(value) for key, value in result.items()}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

