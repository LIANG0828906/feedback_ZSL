from __future__ import annotations

import copy

import numpy as np
import pytest

from inverse_imaging.errors import E_REDUCE_DIVERGE, AppError
from inverse_imaging.params import GROUPS
from inverse_imaging.reduction import (
    ReductionTolerance,
    compute_gradients,
    reduce_chunk_results,
)


def test_reduction_invariant_across_parallelism(scene) -> None:
    results = [
        compute_gradients(scene.params, scene, chunk_size=3, parallel=parallel)
        for parallel in (1, 2, 4, 7)
    ]
    reference = results[0]
    for result in results[1:]:
        assert result.loss == reference.loss
        assert result.gradients_sha256 == reference.gradients_sha256
        assert result.reduction_order == reference.reduction_order
        for group in GROUPS:
            np.testing.assert_array_equal(
                result.gradients[group], reference.gradients[group]
            )


def test_reduction_invariant_across_chunk_sizes(scene) -> None:
    reference = compute_gradients(scene.params, scene, chunk_size=2, parallel=1)
    for chunk_size in (3, 4, 6, 16):
        result = compute_gradients(
            scene.params, scene, chunk_size=chunk_size, parallel=3
        )
        assert result.loss == pytest.approx(reference.loss, abs=1e-13, rel=1e-11)
        for group in GROUPS:
            np.testing.assert_allclose(
                result.gradients[group],
                reference.gradients[group],
                atol=1e-12,
                rtol=1e-10,
            )


def test_reduction_reports_first_divergent_chunk(scene) -> None:
    reference = compute_gradients(scene.params, scene, chunk_size=3, parallel=1)
    bad_chunks = copy.deepcopy(reference.chunk_results)
    bad_chunks[2].gradients["camera"][0] += 0.5
    with pytest.raises(AppError) as error:
        reduce_chunk_results(
            bad_chunks,
            chunk_size=3,
            parallel=1,
            reference=reference,
            tolerance=ReductionTolerance(),
        )
    assert error.value.code == E_REDUCE_DIVERGE
    assert error.value.context["chunk_index"] == 2
    assert error.value.context["chunk_coordinate"] == [0, 6, 3, 9]


def test_gradient_matches_finite_difference(scene) -> None:
    result = compute_gradients(scene.params, scene, chunk_size=4, parallel=1)
    key = "sphere.0.radius"
    group = "geometry"
    index = scene.params.keys(group).index(key)
    original = scene.params.values[group][key]
    epsilon = 1.0e-6
    from inverse_imaging.renderer import render_image

    losses = []
    for delta in (epsilon, -epsilon):
        scene.params.values[group][key] = original + delta
        image = render_image(scene.params, scene, chunk_size=4)
        difference = image - scene.target
        losses.append(
            float(np.sum(difference * difference) / difference.size)
        )
    scene.params.values[group][key] = original
    finite_difference = (losses[0] - losses[1]) / (2.0 * epsilon)
    assert result.gradients[group][index] == pytest.approx(
        finite_difference, rel=2e-5, abs=2e-8
    )

