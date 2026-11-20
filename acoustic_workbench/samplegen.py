from __future__ import annotations

import argparse
import math
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .geometry import SPEED_OF_SOUND_MPS, reflection_paths
from .hashing import canonical_json, sha256_file, stable_seed
from .model import (
    DEFAULT_BAND_CENTERS,
    DEFAULT_CHANNEL_LAYOUT,
    Point,
    Scene,
    Wall,
)
from .wavio import write_wav_atomic


MATERIALS = {
    "concrete": {
        "absorption": [0.02, 0.03, 0.04, 0.05, 0.06, 0.08],
        "description": "painted concrete",
    },
    "wood": {
        "absorption": [0.05, 0.07, 0.09, 0.11, 0.14, 0.18],
        "description": "wood panel",
    },
    "curtain": {
        "absorption": [0.15, 0.25, 0.35, 0.45, 0.55, 0.65],
        "description": "heavy stage curtain",
    },
    "acoustic_panel": {
        "absorption": [0.10, 0.20, 0.35, 0.55, 0.70, 0.80],
        "description": "acoustic absorber",
    },
    "carpet": {
        "absorption": [0.08, 0.12, 0.20, 0.30, 0.40, 0.50],
        "description": "carpeted wall",
    },
}


VENUES: dict[str, dict[str, Any]] = {
    "hall": {
        "width": 24.0,
        "height": 15.0,
        "walls": {
            "north": "concrete",
            "east": "wood",
            "south": "curtain",
            "west": "acoustic_panel",
        },
        "source": (4.2, 6.4, 1.7),
        "listeners": [("L1", 12.0, 8.0, 1.2), ("L2", 18.5, 5.2, 1.2)],
    },
    "club": {
        "width": 13.0,
        "height": 10.0,
        "walls": {
            "north": "carpet",
            "east": "curtain",
            "south": "wood",
            "west": "concrete",
        },
        "source": (2.0, 4.0, 1.4),
        "listeners": [("L1", 7.5, 5.2, 1.2), ("L2", 10.5, 7.0, 1.2)],
    },
    "arena": {
        "width": 38.0,
        "height": 25.0,
        "walls": {
            "north": "acoustic_panel",
            "east": "concrete",
            "south": "acoustic_panel",
            "west": "wood",
        },
        "source": (5.0, 12.0, 2.0),
        "listeners": [("L1", 18.0, 12.5, 1.2), ("L2", 30.0, 8.0, 1.2)],
    },
}


def _scene_for(name: str, spec: dict[str, Any]) -> Scene:
    width = float(spec["width"])
    height = float(spec["height"])
    wall_materials = dict(spec["walls"])
    walls = [
        Wall("north", 0.0, 0.0, width, 0.0, wall_materials["north"]),
        Wall("east", width, 0.0, width, height, wall_materials["east"]),
        Wall("south", width, height, 0.0, height, wall_materials["south"]),
        Wall("west", 0.0, height, 0.0, 0.0, wall_materials["west"]),
    ]
    listeners = [
        Point(listener_id, x, y, z)
        for listener_id, x, y, z in spec["listeners"]
    ]
    source = Point("source", *spec["source"])
    scene = Scene(
        scene_id=name,
        sample_rate=48000,
        channel_layout=list(DEFAULT_CHANNEL_LAYOUT),
        reference_channel="FC",
        source=source,
        listeners=listeners,
        walls=walls,
        ir_files={
            point.id: f"ir_{point.id}.wav" for point in listeners
        },
        test_signal="test_signal.wav",
        alignment={"threshold": 0.015, "max_shift_samples": 4096},
        early={
            "start_ms": 1.0,
            "end_ms": 72.0,
            "threshold_ratio": 0.025,
            "min_gap_ms": 1.2,
            "fft_size": 1024,
        },
        bands={
            "mode": "octave",
            "fraction": 1,
            "center_frequencies": list(DEFAULT_BAND_CENTERS),
        },
        late={
            "tail_ms": 720.0,
            "seed": 20261120,
            "min_t60_s": 0.10,
            "max_t60_s": 6.0,
        },
        render={"max_seconds": 2.4, "normalize_peak": 0.86},
        materials_file="materials.json",
    )
    scene.validate()
    return scene


