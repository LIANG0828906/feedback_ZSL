from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

import pytest

from offline_master.demo import build_manifest
from offline_master.synthesis import synthesize_project


@pytest.fixture()
def project_dir(tmp_path):
    manifest = build_manifest()
    project = tmp_path / "proj"
    project.mkdir()
    manifest_path = project / "manifest.json"
    import json

    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True))
    synthesize_project(manifest, str(project))
    return project, manifest
