from __future__ import annotations

import math
import random
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

from .canonical import sha256_bytes, write_json_atomic
from .color import (
    WHITE_POINTS,
    default_pipeline_config,
    generate_illuminant_document,
    generate_observer_document,
)
from .library import MaterialLibrary
from .materials import MaterialDefinition, parse_material_document
from .models import PipelineConfig, thaw_value


@dataclass(frozen=True)
class GeneratedSamples:
    out_dir: Path
    seed: int
    library_id: str
    pipeline: PipelineConfig
    definitions: tuple[MaterialDefinition, ...]
    baseline_snapshot_hash: str


SPECTRAL_SPECS = (
    {
        "id": "spectral-oxide-red",
        "name": "Oxide Red",
        "start": 380.0,
        "end": 780.0,
        "step": 5.0,
        "base": 0.16,
        "peaks": ((710.0, 0.17, 95.0), (430.0, 0.10, 55.0)),
        "phase": 0.3,
    },
    {
        "id": "spectral-teal-glaze",
        "name": "Teal Glaze",
        "start": 380.0,
        "end": 780.0,
        "step": 10.0,
        "base": 0.22,
        "peaks": ((485.0, 0.43, 78.0), (560.0, 0.10, 95.0)),
        "phase": 1.1,
    },
    {
        "id": "spectral-amber-resin",
        "name": "Amber Resin",
        "start": 380.0,
        "end": 780.0,
        "step": 4.0,
        "base": 0.36,
        "peaks": ((610.0, 0.42, 120.0), (455.0, 0.08, 60.0)),
        "phase": 2.4,
    },
    {
        "id": "spectral-brushed-steel",
        "name": "Brushed Steel",
        "start": 380.0,
        "end": 780.0,
        "step": 2.0,
        "base": 0.48,
        "peaks": ((510.0, 0.10, 170.0), (650.0, 0.06, 150.0)),
        "phase": 0.8,
    },
    {
        "id": "spectral-ink-blue",
        "name": "Ink Blue",
        "start": 400.0,
        "end": 700.0,
        "step": 5.0,
        "base": 0.12,
        "peaks": ((455.0, 0.36, 55.0), (600.0, 0.08, 110.0)),
        "phase": 1.7,
    },
    {
        "id": "spectral-sandstone",
        "name": "Sandstone",
        "start": 380.0,
        "end": 780.0,
        "step": 20.0,
        "base": 0.54,
        "peaks": ((610.0, 0.24, 180.0), (445.0, 0.08, 70.0)),
        "phase": 2.9,
    },
)


def _spectrum_values(spec: Mapping[str, Any], seed: int) -> list[float]:
    start = float(spec["start"])
    end = float(spec["end"])
    step = float(spec["step"])
    count = int(round((end - start) / step)) + 1
    rng = random.Random(f"{seed}:{spec['id']}")
    phases = [rng.random() * math.tau for _ in range(3)]
    values: list[float] = []
    for index in range(count):
        wavelength = start + index * step
        value = float(spec["base"])
        for center, amplitude, width in spec["peaks"]:
            value += float(amplitude) * math.exp(
                -0.5 * ((wavelength - float(center)) / float(width)) ** 2
            )
        value += 0.018 * math.sin(wavelength / 43.0 + float(spec["phase"]))
        value += 0.009 * math.sin(wavelength / 17.0 + phases[0])
        value += 0.004 * math.cos(wavelength / 9.0 + phases[1])
        values.append(round(min(0.98, max(0.015, value)), 8))
    return values


