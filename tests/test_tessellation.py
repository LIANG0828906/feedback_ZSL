"""要求 2：曲面细分、法线、曲率联动、确定性与 E_PARA。"""
import numpy as np
import pytest

from cadrender.errors import E_PARA
from cadrender.loader import load_scene, verify_truth_samples
from cadrender.tessellation import mesh_signature, tessellate


def test_truth_within_tolerance(workdir):
    patches, _, _ = load_scene(workdir)
    for p in patches:
        assert verify_truth_samples(p, 1e-6) <= 1e-6


def test_deterministic_repeat(workdir):
    patches, _, _ = load_scene(workdir)
    a = tessellate(patches[0], 24)
    b = tessellate(patches[0], 24)
    assert mesh_signature(a) == mesh_signature(b)
    np.testing.assert_array_equal(a.vertices, b.vertices)
    np.testing.assert_array_equal(a.normals, b.normals)


def test_normals_unit_and_finite(workdir):
    patches, _, _ = load_scene(workdir)
    m = tessellate(patches[0], 16)
    norms = np.linalg.norm(m.normals, axis=1)
    assert np.all(np.isfinite(m.normals))
    np.testing.assert_allclose(norms, 1.0, atol=1e-9)


def test_density_monotonic_triangles(workdir):
    patches, _, _ = load_scene(workdir)
    counts = [len(tessellate(patches[0], d, adaptive=False).triangles)
              for d in (8, 16, 32)]
    assert all(b >= a for a, b in zip(counts, counts[1:]))


def test_adaptive_density_never_below_base(workdir):
    patches, _, _ = load_scene(workdir)
    m = tessellate(patches[0], 16, adaptive=True)
    assert m.density >= 16


@pytest.mark.parametrize("bad", [3, 999, 1.5, "16", None])
def test_density_out_of_range_e_para(workdir, bad):
    patches, _, _ = load_scene(workdir)
    with pytest.raises(Exception) as ei:
        tessellate(patches[0], bad)
    assert ei.value.code == E_PARA
    assert ei.value.chain[0].field_name == "density"


def test_curvature_positive(workdir):
    patches, _, _ = load_scene(workdir)
    m = tessellate(patches[0], 12)
    assert np.all(m.curvature > 0)
