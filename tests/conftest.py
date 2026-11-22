from __future__ import annotations

import json
import os
from pathlib import Path

import pytest


os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

ROOT = Path(__file__).resolve().parents[1]
DATA = ROOT / "data" / "samples"


@pytest.fixture(scope="session", autouse=True)
def ensure_samples() -> None:
    if not (DATA / "manifest.json").exists():
        from balance_lab.generator import generate_all

        generate_all(DATA)


@pytest.fixture(scope="session")
def alpha():
    from balance_lab.validation import load_candidate_set, load_project

    project = load_project(DATA / "alpha.json")
    candidates = load_candidate_set(DATA / "alpha.candidates.json", project)
    return project, candidates


@pytest.fixture(scope="session")
def injection_specs() -> dict:
    return json.loads((DATA / "injection_cases.json").read_text(encoding="utf-8"))

