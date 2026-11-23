"""要求 8：剖面测量、双读数与版本冻结。"""
import numpy as np

from cadrender.section import Plane


def test_section_endpoints_dual_readout(pipe):
    pipe.render_precise(parallel=False)
    plane = Plane(point=pipe.camera.target,
                  normal=np.array([0.0, 0.0, 1.0]))
    res = pipe.section(plane)
    assert len(res.points) > 0
    ann = res.annotation
    assert "world" in ann["start"] and "params" in ann["start"]
    assert len(ann["start"]["world"]) == 3
    assert len(ann["start"]["params"]) == 2
    assert ann["world_distance"] >= 0


def test_section_locks_with_frozen_version(pipe):
    pipe.render_precise(parallel=False)
    plane = Plane(point=pipe.camera.target, normal=np.array([1, 0, 0.0]))
    res = pipe.section(plane)
    assert res.frozen is True
    assert res.version == pipe.snapshot.version


def test_plane_signed_distance():
    plane = Plane(point=np.zeros(3), normal=np.array([0, 0, 1.0]))
    assert plane.signed(np.array([0, 0, 2.0])) == 2.0
    assert plane.signed(np.array([0, 0, -2.0])) == -2.0
