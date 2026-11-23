from __future__ import annotations

import numpy as np
import pytest

from panoworkbench.config import FeatureConfig, HomographyConfig, PipelineConfig
from panoworkbench.errors import ErrorCode, WorkbenchError
from panoworkbench.state import (
    PairRecord,
    PairResult,
    PairState,
    PanoramaRecord,
    PanoramaState,
)


@pytest.mark.parametrize(
    "config,field",
    [
        (FeatureConfig(max_features=1), "features.max_features"),
        (FeatureConfig(octave_layers=9), "features.octave_layers"),
        (FeatureConfig(contrast_threshold=0.0), "features.contrast_threshold"),
        (HomographyConfig(reproj_threshold=0.0), "homography.reproj_threshold"),
        (HomographyConfig(max_iterations=10), "homography.max_iterations"),
    ],
)
def test_parameter_errors_identify_field(config, field: str) -> None:
    with pytest.raises(WorkbenchError) as context:
        config.validate()
    assert context.value.code is ErrorCode.PARA
    assert context.value.field == field


def _result(pair_id: str = "P0000_0_1") -> PairResult:
    return PairResult(
        pair_id=pair_id,
        image_a=0,
        image_b=1,
        homography_a_to_b=tuple(np.eye(3, dtype=np.float64).reshape(-1)),
        matches=20,
        inliers=15,
        inlier_ratio=0.75,
        residual_median=0.3,
        residual_p95=0.7,
        covariance_condition=12.0,
        content_hash="a" * 64,
        parameter_version="b" * 64,
        algorithm_version="pw-1.0.0",
    )


def test_pair_state_machine_and_committed_immutability() -> None:
    record = PairRecord("P0000_0_1", 0, 1)
    record.begin()
    assert record.state is PairState.REGISTERING
    result = _result()
    record.commit(result)
    assert record.state is PairState.COMMITTED
    assert record.result is result
    with pytest.raises(WorkbenchError) as context:
        record.commit(_result())
    assert context.value.code is ErrorCode.STATE
    assert record.state is PairState.COMMITTED
    assert record.result is result


def test_failed_pair_can_retry_but_illegal_transition_preserves_state() -> None:
    record = PairRecord("P0000_0_1", 0, 1)
    with pytest.raises(WorkbenchError) as context:
        record.fail(WorkbenchError(ErrorCode.MATCH, "bad"))
    assert context.value.code is ErrorCode.STATE
    assert record.state is PairState.PENDING
    record.begin()
    record.fail(WorkbenchError(ErrorCode.MATCH, "bad"))
    assert record.state is PairState.FAILED
    record.begin()
    assert record.state is PairState.REGISTERING


def test_panorama_state_machine() -> None:
    panorama = PanoramaRecord()
    panorama.begin()
    assert panorama.state is PanoramaState.COMPOSITING
    panorama.fail(WorkbenchError(ErrorCode.CLIP, "overflow"))
    assert panorama.state is PanoramaState.FAILED
    panorama.begin()
    panorama.commit({"width": 10, "height": 10})
    assert panorama.state is PanoramaState.COMMITTED
    with pytest.raises(WorkbenchError) as context:
        panorama.begin()
    assert context.value.code is ErrorCode.STATE
    assert panorama.state is PanoramaState.COMMITTED


def test_parameter_version_changes_with_strategy() -> None:
    config = PipelineConfig()
    left = config.parameter_version({"mode": "full", "budget": 5000})
    right = config.parameter_version({"mode": "restricted", "budget": 900})
    assert left != right
