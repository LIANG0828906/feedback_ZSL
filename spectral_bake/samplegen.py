"""Deterministic local sample generator (requirement 8)."""
from __future__ import annotations

import json
from pathlib import Path

import numpy as np

SEED = 20261122


def _gauss(wl, mu, sigma, amp):
    return amp * np.exp(-0.5 * ((wl - mu) / sigma) ** 2)


def _spectral_material(rng, idx, step_nm):
    wl = np.arange(380.0, 780.0 + step_nm, step_nm)
    refl = np.full_like(wl, 0.05 + 0.05 * rng.rand())
    n_lobes = 1 + int(rng.rand() * 3)
    for _ in range(n_lobes):
        mu = 400.0 + 340.0 * rng.rand()
        sigma = 15.0 + 60.0 * rng.rand()
        amp = 0.3 + 0.65 * rng.rand()
        refl = refl + _gauss(wl, mu, sigma, amp)
    refl = np.clip(refl, 0.0, 1.0)
    return {
        "id": f"spec{idx}",
        "type": "spectral",
        "wavelengths_nm": [round(float(x), 3) for x in wl],
        "reflectance": [round(float(x), 6) for x in refl],
    }


def generate_samples(out_dir, n_spectral: int = 9) -> dict:
    """Write materials.json and config.json deterministically."""
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    rng = np.random.RandomState(SEED)
    materials = []
    steps = (5.0, 10.0, 20.0)
    for i in range(n_spectral):
        materials.append(_spectral_material(rng, i, steps[i % len(steps)]))
    materials.append({"id": "srgb0", "type": "srgb", "color": [0.8, 0.2, 0.2]})
    materials.append({"id": "srgb1", "type": "srgb", "color": [0.2, 0.7, 0.3]})
    materials.append({"id": "p3-0", "type": "display-p3", "color": [0.9, 0.4, 0.1]})
    (out / "materials.json").write_text(json.dumps(materials, indent=1), encoding="utf-8")
    config = {
        "parallelism": 4,
        "per_task_memory_bytes": 262144,
        "global_memory_bytes": 1048576,
        "degradation_sequence": [
            "reduce_sphere_samples",
            "shrink_internal_batch",
            "reduce_preview_resolution",
        ],
        "grid": {"start_nm": 380.0, "end_nm": 780.0, "step_nm": 5.0},
        "observer": "cie1931_2deg",
        "white_point": "D65",
        "adaptation": "bradford",
        "targets": ["srgb", "display-p3"],
        "sphere_samples": 256,
        "internal_batch": 8,
        "preview_resolution": 64,
        "samples_dir": str(out),
        "snapshots_dir": str(out / "snapshots"),
        "trace_path": str(out / "snapshots" / "trace.jsonl"),
    }
    (out / "config.json").write_text(json.dumps(config, indent=1), encoding="utf-8")
    return {"materials": out / "materials.json", "config": out / "config.json"}


def generate_baseline(out_dir) -> dict:
    """Run one baseline batch and record its reference hashes."""
    from .config import load_config
    from .engine import run_bake
    from .spectrum import load_materials

    out = Path(out_dir)
    config = load_config(out / "config.json")
    materials = load_materials(out / "materials.json", config.grid)
    manifest = run_bake(config, materials, batch_id="baseline")
    baseline = {
        "batch_id": "baseline",
        "chain_tip": manifest["hash_chain"][-1],
        "entries": {
            e["material_id"]: {"codes": e["codes"], "content_hash": e["content_hash"]}
            for e in manifest["entries"]
        },
    }
    (out / "baseline_hashes.json").write_text(
        json.dumps(baseline, indent=1, sort_keys=True), encoding="utf-8"
    )
    return baseline
