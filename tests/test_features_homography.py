from __future__ import annotations

import numpy as np
import pytest

from panoworkbench.config import FeatureConfig, HomographyConfig
from panoworkbench.errors import ErrorCode, WorkbenchError
from panoworkbench.features import MatchSet, detect_features, match_features
from panoworkbench.geometry import invert_homography
from panoworkbench.homography import estimate_homography


def _features(dataset, budget: int = 1200):
    config = FeatureConfig().with_budget(budget)
    return {
        index: detect_features(dataset.read_image(index), index, config)
        for index in sorted(dataset.images)
    }, config


def test_feature_budget_and_mutual_matches(sample_dataset) -> None:
    features, config = _features(sample_dataset, budget=900)
    assert all(feature.count <= 900 for feature in features.values())
    matches = match_features(features[0], features[1], config)
    assert matches.count >= config.min_matches
    assert np.all(np.isfinite(matches.points_a))
    assert np.all(np.isfinite(matches.points_b))


def test_minimum_matches_error_is_e_match(sample_dataset) -> None:
    features, _ = _features(sample_dataset, budget=900)
    config = FeatureConfig(min_matches=5000)
    with pytest.raises(WorkbenchError) as context:
        match_features(features[0], features[1], config)
    assert context.value.code is ErrorCode.MATCH
    assert context.value.field == "features.min_matches"


def test_homography_accuracy_and_metrics(sample_dataset) -> None:
    features, feature_config = _features(sample_dataset, budget=1800)
    matches = match_features(features[0], features[1], feature_config)
    truth = invert_homography(sample_dataset.images[1].homography)
    config = HomographyConfig()
    estimate = estimate_homography(
        matches,
        (features[0].image_size[0], features[0].image_size[1]),
        config,
        pair_key="truth-check",
        truth_homography=truth,
    )
    assert estimate.truth_ok is True
    assert estimate.truth_corner_error is not None
    assert estimate.truth_corner_error <= config.truth_corner_tolerance
    assert estimate.inliers >= config.min_inliers
    assert np.isfinite(estimate.covariance_condition)
    assert estimate.covariance_condition > 1
    assert estimate.residual_median <= estimate.residual_p95


def test_tighter_reprojection_tolerance_does_not_increase_residual(
    sample_dataset,
) -> None:
    features, feature_config = _features(sample_dataset, budget=1800)
    matches = match_features(features[0], features[1], feature_config)
    truth = invert_homography(sample_dataset.images[1].homography)
    medians = []
    inliers = []
    for threshold in (8.0, 6.0, 4.0, 2.0, 1.0, 0.5):
        estimate = estimate_homography(
            matches,
            (features[0].image_size[0], features[0].image_size[1]),
            HomographyConfig(reproj_threshold=threshold),
            pair_key="monotonic",
            truth_homography=truth,
        )
        medians.append(estimate.residual_median)
        inliers.append(estimate.inliers)
    assert all(
        medians[index] >= medians[index + 1] - 1e-12
        for index in range(len(medians) - 1)
    )
    assert all(
        inliers[index] >= inliers[index + 1]
        for index in range(len(inliers) - 1)
    )


def test_insufficient_inliers_is_e_homo(sample_dataset) -> None:
    features, feature_config = _features(sample_dataset, budget=900)
    matches = match_features(features[0], features[1], feature_config)
    with pytest.raises(WorkbenchError) as context:
        estimate_homography(
            matches,
            (features[0].image_size[0], features[0].image_size[1]),
            HomographyConfig(min_inliers=matches.count + 10),
            pair_key="too-few-inliers",
        )
    assert context.value.code is ErrorCode.HOMO
    assert context.value.field == "homography.min_inliers"


def _match_set(points_a: np.ndarray, points_b: np.ndarray) -> MatchSet:
    count = len(points_a)
    return MatchSet(
        points_a=np.asarray(points_a, dtype=np.float64),
        points_b=np.asarray(points_b, dtype=np.float64),
        source_indices=np.arange(count, dtype=np.int32),
        target_indices=np.arange(count, dtype=np.int32),
        distances=np.ones(count, dtype=np.float64),
    )


@pytest.mark.parametrize(
    "points_a,points_b",
    [
        (
            np.column_stack(
                [np.linspace(10.0, 200.0, 24), np.linspace(20.0, 410.0, 24)]
            ),
            np.column_stack(
                [np.linspace(15.0, 205.0, 24), np.linspace(22.0, 412.0, 24)]
            ),
        ),
        (
            np.repeat(np.array([[80.0, 60.0], [120.0, 90.0]]), 12, axis=0),
            np.repeat(np.array([[90.0, 70.0], [130.0, 100.0]]), 12, axis=0),
        ),
        (
            np.full((20, 2), [100.0, 100.0]),
            np.full((20, 2), [110.0, 120.0]),
        ),
    ],
)
def test_degenerate_ransac_samples_are_e_homo(points_a, points_b) -> None:
    config = HomographyConfig(max_iterations=120, min_inliers=4)
    with pytest.raises(WorkbenchError) as context:
        estimate_homography(
            _match_set(points_a, points_b),
            (500, 400),
            config,
            pair_key="degenerate",
        )
    assert context.value.code is ErrorCode.HOMO
