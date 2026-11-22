from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from terrain_pipeline.generator import generate_project
from terrain_pipeline.model import BakeParams


@pytest.fixture
def project_dir(tmp_path: Path) -> Path:
    root = tmp_path / "project"
    generate_project(
        root,
        seed=1234,
        grid_tiles=(3, 2),
        tile_size=8,
        subdivisions=2,
        points_per_tile=30,
    )
    return root


@pytest.fixture
def params() -> BakeParams:
    return BakeParams(subdivisions=2, max_workers=2)


def read_manifest(project_dir: Path) -> dict[str, Any]:
    return json.loads((project_dir / "manifest.json").read_text(encoding="utf-8"))


def write_manifest(project_dir: Path, manifest: dict[str, Any]) -> None:
    (project_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
