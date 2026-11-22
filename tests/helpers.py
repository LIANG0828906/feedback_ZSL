from __future__ import annotations

from pathlib import Path

from npr_workbench.model import RenderParams, Scene
from npr_workbench.scene import load_scene

ROOT = Path(__file__).resolve().parents[1]


def sample_scene(name: str = "orb_and_crate") -> Scene:
    return load_scene(ROOT / "data" / "scenes" / f"{name}.json")


def fast_params(*, workers: int = 4, levels: int = 5) -> RenderParams:
    return RenderParams(
        levels=levels,
        density=0.30,
        length=12.0,
        preview_width=80,
        preview_height=60,
        exact_width=240,
        exact_height=180,
        tile_width=60,
        tile_height=60,
        max_workers=workers,
        thread_pixel_budget=8192,
        stroke_cache_budget=128,
        guide_width=64,
        guide_height=48,
    )

