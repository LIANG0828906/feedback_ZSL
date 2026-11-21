"""Deterministic local venue, IR and baseline sample generation."""

from __future__ import annotations

import math
import shutil
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Sequence, Tuple

import numpy as np

from .codes import E_CONFIG_INVALID, AcousticError
from .engine import Workbench
from .scene import CHANNEL_ORDER
from .util import atomic_write_json, hash_obj, sha256_file, stable_seed
from .wavio import read_wav_16, write_wav_16_atomic


MATERIAL_TABLE: Dict[str, Dict[str, Any]] = {
    "painted_plaster": {
        "absorption": [0.04, 0.05, 0.06, 0.07, 0.08, 0.09, 0.10, 0.11],
        "description": "hard painted plaster",
    },
    "oak_floor": {
        "absorption": [0.15, 0.12, 0.10, 0.08, 0.07, 0.06, 0.06, 0.07],
        "description": "sealed oak boards",
    },
    "acoustic_tile": {
        "absorption": [0.18, 0.22, 0.35, 0.55, 0.70, 0.75, 0.78, 0.80],
        "description": "porous acoustic ceiling tile",
    },
    "heavy_curtain": {
        "absorption": [0.08, 0.12, 0.24, 0.42, 0.60, 0.68, 0.72, 0.70],
        "description": "pleated heavy curtain",
    },
}

VENUES: Dict[str, Dict[str, Any]] = {
    "small": {
        "scene_id": "small-studio",
        "bounds": [[0.0, 0.0, 0.0], [9.0, 5.0, 4.0]],
        "source": [1.6, 1.65, 2.0],
        "listeners": [
            {"id": "L1", "position": [5.2, 1.25, 1.8], "reference_channel": "FC"},
            {"id": "L2", "position": [7.1, 1.75, 1.1], "reference_channel": "FC"},
        ],
        "surfaces": {
            "wall": "painted_plaster",
            "floor": "oak_floor",
            "ceiling": "acoustic_tile",
        },
        "tail_scale": 0.85,
    },
    "medium": {
        "scene_id": "medium-hall",
        "bounds": [[0.0, 0.0, 0.0], [16.0, 8.0, 7.0]],
        "source": [2.3, 1.7, 3.4],
        "listeners": [
            {"id": "L1", "position": [9.4, 1.35, 3.2], "reference_channel": "FC"},
            {"id": "L2", "position": [13.2, 2.2, 2.1], "reference_channel": "FC"},
        ],
        "surfaces": {
            "wall": "heavy_curtain",
            "floor": "oak_floor",
            "ceiling": "acoustic_tile",
        },
        "tail_scale": 1.0,
    },
    "large": {
        "scene_id": "large-arena",
        "bounds": [[0.0, 0.0, 0.0], [28.0, 13.0, 11.0]],
        "source": [3.0, 2.2, 5.5],
        "listeners": [
            {"id": "L1", "position": [17.0, 1.5, 5.0], "reference_channel": "FC"},
            {"id": "L2", "position": [24.0, 3.0, 4.0], "reference_channel": "FC"},
        ],
        "surfaces": {
            "wall": "painted_plaster",
            "floor": "oak_floor",
            "ceiling": "acoustic_tile",
        },
        "tail_scale": 1.2,
    },
}


def _make_scene(venue_name: str, seed: int) -> Dict[str, Any]:
    venue = VENUES[venue_name]
    listeners = []
    for listener in venue["listeners"]:
        listeners.append(
            {
                "id": listener["id"],
                "position": list(listener["position"]),
                "reference_channel": listener["reference_channel"],
                "ir": f"irs/{listener['id']}.wav",
            }
        )
    return {
        "schema_version": 1,
        "scene_id": venue["scene_id"],
        "venue": {
            "id": venue_name,
            "bounds": {
                "min": list(venue["bounds"][0]),
                "max": list(venue["bounds"][1]),
            },
            "surfaces": dict(venue["surfaces"]),
            "material_table": MATERIAL_TABLE,
        },
        "source": {"id": "stage-source", "position": list(venue["source"])},
        "listeners": listeners,
        "channel_layout": list(CHANNEL_ORDER),
        "sample_rate": 48000,
        "alignment": {
            "threshold": 0.05,
            "normalize_gain": True,
        },
        "window_params": {
            "direct_window_ms": 5.0,
            "early_window_ms": 80.0,
            "tail_ms": 800.0,
            "event_threshold": 0.03,
            "min_event_spacing_ms": 1.0,
        },
        "frequency_config": {
            "bands_hz": [63.0, 125.0, 250.0, 500.0, 1000.0, 2000.0, 4000.0, 8000.0],
            "fft_size": 2048,
        },
        "decay_config": {
            "decay_time_override": None,
            "min_decay_time_sec": 0.05,
        },
        "reverb": {
            "seed": seed,
        },
        "render": {
            "duration_sec": 2.0,
            "signal_seed": seed + 17,
            "target_dbfs": -3.0,
        },
        "sample_generator": {
            "name": "acoustic-workbench-local",
            "version": 1,
            "seed": seed,
            "venue_name": venue_name,
        },
    }


