import numpy as np
import pytest

from spectral_studio import colorimetry as clr
from spectral_studio.errors import ErrorCode, SpectralStudioError
from spectral_studio.spectrum import (import_material, resample_to_grid,
                                      unified_grid)


def _spectral_doc(**overrides):
    doc = {
        "id": "mat-1",
        "kind": "spectral",
        "spectral": {
            "wavelengths_nm": [400.0, 410.0, 420.0, 430.0],
            "interval_nm": 10.0,
            "reflectance": [0.2, 0.4, 0.6, 0.5],
        },
    }
    for key, value in overrides.items():
        doc["spectral"][key] = value
    return doc


def test_unordered_wavelengths_rejected():
    doc = _spectral_doc(wavelengths_nm=[400.0, 420.0, 410.0, 430.0])
    with pytest.raises(SpectralStudioError) as exc:
        import_material(doc)
    assert exc.value.code == ErrorCode.E_SPECTRUM_INVALID


def test_zero_interval_rejected():
    doc = _spectral_doc(interval_nm=0.0)
    with pytest.raises(SpectralStudioError) as exc:
        import_material(doc)
    assert exc.value.code == ErrorCode.E_SPECTRUM_INVALID


def test_out_of_range_reflectance_rejected():
    doc = _spectral_doc(reflectance=[0.2, 1.4, 0.6, 0.5])
    with pytest.raises(SpectralStudioError) as exc:
        import_material(doc)
    assert exc.value.code == ErrorCode.E_SPECTRUM_INVALID


def test_resample_to_unified_grid_and_hash():
    spec = import_material(_spectral_doc())
    grid = unified_grid()
    assert spec.reflectance.shape == grid.shape
    assert spec.content_hash
    # Resampled reference values interpolate the source curve.
    assert spec.reflectance[4] == pytest.approx(0.2)  # 400 nm
    assert spec.reflectance[25] == pytest.approx(0.5)  # 505 nm: holds last value
    # Deterministic: same input, same hash.
    assert import_material(_spectral_doc()).content_hash == spec.content_hash


def test_rgb_material_uplift_roundtrip():
    ref_ill = clr.illuminant_spd("D65")
    ref_cmf = clr.color_matching_functions("1931_2")
    spec = import_material({"id": "rgb-1", "kind": "srgb",
                            "rgb": [0.5, 0.25, 0.75]},
                           ref_ill, ref_cmf)
    assert spec.reflectance.shape == unified_grid().shape
    assert np.all(spec.reflectance >= 0.0) and np.all(spec.reflectance <= 1.0)
    # Uplifted spectrum reproduces the original chromaticity under reference
    # conditions (bounded least squares; luminance may be compressed for
    # saturated colors that no reflectance in [0, 1] can reach).
    xyz = clr.integrate_xyz(spec.reflectance, ref_cmf, ref_ill)
    linear = np.array([0.5, 0.25, 0.75])
    target = clr.rgb_to_xyz_matrix("srgb") @ clr.decode_transfer(linear, "srgb")
    xy = xyz[:2] / xyz.sum()
    target_xy = target[:2] / target.sum()
    assert np.allclose(xy, target_xy, atol=0.1)


def test_rgb_out_of_range_rejected():
    with pytest.raises(SpectralStudioError) as exc:
        import_material({"id": "rgb-2", "kind": "srgb", "rgb": [1.5, 0.0, 0.0]},
                        clr.illuminant_spd("D65"),
                        clr.color_matching_functions("1931_2"))
    assert exc.value.code == ErrorCode.E_SPECTRUM_INVALID
