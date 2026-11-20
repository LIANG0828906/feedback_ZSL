from __future__ import annotations

import json
from pathlib import Path

import pytest

from mixrec.generator import generate_samples


@pytest.fixture(scope="session")
def fixtures(tmp_path_factory):
    root = tmp_path_factory.mktemp("mixrec-fixtures")
    generate_samples(root, seed=20261121, with_baseline=False)
    return root


def job_config_path(fixtures: Path, job_id: str) -> Path:
    return fixtures / "jobs" / job_id / "config.json"


def copy_config_with_paths(
    fixtures: Path,
    job_id: str,
    destination: Path,
    **changes,
) -> Path:
    """Create a config whose track paths are explicit for an isolated output."""

    source = job_config_path(fixtures, job_id)
    value = json.loads(source.read_text(encoding="utf-8"))
    for track in value["tracks"]:
        track["path"] = str((source.parent / track["path"]).resolve())
    value.update(changes)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(value, indent=2), encoding="utf-8")
    return destination
