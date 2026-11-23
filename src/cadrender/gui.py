"""PyQt5 专业桌面界面：预览/精确对照、分块矩阵、延迟分位、差异热力图。"""
from __future__ import annotations

import os
from typing import Optional

import numpy as np

from .pipeline import PipelineConfig, RenderPipeline
from .section import Plane


def _to_qimage(arr: np.ndarray):
    from PyQt5.QtGui import QImage
    rgb = (np.clip(arr, 0, 1) * 255).astype(np.uint8)
    h, w = rgb.shape[:2]
    return QImage(rgb.copy().data, w, h, 3 * w, QImage.Format_RGB888)


class MainWindow:
    """延迟导入 Qt，保证无显示环境也能复用核心管线。"""

    def __init__(self, pipeline: RenderPipeline) -> None:
        from PyQt5.QtWidgets import (QWidget, QLabel, QPushButton,
                                     QGridLayout, QVBoxLayout, QHBoxLayout,
                                     QTextEdit)
        from PyQt5.QtGui import QPixmap
        self.QPixmap = QPixmap
        self.pipe = pipeline
        self.win = QWidget()
        self.win.setWindowTitle("CAD 曲面专业渲染演示")
        root = QVBoxLayout(self.win)
        btns = QHBoxLayout()
        self.preview_btn = QPushButton("预览(低LOD)")
        self.precise_btn = QPushButton("精确(高LOD)")
        self.compare_btn = QPushButton("一致性比对")
        self.section_btn = QPushButton("剖面测量")
        self.export_btn = QPushButton("导出帧/报告")
        for b in (self.preview_btn, self.precise_btn, self.compare_btn,
                  self.section_btn, self.export_btn):
            btns.addWidget(b)
        root.addLayout(btns)

        grid = QGridLayout()
        self.preview_lbl = QLabel("预览帧")
        self.precise_lbl = QLabel("精确帧")
        self.heat_lbl = QLabel("差异热力图")
        self.matrix_lbl = QTextEdit()
        self.matrix_lbl.setReadOnly(True)
        for lbl in (self.preview_lbl, self.precise_lbl, self.heat_lbl):
            lbl.setMinimumSize(pipeline.cfg.width, pipeline.cfg.height)
        grid.addWidget(self.preview_lbl, 0, 0)
        grid.addWidget(self.precise_lbl, 0, 1)
        grid.addWidget(self.heat_lbl, 1, 0)
        grid.addWidget(self.matrix_lbl, 1, 1)
        root.addLayout(grid)
        self.log = QTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumHeight(160)
        root.addWidget(self.log)

        self.preview_btn.clicked.connect(self.do_preview)
        self.precise_btn.clicked.connect(self.do_precise)
        self.compare_btn.clicked.connect(self.do_compare)
        self.section_btn.clicked.connect(self.do_section)
        self.export_btn.clicked.connect(self.do_export)
        self._schedule_result = None

    def _set_image(self, lbl, arr) -> None:
        lbl.setPixmap(self.QPixmap.fromImage(_to_qimage(arr)))

    def do_preview(self) -> None:
        frame = self.pipe.render_preview()
        self._set_image(self.preview_lbl, frame.color)
        self.log.append(f"[预览] LOD={frame.params.lod} "
                        f"shade={frame.params.shade_model} "
                        f"v{frame.params.version} coverage={frame.coverage:.4f}")

    def do_precise(self, parallel: bool = True) -> None:
        frame, result = self.pipe.render_precise(parallel=parallel)
        self._schedule_result = result
        self._set_image(self.precise_lbl, frame.color)
        self.matrix_lbl.setPlainText(self._matrix_text(result))
        self.log.append(f"[精确] v{frame.params.version} "
                        f"complete={getattr(frame, 'complete', True)} "
                        f"tiles={len(result.order)} "
                        f"quantiles={result.quantiles()}")

    def do_compare(self) -> None:
        try:
            report = self.pipe.compare()
            self.log.append("[比对] 通过 " + str(report.to_dict()))
        except Exception as exc:
            self.log.append(f"[比对] 失败 {getattr(exc, 'code', '')} {exc}")
        hm = self.pipe.heatmap()
        heat = np.stack([hm, hm * 0.4, 1.0 - hm], axis=-1)
        self._set_image(self.heat_lbl, heat)

    def do_section(self) -> None:
        center = self.pipe.camera.target
        plane = Plane(point=center, normal=np.array([0.0, 0.0, 1.0]))
        res = self.pipe.section(plane)
        if res.annotation:
            self.log.append("[剖面] " + str(res.annotation))
        else:
            self.log.append("[剖面] 未求到交点")

    def do_export(self) -> None:
        out = self.pipe.cfg.export_dir
        if self.pipe.last_preview is not None:
            self.pipe.export_frame_png(self.pipe.last_preview,
                                       "preview.png")
        if self.pipe.last_precise is not None:
            self.pipe.export_frame_png(self.pipe.last_precise,
                                       "precise.png")
        self.log.append(f"[导出] 帧已写入 {out}")

    @staticmethod
    def _matrix_text(result) -> str:
        lines = ["分块状态矩阵 (tile: state/attempts):"]
        for tid in sorted(result.statuses):
            st = result.statuses[tid]
            lines.append(f"  {tid:03d}: {st.state:9s} "
                         f"attempts={st.attempts} wait={st.wait_ms:.2f}ms "
                         f"run={st.run_ms:.2f}ms")
        lines.append("提交序列: " + str(result.order))
        lines.append("排队事件: " + str(result.queue_events[:6]))
        return "\n".join(lines)

    def show(self) -> None:
        self.win.show()


def run_gui(cfg: PipelineConfig) -> int:  # pragma: no cover
    from PyQt5.QtWidgets import QApplication
    import sys
    app = QApplication(sys.argv)
    pipe = RenderPipeline(cfg)
    ui = MainWindow(pipe)
    ui.show()
    return app.exec_()


if __name__ == "__main__":  # pragma: no cover
    import argparse

    ap = argparse.ArgumentParser()
    ap.add_argument("--workdir", default="workdir")
    ap.add_argument("--checkpoint", default="checkpoint")
    ap.add_argument("--export", default="demo_output")
    ap.add_argument("--size", type=int, default=256)
    args = ap.parse_args()
    cfg = PipelineConfig(workdir=args.workdir,
                         checkpoint_dir=args.checkpoint,
                         export_dir=args.export,
                         width=args.size, height=args.size)
    raise SystemExit(run_gui(cfg))
