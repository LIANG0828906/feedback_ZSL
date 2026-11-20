"""Deterministic WAV sample and baseline dataset generation."""

from __future__ import annotations

import json
import math
import os
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .compare import compare_versions, write_compare_outputs
from .recipes import Recipe, recipe_from_mapping
from .utils import canonical_json, sha256_bytes, sha256_json
from .wav_io import atomic_write_bytes, encode_wav

GENERATOR_VERSION = "1.0.0"


@dataclass(frozen=True)
class SampleSpec:
    sample_id: str
    sample_rate: int
    channels: int
    seed: int
    duration_s: float


def build_recipes() -> dict[int, Recipe]:
    common = {"denoise_strength": 0.55, "gain_db": 2.0}
    return {
        1: recipe_from_mapping({"version": 1, **common}, source="<generated-v1>"),
        2: recipe_from_mapping(
            {
                "version": 2,
                **common,
                "remove_dc": True,
                "silence_threshold_dbfs": -54.0,
            },
            source="<generated-v2>",
        ),
        3: recipe_from_mapping(
            {
                "version": 3,
                **common,
                "remove_dc": True,
                "silence_threshold_dbfs": -54.0,
                "channel_merge_mode": "preserve",
                "spectral_subtraction": {
                    "segment_ms": 32,
                    "oversubtraction": 1.8,
                    "spectral_floor": 0.015,
                },
                "loudness_target_lufs": -20.0,
            },
            source="<generated-v3>",
        ),
    }


def _speech_envelope(t: np.ndarray) -> np.ndarray:
    # Stable syllable pattern with real silent intervals between phrases.
    phrase = (t % 1.35) < 0.92
    syllable_rate = 4.1
    modulation = np.clip(0.15 + 1.05 * (0.5 + 0.5 * np.sin(2 * math.pi * syllable_rate * t)), 0.0, 1.2)
    phrase_fade = np.minimum(1.0, np.minimum(t % 1.35, np.maximum(0.92 - (t % 1.35), 0.0)) / 0.025)
    return phrase.astype(np.float64) * modulation * np.clip(phrase_fade, 0.0, 1.0)


def synthesize(spec: SampleSpec) -> np.ndarray:
    rng = np.random.default_rng(spec.seed)
    frames = int(round(spec.sample_rate * spec.duration_s))
    t = np.arange(frames, dtype=np.float64) / spec.sample_rate
    envelope = _speech_envelope(t)
    f0 = 118.0 + 7.0 * np.sin(2 * math.pi * 0.31 * t)
    phase = 2 * math.pi * np.cumsum(f0) / spec.sample_rate

    base = (
        0.26 * np.sin(phase)
        + 0.12 * np.sin(2.01 * phase + 0.2)
        + 0.07 * np.sin(3.03 * phase + 1.1)
        + 0.035 * np.sin(5.07 * phase + 0.4)
    )
    base *= envelope
    noise = rng.normal(0.0, 0.008, size=frames)
    channels = []
    for channel in range(spec.channels):
        dc = 0.032 if channel == 0 else -0.021
        channel_shift = 0.0 if channel == 0 else 0.18
        voice = base * (1.0 if channel == 0 else 0.86)
        voice += 0.035 * np.sin(1.5 * phase + channel_shift) * envelope
        local_noise = noise if channel == 0 else rng.normal(0.0, 0.0095, size=frames)
        channels.append(dc + voice + local_noise)
    mixed = np.stack(channels, axis=1)
    return np.clip(mixed, -0.94, 0.94)


def _recipe_document(recipe: Recipe) -> dict[str, Any]:
    return recipe.semantic_dict()


def _normalized_rel(path: Path, root: Path) -> str:
    return path.relative_to(root).as_posix()


