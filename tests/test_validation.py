from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from scene_audio.errors import ValidationFailure
from scene_audio.scene import load_scene


def _load_and_modify(scene_path: Path, destination: Path) -> dict:
    scene = json.loads(scene_path.read_text(encoding="utf-8"))
    base = scene_path.parent
    for track in scene["tracks"]:
        source = base / track["path"]
        target = destination / track["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(source.read_bytes())
    return scene


def _write_scene(destination: Path, scene: dict) -> Path:
    path = destination / "scene.json"
    path.write_text(json.dumps(scene, indent=2), encoding="utf-8")
    return path


@pytest.mark.parametrize(
    ("mutation", "expected_code", "expected_track", "expected_event"),
    [
        ("rate", "E_TRACK_RATE_MISMATCH", "dialogue", None),
        ("missing", "E_TRACK_MISSING", "dialogue", None),
        ("camera", "E_CAMERA_TIME", None, None),
        ("overlap", "E_EVENT_OVERLAP", "dialogue", "DLG_002"),
        ("negative", "E_EVENT_TIME", "dialogue", "DLG_001"),
    ],
)
def test_structured_hard_rejection(
    demo_scene: Path,
    tmp_path: Path,
    mutation: str,
    expected_code: str,
    expected_track: str | None,
    expected_event: str | None,
) -> None:
    scene = _load_and_modify(demo_scene, tmp_path)
    if mutation == "rate":
        scene["tracks"][0]["sample_rate"] = 48000
    elif mutation == "missing":
        scene["tracks"][0]["path"] = "assets/does_not_exist.wav"
    elif mutation == "camera":
        scene["camera"]["path"][1]["time"] = scene["camera"]["path"][0]["time"]
    elif mutation == "overlap":
        scene["tracks"][0]["events"][1]["start_time"] = 0.9
    elif mutation == "negative":
        scene["tracks"][0]["events"][0]["start_time"] = -0.1
    path = _write_scene(tmp_path, scene)

    with pytest.raises(ValidationFailure) as captured:
        load_scene(path)
    failure = captured.value
    matching = [issue for issue in failure.issues if issue.code == expected_code]
    assert matching, [issue.to_dict() for issue in failure.issues]
    issue = matching[0]
    assert issue.track == expected_track
    assert issue.event_id == expected_event


def test_cli_rejects_without_partial_master(
    demo_scene: Path, tmp_path: Path
) -> None:
    scene = _load_and_modify(demo_scene, tmp_path)
    scene["tracks"][1]["sample_rate"] = 44100
    path = _write_scene(tmp_path, scene)
    out = tmp_path / "rejected"
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "scene_audio",
            "render",
            "--scene",
            str(path),
            "--out",
            str(out),
        ],
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert completed.returncode == 2
    assert not (out / "master.wav").exists()
    error = json.loads((out / "error.json").read_text(encoding="utf-8"))
    assert error["ok"] is False
    assert any(
        issue["code"] == "E_TRACK_RATE_MISMATCH"
        and issue["track"] == "effects"
        for issue in error["issues"]
    )

