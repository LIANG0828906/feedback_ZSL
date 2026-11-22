from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from lcft.generator import generate_sample


@pytest.fixture(scope="session")
def sample_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    directory = tmp_path_factory.mktemp("sample")
    generate_sample(
        directory,
        seed=20261122,
        sentence_count=18,
        languages=("zh", "en"),
        channels=1,
        duration_seconds="42",
        description_density_permille=280,
        contention_permille=500,
    )
    return directory


@pytest.fixture(scope="session")
def sample_bundle_path(sample_dir: Path) -> Path:
    return sample_dir / "bundle.json"


@pytest.fixture(scope="session")
def sample_raw(sample_dir: Path) -> dict:
    with open(sample_dir / "bundle.json", "r", encoding="utf-8") as handle:
        return json.load(handle)


@pytest.fixture(scope="session")
def descriptor_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    directory = tmp_path_factory.mktemp("descriptor-sample")
    generate_sample(
        directory,
        seed=7711,
        sentence_count=2,
        languages=("en",),
        channels=1,
        duration_seconds="20",
        description_density_permille=500,
        contention_permille=1000,
    )
    return directory


@pytest.fixture(scope="session")
def descriptor_raw(descriptor_dir: Path) -> dict:
    with open(descriptor_dir / "bundle.json", "r", encoding="utf-8") as handle:
        return json.load(handle)


@pytest.fixture(scope="session")
def surround_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    directory = tmp_path_factory.mktemp("surround-sample")
    generate_sample(
        directory,
        seed=991,
        sentence_count=2,
        languages=("zh",),
        channels=6,
        duration_seconds="6",
        description_density_permille=0,
        contention_permille=0,
    )
    return directory


def descriptor_variant(
    descriptor_raw: dict,
    *,
    description_text: str,
    description_priority: int,
    speech_priority: int = 3,
) -> dict:
    raw = copy.deepcopy(descriptor_raw)
    speech = raw["tracks"]["en"]["sentences"]
    speech[0]["text"] = "short"
    speech[0]["max_read_speed"] = 10
    for sentence in speech:
        sentence["priority"] = speech_priority
    raw["descriptions"] = [
        {
            "id": "desc-test",
            "text": description_text,
            "start": 48_000,
            "end": 144_000,
            "priority": description_priority,
            "speaker": "",
        }
    ]
    return raw

