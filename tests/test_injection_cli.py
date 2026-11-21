from __future__ import annotations

import json

import pytest

from volrender.cli import main
from volrender.errors import AppError, ErrorCode
from volrender.injection import INJECTIONS, load_inputs


EXPECTED = {
    "corrupt_tile_index": ErrorCode.E_TILE_INDEX_CORRUPT,
    "missing_medium": ErrorCode.E_MEDIUM_MISSING,
    "nonfinite_camera": ErrorCode.E_CAMERA_MATRIX_NONFINITE,
    "truncated_density": ErrorCode.E_DENSITY_TRUNCATED,
    "budget_oversample": ErrorCode.E_BUDGET_CONFIG_INVALID,
}


@pytest.mark.parametrize("injection", INJECTIONS)
def test_each_injection_has_independent_code(sample_bundle, injection):
    with pytest.raises(AppError) as caught:
        load_inputs(
            sample_bundle["scene_path"],
            sample_bundle["config_path"],
            injection=injection,
        )
    error = caught.value
    assert error.code == EXPECTED[injection]
    assert error.injection_point
    assert error.details["injected"] == injection


@pytest.mark.parametrize("injection", INJECTIONS)
def test_injection_writes_no_color_output(
    sample_bundle, injection, tmp_path, capsys
):
    output = tmp_path / "never-written"
    code = main(
        [
            "render",
            "--scene",
            str(sample_bundle["scene_path"]),
            "--config",
            str(sample_bundle["config_path"]),
            "--out",
            str(output),
            "--inject",
            injection,
        ]
    )
    assert code == 2
    assert not output.exists()
    payload = json.loads(capsys.readouterr().out.strip())
    assert payload["code"] == EXPECTED[injection].value
    assert payload["injection_point"]


def test_cli_generate_and_float_render(tmp_path, capsys):
    sample = tmp_path / "sample"
    assert main(["generate", "--out", str(sample)]) == 0
    capsys.readouterr()
    output = tmp_path / "preview"
    code = main(
        [
            "render",
            "--scene",
            str(sample / "scene.volbin"),
            "--config",
            str(sample / "render_config.json"),
            "--out",
            str(output),
            "--mode",
            "float64",
        ]
    )
    assert code == 0
    payload = json.loads(capsys.readouterr().out.strip())
    assert payload["ok"] is True
    assert (output / "current" / "color_float64.bin").exists()