def _band_noise(
    length: int,
    sample_rate: int,
    center_hz: float,
    seed: int,
) -> np.ndarray:
    rng = np.random.Generator(np.random.PCG64(seed))
    noise = rng.standard_normal(length)
    spectrum = np.fft.rfft(noise)
    frequencies = np.fft.rfftfreq(length, d=1.0 / sample_rate)
    low = center_hz / math.sqrt(2.0)
    high = min(sample_rate / 2.0 - 1.0, center_hz * math.sqrt(2.0))
    mask = (frequencies >= low) & (frequencies < high)
    spectrum[~mask] = 0.0
    output = np.fft.irfft(spectrum, n=length)
    rms = float(np.sqrt(np.mean(output**2)))
    if rms <= 1e-15:
        return np.zeros(length, dtype=np.float64)
    return output / rms


def _add_burst(
    target: np.ndarray,
    channel_index: int,
    start_sample: int,
    duration_samples: int,
    center_hz: float,
    amplitude: float,
    sample_rate: int,
    phase: float,
) -> None:
    if start_sample >= target.shape[1] or amplitude <= 0.0:
        return
    count = min(duration_samples, target.shape[1] - start_sample)
    if count <= 0:
        return
    index = np.arange(count, dtype=np.float64)
    attack = min(count, max(1, int(0.0015 * sample_rate)))
    envelope = np.exp(-index / max(1.0, 0.0045 * sample_rate))
    if attack > 1:
        envelope[:attack] *= np.linspace(0.0, 1.0, attack)
    carrier = np.sin(
        2.0 * math.pi * center_hz * index / sample_rate + phase
    )
    target[channel_index, start_sample : start_sample + count] += (
        amplitude * envelope * carrier
    )


def synthesize_impulse_response(
    scene: Scene,
    listener_id: str,
    materials: dict[str, Any],
    *,
    duration_s: float = 1.35,
) -> np.ndarray:
    sample_rate = scene.sample_rate
    frames = int(round(duration_s * sample_rate))
    response = np.zeros((6, frames), dtype=np.float64)
    centers = [float(value) for value in scene.bands["center_frequencies"]]
    listener = scene.listener(listener_id)
    direct_distance = math.hypot(
        scene.source.x - listener.x,
        scene.source.y - listener.y,
    )
    channel_gain = [1.00, 0.96, 0.91, 0.62, 0.84, 0.81]
    channel_offset = [0, 1, 2, 3, 4, 5]
    phase_table = [[0.13, 0.29, 0.47, 0.61, 0.73, 0.89] for _ in range(6)]
    retreat = int(round(0.00045 * sample_rate))
    direct_peak = 0.20 / (1.0 + direct_distance / 18.0)

    for channel_index in range(6):
        direct_index = int(
            round(direct_distance / SPEED_OF_SOUND_MPS * sample_rate)
        ) + channel_offset[channel_index]
        if direct_index >= frames:
            continue
        response[channel_index, direct_index] += (
            direct_peak * channel_gain[channel_index]
        )
        for band_index, center in enumerate(centers):
            band_gain = 1.0 / math.sqrt(1.0 + center / 900.0)
            _add_burst(
                response,
                channel_index,
                direct_index + band_index * retreat,
                int(0.030 * sample_rate),
                center,
                direct_peak * channel_gain[channel_index] * band_gain * 0.42,
                sample_rate,
                phase_table[channel_index][band_index],
            )

    paths = reflection_paths(scene, listener_id)
    for path_index, path in enumerate(paths):
        wall = scene.wall(str(path["wall_id"]))
        material = materials[wall.material]
        alpha = [float(value) for value in material["absorption"]]
        alpha += [alpha[-1]] * (len(centers) - len(alpha))
        arrival = int(
            round(float(path["path_length_m"]) / SPEED_OF_SOUND_MPS * sample_rate)
        )
        altitude_gain = 0.76 / (1.0 + float(path["path_length_m"]) / 16.0)
        for channel_index in range(6):
            channel_time = arrival + channel_offset[channel_index]
            channel_factor = channel_gain[(channel_index + path_index) % 6]
            for band_index, center in enumerate(centers):
                absorption_gain = max(0.03, 1.0 - alpha[band_index])
                amplitude = (
                    altitude_gain
                    * channel_factor
                    * absorption_gain
                    * (0.58 + 0.42 / (1.0 + band_index))
                )
                _add_burst(
                    response,
                    channel_index,
                    channel_time + band_index * retreat,
                    int(0.055 * sample_rate),
                    center,
                    amplitude,
                    sample_rate,
                    phase_table[channel_index][band_index]
                    + 0.31 * (path_index + 1),
                )

    late_start = int(round(scene.early["end_ms"] * sample_rate / 1000.0))
    tail_length = min(
        frames - late_start,
        int(round(scene.late["tail_ms"] * sample_rate / 1000.0)),
    )
    if tail_length > 0:
        average_alpha = []
        for band_index in range(len(centers)):
            values = []
            for wall in scene.walls:
                alpha = materials[wall.material]["absorption"]
                values.append(float(alpha[min(band_index, len(alpha) - 1)]))
            average_alpha.append(float(np.mean(values)))
        time = np.arange(tail_length, dtype=np.float64) / sample_rate
        for channel_index in range(6):
            for band_index, center in enumerate(centers):
                t60 = min(
                    2.6,
                    max(0.18, 0.54 + 2.8 * (1.0 - average_alpha[band_index])),
                )
                noise = _band_noise(
                    tail_length,
                    sample_rate,
                    center,
                    stable_seed(
                        "sample-late",
                        scene.scene_id,
                        listener_id,
                        channel_index,
                        band_index,
                    ),
                )
                level = 0.008 * channel_gain[channel_index] / math.sqrt(
                    1.0 + band_index
                )
                response[
                    channel_index,
                    late_start : late_start + tail_length,
                ] += (
                    level
                    * noise
                    * np.power(10.0, -3.0 * time / t60)
                )

    peak = float(np.max(np.abs(response)))
    if peak > 0.94:
        response *= 0.94 / peak
    return np.ascontiguousarray(response)


