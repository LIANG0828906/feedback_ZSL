from __future__ import annotations

import json
from pathlib import Path

import pytest

from festival_master.demo import write_demo_manifest
from festival_master.manifest import load_manifest
from festival_master.synth import generate_material


@pytest.fixture()
def project(tmp_path: Path) -> dict[str, Path]:
    manifest_path = tmp_path / "manifest.json"
    write_demo_manifest(
        manifest_path,
        duration_seconds=1.2,
        block_samples=1024,
        target_loudness_lufs=-16.0,
    )
    pending = load_manifest(manifest_path, check_audio=False)
    generate_material(pending)
    manifest = load_manifest(manifest_path, check_audio=True)
    return {
        "root": tmp_path,
        "manifest": manifest_path,
        "manifest_object": manifest,
    }


def write_json(path: Path, value: dict) -> Path:
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return path
