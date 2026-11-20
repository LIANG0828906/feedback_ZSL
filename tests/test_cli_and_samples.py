from __future__ import annotations

import json

from dustrender.cli import main
from dustrender.samplegen import generate_sample_set


def test_validate_cli(smoke_path, capsys) -> None:
    assert main(["validate", str(smoke_path)]) == 0
    value = json.loads(capsys.readouterr().out)
    assert value["ok"] is True
    assert value["scene_id"] == "smoke_small-20261132"


def test_sample_generation_is_deterministic(tmp_path) -> None:
    first = generate_sample_set(tmp_path / "first", render_baselines=False)
    second = generate_sample_set(tmp_path / "second", render_baselines=False)
    assert first["samples"] == second["samples"]
    for sample in first["samples"]:
        left = (tmp_path / "first" / sample["file"]).read_bytes()
        right = (tmp_path / "second" / sample["file"]).read_bytes()
        assert left == right