def make_test_signal(
    sample_rate: int = 48000,
    duration_s: float = 1.6,
) -> np.ndarray:
    frames = int(round(sample_rate * duration_s))
    time = np.arange(frames, dtype=np.float64) / sample_rate
    signal = np.zeros(frames, dtype=np.float64)
    components = [
        (220.0, 0.40, 1.4),
        (440.0, 0.28, 1.1),
        (880.0, 0.18, 0.85),
        (1320.0, 0.10, 0.65),
        (1760.0, 0.06, 0.50),
    ]
    for frequency, amplitude, decay in components:
        phase = (frequency * 0.37) % (2.0 * math.pi)
        signal += (
            amplitude
            * np.sin(2.0 * math.pi * frequency * time + phase)
            * np.exp(-time / decay)
        )
    attack = max(1, int(0.01 * sample_rate))
    signal[:attack] *= np.linspace(0.0, 1.0, attack)
    signal[-attack:] *= np.linspace(1.0, 0.0, attack)
    peak = float(np.max(np.abs(signal)))
    signal *= 0.72 / peak
    return signal[np.newaxis, :]


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    path.write_text(canonical_json(payload), encoding="utf-8")


def generate_venue(
    output_root: Path,
    name: str,
    spec: dict[str, Any],
    *,
    build_baseline: bool = True,
) -> dict[str, Any]:
    venue_dir = output_root / f"venue_{name}"
    venue_dir.mkdir(parents=True, exist_ok=True)
    scene = _scene_for(name, spec)
    _write_json(venue_dir / "materials.json", MATERIALS)
    _write_json(venue_dir / "scene.json", scene.to_dict())
    ir_hashes: dict[str, str] = {}
    for listener in scene.listeners:
        response = synthesize_impulse_response(scene, listener.id, MATERIALS)
        ir_path = venue_dir / scene.ir_files[listener.id]
        write_wav_atomic(ir_path, response, sample_rate=scene.sample_rate)
        ir_hashes[listener.id] = sha256_file(ir_path)
    signal_path = venue_dir / scene.test_signal
    write_wav_atomic(
        signal_path,
        make_test_signal(scene.sample_rate),
        sample_rate=scene.sample_rate,
    )
    layout = {
        "format": "PCM16 WAV",
        "sample_rate": scene.sample_rate,
        "channels": 6,
        "channel_layout": scene.channel_layout,
        "reference_channel": scene.reference_channel,
        "file_channel_order": scene.channel_layout,
    }
    _write_json(venue_dir / "ir_layout.json", layout)

    baseline_info: dict[str, Any] = {}
    if build_baseline:
        from .service import AcousticEngine

        baseline_dir = venue_dir / "baseline"
        if baseline_dir.exists():
            shutil.rmtree(baseline_dir)
        (baseline_dir / "fields").mkdir(parents=True, exist_ok=True)
        (baseline_dir / "renders").mkdir(parents=True, exist_ok=True)
        engine = AcousticEngine(venue_dir, scene=scene)
        report = engine.rebuild(scene, mode="baseline")
        field_index: dict[str, Any] = {}
        for point_report in report.points:
            field_source = Path(point_report.coefficient_path)
            field_destination = (
                baseline_dir / "fields" / f"{point_report.point_id}_coefficients.npy"
            )
            shutil.copyfile(field_source, field_destination)
            manifest = dict(point_report.field_manifest)
            manifest["coefficients_file"] = field_destination.name
            _write_json(
                baseline_dir / "fields" / f"{point_report.point_id}_field.json",
                manifest,
            )
            render = engine.render_point(scene, point_report.point_id)
            render_destination = (
                baseline_dir / "renders" / f"{point_report.point_id}_audition.wav"
            )
            shutil.copyfile(render["render_path"], render_destination)
            field_index[point_report.point_id] = {
                "field_version": point_report.field_version,
                "coefficient_hash": point_report.coefficient_hash,
                "coefficients": field_destination.name,
                "field_manifest": f"{point_report.point_id}_field.json",
                "render": render_destination.name,
            }
        _write_json(baseline_dir / "fields" / "index.json", field_index)
        baseline_info = field_index

    manifest = {
        "scene_id": scene.scene_id,
        "venue_dir": venue_dir.name,
        "sample_rate": scene.sample_rate,
        "channel_layout": scene.channel_layout,
        "ir_sha256": ir_hashes,
        "test_signal_sha256": sha256_file(signal_path),
        "baseline": baseline_info,
    }
    _write_json(venue_dir / "manifest.json", manifest)
    return manifest


