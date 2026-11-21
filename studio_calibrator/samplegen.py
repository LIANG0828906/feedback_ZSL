from __future__ import annotations

import argparse
import math
import shutil
import sys
import wave
import zlib
from pathlib import Path
from typing import Dict, Iterable, List, Optional, Tuple

import numpy as np

from .errors import E_IO, BusinessError
from .util import canonical_json_bytes, write_json_atomic


SAMPLE_RATE = 48000
DEFAULT_SPEAKERS = ("FL", "FR", "FC", "LFE", "BL", "BR")
DEFAULT_RECEIVERS = ("FL", "FR", "FC", "LFE", "BL", "BR")
DEFAULT_SPEED_OF_SOUND = 343.0
DEFAULT_BANDS = (
    ("B0", 20.0, 120.0, 20.0),
    ("B1", 120.0, 500.0, 30.0),
    ("B2", 500.0, 2000.0, 60.0),
    ("B3", 2000.0, 6000.0, 200.0),
    ("B4", 6000.0, 12000.0, 400.0),
    ("B5", 12000.0, 20000.0, 800.0),
)
DEFAULT_SPEAKER_POSITIONS = {
    "FL": (-1.7, 1.0, 0.2),
    "FR": (1.7, 1.0, 0.2),
    "FC": (0.0, 1.2, 0.2),
    "LFE": (0.0, 0.6, 0.0),
    "BL": (-1.2, -1.2, 0.3),
    "BR": (1.2, -1.2, 0.3),
}
DEFAULT_RECEIVER_POSITIONS = {
    "FL": (-0.75, 0.35, 0.1),
    "FR": (0.75, 0.35, 0.1),
    "FC": (0.0, 0.55, 0.1),
    "LFE": (0.0, 0.0, 0.0),
    "BL": (-0.65, -0.45, 0.1),
    "BR": (0.65, -0.45, 0.1),
}


def _synthesis_weight(
    frequencies: np.ndarray,
    low: float,
    high: float,
    transition: float,
) -> np.ndarray:
    weight = np.zeros_like(frequencies, dtype=np.float64)
    half = transition / 2.0
    rise_start = low - half
    rise_end = low + half
    fall_start = high - half
    fall_end = high + half
    rising = (frequencies > rise_start) & (frequencies < rise_end)
    plateau = (frequencies >= rise_end) & (frequencies <= fall_start)
    falling = (frequencies > fall_start) & (frequencies < fall_end)
    weight[rising] = 0.5 * (
        1.0
        - np.cos(
            math.pi
            * (frequencies[rising] - rise_start)
            / max(rise_end - rise_start, np.finfo(float).eps)
        )
    )
    weight[plateau] = 1.0
    weight[falling] = 0.5 * (
        1.0
        + np.cos(
            math.pi
            * (frequencies[falling] - fall_start)
            / max(fall_end - fall_start, np.finfo(float).eps)
        )
    )
    return weight


def _pcm16(values: np.ndarray) -> np.ndarray:
    return np.clip(np.rint(values * 32768.0), -32768, 32767).astype("<i2")


