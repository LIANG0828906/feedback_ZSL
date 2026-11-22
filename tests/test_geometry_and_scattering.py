from __future__ import annotations

import math

import numpy as np
import pytest

from transparent_pipeline.errors import E_PARA, PipelineError
from transparent_pipeline.geometry import Hit, refract_direction, trace_medium_path
from transparent_pipeline.renderer import RenderConfig, trace_ray
from transparent_pipeline.scattering import analytic_segment, hg_phase, integrate_segment
from transparent_pipeline.verify import verify_truth


def test_truth_verification(scene, truth):
    report = verify_truth(scene, truth, RenderConfig(step_size=0.05))
    assert report["ok"], report["failures"]
    assert report["max_errors"]["boundary_exit"] <= 1.0e-8
    assert report["max_errors"]["transmittance"] <= 1.0e-10
    assert report["max_errors"]["scattering"] <= 2.0e-3
    assert report["deterministic_repeat"] is True
    assert report["convergence"]["ok"] is True
    assert report["convergence"]["monotonic"] is True


def test_same_ray_bitwise_repeat(scene, truth):
    case = truth["boundary_exit"][0]
    config = RenderConfig(step_size=0.05)
    first = trace_ray(
        scene,
        np.asarray(case["origin"], dtype=np.float64),
        np.asarray(case["direction"], dtype=np.float64),
        int(case["wavelength_index"]),
        config,
    )
    second = trace_ray(
        scene,
        np.asarray(case["origin"], dtype=np.float64),
        np.asarray(case["direction"], dtype=np.float64),
        int(case["wavelength_index"]),
        config,
    )
    assert first["exit_point"].tobytes() == second["exit_point"].tobytes()
    assert first["path_units"] == second["path_units"]
    assert first["optical_units"] == second["optical_units"]
    assert first["transmission"].hex() == second["transmission"].hex()
    assert first["scattering"].hex() == second["scattering"].hex()


def test_total_internal_reflection_branch():
    incident = np.array([math.sin(math.radians(60.0)), math.cos(math.radians(60.0)), 0.0])
    normal = np.array([0.0, 1.0, 0.0])
    direction, tir = refract_direction(incident, normal, 1.5, 1.0)
    assert tir is True
    assert np.allclose(np.linalg.norm(direction), 1.0)
    assert direction[1] < 0.0
    _, transmitted = refract_direction(np.array([0.0, 1.0, 0.0]), normal, 1.5, 1.0)
    assert transmitted is False


def test_invalid_refraction_parameters():
    with pytest.raises(PipelineError) as caught:
        refract_direction(np.array([0.0, 1.0, 0.0]), np.array([0.0, -1.0, 0.0]), 0.0, 1.0)
    assert caught.value.code == E_PARA


def test_internal_tir_bounce_chain():
    medium = {
        "id": "narrow_glass",
        "ior": [1.5],
        "boundary": {"type": "box", "min": [-0.02, -1.0, -0.02], "max": [0.02, 1.0, 0.02]},
    }
    direction = np.array([0.1, -0.995, 0.0], dtype=np.float64)
    direction /= np.linalg.norm(direction)
    origin = np.array([-0.1, 2.0, 0.0], dtype=np.float64)
    entry_t = (1.0 - origin[1]) / direction[1]
    entry_point = origin + entry_t * direction
    entry = Hit(entry_t, entry_point, np.array([0.0, 1.0, 0.0]), "narrow_glass", "box")
    path = trace_medium_path(medium, entry, direction, 0, max_segments=12)
    assert path.complete is True
    assert path.tir_count >= 2
    assert len(path.segments) >= 3
    assert path.exit_point[1] < 0.0


def test_quadrature_convergence_and_analytic_match():
    parameters = {
        "length": 1.37,
        "sigma_s": 0.06,
        "sigma_t": 0.08,
        "radiance": 1.2,
        "phase": 0.08,
    }
    reference = analytic_segment(**parameters)
    errors = []
    for step in (0.2, 0.1, 0.05, 0.025, 0.0125):
        value = integrate_segment(**parameters, step_size=step, strategy="midpoint")
        errors.append(abs(value - reference))
    assert all(errors[index] <= errors[index - 1] + 1.0e-15 for index in range(1, len(errors)))
    assert errors[-1] < 1.0e-8


def test_phase_and_step_parameters():
    assert hg_phase(1.0, 0.2) > hg_phase(-1.0, 0.2)
    with pytest.raises(PipelineError) as caught:
        integrate_segment(
            length=1.0,
            sigma_s=0.1,
            sigma_t=0.2,
            radiance=1.0,
            phase=0.1,
            step_size=-1.0,
        )
    assert caught.value.code == E_PARA
    assert caught.value.field == "render.step_size"
    with pytest.raises(PipelineError) as caught:
        integrate_segment(
            length=1.0,
            sigma_s=-0.1,
            sigma_t=0.2,
            radiance=1.0,
            phase=0.1,
            step_size=0.1,
        )
    assert caught.value.code == E_PARA

