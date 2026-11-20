"""PyQt6 图形预览界面。

提供三块内容：

* 左侧：目标骨架姿态时间轴播放（2D 投影，随帧刷新）；
* 右侧上：关节映射表（源→目标、匹配方式、回退/冲突标记）；
* 右侧下：逐关节逐帧偏差热力网格（绿色→红色，超差高亮）。

在无显示环境（CI/无头服务器）下，:func:`create_app_offscreen` 可使用
``QT_QPA_PLATFORM=offscreen`` 构建界面做自检，不依赖本地显示器。
"""

from __future__ import annotations

import os

from .mapping import MATCH_NONE, JointMapping
from .model import RetargetResult
from .validate import ValidationReport

try:
    from PyQt6.QtCore import Qt, QTimer
    from PyQt6.QtGui import QColor, QPainter, QPen, QBrush
    from PyQt6.QtWidgets import (
        QApplication,
        QGridLayout,
        QLabel,
        QMainWindow,
        QPushButton,
        QSlider,
        QTableWidget,
        QTableWidgetItem,
        QWidget,
    )
    _QT_AVAILABLE = True
except Exception:  # pragma: no cover - 环境无 Qt 时降级
    _QT_AVAILABLE = False


def create_app_offscreen():
    """在无头环境创建 offscreen QApplication（测试/自检用）。"""

    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    if not _QT_AVAILABLE:
        raise RuntimeError("PyQt6 不可用，无法创建界面")
    app = QApplication.instance() or QApplication([])
    return app


