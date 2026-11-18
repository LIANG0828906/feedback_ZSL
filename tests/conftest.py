from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

from orchestral_mastering.pipeline import RenderConfig, render_project
from orchestral_mastering.synthesis import create_demo_project


PROJECT_ROOT = Path(__file__).resolve().parents[1]


@pytest.fixture(scope="session")
def demo_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("demo")
    create_demo_project(root, seed=24701, duration_seconds=8.0, force=True)
    return root


@pytest.fixture(scope="session")
def rendered(demo_root: Path, tmp_path_factory: pytest.TempPathFactory) -> dict:
    root = tmp_path_factory.mktemp("rendered")
    report = render_project(
        RenderConfig(
            manifest_path=demo_root / "manifest.json",
            output_path=root / "master.wav",
            report_path=root / "report.json",
            snapshot_path=root / "snapshot.json",
            block_frames=16384,
        )
    )
    return {"root": root, "report": report}


@pytest.fixture()
def cli_env() -> dict[str, str]:
    import os

    env = dict(os.environ)
    env["PYTHONPATH"] = str(PROJECT_ROOT) + (
        os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else ""
    )
    return env


def absolutize_manifest(source: Path, destination: Path) -> dict:
    manifest = json.loads(source.read_text(encoding="utf-8"))
    base = source.resolve().parent
    for track in manifest["tracks"]:
        track["path"] = str((base / track["path"]).resolve())
        track["reference_path"] = str((base / track["reference_path"]).resolve())
    destination.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return manifest


def write_manifest(manifest: dict, destination: Path) -> None:
    destination.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def python_executable() -> str:
    return sys.executable

