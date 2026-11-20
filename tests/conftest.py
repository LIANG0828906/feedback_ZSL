from __future__ import annotations

import os
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from spectral_editor.color import default_pipeline_config
from spectral_editor.library import MaterialLibrary
from spectral_editor.materials import parse_material_document
from spectral_editor.samples import sample_document


@pytest.fixture
def pipeline_config():
    return default_pipeline_config(seed=1907)


@pytest.fixture
def definitions(pipeline_config):
    document = sample_document(1907)
    return parse_material_document(document, pipeline_config.grid)


@pytest.fixture
def library(tmp_path: Path, definitions, pipeline_config):
    snapshot_path = tmp_path / "state" / "snapshot.json"
    trace_path = tmp_path / "trace.jsonl"
    instance = MaterialLibrary.create(
        definitions,
        pipeline_config,
        library_id="test-library",
        seed=1907,
        snapshot_path=snapshot_path,
        trace_path=trace_path,
    )
    instance.snapshot_fixture_path = snapshot_path  # type: ignore[attr-defined]
    yield instance
    instance.close()