def generate_all(
    output_root: str | Path = "artifacts/samples",
    *,
    venues: list[str] | None = None,
    build_baseline: bool = True,
) -> dict[str, Any]:
    output_root = Path(output_root)
    output_root.mkdir(parents=True, exist_ok=True)
    selected = venues or list(VENUES)
    unknown = [name for name in selected if name not in VENUES]
    if unknown:
        raise ValueError(f"unknown venue(s): {', '.join(unknown)}")
    manifests = {
        name: generate_venue(
            output_root,
            name,
            VENUES[name],
            build_baseline=build_baseline,
        )
        for name in selected
    }
    root_manifest = {
        "generator": "acoustic_workbench.samplegen/v1",
        "venues": manifests,
    }
    _write_json(output_root / "manifest.json", root_manifest)
    return root_manifest


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Generate deterministic 5.1 impulse-response venues."
    )
    parser.add_argument(
        "--output",
        default="artifacts/samples",
        help="sample output root (default: artifacts/samples)",
    )
    parser.add_argument(
        "--venues",
        nargs="+",
        choices=sorted(VENUES),
        help="venue names to generate (default: all)",
    )
    parser.add_argument(
        "--no-baseline",
        action="store_true",
        help="skip baseline field and audition generation",
    )
    args = parser.parse_args(argv)
    manifest = generate_all(
        args.output,
        venues=args.venues,
        build_baseline=not args.no_baseline,
    )
    print(canonical_json(manifest))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

