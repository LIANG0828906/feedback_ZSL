from __future__ import annotations

import numpy as np
import pytest

from edgepipe.config import PipelineConfig
from edgepipe.engine import PipelineEngine
from edgepipe.errors import EdgePipeError, ErrorCode


def test_threshold_order_reports_low_threshold_field():
    with pytest.raises(EdgePipeError) as caught:
        PipelineConfig(low_threshold=40.0, high_threshold=10.0)
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == "low_threshold"


@pytest.mark.parametrize(
    ("kwargs", "field"),
    [
        ({"low_threshold": -1.0}, "low_threshold"),
        ({"high_threshold": 9000.0}, "high_threshold"),
        ({"open_kernel_size": 4}, "open_kernel_size"),
        ({"block_size": 8}, "block_size"),
        ({"max_workers": 9}, "max_workers"),
        ({"gradient_operator": "laplace"}, "gradient_operator"),
    ],
)
def test_parameter_range_and_enum_errors(kwargs, field):
    with pytest.raises(EdgePipeError) as caught:
        PipelineConfig(**kwargs)
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == field


def test_insufficient_band_returns_band_code():
    with pytest.raises(EdgePipeError) as caught:
        PipelineConfig(band_width=1)
    assert caught.value.code == ErrorCode.BAND
    assert caught.value.field == "band_width"
    assert caught.value.details["required"] >= 2


def test_thread_buffer_limit_is_enforced():
    with pytest.raises(EdgePipeError) as caught:
        PipelineConfig(
            block_size=512,
            band_width=100,
            thread_buffer_bytes=1024 * 1024,
        )
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == "thread_buffer_bytes"


def test_image_dtype_and_channel_validation():
    config = PipelineConfig()
    with pytest.raises(EdgePipeError) as caught:
        config.validate_image(np.zeros((8, 8), dtype=np.float32))
    assert caught.value.field == "image.dtype"

    with pytest.raises(EdgePipeError) as caught:
        config.validate_image(np.zeros((8, 8, 2), dtype=np.uint8))
    assert caught.value.field == "image.shape"


@pytest.mark.parametrize(
    "shape",
    [
        (0, 8),
        (8, 0),
        (0, 0),
        (0, 8, 1),
        (8, 0, 1),
        (0, 8, 3),
        (8, 0, 3),
    ],
)
def test_empty_image_dimensions_return_para(shape):
    config = PipelineConfig()
    with pytest.raises(EdgePipeError) as caught:
        config.validate_image(np.zeros(shape, dtype=np.uint8))
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == "image.shape"
    assert caught.value.details["shape"] == list(shape)


@pytest.mark.parametrize("shape", [(0, 8), (8, 0), (0, 8, 3), (8, 0, 3)])
def test_engine_run_rejects_empty_image_before_creating_checkpoints(
    tmp_path, shape
):
    checkpoint_dir = tmp_path / "checkpoints"
    engine = PipelineEngine(PipelineConfig(), checkpoint_dir)
    with pytest.raises(EdgePipeError) as caught:
        engine.run(np.zeros(shape, dtype=np.uint8), mode="full")
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == "image.shape"
    assert caught.value.details["shape"] == list(shape)
    assert not checkpoint_dir.exists()


def test_unknown_mapping_field_returns_para():
    with pytest.raises(EdgePipeError) as caught:
        PipelineConfig.from_mapping({"unknown_parameter": 1})
    assert caught.value.code == ErrorCode.PARA
    assert caught.value.field == "unknown_parameter"