def _ir_for_listener(
    venue_name: str,
    listener: Mapping[str, Any],
    source_position: Sequence[float],
    seed: int,
) -> np.ndarray:
    venue = VENUES[venue_name]
    sample_rate = 48000
    frame_count = int(round(1.6 * sample_rate))
    samples = np.zeros((frame_count, len(CHANNEL_ORDER)), dtype=np.float64)
    source = np.asarray(source_position, dtype=np.float64)
    listener_position = np.asarray(listener["position"], dtype=np.float64)
    distance = float(np.linalg.norm(listener_position - source))
    reference_arrival = int(round(480 + distance * sample_rate / 343.0))
    offsets = [37, 19, 0, 11, 29, 41]
    base_amplitudes = [0.82, 0.78, 1.0, 0.64, 0.56, 0.59]
    rng = np.random.Generator(
        np.random.PCG64(stable_seed(f"ir:{venue_name}:{listener['id']}:{seed}"))
    )
    for channel_index, channel in enumerate(CHANNEL_ORDER):
        arrival = reference_arrival + offsets[channel_index]
        if arrival >= frame_count:
            arrival = frame_count - 2
        amplitude = base_amplitudes[channel_index]
        samples[arrival, channel_index] += amplitude
        if arrival + 1 < frame_count:
            samples[arrival + 1, channel_index] += amplitude * 0.18
        if arrival + 2 < frame_count:
            samples[arrival + 2, channel_index] += amplitude * 0.06
        jitter = (channel_index - 2) * 0.35
        for event_number, delay_ms in enumerate((8.0, 15.0, 26.0, 41.0, 59.0, 72.0)):
            delay = int(round((delay_ms + jitter) * sample_rate / 1000.0))
            event_index = arrival + delay
            if event_index >= frame_count:
                continue
            sign = -1.0 if (event_number + channel_index) % 2 else 1.0
            amplitude_event = (
                sign
                * amplitude
                * (0.20 / (1.0 + event_number * 0.42))
                * (0.8 + 0.05 * channel_index)
            )
            samples[event_index, channel_index] += amplitude_event

        tail_start = min(frame_count, arrival + int(0.08 * sample_rate))
        tail_length = frame_count - tail_start
        if tail_length > 0:
            tail_time = np.arange(tail_length, dtype=np.float64) / sample_rate
            decay = 0.55
            noise = rng.standard_normal(tail_length)
            smooth = np.convolve(noise, np.ones(17) / 17.0, mode="same")
            envelope = np.exp(-6.907755278982137 * tail_time / decay)
            samples[tail_start:, channel_index] += (
                0.036
                * venue["tail_scale"]
                * (0.78 + 0.04 * channel_index)
                * smooth
                * envelope
            )
    peak = float(np.max(np.abs(samples)))
    if peak > 0:
        samples *= 0.84 / peak
    return samples


def generate_samples(
    output: str | Path,
    *,
    seed: int = 20260101,
    venue_names: Sequence[str] = ("small", "medium", "large"),
    build_baselines: bool = True,
) -> Dict[str, Any]:
    output_path = Path(output)
    output_path.mkdir(parents=True, exist_ok=True)
    manifest: Dict[str, Any] = {
        "schema_version": 1,
        "generator": "acoustic_workbench.samplegen",
        "seed": seed,
        "venues": {},
    }
    for venue_name in venue_names:
        if venue_name not in VENUES:
            raise AcousticError(
                E_CONFIG_INVALID,
                f"Unknown venue sample: {venue_name}",
                injection_point="samplegen:venue_names",
            )
        venue_dir = output_path / "scenes" / venue_name
        ir_dir = venue_dir / "irs"
        ir_dir.mkdir(parents=True, exist_ok=True)
        scene = _make_scene(venue_name, seed)
        scene_path = venue_dir / "scene.json"
        atomic_write_json(scene_path, scene)
        ir_hashes: Dict[str, Dict[str, str]] = {}
        for listener in scene["listeners"]:
            samples = _ir_for_listener(
                venue_name,
                listener,
                scene["source"]["position"],
                seed,
            )
            wav_path = venue_dir / listener["ir"]
            write_wav_16_atomic(wav_path, samples, 48000, 0x3F)
            ir_hashes[listener["id"]] = {
                "path": str(Path("scenes") / venue_name / listener["ir"]),
                "sha256": sha256_file(wav_path),
            }
        venue_record = {
            "scene": str(Path("scenes") / venue_name / "scene.json"),
            "scene_sha256": sha256_file(scene_path),
            "irs": {
                listener_id: record
                for listener_id, record in sorted(ir_hashes.items())
            },
        }
        if build_baselines:
            workbench = Workbench(venue_dir, scene_path="scene.json")
            baseline_records: Dict[str, Any] = {}
            baseline_dir = venue_dir / "baselines"
            baseline_dir.mkdir(parents=True, exist_ok=True)
            for listener in scene["listeners"]:
                result = workbench.rebuild(listener["id"])
                baseline_path = baseline_dir / f"{listener['id']}.json"
                atomic_write_json(baseline_path, result["field"])
                baseline_records[listener["id"]] = {
                    "path": str(
                        Path("scenes")
                        / venue_name
                        / "baselines"
                        / f"{listener['id']}.json"
                    ),
                    "field_version": result["field_version"],
                    "sha256": sha256_file(baseline_path),
                }
            baseline_index = {
                "schema_version": 1,
                "scene_id": scene["scene_id"],
                "fields": baseline_records,
            }
            atomic_write_json(baseline_dir / "index.json", baseline_index)
            venue_record["baselines"] = baseline_records
        manifest["venues"][venue_name] = venue_record
    atomic_write_json(output_path / "manifest.json", manifest)
    return manifest


def mutate_scene(
    scene_path: str | Path,
    mutation: Mapping[str, Any],
) -> Dict[str, Any]:
    from .scene import load_scene
    from .util import canonical_json_text

    scene = load_scene(Path(scene_path))
    for path, value in mutation.items():
        current: Any = scene
        parts = path.split(".")
        for part in parts[:-1]:
            key: Any = int(part) if isinstance(current, list) else part
            current = current[key]
        final_key: Any = int(parts[-1]) if isinstance(current, list) else parts[-1]
        current[final_key] = value
    atomic_write_json(scene_path, scene)
    return scene