def generate_dataset(
    out_dir: str | os.PathLike[str],
    *,
    seed: int = 190_093,
    sample_rates: Sequence[int] = (8000, 16000, 44100, 48000),
    channel_layouts: Sequence[int] = (1, 2),
    duration_s: float = 1.8,
    segment_seconds: float = 1.0,
    clean: bool = True,
) -> dict[str, Any]:
    """Generate WAV/recipe grid, three-version runs and deterministic traces."""

    root = Path(out_dir)
    if clean and root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)
    (root / "samples").mkdir(parents=True, exist_ok=True)
    (root / "recipes").mkdir(parents=True, exist_ok=True)
    (root / "runs").mkdir(parents=True, exist_ok=True)
    recipes = build_recipes()
    recipe_hashes = {version: recipe.hash() for version, recipe in recipes.items()}
    for version, recipe in recipes.items():
        payload = (
            json.dumps(
                _recipe_document(recipe),
                ensure_ascii=False,
                sort_keys=True,
                indent=2,
            ).encode("utf-8")
            + b"\n"
        )
        atomic_write_bytes(root / "recipes" / f"v{version}.json", payload)

    specs: list[SampleSpec] = []
    for sample_rate in sample_rates:
        for channels in channel_layouts:
            sample_id = f"sr{sample_rate}_ch{channels}"
            specs.append(
                SampleSpec(
                    sample_id=sample_id,
                    sample_rate=int(sample_rate),
                    channels=int(channels),
                    seed=seed + sample_rate * 10 + channels,
                    duration_s=float(duration_s),
                )
            )

    trace_lines: list[bytes] = []
    trace_header = {
        "trace_version": 1,
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "duration_s": duration_s,
        "segment_seconds": segment_seconds,
        "sample_rates": list(sample_rates),
        "channel_layouts": list(channel_layouts),
        "recipe_hashes": {f"v{key}": value for key, value in recipe_hashes.items()},
    }
    trace_lines.append((canonical_json(trace_header) + "\n").encode("utf-8"))

    baseline_rows: list[dict[str, Any]] = []
    source_refs: dict[str, Any] = {}
    compare_summaries: dict[str, Any] = {}
    for spec in specs:
        samples = synthesize(spec)
        wav_bytes = encode_wav(samples, spec.sample_rate)
        sample_path = root / "samples" / f"{spec.sample_id}.wav"
        atomic_write_bytes(sample_path, wav_bytes)
        source_hash = sha256_bytes(wav_bytes)
        run_root = root / "runs" / spec.sample_id
        comparison = compare_versions(
            sample_path,
            [recipes[1], recipes[2], recipes[3]],
            run_root,
            segment_seconds=segment_seconds,
        )
        write_compare_outputs(
            comparison,
            run_root / "comparison.json",
            run_root / "comparison_trace.jsonl",
        )
        compare_summaries[spec.sample_id] = {
            "comparison_hash": comparison["hash"],
            "summary": comparison["summary"],
            "report": _normalized_rel(run_root / "comparison.json", root),
            "trace": _normalized_rel(run_root / "comparison_trace.jsonl", root),
        }
        source_refs[spec.sample_id] = {
            "path": _normalized_rel(sample_path, root),
            "sha256": source_hash,
            "sample_rate": spec.sample_rate,
            "channels": spec.channels,
            "frames": int(samples.shape[0]),
        }
        for version in (1, 2, 3):
            report_path = run_root / f"v{version}" / "report.json"
            report = json.loads(report_path.read_text("utf-8"))
            for entry in report["segments"]:
                trace_row = {
                    "sample_id": spec.sample_id,
                    "sample_rate": spec.sample_rate,
                    "input_channels": spec.channels,
                    "source_sha256": source_hash,
                    "segment": entry["index"],
                    "recipe_version": version,
                    "field_mapping": recipes[version].field_mapping,
                    "metrics": entry["metrics"],
                    "content_sha256": entry["content_sha256"],
                    "chain_hash": entry["chain_hash"],
                    "output_sha256": report["output"]["sha256"],
                }
                trace_lines.append((canonical_json(trace_row) + "\n").encode("utf-8"))
            baseline_rows.append(
                {
                    "sample_id": spec.sample_id,
                    "recipe_version": version,
                    "recipe_hash": recipe_hashes[version],
                    "source_sha256": source_hash,
                    "output_sha256": report["output"]["sha256"],
                    "aggregate_metrics": report["aggregate_metrics"],
                    "segment_count": len(report["segments"]),
                    "segments": [
                        {
                            "segment": entry["index"],
                            "metrics": entry["metrics"],
                            "content_sha256": entry["content_sha256"],
                        }
                        for entry in report["segments"]
                    ],
                }
            )

    baseline = {
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "duration_s": duration_s,
        "segment_seconds": segment_seconds,
        "recipes": {
            f"v{version}": {
                "hash": recipe_hashes[version],
                "document": _recipe_document(recipe),
            }
            for version, recipe in recipes.items()
        },
        "sources": source_refs,
        "baselines": baseline_rows,
        "comparisons": compare_summaries,
        "trace_sha256": "",
        "hash": "",
    }
    trace_bytes = b"".join(trace_lines)
    baseline["trace_sha256"] = sha256_bytes(trace_bytes)
    baseline["hash"] = sha256_json(
        {key: value for key, value in baseline.items() if key != "hash"}
    )
    baseline_bytes = (
        json.dumps(
            baseline,
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
        ).encode("utf-8")
        + b"\n"
    )
    atomic_write_bytes(root / "trace.jsonl", trace_bytes)
    atomic_write_bytes(root / "baseline.json", baseline_bytes)
    return {
        "out_dir": str(root),
        "sample_count": len(specs),
        "run_count": len(specs) * 3,
        "segment_trace_rows": len(trace_lines) - 1,
        "baseline_hash": baseline["hash"],
        "trace_sha256": baseline["trace_sha256"],
    }