def _write_wav(path: Path, int16: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = np.asarray(int16, dtype="<i2")
    if data.ndim == 1:
        data = data[:, None]
    interleaved = np.ascontiguousarray(data)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(int(data.shape[1]))
        handle.setsampwidth(2)
        handle.setframerate(SAMPLE_RATE)
        handle.writeframes(interleaved.tobytes(order="C"))


def _stable_seed(base: int, *parts: object) -> int:
    label = "|".join([str(base)] + [str(part) for part in parts])
    return int(zlib.crc32(label.encode("utf-8")) & 0xFFFFFFFF)


def _distance(left: Tuple[float, float, float], right: Tuple[float, float, float]) -> float:
    return math.sqrt(sum((a - b) ** 2 for a, b in zip(left, right)))


def _source_signal(seed: int, source_index: int, duration: float) -> np.ndarray:
    rng = np.random.default_rng(_stable_seed(seed, "source", source_index))
    count = int(round(SAMPLE_RATE * duration))
    signal = rng.standard_normal(count)
    signal -= float(np.mean(signal))
    spectrum = np.fft.rfft(signal)
    frequencies = np.fft.rfftfreq(count, 1.0 / SAMPLE_RATE)
    spectrum[frequencies < 20.0] = 0.0
    spectrum[frequencies > 20000.0] = 0.0
    signal = np.fft.irfft(spectrum, n=count)
    rms = float(np.sqrt(np.mean(signal**2)))
    signal = signal / max(rms, 1e-12) * 0.18
    peak = float(np.max(np.abs(signal)))
    if peak > 0.92:
        signal *= 0.92 / peak
    return _pcm16(signal).astype(np.float64) / 32768.0


def _coupling_plan(
    seed: int,
    speakers: Tuple[str, ...],
    bands: Tuple[Tuple[str, float, float, float], ...],
) -> Dict[Tuple[str, str], Dict[str, Dict[str, float]]]:
    plan: Dict[Tuple[str, str], Dict[str, Dict[str, float]]] = {}
    for source_index, source in enumerate(speakers):
        for receiver_index, receiver in enumerate(speakers):
            if source == receiver:
                plan[(source, receiver)] = {
                    band[0]: {"gain_db": 0.0, "phase_deg": 0.0} for band in bands
                }
                continue
            rng = np.random.default_rng(
                _stable_seed(seed, "coupling", source, receiver)
            )
            base_gain = -10.0 - float(rng.integers(0, 12))
            per_band: Dict[str, Dict[str, float]] = {}
            for band_index, band in enumerate(bands):
                gain = base_gain + float(rng.uniform(-2.0, 2.0))
                phase = float(rng.uniform(-22.0, 22.0))
                if (source_index + receiver_index + band_index) % 3 == 0:
                    phase += 5.0
                per_band[band[0]] = {
                    "gain_db": round(gain, 9),
                    "phase_deg": round(phase, 9),
                }
            if (source, receiver) == ("FL", "FR"):
                for value in per_band.values():
                    value["phase_deg"] = round(value["phase_deg"] + 180.0, 9)
            if (source, receiver) == ("FR", "FL"):
                offsets = (-150.0, -100.0, -20.0, 60.0, 130.0, 175.0)
                for band_index, value in enumerate(per_band.values()):
                    value["phase_deg"] = offsets[band_index % len(offsets)]
            plan[(source, receiver)] = per_band
    return plan


def _transfer(
    frequencies: np.ndarray,
    delay_samples: float,
    plan: Dict[str, Dict[str, float]],
    bands: Tuple[Tuple[str, float, float, float], ...],
) -> np.ndarray:
    transfer = np.zeros_like(frequencies, dtype=np.complex128)
    for band_id, low, high, transition in bands:
        gain = 10.0 ** (plan[band_id]["gain_db"] / 20.0)
        phase = math.radians(plan[band_id]["phase_deg"])
        transfer += gain * np.exp(1j * phase) * _synthesis_weight(
            frequencies, low, high, transition
        )
    transfer *= np.exp(-2j * math.pi * frequencies * delay_samples / SAMPLE_RATE)
    return transfer


def _quantization_noise(
    seed: int,
    source: str,
    receiver: str,
    count: int,
) -> np.ndarray:
    rng = np.random.default_rng(_stable_seed(seed, "noise", source, receiver))
    return rng.integers(-3, 4, size=count, dtype=np.int16)


def generate_sample(
    output: Path,
    seed: int = 42,
    duration: float = 1.0,
    force: bool = False,
) -> Dict[str, object]:
    if duration <= 0.0:
        raise BusinessError(E_IO, "duration must be positive")
    if output.exists() and any(output.iterdir()):
        if not force:
            raise BusinessError(
                E_IO,
                "output directory is not empty; pass --force to replace it",
                details={"output": str(output)},
            )
        shutil.rmtree(output)
    output.mkdir(parents=True, exist_ok=True)
    (output / "stimuli").mkdir()
    (output / "mono").mkdir()
    (output / "pickups_5_1").mkdir()

    speakers = DEFAULT_SPEAKERS
    receivers = DEFAULT_RECEIVERS
    bands = DEFAULT_BANDS
    coupling = _coupling_plan(seed, speakers, bands)
    count = int(round(SAMPLE_RATE * duration))
    frequencies = np.fft.rfftfreq(count, 1.0 / SAMPLE_RATE)
    stimuli: Dict[str, np.ndarray] = {}
    for source_index, source in enumerate(speakers):
        signal = _source_signal(seed, source_index, duration)
        stimuli[source] = signal
        _write_wav(output / "stimuli" / f"{source}.wav", _pcm16(signal))

    distances: Dict[Tuple[str, str], Dict[str, float]] = {}
    for source in speakers:
        source_position = DEFAULT_SPEAKER_POSITIONS[source]
        for receiver in receivers:
            receiver_position = DEFAULT_RECEIVER_POSITIONS[receiver]
            raw_distance = _distance(source_position, receiver_position)
            delay = int(round(raw_distance / DEFAULT_SPEED_OF_SOUND * SAMPLE_RATE))
            adjusted_distance = delay * DEFAULT_SPEED_OF_SOUND / SAMPLE_RATE
            distances[(source, receiver)] = {
                "raw_distance_m": raw_distance,
                "distance_m": adjusted_distance,
                "delay_samples": float(delay),
            }

    pair_components: Dict[Tuple[str, str], np.ndarray] = {}
    multichannel: Dict[str, np.ndarray] = {}
    for source in speakers:
        source_spectrum = np.fft.rfft(stimuli[source])
        capture = np.zeros((count, len(receivers)), dtype=np.int16)
        for receiver_index, receiver in enumerate(receivers):
            delay = distances[(source, receiver)]["delay_samples"]
            transfer = _transfer(frequencies, delay, coupling[(source, receiver)], bands)
            signal = np.fft.irfft(source_spectrum * transfer, n=count)
            noise = _quantization_noise(seed, source, receiver, count)
            quantized = np.clip(
                _pcm16(signal).astype(np.int32) + noise.astype(np.int32),
                -32768,
                32767,
            ).astype("<i2")
            pair_components[(source, receiver)] = quantized
            capture[:, receiver_index] = quantized
            _write_wav(
                output / "mono" / f"{source}__{receiver}.wav",
                quantized,
            )
        multichannel[source] = capture
        _write_wav(
            output / "pickups_5_1" / f"{source}.wav",
            capture,
        )

    manifest = {
        "schema_version": 1,
        "sample_rate": SAMPLE_RATE,
        "speakers": list(speakers),
        "receivers": list(receivers),
        "main_receivers": {source: source for source in speakers},
        "stimuli": {source: f"stimuli/{source}.wav" for source in speakers},
        "layouts": {
            "mono": {
                "description": "one independently quantized WAV per source-receiver pair",
                "pairs": [
                    {
                        "source": source,
                        "receiver": receiver,
                        "path": f"mono/{source}__{receiver}.wav",
                        "channel": 0,
                    }
                    for source in speakers
                    for receiver in receivers
                ],
            },
            "5.1": {
                "description": "six-channel pickup capture in WAVE FL FR FC LFE BL BR order",
                "channel_order": list(receivers),
                "pairs": [
                    {
                        "source": source,
                        "receiver": receiver,
                        "path": f"pickups_5_1/{source}.wav",
                        "channel": receiver_index,
                    }
                    for source in speakers
                    for receiver_index, receiver in enumerate(receivers)
                ],
            },
        },
    }
    geometry = {
        "schema_version": 1,
        "speed_of_sound_m_s": DEFAULT_SPEED_OF_SOUND,
        "distance_basis": "explicit_distance_quantized_from_3d_positions",
        "speaker_positions_m": {
            source: list(DEFAULT_SPEAKER_POSITIONS[source]) for source in speakers
        },
        "receiver_positions_m": {
            receiver: list(DEFAULT_RECEIVER_POSITIONS[receiver])
            for receiver in receivers
        },
        "pairs": [
            {
                "source": source,
                "receiver": receiver,
                "distance_m": distances[(source, receiver)]["distance_m"],
            }
            for source in speakers
            for receiver in receivers
        ],
    }
    config = {
        "schema_version": 1,
        "measurement_layout": "mono",
        "max_cancellation_gain_db": 12.0,
        "target_suppression_db": 12.0,
        "min_band_samples": 64,
        "estimator_version": "band-ls-v1",
        "bands": [
            {
                "id": band_id,
                "low_hz": low,
                "high_hz": high,
                "transition_hz": transition,
            }
            for band_id, low, high, transition in bands
        ],
        "phase": {
            "jump_threshold_deg": 120.0,
            "polarity_tolerance_deg": 35.0,
            "low_frequency_max_hz": 500.0,
            "group_delay_jump_samples": 2.5,
            "ambiguity_confidence": 0.6,
        },
        "calibration": {
            "gain_step_db": 0.05,
            "residual_floor": 1e-12,
        },
    }
    truth = {
        "schema_version": 1,
        "seed": seed,
        "duration_seconds": duration,
        "sample_rate": SAMPLE_RATE,
        "quantization_noise_lsb": [-3, 3],
        "speakers": list(speakers),
        "receivers": list(receivers),
        "delays": {
            f"{source}|{receiver}": distances[(source, receiver)]["delay_samples"]
            for source in speakers
            for receiver in receivers
        },
        "distances_m": {
            f"{source}|{receiver}": distances[(source, receiver)]["distance_m"]
            for source in speakers
            for receiver in receivers
        },
        "coupling": {
            f"{source}|{receiver}": coupling[(source, receiver)]
            for source in speakers
            for receiver in receivers
        },
    }
    write_json_atomic(output / "manifest.json", manifest)
    write_json_atomic(output / "geometry.json", geometry)
    write_json_atomic(output / "config.json", config)
    write_json_atomic(output / "truth.json", truth)
    return {
        "output": str(output),
        "seed": seed,
        "duration_seconds": duration,
        "manifest": str(output / "manifest.json"),
        "geometry": str(output / "geometry.json"),
        "config": str(output / "config.json"),
        "truth": str(output / "truth.json"),
        "layouts": ["mono", "5.1"],
        "pair_count": len(pair_components),
        "bands": [band[0] for band in bands],
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Generate deterministic PCM WAV samples")
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--duration", type=float, default=1.0)
    parser.add_argument("--force", action="store_true")
    return parser


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        result = generate_sample(
            args.out, seed=args.seed, duration=args.duration, force=args.force
        )
    except BusinessError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    sys.stdout.buffer.write(canonical_json_bytes(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
