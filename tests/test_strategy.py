from __future__ import annotations

import pytest

from diffrender.errors import AppError
from diffrender.mode_compare import compare_downsample_modes
from diffrender.strategy import ScaleConfig, choose_strategy


def test_auto_strategy_selects_all_modes():
    exact = choose_strategy(
        object_count=1,
        width=64,
        height=48,
        image_count=2,
        iterations=20,
        config=ScaleConfig(memory_budget_mb=64),
    )
    assert exact.mode == "exact"
    assert exact.downsample_factor == 1

    tiled = choose_strategy(
        object_count=3,
        width=256,
        height=256,
        image_count=4,
        iterations=20,
        config=ScaleConfig(memory_budget_mb=1),
    )
    assert tiled.mode == "tiled"
    assert tiled.buffer_limit_elements > 0

    downsampled = choose_strategy(
        object_count=10,
        width=1024,
        height=768,
        image_count=4,
        iterations=200,
        config=ScaleConfig(memory_budget_mb=8),
    )
    assert downsampled.mode == "downsample"
    assert downsampled.downsample_factor in {2, 4, 8, 16}


def test_strategy_parameters_are_strict():
    with pytest.raises(AppError) as caught:
        choose_strategy(
            object_count=1,
            width=64,
            height=48,
            image_count=1,
            iterations=1,
            config=ScaleConfig(tile_size=1),
        )
    assert caught.value.code == "E_PARA"
    assert caught.value.field_path == "tile_size"


def test_downsample_difference_is_monotonic(dataset):
    task = dataset.task("single")
    report = compare_downsample_modes(task, factors=(1, 2, 4, 8))
    assert report["rows"][0]["mean_mae"] == 0.0
    assert report["monotonic_mae"]
    assert report["monotonic_rmse"]
    assert report["monotonic_non_decreasing"]

