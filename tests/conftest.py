import json
import os

import pytest

from applause_master.cli import build_demo_project
from applause_master.project import load_project
from applause_master.synth import generate_project_audio


@pytest.fixture
def demo_workspace(tmp_path):
    work = tmp_path / "work"
    project_path = build_demo_project(str(work), seed=777, duration_seconds=8.0)
    # Files do not exist yet at scaffolding time; synthesize then validate.
    with open(project_path, encoding="utf-8") as handle:
        raw = json.load(handle)
    base = os.path.dirname(project_path)
    for mic in raw["microphones"]:
        mic["file"] = os.path.join(base, mic["file"])
    raw["project_path"] = project_path
    truth = generate_project_audio(raw)
    project = load_project(project_path)
    with open(os.path.join(str(work), "ground_truth.json"), "w", encoding="utf-8") as handle:
        json.dump(truth, handle, indent=2)
    return {"work": str(work), "project_path": project_path, "project": project, "truth": truth}
