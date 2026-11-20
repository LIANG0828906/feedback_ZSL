from __future__ import annotations

import json
from pathlib import Path

import pytest

from game_audio_restorer.errors import BusinessError, ErrorCode
from game_audio_restorer.recipes import load_recipe, recipe_from_mapping


def _invalid(payload: dict, field: str) -> BusinessError:
    with pytest.raises(BusinessError) as caught:
        recipe_from_mapping(payload, source="<test>")
    error = caught.value
    assert error.code == ErrorCode.RECIPE_INVALID
    assert error.details["location"]["field"] == field
    return error


def test_v1_upgrade_defaults_and_sources() -> None:
    recipe = recipe_from_mapping(
        {"version": 1, "denoise_strength": 0.4, "gain_db": 1.5}
    )
    assert recipe.remove_dc is False
    assert recipe.silence_threshold_dbfs is None
    assert recipe.channel_merge_mode == "preserve"
    assert recipe.spectral_subtraction.segment_ms == 32
    assert recipe.loudness_target_lufs is None
    mapping = {row["field"]: row for row in recipe.field_mapping}
    assert mapping["remove_dc"]["source"] == "upgrade_default"
    assert mapping["channel_merge_mode"]["source"] == "compatibility_default"
    assert mapping["loudness_target_lufs"]["source"] == "disabled_default"
    lost = {row["field"] for row in recipe.lost_fields}
    assert "remove_dc" in lost
    assert "loudness_target_lufs" in lost


def test_v2_and_v3_normalization() -> None:
    v2 = recipe_from_mapping(
        {
            "version": 2,
            "denoise_strength": 0.6,
            "gain_db": -1,
            "remove_dc": True,
            "silence_threshold_dbfs": -50,
        }
    )
    assert v2.remove_dc is True
    assert v2.silence_threshold_dbfs == -50.0
    assert v2.loudness_target_lufs is None
    assert {row["field"] for row in v2.lost_fields} == {
        "channel_merge_mode",
        "spectral_subtraction.segment_ms",
        "spectral_subtraction.oversubtraction",
        "spectral_subtraction.spectral_floor",
        "loudness_target_lufs",
    }

    v3 = recipe_from_mapping(
        {
            "version": 3,
            "denoise_strength": 0.8,
            "gain_db": 0,
            "remove_dc": False,
            "silence_threshold_dbfs": -45,
            "channel_merge_mode": "mono_mix",
            "spectral_subtraction": {
                "segment_ms": 24,
                "oversubtraction": 2.2,
                "spectral_floor": 0.01,
            },
            "loudness_target_lufs": -22,
        }
    )
    assert v3.channel_merge_mode == "mono_mix"
    assert v3.spectral_subtraction.oversubtraction == 2.2
    assert v3.loudness_target_lufs == -22
    assert v3.lost_fields == ()
    assert recipe_from_mapping(v3.semantic_dict()).hash() == v3.hash()


@pytest.mark.parametrize(
    ("payload", "field"),
    [
        ({"version": 1, "gain_db": 0}, "denoise_strength"),
        ({"version": 2, "denoise_strength": 0.5, "gain_db": 0, "remove_dc": True}, "silence_threshold_dbfs"),
        ({"version": 4, "denoise_strength": 0.5, "gain_db": 0}, "version"),
        ({"version": 1, "denoise_strength": 1.2, "gain_db": 0}, "denoise_strength"),
        ({"version": 1, "denoise_strength": 0.5, "gain_db": 25}, "gain_db"),
        (
            {
                "version": 2,
                "denoise_strength": 0.5,
                "gain_db": 0,
                "remove_dc": 1,
                "silence_threshold_dbfs": -50,
            },
            "remove_dc",
        ),
        (
            {
                "version": 3,
                "denoise_strength": 0.5,
                "gain_db": 0,
                "remove_dc": True,
                "silence_threshold_dbfs": -50,
                "channel_merge_mode": "bogus",
                "spectral_subtraction": {
                    "segment_ms": 32,
                    "oversubtraction": 1.5,
                    "spectral_floor": 0.02,
                },
                "loudness_target_lufs": -20,
            },
            "channel_merge_mode",
        ),
        (
            {
                "version": 3,
                "denoise_strength": 0.5,
                "gain_db": 0,
                "remove_dc": True,
                "silence_threshold_dbfs": -50,
                "channel_merge_mode": "preserve",
                "spectral_subtraction": {
                    "segment_ms": 4,
                    "oversubtraction": 1.5,
                    "spectral_floor": 0.02,
                },
                "loudness_target_lufs": -20,
            },
            "segment_ms",
        ),
    ],
)
def test_invalid_recipes(payload: dict, field: str) -> None:
    _invalid(payload, field)


def test_load_recipe_file_and_bad_json(tmp_path: Path) -> None:
    path = tmp_path / "v1.json"
    path.write_text(
        json.dumps({"version": 1, "denoise_strength": 0.3, "gain_db": 1}),
        encoding="utf-8",
    )
    recipe = load_recipe(path)
    assert recipe.version == 1

    bad = tmp_path / "bad.json"
    bad.write_text("{not json", encoding="utf-8")
    with pytest.raises(BusinessError) as caught:
        load_recipe(bad)
    assert caught.value.code == ErrorCode.RECIPE_INVALID
    assert "line" in caught.value.details["location"]
