from __future__ import annotations

import json
import os
import shutil
import sys
from pathlib import Path
from typing import Any

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from streamforge.samples import generate_samples


@pytest.fixture(scope="session")
def sample_bundle(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Path]:
    root = tmp_path_factory.mktemp("samples")
    return generate_samples(root, seed=20261121, duration_ms=1200)


def read_json(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")


@pytest.fixture
def config_factory(sample_bundle: dict[str, Path], tmp_path: Path):
    def make(
        *,
        tier_updates: dict[str, dict[str, Any]] | None = None,
        budget_updates: dict[str, Any] | None = None,
        source_audio: Path | None = None,
        source_video: Path | None = None,
        config_version: str | None = None,
        new_tier: dict[str, Any] | None = None,
    ) -> tuple[Path, Path]:
        document = read_json(sample_bundle["track_config"])
        document["source"]["audio"] = str(
            (source_audio or sample_bundle["audio"]).resolve()
        )
        document["source"]["video"] = str(
            (source_video or sample_bundle["video"]).resolve()
        )
        if config_version is not None:
            document["config_version"] = config_version
        for tier in document["tiers"]:
            if tier_updates and tier["id"] in tier_updates:
                tier.update(tier_updates[tier["id"]])
        if new_tier is not None:
            document["tiers"].append(new_tier)
        budget = read_json(sample_bundle["budgets"])
        if budget_updates:
            budget.update(budget_updates)
        config_path = tmp_path / "track_config.json"
        budget_path = tmp_path / "budgets.json"
        write_json(config_path, document)
        write_json(budget_path, budget)
        shutil.copyfile(sample_bundle["playback"], tmp_path / "playback.json")
        shutil.copyfile(sample_bundle["bandwidth"], tmp_path / "bandwidth.json")
        return config_path, budget_path

    return make
