from __future__ import annotations

import json
from pathlib import Path

from terrain_pipeline.cli import main


def test_cli_edit_creates_new_edits_file_and_bakes(project_dir: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    edits = tmp_path / "edits.json"
    code = main(
        [
            "edit",
            str(project_dir),
            "--cache",
            str(cache),
            "--edits",
            str(edits),
            "--tile",
            "T0004",
            "--material",
            "rock",
            "1.5",
            "--subdivisions",
            "2",
            "--workers",
            "2",
        ]
    )
    assert code == 0
    assert edits.is_file()
    assert (cache / "snapshot.json").is_file()
    payload = json.loads(edits.read_text(encoding="utf-8"))
    assert payload["material_edits"][0]["tile"] == "T0004"

    inspect_code = main(["inspect", str(cache)])
    assert inspect_code == 0


def test_cli_bake_and_compare(project_dir: Path, tmp_path: Path) -> None:
    left = tmp_path / "left"
    right = tmp_path / "right"
    common = [
        "--subdivisions",
        "2",
        "--workers",
        "1",
        "--mode",
        "full",
    ]
    assert main(["bake", str(project_dir), "--cache", str(left), *common]) == 0
    assert main(["bake", str(project_dir), "--cache", str(right), *common]) == 0
    assert main(["compare", str(left), str(right)]) == 0
