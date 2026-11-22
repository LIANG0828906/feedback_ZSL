"""Requirement 2: three-plane resampling and window/level mapping."""

import numpy as np
import pytest

from volview.errors import ErrorCode, VolViewError
from volview.model import VolumeModel
from volview.resample import (INTERSECTION_TOL_GRAY, apply_window,
                              plane_shape, render_plane, resample_plane,
                              validate_window)


def _bilinear(image, row, col):
    r0, c0 = int(np.floor(row)), int(np.floor(col))
    r1, c1 = min(r0 + 1, image.shape[0] - 1), min(c0 + 1, image.shape[1] - 1)
    fr, fc = row - r0, col - c0
    return ((1 - fr) * (1 - fc) * image[r0, c0]
            + fr * (1 - fc) * image[r1, c0]
            + (1 - fr) * fc * image[r0, c1]
            + fr * fc * image[r1, c1])


def test_window_width_must_be_positive(model):
    with pytest.raises(VolViewError) as exc:
        validate_window(model, 0.0, 300.0)
    assert exc.value.code == ErrorCode.E_WIN
    assert exc.value.field == "window_width"
    with pytest.raises(VolViewError) as exc:
        validate_window(model, -5.0, 300.0)
    assert exc.value.code == ErrorCode.E_WIN


def test_window_level_must_be_in_declared_range(model):
    lo, hi = model.manifest.gray_range
    with pytest.raises(VolViewError) as exc:
        validate_window(model, 1000.0, hi + 1.0)
    assert exc.value.code == ErrorCode.E_WIN
    assert exc.value.field == "window_level"
    assert exc.value.details["gray_range"] == [lo, hi]
    with pytest.raises(VolViewError):
        validate_window(model, 1000.0, lo - 1.0)


def test_plane_shapes_follow_convention(samples_dir):
    import os
    model_b = VolumeModel.load(os.path.join(samples_dir, "vol_b"))
    # vol_b: axis_order XZY, shape (48, 36, 48) -> X=48, Z=36, Y=48
    assert plane_shape(model_b, "axial") == (48, 48)    # (Y, X)
    assert plane_shape(model_b, "coronal") == (36, 48)  # (Z, X)
    assert plane_shape(model_b, "sagittal") == (36, 48)  # (Z, Y)


def test_planes_intersect_at_volume_center(model):
    cx, cy, cz = model.center_world()
    # calibrated gray at the exact centre, bilinearly sampled per plane
    axial = resample_plane(model, "axial", cz, order=1)
    coronal = resample_plane(model, "coronal", cy, order=1)
    sagittal = resample_plane(model, "sagittal", cx, order=1)
    ga = _bilinear(axial, cy / 1.0, cx / 1.0)
    gc = _bilinear(coronal, cz / 2.5, cx / 1.0)
    gs = _bilinear(sagittal, cz / 2.5, cy / 1.0)
    assert abs(ga - gc) < 1e-6
    assert abs(ga - gs) < 1e-6

    # display gray (order 0 included) agrees within the declared tolerance
    for order in (0, 1):
        da = render_plane(model, "axial", cz, 4000.0, 1000.0, order)
        dc = render_plane(model, "coronal", cy, 4000.0, 1000.0, order)
        ds = render_plane(model, "sagittal", cx, 4000.0, 1000.0, order)
        pa = _bilinear(da.astype(float), cy, cx)
        pc = _bilinear(dc.astype(float), cz / 2.5, cx)
        ps = _bilinear(ds.astype(float), cz / 2.5, cy)
        assert max(pa, pc, ps) - min(pa, pc, ps) <= INTERSECTION_TOL_GRAY


def test_interp_order_switch_keeps_geometry(model):
    pos = 25.0  # exactly on a slice (25 / 2.5 = 10)
    img0 = resample_plane(model, "axial", pos, order=0)
    img1 = resample_plane(model, "axial", pos, order=1)
    assert img0.shape == img1.shape == plane_shape(model, "axial")
    # identical sampling convention: on-slice position -> identical samples
    np.testing.assert_array_equal(img0, img1)


def test_plane_position_out_of_range(model):
    with pytest.raises(VolViewError) as exc:
        render_plane(model, "axial", 1.0e6, 1500.0, 300.0)
    assert exc.value.code == ErrorCode.E_RANGE
    assert exc.value.field == "plane_position"
    assert exc.value.details["axis"] == "Z"


def test_window_mapping_is_deterministic_and_clipped(model):
    gray = resample_plane(model, "axial", 50.0, order=1)
    a = apply_window(gray, 1500.0, 300.0)
    b = apply_window(gray, 1500.0, 300.0)
    np.testing.assert_array_equal(a, b)
    assert a.dtype == np.uint8
    synthetic = np.array([[-2000.0, 300.0], [1050.0, 5000.0]])
    mapped = apply_window(synthetic, 1500.0, 300.0)
    assert mapped[0, 0] == 0      # below window -> clipped to 0
    assert mapped[1, 1] == 255    # above window -> clipped to 255
    assert mapped[0, 1] == 128    # window level -> mid gray
