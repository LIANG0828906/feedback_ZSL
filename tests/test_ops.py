from __future__ import annotations

import shutil
from pathlib import Path

import cv2
import numpy as np
import pytest

from edgepipe.config import STAGES, PipelineConfig
from edgepipe.engine import PipelineEngine
from edgepipe.ops import (
    BlockGrid,
    apply_stage,
    prune_stage,
    run_whole_pipeline,
)
from edgepipe.samples import decode_image, sample_image_path, truth_from_sample


def _shape_image(channels: int, shape=(64, 72)) -> np.ndarray:
    height, width = shape
    if channels == 1:
        image = np.full((height, width), 40, dtype=np.uint8)
        color = 220
    else:
        image = np.full((height, width, channels), 40, dtype=np.uint8)
        color = (220, 80, 160)
    cv2.rectangle(image, (12, 10), (50, 40), color, 3)
    cv2.line(image, (8, 55), (64, 50), color, 2)
    return image


@pytest.mark.parametrize("channels", [1, 3])
def test_gradient_preserves_size_channels_and_binary_values(channels):
    image = _shape_image(channels)
    config = PipelineConfig(low_threshold=10.0, high_threshold=30.0)
    output = apply_stage(image, "gradient", config)
    assert output.shape == image.shape
    assert output.dtype == np.uint8
    assert set(np.unique(output)).issubset({0, 255})
    assert np.count_nonzero(output) > 0


def test_singleton_channel_axis_is_preserved():
    image = _shape_image(1)[:, :, None]
    config = PipelineConfig(low_threshold=10.0, high_threshold=30.0)
    output = apply_stage(image, "gradient", config)
    assert output.shape == image.shape


def test_all_stage_outputs_keep_exact_input_size():
    image = _shape_image(3)
    config = PipelineConfig(
        low_threshold=10.0,
        high_threshold=30.0,
        open_kernel_size=1,
        close_kernel_size=3,
        thinning_iterations=2,
        prune_length=4,
    )
    current = image
    for stage in STAGES:
        current = apply_stage(current, stage, config)
        assert current.shape == image.shape
        assert current.dtype == np.uint8


def test_short_component_pruning_removes_only_short_lines():
    mask = np.zeros((48, 64), dtype=np.uint8)
    mask[8, 5:9] = 255
    mask[25, 4:56] = 255
    pruned = prune_stage(mask, max_length=10)
    assert np.count_nonzero(pruned[8, 5:9]) == 0
    assert np.count_nonzero(pruned[25, 4:56]) == 52


def test_chunked_stages_equal_whole_image_stages(
    tmp_path: Path, first_sample, samples_dir: Path
):
    image = decode_image(sample_image_path(first_sample, samples_dir))
    config = PipelineConfig.from_mapping(
        {
            **first_sample["recommended_config"],
            "open_kernel_size": 3,
            "open_iterations": 2,
            "close_kernel_size": 5,
            "close_iterations": 1,
            "thinning_iterations": 4,
            "prune_length": 8,
            "block_size": 32,
            "max_workers": 3,
        }
    )
    checkpoint = tmp_path / "chunked"
    result = PipelineEngine(config, checkpoint).run(
        image, truth=truth_from_sample(first_sample), mode="full"
    )
    whole_mask, whole_stages = run_whole_pipeline(image, config)
    assert result.complete
    for stage in STAGES:
        assert np.array_equal(result.stage_masks[stage], whole_stages[stage]), stage
    assert np.array_equal(result.final_mask, whole_mask)


def test_block_grid_maps_pixels_to_expected_blocks():
    grid = BlockGrid.create(100, 130, 48)
    assert grid.rows == 3
    assert grid.cols == 3
    assert grid.block_at(0, 0).block_id == "b0000"
    assert grid.block_at(49, 50).block_id == "b0004"
    assert grid.block_at(99, 129).block_id == "b0008"

