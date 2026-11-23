"""要求 3/4：双路径渲染与自动一致性比对。"""
import numpy as np
import pytest

from cadrender.compare import compare_frames, diff_heatmap
from cadrender.errors import E_DIVERGE, DivergeError


def test_preview_and_precise_share_camera(pipe):
    preview = pipe.render_preview()
    precise, _ = pipe.render_precise(parallel=False)
    assert preview.camera_sig == precise.camera_sig
    assert preview.params.shade_model == "simple"
    assert precise.params.shade_model == "full"
    assert preview.params.lod < precise.params.lod
    assert precise.params.version == pipe.snapshot.version


def test_compare_records_metrics(pipe):
    pipe.render_preview()
    pipe.render_precise(parallel=False)
    report = pipe.compare()
    assert report.passed is True
    assert report.measured["mismatch_pixel_count"] >= 0


def test_compare_rejects_missing_frame(pipe):
    with pytest.raises(DivergeError) as ei:
        compare_frames(None, None, {"coverage": 0.01,
                                    "bbox_projection_px": 1,
                                    "shade_levels": 1})
    assert ei.value.code == E_DIVERGE


def test_divergence_detected_on_geometry_change(pipe):
    preview = pipe.render_preview()
    # 大幅移动控制点制造几何轮廓差异，再以新版本精确帧比对
    pipe.edit_control_point(pipe.patches[0].name, 0, 0,
                            np.array([2.5, 2.5, 2.5]))
    precise, _ = pipe.render_precise(parallel=False)
    tol = {"coverage": 0.0001, "bbox_projection_px": 0.0,
           "shade_levels": 0}
    with pytest.raises(DivergeError) as ei:
        compare_frames(preview, precise, tol)
    ctx = ei.value.to_dict()["chain"][0]["context"]
    assert "coverage_delta" in ctx
    assert "bbox_delta_px" in ctx
    assert "shade_level_delta" in ctx


def test_heatmap_shape(pipe):
    pipe.render_preview()
    pipe.render_precise(parallel=False)
    hm = pipe.heatmap()
    assert hm.shape == (pipe.cfg.height, pipe.cfg.width)


def test_camera_invalid_planes():
    from cadrender.camera import Camera
    from cadrender.errors import E_PARA
    with pytest.raises(Exception) as ei:
        Camera(eye=np.zeros(3), target=np.array([0, 0, -1]),
               up=(0, 1, 0), fov_y_deg=45, near=10, far=1,
               width=64, height=64)
    assert ei.value.code == E_PARA
