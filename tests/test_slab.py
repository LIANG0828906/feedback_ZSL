"""Requirement 3: slab thickness equivalence and weighted averaging."""

import numpy as np
import pytest

from volview.errors import ErrorCode, VolViewError
from volview.resample import plane_shape, resample_plane
from volview.slab import (high_frequency_energy, max_thickness, render_slab,
                          resample_slab, slab_weights, validate_thickness)


def test_weights_normalised_and_spacing_linked(model):
    pos = model.center_world()[2]
    for thickness in (2.5, 5.0, 10.0, 20.0):
        indices, weights = slab_weights(model, "axial", pos, thickness)
        assert weights.sum() == pytest.approx(1.0)
        assert np.all(weights > 0)
        # window half-width grows with thickness (in units of Z spacing 2.5)
        assert indices.size >= max(1, int(thickness // 2.5))
    # thicker slab -> never fewer contributing slices
    n_small = slab_weights(model, "axial", pos, 5.0)[0].size
    n_large = slab_weights(model, "axial", pos, 15.0)[0].size
    assert n_large >= n_small


def test_thickness_overflow_reports_available_range(model):
    hi = max_thickness(model, "axial")
    assert hi == pytest.approx(39 * 2.5)
    with pytest.raises(VolViewError) as exc:
        validate_thickness(model, "axial", hi + 0.5)
    assert exc.value.code == ErrorCode.E_THICK
    assert exc.value.field == "thickness"
    assert exc.value.details["available"] == [0.0, hi]
    with pytest.raises(VolViewError):
        validate_thickness(model, "axial", -1.0)


def test_zero_thickness_equals_single_slice(model):
    pos = 30.0
    slab = resample_slab(model, "axial", pos, 0.0, order=1)
    single = resample_plane(model, "axial", pos, order=1)
    np.testing.assert_array_equal(slab, single)


def test_slab_keeps_shape_and_never_zero_pads(model):
    pos = model.center_world()[2]
    shape = plane_shape(model, "axial")
    for thickness in (0.0, 2.5, 7.5, 20.0, max_thickness(model, "axial")):
        image = render_slab(model, "axial", pos, thickness, 1500.0, 300.0)
        assert image.shape == shape
        # no synthetic zero padding: borders carry real calibrated signal
        assert image[0, :].any() or image[-1, :].any()
    # off-centre slab near the edge: window clipped, still full-size output
    edge = render_slab(model, "axial", 1.0, 10.0, 1500.0, 300.0)
    assert edge.shape == shape


def test_high_frequency_energy_monotonically_non_increasing(model):
    pos = model.center_world()[2]
    energies = []
    for thickness in (0.0, 2.5, 5.0, 10.0, 20.0, 40.0):
        image = render_slab(model, "axial", pos, thickness, 1500.0, 300.0)
        energies.append(high_frequency_energy(image))
    for earlier, later in zip(energies, energies[1:]):
        assert later <= earlier * (1.0 + 1e-9)


def test_slab_deterministic(model):
    pos = model.center_world()[2]
    a = render_slab(model, "coronal", model.center_world()[1], 8.0, 1500.0, 300.0)
    b = render_slab(model, "coronal", model.center_world()[1], 8.0, 1500.0, 300.0)
    np.testing.assert_array_equal(a, b)
