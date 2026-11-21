from __future__ import annotations

from pathlib import Path

import pytest

from lightprobe.bake_types import BakeConfig
from lightprobe.scene import load_scene

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


@pytest.fixture(scope="session")
def room():
    return load_scene(SAMPLES / "street_corner_room.json")


@pytest.fixture(scope="session")
def city():
    return load_scene(SAMPLES / "city_district.json")


def fast_config(scene, **overrides):
    values = {
        "validation_probe_limit": 8,
        "validation_direction_count": 64,
        "chunk_size": 16,
    }
    values.update(overrides)
    return BakeConfig.from_scene(scene, **values)

