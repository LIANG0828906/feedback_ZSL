import numpy as np
import pytest

from spectral_studio import colorimetry as clr
from spectral_studio.errors import ErrorCode, SpectralStudioError
from spectral_studio.pipeline import (PipelineEnvironment, SharedCaches,
                                      run_chain)
from spectral_studio.spectrum import import_material


def _spec():
    return import_material({
        "id": "m", "kind": "spectral",
        "spectral": {
            "wavelengths_nm": [float(v) for v in range(380, 785, 5)],
            "interval_nm": 5.0,
            "reflectance": [0.5] * 81,
        },
    })


def test_chain_intermediates_and_error_summaries():
    env = PipelineEnvironment()
    result = run_chain(_spec(), env, SharedCaches())
    assert set(result["encoded"]) == {"srgb", "display-p3"}
    inter = result["intermediates"]
    assert inter["precision"] == 1e-12
    assert inter["integration"]["abs_error"][1] < 1e-3
    assert inter["adaptation"]["matrix_inverse_residual"] < 1e-9
    for space in ("srgb", "display-p3"):
        summary = inter["transform"][space]
        assert summary["encode_roundtrip_error"] < 1e-9
        assert all(0.0 <= v <= 1.0 for v in result["encoded"][space])
    # 0.5 flat reflectance under D65 -> mid gray, Y ~= 0.5.
    assert result["xyz"][1] == pytest.approx(0.5, abs=1e-6)


def test_unknown_target_space_aborts():
    with pytest.raises(SpectralStudioError) as exc:
        PipelineEnvironment(targets=("srgb", "not-a-space"))
    assert exc.value.code == ErrorCode.E_COLOR_INVALID


def test_non_finite_tristimulus_aborts_without_version():
    spec = _spec()
    spec.reflectance = spec.reflectance.copy()
    spec.reflectance[10] = np.nan
    with pytest.raises(SpectralStudioError) as exc:
        run_chain(spec, PipelineEnvironment(), SharedCaches())
    assert exc.value.code == ErrorCode.E_COLOR_INVALID


def test_adaptation_strategies_change_result():
    spec = import_material({
        "id": "m2", "kind": "spectral",
        "spectral": {
            "wavelengths_nm": [float(v) for v in range(380, 785, 5)],
            "interval_nm": 5.0,
            # Non-neutral spectrum: adaptation strategies must diverge.
            "reflectance": [0.1 + 0.8 * (i % 7) / 6.0 for i in range(81)],
        },
    })
    env_a = PipelineEnvironment(white_point="D50", adaptation="bradford")
    env_b = PipelineEnvironment(white_point="D50", adaptation="von_kries")
    env_c = PipelineEnvironment(white_point="D50", adaptation="none")
    caches = SharedCaches()
    out_a = run_chain(spec, env_a, caches)["encoded"]["srgb"]
    out_b = run_chain(spec, env_b, caches)["encoded"]["srgb"]
    out_c = run_chain(spec, env_c, caches)["encoded"]["srgb"]
    assert not np.allclose(out_a, out_b)
    assert not np.allclose(out_a, out_c)


def test_shared_cache_keyed_by_content_hash():
    caches = SharedCaches()
    env = PipelineEnvironment()
    run_chain(_spec(), env, caches)
    assert caches.stats["cmf_miss"] == 1
    run_chain(_spec(), env, caches)
    assert caches.stats["cmf_hit"] == 1
    # White point change reuses the CMF cache but recomputes adaptation.
    env2 = PipelineEnvironment(white_point="D50")
    run_chain(_spec(), env2, caches)
    assert caches.stats["cmf_hit"] == 2
    assert caches.stats["adapt_miss"] == 2
