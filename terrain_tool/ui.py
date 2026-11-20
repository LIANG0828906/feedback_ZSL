"""PySide6 示例界面。

显示高程热力、分块网格、失效高亮、补丁批次列表与离屏渲染预览；
支持应用补丁、调节块尺寸/误差阈值、取消重算。重算在 QThread 后台执行，
主线程不阻塞，同一地形同时只允许一个重算任务（重复启动 E_BUSY）。
"""

from __future__ import annotations

import os

import numpy as np
from PySide6.QtCore import Qt
from PySide6.QtGui import QPixmap
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QHBoxLayout,
    QLabel,
    QListWidget,
    QListWidgetItem,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSpinBox,
    QDoubleSpinBox,
    QVBoxLayout,
    QWidget,
)

from .binary_io import load_heightfield, load_material, load_patch
from .dependency import NodeKind
from .engine import TerrainEngine
from .errors import ErrorCode, TerrainError
from .render import OffscreenRenderer
from .samples import TERRAIN_TYPES, generate_sample
from .worker import RecomputeManager


class TerrainMainWindow(QMainWindow):
    def __init__(self, sample_root: str, snapshot_dir: str) -> None:
        super().__init__()
        self.setWindowTitle("地形增量更新可视化")
        self.sample_root = sample_root
        self.snapshot_dir = snapshot_dir
        self.terrain_type = TERRAIN_TYPES[0]
        self.engine: TerrainEngine | None = None
        self.renderer: OffscreenRenderer | None = None
        self.patch_paths: list[str] = []
        self._next_patch = 0

        self.manager = RecomputeManager()
        self.manager.job_finished.connect(self._on_job_done)
        self.manager.job_failed.connect(self._on_job_failed)
        self.manager.busy_changed.connect(self._on_busy_changed)

        self._build_widgets()
        self._regenerate()

    # ------------------------------------------------------------------ #
    def _build_widgets(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)

        controls = QHBoxLayout()
        self.terrain_combo = QComboBox()
        self.terrain_combo.addItems(TERRAIN_TYPES)
        self.terrain_combo.currentTextChanged.connect(self._on_terrain_changed)
        controls.addWidget(QLabel("地形"))
        controls.addWidget(self.terrain_combo)

        controls.addWidget(QLabel("块尺寸"))
        self.chunk_spin = QSpinBox()
        self.chunk_spin.setRange(4, 64)
        self.chunk_spin.setValue(16)
        self.chunk_spin.setSingleStep(4)
        self.chunk_spin.editingFinished.connect(self._regenerate)
        controls.addWidget(self.chunk_spin)

        controls.addWidget(QLabel("误差阈值"))
        self.threshold_spin = QDoubleSpinBox()
        self.threshold_spin.setRange(0.01, 20.0)
        self.threshold_spin.setValue(1.0)
        self.threshold_spin.setSingleStep(0.1)
        self.threshold_spin.editingFinished.connect(self._regenerate)
        controls.addWidget(self.threshold_spin)

        self.apply_btn = QPushButton("应用下一补丁")
        self.apply_btn.clicked.connect(self._apply_next_patch)
        controls.addWidget(self.apply_btn)
        self.full_btn = QPushButton("全量重算")
        self.full_btn.clicked.connect(self._full_recompute)
        controls.addWidget(self.full_btn)
        self.cancel_btn = QPushButton("取消重算")
        self.cancel_btn.clicked.connect(self._cancel)
        self.cancel_btn.setEnabled(False)
        controls.addWidget(self.cancel_btn)
        layout.addLayout(controls)

        body = QHBoxLayout()
        self.preview = QLabel("渲染预览")
        self.preview.setMinimumSize(380, 320)
        self.preview.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.heat_preview = QLabel("误差热力")
        self.heat_preview.setMinimumSize(380, 320)
        self.heat_preview.setAlignment(Qt.AlignmentFlag.AlignCenter)
        body.addWidget(self.preview)
        body.addWidget(self.heat_preview)

        side = QVBoxLayout()
        self.patch_list = QListWidget()
        side.addWidget(QLabel("补丁批次列表"))
        side.addWidget(self.patch_list)
        self.status_label = QLabel("就绪")
        self.status_label.setWordWrap(True)
        side.addWidget(self.status_label)
        side_widget = QWidget()
        side_widget.setLayout(side)
        side_widget.setFixedWidth(320)
        body.addWidget(side_widget)
        layout.addLayout(body)
        self.setCentralWidget(central)

    # ------------------------------------------------------------------ #
    def _on_terrain_changed(self, text: str) -> None:
        self.terrain_type = text
        self._regenerate()

    def _regenerate(self) -> None:
        out_dir = os.path.join(self.sample_root, self.terrain_type)
        bundle = generate_sample(
            out_dir,
            terrain_type=self.terrain_type,
            chunk_size=int(self.chunk_spin.value()),
        )
        hf = load_heightfield(bundle.height_path)
        mat = load_material(bundle.material_path)
        self.engine = TerrainEngine(
            hf,
            mat,
            chunk_size=int(self.chunk_spin.value()),
            lod_threshold=float(self.threshold_spin.value()),
            crack_tolerance=1e9,
        )
        self.engine.full_rebuild(seq=0)
        self.renderer = OffscreenRenderer(self.engine)
        self.renderer.render_full()
        self.patch_paths = list(bundle.patch_paths)
        self._next_patch = 0
        self.patch_list.clear()
        for path in self.patch_paths:
            self.patch_list.addItem(QListWidgetItem(os.path.basename(path)))
        self._refresh_preview()
        self.status_label.setText(
            f"已生成 {self.terrain_type}：{len(self.engine.chunks)} 个块"
        )

    def _apply_next_patch(self) -> None:
        if self._next_patch >= len(self.patch_paths):
            self.status_label.setText("所有补丁已应用完毕")
            return
        patch_path = self.patch_paths[self._next_patch]
        self._submit(
            kind="incremental",
            job=lambda token, progress: self._incremental_job(patch_path, token, progress),
        )

    def _full_recompute(self) -> None:
        self._submit(
            kind="full",
            job=lambda token, progress: self._full_job(token, progress),
        )

    def _submit(self, kind: str, job) -> None:
        try:
            self.manager.submit(job)
        except TerrainError as exc:
            if exc.code == ErrorCode.E_BUSY:
                QMessageBox.warning(self, "任务忙碌", "已有重算任务运行中（E_BUSY）")
            else:
                QMessageBox.critical(self, "错误", str(exc))

    def _cancel(self) -> None:
        if self.manager.cancel():
            self.status_label.setText("正在取消重算……")

    # ------------------------------------------------------------------ #
    # 后台任务（工作线程执行）
    # ------------------------------------------------------------------ #
    def _incremental_job(self, patch_path: str, token, progress):
        patch = load_patch(patch_path)
        progress(1, 3)
        token.raise_if_cancelled()
        report = self.engine.apply_patch(patch)
        progress(2, 3)
        token.raise_if_cancelled()
        summary = self.renderer.render_incremental(report)
        progress(3, 3)
        return {
            "kind": "incremental",
            "patch": os.path.basename(patch_path),
            "report": report,
            "summary": summary,
        }

    def _full_job(self, token, progress):
        token.raise_if_cancelled()
        report = self.engine.full_rebuild()
        progress(1, 2)
        token.raise_if_cancelled()
        summary = self.renderer.render_full(seq=self.engine.version)
        progress(2, 2)
        return {"kind": "full", "report": report, "summary": summary}

    # ------------------------------------------------------------------ #
    def _on_job_done(self, result: dict) -> None:
        report = result["report"]
        summary = result["summary"]
        if result["kind"] == "incremental":
            self._next_patch += 1
            self.patch_list.item(self._next_patch - 1).setText(
                f"✓ {os.path.basename(self.patch_paths[self._next_patch - 1])}"
            )
        self._refresh_preview(report)
        crack = "E_CRACK_UNRESOLVED" if report.crack else summary.seam_status
        self.status_label.setText(
            f"序号 {summary.seq} | 重算 {summary.invalidated_count} | 复用 "
            f"{summary.reused_count} | 接缝 {crack}\n颜色哈希 {summary.color_hash[:16]}"
        )

    def _on_job_failed(self, exc: TerrainError) -> None:
        if exc.code == ErrorCode.E_CANCELLED:
            # 协作式取消：释放离屏缓冲，保留最后一次已提交快照
            if self.renderer is not None:
                self.renderer.release_buffers()
            self.status_label.setText("重算已取消，离屏缓冲已释放，快照保持不变")
        else:
            self.status_label.setText(f"{exc.code}: {exc.message}")

    def _on_busy_changed(self, busy: bool) -> None:
        self.cancel_btn.setEnabled(busy)
        self.apply_btn.setEnabled(not busy)
        self.full_btn.setEnabled(not busy)

    def _refresh_preview(self, report=None) -> None:
        if self.renderer is None:
            return
        color_img = self.renderer.color_qimage()
        heat_img = self.renderer.heat_qimage()
        if report is not None:
            color_img = self._overlay_chunks(color_img, report)
        self.preview.setPixmap(QPixmap.fromImage(color_img).scaled(
            self.preview.size(), Qt.AspectRatioMode.KeepAspectRatio,
            Qt.TransformationMode.FastTransformation,
        ))
        self.heat_preview.setPixmap(QPixmap.fromImage(heat_img).scaled(
            self.heat_preview.size(), Qt.AspectRatioMode.KeepAspectRatio,
            Qt.TransformationMode.FastTransformation,
        ))

    def _overlay_chunks(self, image, report):
        """在预览上绘制分块网格与失效高亮。"""
        from PySide6.QtGui import QPainter, QColor, QPen
        painter = QPainter(image)
        invalid = set(report.invalidation.invalid[NodeKind.RENDER])
        for (cx, cy), chunk in self.engine.chunks.items():
            if (cx, cy) in invalid:
                painter.fillRect(
                    chunk.x0, chunk.y0, chunk.x1 - chunk.x0, chunk.y1 - chunk.y0,
                    QColor(255, 0, 0, 70),
                )
            painter.setPen(QPen(QColor(255, 255, 255, 120), 1))
            painter.drawRect(
                chunk.x0, chunk.y0, chunk.x1 - chunk.x0, chunk.y1 - chunk.y0
            )
        painter.end()
        return image

    def closeEvent(self, event) -> None:  # noqa: N802 (Qt 命名)
        self.manager.shutdown()
        super().closeEvent(event)


def run_gui(sample_root: str, snapshot_dir: str) -> int:
    import sys

    app = QApplication.instance() or QApplication(sys.argv)
    window = TerrainMainWindow(sample_root, snapshot_dir)
    window.resize(1180, 560)
    window.show()
    return app.exec()
