from __future__ import annotations

import dataclasses

import pytest

from probe_baker.samples import room_scene_payload
from probe_baker.scene import Scene, parse_scene
from probe_baker.strategy import STRATEGY_DENSE, BakeConfig


@pytest.fixture
def room_scene() -> Scene:
    return parse_scene(room_scene_payload())


@pytest.fixture
def small_config() -> BakeConfig:
    return BakeConfig(
        spacing=1.0,
        sh_order=2,
        direction_count=32,
        compensation_strength=0.3,
        strategy=STRATEGY_DENSE,
        chunk_size=16,
    )

