from __future__ import annotations

from pathlib import Path

import pytest

from scene_audio.generator import generate_demo
from scene_audio.render import render_scene


@pytest.fixture(scope="session")
def demo_scene(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("demo_assets")
    generate_demo(root)
    return root / "scene.json"


@pytest.fixture(scope="session")
def full_render(
    demo_scene: Path, tmp_path_factory: pytest.TempPathFactory
) -> tuple[Path, dict]:
    out = tmp_path_factory.mktemp("full_render")
    result = render_scene(demo_scene, out)
    assert result.exit_code == 0
    assert result.report is not None
    return out, result.report

