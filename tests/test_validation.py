from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from festival_master.errors import MasterError
from festival_master.manifest import load_manifest


def _load(project: dict[str, Path]):
    return json.loads(project["manifest"].read_text(encoding="utf-8"))


def _write(project: dict[str, Path], data: dict) -> Path:
    path = project["root"] / "invalid.json"
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path


def _assert_error(path: Path, code: str, channel: str | None = None) -> None:
    with pytest.raises(MasterError) as error:
        load_manifest(path)
    assert error.value.exit_code != 0
    assert any(issue.code == code for issue in error.value.issues)
    if channel is not None:
        assert any(issue.channel == channel for issue in error.value.issues)


def test_missing_file_is_structured(project: dict[str, Path]) -> None:
    data = _load(project)
    data["channels"][0]["path"] = "audio/not_there.wav"
    _assert_error(_write(project, data), "E_MISSING_FILE", "lead_vocal")


def test_sample_rate_bit_depth_and_channel_count_mismatches(project: dict[str, Path]) -> None:
    data = _load(project)
    data["sample_rate"] = 44100
    _assert_error(_write(project, data), "E_SAMPLE_RATE", "lead_vocal")

    data = _load(project)
    data["bit_depth"] = 16
    _assert_error(_write(project, data), "E_BIT_DEPTH", "lead_vocal")

    data = _load(project)
    data["num_channels"] = 2
    _assert_error(_write(project, data), "E_CHANNELS", "lead_vocal")


def test_duplicate_instrument_anchor_and_target_errors(project: dict[str, Path]) -> None:
    data = _load(project)
    data["channels"][1]["instrument"] = "主唱"
    _assert_error(_write(project, data), "E_DUPLICATE_INSTRUMENT", "guitar")

    data = _load(project)
    data["channels"][2]["anchor"]["x"] = 100.0
    _assert_error(_write(project, data), "E_ANCHOR_RANGE", "bass")

    data = _load(project)
    data.pop("target_loudness_lufs")
    _assert_error(_write(project, data), "E_TARGET_LOUDNESS", "MASTER")


def test_cli_returns_nonzero_and_business_code(project: dict[str, Path], capsys) -> None:
    from festival_master.cli import main

    data = _load(project)
    data.pop("target_loudness_lufs")
    invalid_path = _write(project, data)
    exit_code = main(
        [
            "render",
            "--manifest",
            str(invalid_path),
            "--output",
            str(project["root"] / "out.wav"),
            "--report",
            str(project["root"] / "report.json"),
        ]
    )
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert exit_code != 0
    assert payload["ok"] is False
    assert payload["code"] == "E_TARGET_LOUDNESS"
    assert payload["errors"][0]["channel"] == "MASTER"
