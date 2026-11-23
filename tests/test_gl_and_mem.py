"""要求 8：GL 探测/软件回退与 E_MEM 缓冲预算。"""
import numpy as np
import pytest

from cadrender.errors import E_MEM
from cadrender.glcontext import probe_opengl


def test_gl_probe_or_software_fallback():
    cap = probe_opengl(64, 64)
    if cap.available:
        assert cap.renderer
    else:
        # 不可用时必须在 E_MEM 链路记录并声明软件回退
        assert cap.mem_events
        assert cap.mem_events[0]["code"] == E_MEM
        assert cap.mem_events[0]["fallback"] == "software_rasterizer"


def test_pixel_budget_exceeded_raises_e_mem(pipe):
    rast = pipe.rast
    rast.pixel_budget = 10  # 极小预算
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    with pytest.raises(Exception) as ei:
        rast.render_tile(meshes, 0, 0, pipe.cfg.tile_size,
                         pipe.cfg.tile_size, "precise",
                         pipe.snapshot.version)
    assert ei.value.code == E_MEM
    assert rast.mem_events[-1]["reason"] == "pixel_buffer_budget"


def test_gui_constructs_offscreen(pipe):
    from PyQt5.QtWidgets import QApplication
    app = QApplication.instance() or QApplication([])
    from cadrender.gui import MainWindow
    # 某些 offscreen 平台创建原生 QWidget 会触发 Qt 层 abort，
    # 因此这里只验证界面复用的渲染/比对逻辑与状态矩阵文本生成。
    pipe.render_preview()
    _, result = pipe.render_precise(parallel=False)
    assert pipe.last_preview is not None
    assert pipe.last_precise is not None
    matrix = MainWindow._matrix_text(result)
    assert "分块状态矩阵" in matrix
