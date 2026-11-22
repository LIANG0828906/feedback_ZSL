"""Reproducible incremental update and atomic rollback demonstration."""

from __future__ import annotations

import argparse
import copy
import json
import sys
from dataclasses import replace
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender.config import parse_config
from volrender.errors import BusinessError
from volrender.incremental import ChangeSet, IncrementalRenderer, infer_changes
from volrender.io_utils import atomic_write_bytes, json_bytes
from volrender.model import (
    MEDIUM_BOX,
    Camera,
    DensityField,
    DirectionalLight,
    Medium,
    Scene,
)


def _field(field_id: int, scale: float) -> DensityField:
    dims = (6, 6, 6)
    chunk = (2, 2, 2)
    x = np.linspace(0.0, 1.0, dims[0])[None, None, :]
    y = np.linspace(0.0, 1.0, dims[1])[None, :, None]
    z = np.linspace(0.0, 1.0, dims[2])[:, None, None]
    full = scale * (
        0.45 + 0.9 * np.exp(-((x - 0.5) ** 2 + (y - 0.5) ** 2 + (z - 0.5) ** 2) / 0.13)
    )
    chunks: dict[tuple[int, int, int], np.ndarray] = {}
    for kz in range(3):
        for ky in range(3):
            for kx in range(3):
                chunks[(kx, ky, kz)] = np.ascontiguousarray(
                    full[kz * 2 : kz * 2 + 2, ky * 2 : ky * 2 + 2, kx * 2 : kx * 2 + 2]
                )
    return DensityField(field_id, dims, chunk, chunks)


def _box(
    medium_id: int,
    field_id: int,
    lo: tuple[float, float, float],
    hi: tuple[float, float, float],
    sigma_s: tuple[float, float, float],
) -> Medium:
    return Medium(
        id=medium_id,
        kind=MEDIUM_BOX,
        density_field_id=field_id,
        sigma_s=np.array(sigma_s, dtype=np.float64),
        sigma_a=np.array([0.03, 0.03, 0.03], dtype=np.float64),
        g=0.15,
        density_scale=1.0,
        shape_a=np.array(lo, dtype=np.float64),
        shape_b=np.array([hi[0], hi[1], hi[2], 0.0], dtype=np.float64),
    )


def make_scene() -> Scene:
    width, height = 40, 30
    focal = 1.5 * width
    intrinsics = np.array(
        [
            [focal, 0.0, (width - 1) * 0.5],
            [0.0, focal, (height - 1) * 0.5],
            [0.0, 0.0, 1.0],
        ],
        dtype=np.float64,
    )
    view = np.eye(4, dtype=np.float64)
    view[2, 3] = 3.2
    return Scene(
        width=width,
        height=height,
        camera=Camera(intrinsics, view),
        light=DirectionalLight([0.2, 0.25, 1.0], [1.1, 1.0, 0.9]),
        media=[
            _box(10, 7, (-1.20, -0.50, -0.38), (-0.35, 0.50, 0.38), (0.20, 0.14, 0.10)),
            _box(11, 8, (0.35, -0.50, -0.38), (1.20, 0.50, 0.38), (0.16, 0.20, 0.12)),
        ],
        density_fields={7: _field(7, 1.0), 8: _field(8, 1.2)},
    )


def make_config():
    return parse_config(
        json.dumps(
            {
                "tile_size": 10,
                "max_parallelism": 2,
                "sample_budget": 5_000_000,
                "base_step": 0.18,
                "min_step": 0.012,
                "precision": "float",
                "degrade_sequence": [
                    {"name": "S0_FULL", "step_multiplier": 1.0, "shadow_samples": 2},
                    {"name": "S1_STEP_4X", "step_multiplier": 4.0, "shadow_samples": 1},
                ],
                "seam": {
                    "enabled": False,
                    "tolerance": 0.0,
                    "truth_budget": 20_000_000,
                    "truth_stage": "S0_FULL",
                },
                "limits": {
                    "max_parallelism": 8,
                    "max_sample_budget": 50_000_000,
                },
            }
        )
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    scene = make_scene()
    config = make_config()
    renderer = IncrementalRenderer()
    initial = renderer.render(scene, config, changes=ChangeSet(), validate_seams=False)

    changed = copy.deepcopy(scene)
    changed.media_by_id()[10].sigma_s *= 1.4
    changes = infer_changes(scene, changed)
    updated = renderer.render(changed, config, changes=changes, validate_seams=False)
    snapshot_after_update = renderer.get_snapshot()
    assert snapshot_after_update is not None
    version_before_failure = snapshot_after_update.version
    rollback_error = None
    rejected_config = replace(config, sample_budget=1)
    try:
        renderer.render(
            changed,
            rejected_config,
            changes=ChangeSet(camera_changed=True),
            validate_seams=False,
        )
    except BusinessError as exc:
        rollback_error = exc.to_dict()
    snapshot_after_failure = renderer.get_snapshot()
    assert snapshot_after_failure is not None
    report = {
        "initial": initial.summary["incremental"],
        "changes": {
            "medium_material_changed": sorted(changes.medium_material_changed),
            "density_chunks_changed": [list(value) for value in sorted(changes.density_chunks_changed)],
        },
        "updated": updated.summary["incremental"],
        "reused_hashes_preserved": all(
            {tile.tile.tile_id: tile.content_hash for tile in initial.tile_results}[tile_id]
            == {
                tile.tile.tile_id: tile.content_hash
                for tile in updated.tile_results
            }[tile_id]
            for tile_id in updated.summary["incremental"]["reused_tiles"]
        ),
        "atomic_failure": {
            "error": rollback_error,
            "version_before_failure": version_before_failure,
            "version_after_failure": snapshot_after_failure.version,
            "snapshot_unchanged": version_before_failure
            == snapshot_after_failure.version,
        },
    }
    atomic_write_bytes(args.output, json_bytes(report))
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