if _QT_AVAILABLE:

    class PoseCanvas(QWidget):
        """目标骨架 2D 投影画布。"""

        def __init__(self, target_doc, result: RetargetResult):
            super().__init__()
            self.target_doc = target_doc
            self.result = result
            self.frame_index = 0
            self.setMinimumSize(420, 420)

        def set_frame(self, index: int) -> None:
            self.frame_index = max(0, min(index, self.result.frame_count - 1))
            self.update()

        def paintEvent(self, event) -> None:  # noqa: N802 (Qt 命名)
            painter = QPainter(self)
            painter.setRenderHint(QPainter.RenderHint.Antialiasing)
            painter.fillRect(self.rect(), QColor(245, 245, 245))
            resolved = [f for f in self.result.frames if f.resolved]
            if not resolved:
                painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "无可播放帧")
                return
            frame = resolved[min(self.frame_index, len(resolved) - 1)]
            # 用全局旋转把绑定偏移投到 2D（取 x/y 平面），构造简单棍图。
            from .math3d import quat_rotate

            positions = [None] * self.target_doc.joint_count
            root_t = frame.root_translation
            for idx in range(self.target_doc.joint_count):
                parent = self.target_doc.joints[idx].parent
                offset = self.target_doc.joints[idx].bind_matrix[:3, 3]
                offset = offset * float(self.target_doc.joints[idx].scale)
                world = quat_rotate(frame.global_rotations[idx], offset)
                if parent is None or parent < 0:
                    positions[idx] = root_t + world
                else:
                    positions[idx] = positions[parent] + world

            scale = min(self.width(), self.height()) / 6.0
            center = self.rect().center()

            def project(point):
                return (
                    center.x() + int(point[0] * scale),
                    center.y() - int(point[1] * scale),
                )

            pen = QPen(QColor(40, 90, 160), 3)
            painter.setPen(pen)
            for idx, joint in enumerate(self.target_doc.joints):
                if joint.parent is None or joint.parent < 0:
                    continue
                a = project(positions[joint.parent])
                b = project(positions[idx])
                painter.drawLine(a[0], a[1], b[0], b[1])
            painter.setBrush(QBrush(QColor(210, 70, 70)))
            for point in positions:
                x, y = project(point)
                painter.drawEllipse(x - 4, y - 4, 8, 8)
            painter.setPen(QPen(QColor(30, 30, 30)))
            painter.drawText(10, 20, f"frame {frame.frame}")

    class MainWindow(QMainWindow):
        def __init__(self, target_doc, result: RetargetResult, mapping: JointMapping,
                     report: ValidationReport):
            super().__init__()
            self.setWindowTitle("骨骼动画重定向预览")
            self.target_doc = target_doc
            self.result = result
            self.mapping = mapping
            self.report = report
            self.timer = QTimer(self)
            self.timer.timeout.connect(self._advance)
            self._build_ui()

        def _build_ui(self) -> None:
            central = QWidget()
            grid = QGridLayout(central)

            self.canvas = PoseCanvas(self.target_doc, self.result)
            grid.addWidget(self.canvas, 0, 0, 3, 1)

            self.slider = QSlider(Qt.Orientation.Horizontal)
            resolved = len(self.result.resolved_frames)
            self.slider.setRange(0, max(0, resolved - 1))
            self.slider.valueChanged.connect(self.canvas.set_frame)
            grid.addWidget(self.slider, 3, 0)

            self.play_btn = QPushButton("播放")
            self.play_btn.clicked.connect(self._toggle_play)
            grid.addWidget(self.play_btn, 4, 0)

            self.table = QTableWidget()
            self._fill_mapping_table()
            grid.addWidget(self.table, 0, 1)

            self.heat = QTableWidget()
            self._fill_heatmap()
            grid.addWidget(self.heat, 1, 1)

            summary = QLabel(
                f"最大偏差关节: {self.report.max_joint} | "
                f"最大偏差: {self.report.max_angle_rad:.3e} rad | "
                f"跳过帧: {len(self.report.skipped_frames)} | "
                f"容差: {self.report.tolerance_rad:.1e}"
            )
            grid.addWidget(summary, 2, 1)
            self.setCentralWidget(central)
            self.resize(980, 720)

        def _fill_mapping_table(self) -> None:
            entries = [e for e in self.mapping.entries]
            self.table.setRowCount(len(entries))
            self.table.setColumnCount(4)
            self.table.setHorizontalHeaderLabels(["源关节", "目标关节", "匹配方式", "说明"])
            for row, entry in enumerate(entries):
                values = [entry.source_name, entry.target_name or "-",
                          entry.match_type, entry.detail]
                for col, value in enumerate(values):
                    item = QTableWidgetItem(str(value))
                    if entry.match_type == MATCH_NONE:
                        item.setBackground(QColor(255, 220, 220))
                    self.table.setItem(row, col, item)
            self.table.resizeColumnsToContents()

        def _fill_heatmap(self) -> None:
            frames = sorted({c.frame for c in self.report.cells})
            joints = sorted({c.target_name for c in self.report.cells})
            self.heat.setRowCount(len(joints))
            self.heat.setColumnCount(len(frames))
            self.heat.setHorizontalHeaderLabels([str(f) for f in frames])
            self.heat.setVerticalHeaderLabels(joints)
            lookup = {(c.target_name, c.frame): c.angle_rad for c in self.report.cells}
            tol = max(self.report.tolerance_rad, 1e-12)
            for row, joint in enumerate(joints):
                for col, frame in enumerate(frames):
                    angle = lookup.get((joint, frame), 0.0)
                    ratio = min(1.0, angle / (tol * 10.0))
                    color = QColor(int(255 * ratio), int(200 * (1 - ratio)), 80)
                    item = QTableWidgetItem(f"{angle:.1e}")
                    item.setBackground(color)
                    self.heat.setItem(row, col, item)

        def _toggle_play(self) -> None:
            if self.timer.isActive():
                self.timer.stop()
                self.play_btn.setText("播放")
            else:
                self.timer.start(80)
                self.play_btn.setText("暂停")

        def _advance(self) -> None:
            resolved = len(self.result.resolved_frames)
            nxt = (self.slider.value() + 1) % max(1, resolved)
            self.slider.setValue(nxt)

    def launch_gui(target_doc, result: RetargetResult, mapping: JointMapping,
                   report: ValidationReport, *, offscreen: bool = False) -> int:
        """启动 GUI 并进入事件循环；``offscreen`` 用于无头自检（立即退出）。"""

        app = create_app_offscreen() if offscreen else QApplication.instance() or QApplication([])
        window = MainWindow(target_doc, result, mapping, report)
        window.show()
        if offscreen:
            # 无头环境不进入阻塞循环，处理一次事件后立即返回，验证可构建性。
            app.processEvents()
            return 0
        return app.exec()

else:  # pragma: no cover
    def launch_gui(*_args, **_kwargs):
        raise RuntimeError("当前环境未安装 PyQt6")
