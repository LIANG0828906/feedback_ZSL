from __future__ import annotations

import math

import numpy as np
import pytest

from lightprobe.bake import Baker
from lightprobe.bake_types import BakeConfig
from lightprobe.directions import make_directions
from lightprobe.errors import ProbeError
from lightprobe.sampling import sample_probe
from lightprobe.sh import project_radiance, real_sh_basis

from conftest import fast_config


def test_real_sh_basis_is_orthonormal():
    directions = make_directions(12000, 3).vectors
    basis = real_sh_basis(directions, 4)
    gram = (4.0 * math.pi / len(directions)) * (basis.T @ basis)
    assert np.max(np.abs(gram - np.eye(25))) < 2.0e-3


@pytest.mark.parametrize("order", [1, 7, True])
def test_invalid_sh_order(room, order):
    with pytest.raises(ProbeError) as caught:
        Baker().bake(room, BakeConfig.from_scene(room, sh_order=order))
    assert caught.value.code == "E_SH_INVALID"


def test_empty_direction_set_and_zero_projection(room):
    with pytest.raises(ProbeError) as caught:
        BakeConfig.from_scene(room, direction_count=0).validated()
    assert caught.value.code == "E_SH_INVALID"
    with pytest.raises(ProbeError) as caught:
        project_radiance(np.empty((0, 3)), np.empty((0, 3)), 3)
    assert caught.value.code == "E_SH_INVALID"
    directions = make_directions(32, 0).vectors
    with pytest.raises(ProbeError) as caught:
        project_radiance(directions, np.zeros((32, 3)), 3)
    assert caught.value.code == "E_SH_INVALID"


def test_each_probe_has_nonzero_coefficients_and_estimates(room):
    result = Baker().bake(room, fast_config(room))
    assert len(result.state.records) == result.report.probe_count
    for record in result.state.records.values():
        assert np.max(np.abs(record.coefficients)) > 0.0
        assert np.all(np.isfinite(record.coefficients))
        assert 0.0 <= record.truncation_error < 100.0
        assert 0.0 <= record.visibility_fraction <= 1.0


def test_direction_change_recomputes_all_and_reports_deviation(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    changed = room.with_updates({"settings": {"direction_count": 48}})
    second = baker.bake(changed, fast_config(changed), base_state=first.state)
    assert second.report.recomputed_count == first.report.probe_count
    assert second.report.reused_count == 0
    assert "direction_set" in second.report.influence["changed_factors"]
    assert second.report.max_coefficient_deviation > 1.0e-6