def sample_document(seed: int) -> dict[str, Any]:
    pipeline = default_pipeline_config(seed=seed)
    materials: list[dict[str, Any]] = []
    for spec in SPECTRAL_SPECS:
        start = float(spec["start"])
        step = float(spec["step"])
        count = int(round((float(spec["end"]) - start) / step)) + 1
        wavelengths = [round(start + index * step, 8) for index in range(count)]
        materials.append(
            {
                "id": spec["id"],
                "kind": "spectral",
                "name": spec["name"],
                "targets": ["srgb", "display-p3"],
                "truth_source": f"deterministic-spectrophotometer-seed-{seed}",
                "spectrum": {
                    "wavelengths": wavelengths,
                    "interval_nm": step,
                    "values": _spectrum_values(spec, seed),
                },
            }
        )
    materials.extend(
        [
            {
                "id": "display-srgb-blueprint",
                "kind": "display",
                "name": "sRGB Blueprint",
                "targets": ["srgb", "display-p3"],
                "truth_source": "existing-srgb-material-descriptor",
                "color_space": "srgb",
                "encoded": [0.082, 0.258744669475, 0.486],
            },
            {
                "id": "display-p3-cyan",
                "kind": "display",
                "name": "Display-P3 Cyan",
                "targets": ["display-p3", "srgb"],
                "truth_source": "existing-display-p3-material-descriptor",
                "color_space": "display-p3",
                "encoded": [0.031, 0.712, 0.782203085142],
            },
        ]
    )
    return {
        "schema": "spectral-material-input/v1",
        "library_id": f"sample-library-{seed}",
        "seed": seed,
        "pipeline": pipeline.to_dict(),
        "materials": materials,
    }


def generate_samples(out_dir: str | Path, seed: int = 1907) -> GeneratedSamples:
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    state = out / "state"
    state.mkdir(parents=True, exist_ok=True)
    for stale in (
        out / "trace.jsonl",
        out / "summary.json",
        state / "snapshot.json",
        state / "pending.json",
    ):
        stale.unlink(missing_ok=True)
    document = sample_document(seed)
    pipeline = PipelineConfig.from_dict(document["pipeline"], "pipeline")
    definitions = parse_material_document(document, pipeline.grid)
    write_json_atomic(out / "materials.json", document)
    write_json_atomic(
        out / "observer_2deg.json",
        generate_observer_document("2deg", pipeline.grid),
    )
    write_json_atomic(
        out / "observer_10deg.json",
        generate_observer_document("10deg", pipeline.grid),
    )
    write_json_atomic(
        out / "illuminant_d65.json",
        generate_illuminant_document("D65", 6504.0, pipeline.grid),
    )
    snapshot_path = state / "snapshot.json"
    library = MaterialLibrary.create(
        definitions,
        pipeline,
        library_id=str(document["library_id"]),
        seed=seed,
        snapshot_path=snapshot_path,
        trace_path=out / "trace.jsonl",
    )
    snapshot = library.current_snapshot
    baseline = {
        "schema": "spectral-material-baseline/v1",
        "library_id": snapshot.library_id,
        "seed": seed,
        "generation": snapshot.generation,
        "publication_hash": snapshot.publication_hash,
        "materials": snapshot.current_materials(),
        "snapshot_hash": sha256_bytes(snapshot_path.read_bytes()),
    }
    write_json_atomic(out / "baseline_hashes.json", baseline)
    library.close()
    return GeneratedSamples(
        out_dir=out,
        seed=seed,
        library_id=snapshot.library_id,
        pipeline=pipeline,
        definitions=tuple(definitions),
        baseline_snapshot_hash=baseline["snapshot_hash"],
    )


def material_descriptor_from_params(params: Mapping[str, Any]) -> dict[str, Any]:
    value = thaw_value(params)
    if value.get("kind") == "spectral":
        spectrum = value["spectrum"]
        return {
            "id": value["id"],
            "kind": "spectral",
            "name": value["name"],
            "targets": value["targets"],
            "truth_source": value.get("truth_source", "edited"),
            "spectrum": {
                "wavelengths": spectrum["original_wavelengths_nm"],
                "values": spectrum["original_values"],
                "interval_nm": spectrum["original_interval_nm"],
            },
        }
    return {
        "id": value["id"],
        "kind": "display",
        "name": value["name"],
        "targets": value["targets"],
        "truth_source": value.get("truth_source", "edited"),
        "color_space": value["color_space"],
        "encoded": value["encoded"],
    }
