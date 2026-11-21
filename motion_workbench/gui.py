from __future__ import annotations

import json
import math
import time
from pathlib import Path
from typing import Any

from PySide6 import QtCore, QtGui, QtWidgets

from .bvh import BVHData, load_bvh
from .constraints import ConstraintSet, load_constraints
from .errors import WorkbenchError
from .generator import SampleSpec, generate_sample
from .session import WorkbenchSession, resume_session


def _residual_color(ratio: float) -> QtGui.QColor:
    ratio = max(0.0, min(1.0, ratio))
    if ratio < 0.5:
        hue = 120 - int(120 * (ratio / 0.5))
    else:
        hue = 0
    return QtGui.QColor.fromHsv(hue, 220, 235)


class SkeletonViewport(QtWidgets.QWidget):
    def __init__(self, parent=None):
        super().__init__(parent)
        self.setMinimumSize(520, 420)
        self.skeleton: BVHData | None = None
        self.frame_positions: list[tuple[float, float, float]] = []
        self.end_positions: list[tuple[float, float, float]] = []
        self.joint_residuals: list[float] = []
        self.frame = 0
        self.total_residual = 0.0
        self.yaw = -0.55
        self.pitch = 0.18
        self.zoom = 1.0
        self._drag_origin = None

    def set_frame(
        self,
        skeleton: BVHData,
        frame: int,
        positions,
        end_positions,
        joint_residuals,
        total_residual: float,
    ) -> None:
        self.skeleton = skeleton
        self.frame = frame
        self.frame_positions = list(positions)
        self.end_positions = list(end_positions)
        self.joint_residuals = list(joint_residuals)
        self.total_residual = total_residual
        self.update()

    def mousePressEvent(self, event):
        if event.button() == QtCore.Qt.MouseButton.LeftButton:
            self._drag_origin = event.position()

    def mouseMoveEvent(self, event):
        if self._drag_origin is None:
            return
        delta = event.position() - self._drag_origin
        self._drag_origin = event.position()
        self.yaw += delta.x() * 0.008
        self.pitch = max(-1.2, min(1.2, self.pitch + delta.y() * 0.008))
        self.update()

    def mouseReleaseEvent(self, event):
        self._drag_origin = None

    def wheelEvent(self, event):
        self.zoom *= 1.0015 ** event.angleDelta().y()
        self.zoom = max(0.3, min(5.0, self.zoom))
        self.update()

    def _project(self, point):
        cy = math.cos(self.yaw)
        sy = math.sin(self.yaw)
        cp = math.cos(self.pitch)
        sp = math.sin(self.pitch)
        x, y, z = point
        xr = cy * x + sy * z
        zr = -sy * x + cy * z
        yr = cp * y - sp * zr
        zr = sp * y + cp * zr
        scale = 155.0 * self.zoom / (1.0 + 0.04 * max(0.0, zr))
        return QtCore.QPointF(
            self.width() * 0.5 + xr * scale,
            self.height() * 0.58 - yr * scale,
        ), zr

    def paintEvent(self, event):
        painter = QtGui.QPainter(self)
        painter.setRenderHint(QtGui.QPainter.RenderHint.Antialiasing)
        gradient = QtGui.QLinearGradient(0, 0, 0, self.height())
        gradient.setColorAt(0.0, QtGui.QColor("#111827"))
        gradient.setColorAt(1.0, QtGui.QColor("#05070c"))
        painter.fillRect(self.rect(), gradient)

        painter.setPen(QtGui.QPen(QtGui.QColor(40, 52, 70), 1))
        center = QtCore.QPointF(self.width() * 0.5, self.height() * 0.68)
        for index in range(-6, 7):
            start, _ = self._project((index * 0.4, -1.8, -2.4))
            end, _ = self._project((index * 0.4, -1.8, 2.4))
            painter.drawLine(start, end)
            start, _ = self._project((-2.4, -1.8, index * 0.4))
            end, _ = self._project((2.4, -1.8, index * 0.4))
            painter.drawLine(start, end)

        if self.skeleton is None or not self.frame_positions:
            painter.setPen(QtGui.QColor("#8ca3c7"))
            painter.setFont(QtGui.QFont("Segoe UI", 14))
            painter.drawText(
                self.rect(),
                QtCore.Qt.AlignmentFlag.AlignCenter,
                "载入 BVH 或生成样本以开始",
            )
            return

        projections = [self._project(point) for point in self.frame_positions]
        for index, joint in enumerate(self.skeleton.joints):
            if joint.parent_index < 0:
                continue
            parent = projections[joint.parent_index][0]
            child = projections[index][0]
            ratio = (
                self.joint_residuals[index]
                if index < len(self.joint_residuals)
                else 0.0
            )
            color = _residual_color(ratio)
            painter.setPen(QtGui.QPen(color, 4.0))
            painter.drawLine(parent, child)
        for index, (point, _) in enumerate(projections):
            ratio = (
                self.joint_residuals[index]
                if index < len(self.joint_residuals)
                else 0.0
            )
            color = _residual_color(ratio)
            radius = 5.5 if self.skeleton.joints[index].parent_index >= 0 else 7.0
            painter.setBrush(color)
            painter.setPen(QtGui.QPen(QtGui.QColor("#dce8ff"), 1.0))
            painter.drawEllipse(point, radius, radius)
        for end_point in self.end_positions:
            point, _ = self._project(end_point)
            painter.setBrush(QtGui.QColor("#f5f7ff"))
            painter.setPen(QtCore.Qt.PenStyle.NoPen)
            painter.drawEllipse(point, 3.0, 3.0)

        painter.setPen(QtGui.QColor("#cfe2ff"))
        painter.setFont(QtGui.QFont("Segoe UI", 10))
        painter.drawText(
            18,
            28,
            f"Frame {self.frame:04d}   residual {self.total_residual:.5f}",
        )
        painter.setPen(QtGui.QColor("#7288a8"))
        painter.drawText(
            18,
            self.height() - 18,
            "Drag: orbit   Wheel: zoom   Green→red: residual heat",
        )


class ResidualHeatmap(QtWidgets.QWidget):
    frameSelected = QtCore.Signal(int)

    def __init__(self, parent=None):
        super().__init__(parent)
        self.setMinimumHeight(72)
        self.values: list[float] = []
        self.frame = 0

    def set_values(self, values: list[float], frame: int = 0) -> None:
        self.values = values
        self.frame = frame
        self.update()

    def paintEvent(self, event):
        painter = QtGui.QPainter(self)
        painter.fillRect(self.rect(), QtGui.QColor("#0b1019"))
        if not self.values:
            painter.setPen(QtGui.QColor("#64748b"))
            painter.drawText(
                self.rect(),
                QtCore.Qt.AlignmentFlag.AlignCenter,
                "Per-frame residual heat",
            )
            return
        count = len(self.values)
        width = max(1.0, self.width() / count)
        maximum = max(self.values) or 1.0
        for index, value in enumerate(self.values):
            ratio = value / maximum if maximum > 0 else 0.0
            color = _residual_color(ratio)
            height = 18 + 42 * min(1.0, ratio)
            painter.fillRect(
                QtCore.QRectF(
                    index * width,
                    self.height() - height,
                    max(1.0, width - 1.0),
                    height,
                ),
                color,
            )
        playhead = (self.frame + 0.5) * width
        painter.setPen(QtGui.QPen(QtGui.QColor("#f8fafc"), 2))
        painter.drawLine(
            QtCore.QPointF(playhead, 0),
            QtCore.QPointF(playhead, self.height()),
        )

    def mousePressEvent(self, event):
        if not self.values:
            return
        frame = int(
            max(0, min(len(self.values) - 1, event.position().x() / self.width() * len(self.values)))
        )
        self.frameSelected.emit(frame)


class SolverWorker(QtCore.QThread):
    progress = QtCore.Signal(dict)
    finished_report = QtCore.Signal(dict)
    failed = QtCore.Signal(dict)

    def __init__(self, session: WorkbenchSession, cancel_event, pause_event, chunk_size, run_id):
        super().__init__()
        self.session = session
        self.cancel_event = cancel_event
        self.pause_event = pause_event
        self.chunk_size = chunk_size
        self.run_id = run_id

    def run(self):
        try:
            report = self.session.solve(
                run_id=self.run_id,
                cancel_event=self.cancel_event,
                pause_event=self.pause_event,
                chunk_size=self.chunk_size,
                progress_callback=self.progress.emit,
            )
            self.finished_report.emit(report.to_dict())
        except WorkbenchError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:  # UI boundary: always surface a report
            self.failed.emit(
                {
                    "ok": False,
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "details": {},
                }
            )


class MainWindow(QtWidgets.QMainWindow):
    def __init__(self):
        super().__init__()
        self.setWindowTitle("Motion Workbench · deterministic incremental IK")
        self.resize(1240, 820)
        self.session: WorkbenchSession | None = None
        self.bvh: BVHData | None = None
        self.constraints: ConstraintSet | None = None
        self.frames: dict[int, Any] = {}
        self.frame_positions: dict[int, Any] = {}
        self.joint_heat: dict[int, list[float]] = {}
        self.frame_residuals: list[float] = []
        self.current_frame = 0
        self.worker: SolverWorker | None = None
        import threading

        self.cancel_event = threading.Event()
        self.pause_event = threading.Event()
        self.play_timer = QtCore.QTimer(self)
        self.play_timer.timeout.connect(self._advance_playback)
        self._build_ui()
        self._apply_dark_style()

    def _build_ui(self):
        central = QtWidgets.QWidget()
        root_layout = QtWidgets.QVBoxLayout(central)
        root_layout.setContentsMargins(10, 10, 10, 10)
        splitter = QtWidgets.QSplitter(QtCore.Qt.Orientation.Horizontal)
        root_layout.addWidget(splitter, 1)

        left = QtWidgets.QWidget()
        left_layout = QtWidgets.QVBoxLayout(left)
        left_layout.setContentsMargins(0, 0, 0, 0)
        self.viewport = SkeletonViewport()
        left_layout.addWidget(self.viewport, 1)
        self.heatmap = ResidualHeatmap()
        self.heatmap.frameSelected.connect(self.set_frame)
        left_layout.addWidget(self.heatmap)
        timeline_row = QtWidgets.QHBoxLayout()
        self.timeline = QtWidgets.QSlider(QtCore.Qt.Orientation.Horizontal)
        self.timeline.valueChanged.connect(self.set_frame)
        self.frame_label = QtWidgets.QLabel("0 / 0")
        self.play_button = QtWidgets.QPushButton("播放")
        self.play_button.setCheckable(True)
        self.play_button.clicked.connect(self.toggle_playback)
        timeline_row.addWidget(self.timeline, 1)
        timeline_row.addWidget(self.frame_label)
        timeline_row.addWidget(self.play_button)
        left_layout.addLayout(timeline_row)
        splitter.addWidget(left)

        right = QtWidgets.QWidget()
        right.setMinimumWidth(390)
        right_layout = QtWidgets.QVBoxLayout(right)
        right_layout.setContentsMargins(0, 0, 0, 0)

        load_group = QtWidgets.QGroupBox("工作区")
        load_layout = QtWidgets.QGridLayout(load_group)
        self.bvh_edit = QtWidgets.QLineEdit()
        self.constraints_edit = QtWidgets.QLineEdit()
        self.session_edit = QtWidgets.QLineEdit()
        bvh_button = QtWidgets.QPushButton("载入 BVH")
        constraints_button = QtWidgets.QPushButton("载入约束")
        session_button = QtWidgets.QPushButton("会话目录")
        bvh_button.clicked.connect(self.choose_bvh)
        constraints_button.clicked.connect(self.choose_constraints)
        session_button.clicked.connect(self.choose_session)
        load_layout.addWidget(bvh_button, 0, 0)
        load_layout.addWidget(self.bvh_edit, 0, 1)
        load_layout.addWidget(constraints_button, 1, 0)
        load_layout.addWidget(self.constraints_edit, 1, 1)
        load_layout.addWidget(session_button, 2, 0)
        load_layout.addWidget(self.session_edit, 2, 1)
        self.generate_button = QtWidgets.QPushButton("生成确定性样本")
        self.generate_button.clicked.connect(self.generate_demo)
        load_layout.addWidget(self.generate_button, 3, 0, 1, 2)
        right_layout.addWidget(load_group)

        controls = QtWidgets.QGroupBox("求解控制")
        control_layout = QtWidgets.QGridLayout(controls)
        self.start_button = QtWidgets.QPushButton("启动增量求解")
        self.pause_button = QtWidgets.QPushButton("暂停")
        self.cancel_button = QtWidgets.QPushButton("协作式取消")
        self.resume_button = QtWidgets.QPushButton("从磁盘快照恢复")
        self.start_button.clicked.connect(lambda: self.start_solve(False))
        self.pause_button.clicked.connect(self.toggle_pause)
        self.cancel_button.clicked.connect(self.cancel_solve)
        self.resume_button.clicked.connect(lambda: self.start_solve(True))
        control_layout.addWidget(self.start_button, 0, 0)
        control_layout.addWidget(self.pause_button, 0, 1)
        control_layout.addWidget(self.cancel_button, 1, 0)
        control_layout.addWidget(self.resume_button, 1, 1)
        self.status_label = QtWidgets.QLabel("就绪")
        self.status_label.setWordWrap(True)
        control_layout.addWidget(self.status_label, 2, 0, 1, 2)
        right_layout.addWidget(controls)

        constraint_group = QtWidgets.QGroupBox("约束权重 / 容差")
        constraint_layout = QtWidgets.QVBoxLayout(constraint_group)
        self.table = QtWidgets.QTableWidget(0, 5)
        self.table.setHorizontalHeaderLabels(
            ["ID", "类型", "关节", "权重", "容差"]
        )
        self.table.horizontalHeader().setSectionResizeMode(
            0, QtWidgets.QHeaderView.ResizeMode.Stretch
        )
        self.table.verticalHeader().setVisible(False)
        constraint_layout.addWidget(self.table, 1)
        apply_button = QtWidgets.QPushButton("应用参数并规划增量区间")
        apply_button.clicked.connect(self.apply_constraint_edits)
        constraint_layout.addWidget(apply_button)
        right_layout.addWidget(constraint_group, 1)

        self.log = QtWidgets.QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(500)
        right_layout.addWidget(self.log, 1)
        splitter.addWidget(right)
        splitter.setStretchFactor(0, 3)
        splitter.setStretchFactor(1, 2)
        self.setCentralWidget(central)

    def _apply_dark_style(self):
        self.setStyleSheet(
            """
            QWidget { background: #111827; color: #d8e2f0; font-family: 'Segoe UI'; font-size: 10pt; }
            QGroupBox { border: 1px solid #263449; border-radius: 6px; margin-top: 10px; padding-top: 8px; }
            QGroupBox::title { subcontrol-origin: margin; left: 10px; color: #8fb4e8; }
            QPushButton { background: #1f2b3d; border: 1px solid #33465f; border-radius: 5px; padding: 6px 9px; }
            QPushButton:hover { background: #293b52; }
            QPushButton:disabled { color: #667085; background: #17202d; }
            QLineEdit, QPlainTextEdit, QTableWidget { background: #0b111b; border: 1px solid #263449; border-radius: 4px; }
            QHeaderView::section { background: #1a2534; border: 0; padding: 4px; }
            QSlider::groove:horizontal { height: 5px; background: #263449; border-radius: 2px; }
            QSlider::handle:horizontal { width: 14px; margin: -5px 0; border-radius: 7px; background: #60a5fa; }
            """
        )

    def _log(self, text: str):
        self.log.appendPlainText(time.strftime("%H:%M:%S") + "  " + text)

    def choose_bvh(self):
        path, _ = QtWidgets.QFileDialog.getOpenFileName(
            self, "载入 BVH", "", "BVH (*.bvh);;All files (*)"
        )
        if not path:
            return
        self.bvh_edit.setText(path)
        if not self.session_edit.text():
            self.session_edit.setText(
                str(Path(path).with_suffix("")) + ".session"
            )
        self.load_bvh_file()

    def choose_constraints(self):
        path, _ = QtWidgets.QFileDialog.getOpenFileName(
            self, "载入约束", "", "JSON (*.json);;All files (*)"
        )
        if path:
            self.constraints_edit.setText(path)
            self.load_constraint_file()

    def choose_session(self):
        path = QtWidgets.QFileDialog.getExistingDirectory(
            self, "选择会话目录"
        )
        if path:
            self.session_edit.setText(path)

    def load_bvh_file(self):
        try:
            self.bvh = load_bvh(self.bvh_edit.text())
            self.session = None
            self.status_label.setText(
                f"BVH: {len(self.bvh.joints)} joints × {self.bvh.frame_count} frames"
            )
            self.timeline.setMaximum(max(0, self.bvh.frame_count - 1))
            if self.constraints_edit.text():
                self.load_constraint_file()
            self._preview_base_pose()
        except WorkbenchError as exc:
            self._error(exc)

    def load_constraint_file(self):
        if self.bvh is None:
            return
        try:
            self.constraints = load_constraints(
                self.constraints_edit.text(), self.bvh
            )
            self._populate_table()
            self.status_label.setText(
                f"{len(self.constraints.constraints)} constraints loaded"
            )
        except WorkbenchError as exc:
            self._error(exc)

    def _session(self) -> WorkbenchSession | None:
        if (
            self.bvh is None
            or self.constraints is None
            or not self.bvh_edit.text()
            or not self.constraints_edit.text()
            or not self.session_edit.text()
        ):
            self._log("请先载入 BVH、约束并选择会话目录。")
            return None
        if (
            self.session is None
            or Path(self.session.session_dir) != Path(self.session_edit.text())
            or self.session.bvh.content_digest != self.bvh.content_digest
        ):
            self.session = WorkbenchSession(
                self.bvh_edit.text(),
                self.constraints_edit.text(),
                self.session_edit.text(),
                constraints=self.constraints,
            )
        else:
            self.session.set_constraints(self.constraints)
        return self.session

    def _populate_table(self):
        if self.constraints is None:
            return
        self.table.setRowCount(0)
        for constraint in self.constraints.constraints:
            row = self.table.rowCount()
            self.table.insertRow(row)
            values = [
                constraint.id,
                constraint.type,
                constraint.joint,
                str(constraint.weight),
                str(constraint.tolerance),
            ]
            for column, value in enumerate(values):
                item = QtWidgets.QTableWidgetItem(value)
                if column < 3:
                    item.setFlags(item.flags() & ~QtCore.Qt.ItemFlag.ItemIsEditable)
                self.table.setItem(row, column, item)

    def apply_constraint_edits(self):
        if self.constraints is None:
            return
        updated = self.constraints
        try:
            for row in range(self.table.rowCount()):
                constraint_id = self.table.item(row, 0).text()
                weight = float(self.table.item(row, 3).text())
                tolerance = float(self.table.item(row, 4).text())
                constraint = updated.by_id.get(constraint_id)
                if constraint is None:
                    continue
                if weight != constraint.weight or tolerance != constraint.tolerance:
                    updated = updated.with_constraint(
                        constraint.with_changes(
                            weight=weight, tolerance=tolerance
                        )
                    )
            updated.validate(self.bvh)
            self.constraints = updated
            if self.session is not None:
                self.session.set_constraints(updated)
            self.status_label.setText("参数已应用；启动求解以计算增量区间")
            self._log("约束参数已应用。")
        except (ValueError, WorkbenchError) as exc:
            if isinstance(exc, WorkbenchError):
                self._error(exc)
            else:
                self._log(f"参数错误: {exc}")

    def _preview_base_pose(self):
        if self.bvh is None:
            return
        self.frames = {}
        self.frame_positions = {}
        self.joint_heat = {}
        self.frame_residuals = [0.0] * self.bvh.frame_count
        for frame in range(self.bvh.frame_count):
            transform = self.bvh.evaluate(frame)
            self.frame_positions[frame] = transform
            self.joint_heat[frame] = [0.0] * len(self.bvh.joints)
        self.heatmap.set_values(self.frame_residuals, 0)
        self.set_frame(0)

    def set_frame(self, frame: int):
        if self.bvh is None:
            return
        frame = max(0, min(self.bvh.frame_count - 1, int(frame)))
        self.current_frame = frame
        self.timeline.blockSignals(True)
        self.timeline.setValue(frame)
        self.timeline.blockSignals(False)
        self.heatmap.frame = frame
        self.heatmap.update()
        transform = self.frame_positions.get(frame)
        if transform is None:
            transform = self.bvh.evaluate(frame)
            self.frame_positions[frame] = transform
        heat = self.joint_heat.get(frame, [0.0] * len(self.bvh.joints))
        residual = (
            self.frame_residuals[frame]
            if frame < len(self.frame_residuals)
            else 0.0
        )
        self.viewport.set_frame(
            self.bvh,
            frame,
            transform.world_positions,
            transform.end_site_positions,
            heat,
            residual,
        )
        self.frame_label.setText(f"{frame} / {max(0, self.bvh.frame_count - 1)}")

    def toggle_playback(self):
        if self.play_timer.isActive():
            self.play_timer.stop()
            self.play_button.setText("播放")
        else:
            self.play_timer.start(33)
            self.play_button.setText("暂停播放")

    def _advance_playback(self):
        if self.bvh is None or self.bvh.frame_count == 0:
            return
        self.set_frame((self.current_frame + 1) % self.bvh.frame_count)

    def start_solve(self, resume: bool):
        if self.worker is not None and self.worker.isRunning():
            self._log("已有求解任务运行。")
            return
        if resume:
            directory = Path(self.session_edit.text())
            if not directory.exists():
                self._log("会话目录不存在。")
                return
            try:
                self.session = resume_session(directory)
                self.bvh = self.session.bvh
                self.constraints = self.session.constraints
                self.bvh_edit.setText(str(self.session.bvh_path))
                self._populate_table()
            except WorkbenchError as exc:
                self._error(exc)
                return
        else:
            self.apply_constraint_edits()
            session = self._session()
            if session is None:
                return
        self.cancel_event.clear()
        self.pause_event.clear()
        self.start_button.setEnabled(False)
        self.resume_button.setEnabled(False)
        self.pause_button.setEnabled(True)
        self.cancel_button.setEnabled(True)
        self.status_label.setText("求解中…")
        run_id = ("resume" if resume else "gui") + f":{int(time.time() * 1000)}"
        self.worker = SolverWorker(
            self.session,
            self.cancel_event,
            self.pause_event,
            8,
            run_id,
        )
        self.worker.progress.connect(self.on_progress)
        self.worker.finished_report.connect(self.on_report)
        self.worker.failed.connect(self.on_failure)
        self.worker.finished.connect(self._worker_finished)
        self.worker.start()
        self._log(("恢复" if resume else "增量") + "求解已启动。")

    def on_progress(self, event: dict):
        if event.get("event") == "impact_plan":
            count = sum(
                end - start + 1
                for start, end in event.get("recomputed_ranges", [])
            )
            self._log(
                "影响闭包: "
                + json.dumps(event.get("recomputed_ranges"), ensure_ascii=False)
                + f"（{count} 帧）"
            )
        elif event.get("event") == "progress":
            self.status_label.setText(
                f"已提交到第 {event.get('last_committed_frame')} 帧"
            )
        elif event.get("event") == "conflict":
            diagnostic = event.get("diagnostic", {})
            self._log(
                f"冲突: frame {diagnostic.get('frame')} / "
                f"worst {diagnostic.get('worst', {}).get('constraint_id')}"
            )

    def on_report(self, report: dict):
        self.status_label.setText(
            f"{report.get('status')} · computed {len(report.get('computed_frames', []))} "
            f"· cached {len(report.get('cached_frames', []))} · "
            f"residual {report.get('residual_digest', '')[:12]}"
        )
        self._log(
            f"完成: status={report.get('status')} "
            f"intervals={len(report.get('intervals', []))} "
            f"next_frame={report.get('recovery', {}).get('next_frame')}"
        )
        if report.get("conflict"):
            diagnostic = report["conflict"].get("diagnostic", {})
            self._log(
                "冲突证据: " + json.dumps(diagnostic, ensure_ascii=False)[:900]
            )
        self._load_solved_frames()

    def on_failure(self, error: dict):
        self._log(
            f"{error.get('code')}: {error.get('message')} "
            + json.dumps(error.get("details", {}), ensure_ascii=False)[:600]
        )
        self.status_label.setText(str(error.get("code", "E_INTERNAL")))

    def _worker_finished(self):
        self.start_button.setEnabled(True)
        self.resume_button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)

    def _load_solved_frames(self):
        if self.session is None or self.bvh is None:
            return
        try:
            state = self.session.store.load_committed(
                self.bvh, constraints=self.session.constraints
            )
        except WorkbenchError as exc:
            self._error(exc)
            return
        frames = {}
        positions = {}
        heat = {}
        residuals = [0.0] * self.bvh.frame_count
        for frame, record in state.frame_map.items():
            solution = next(
                (item for item in record.frames if item.frame == frame), None
            )
            if solution is None:
                continue
            frames[frame] = solution
            transform = self.bvh.evaluate(
                frame,
                rotations=solution.pose.rotations,
                scales=solution.pose.scales,
                translations=solution.pose.translations,
            )
            positions[frame] = transform
            joint_heat = [0.0] * len(self.bvh.joints)
            for residual in solution.residuals:
                if residual.joint in self.bvh.joint_by_name:
                    index = self.bvh.joint_by_name[residual.joint]
                    joint_heat[index] = max(
                        joint_heat[index],
                        residual.normalized if residual.enforced else 0.0,
                    )
            heat[frame] = joint_heat
            residuals[frame] = solution.total_residual
        self.frames = frames
        self.frame_positions = {
            frame: (
                positions[frame]
                if frame in positions
                else self.bvh.evaluate(frame)
            )
            for frame in range(self.bvh.frame_count)
        }
        self.joint_heat = {
            frame: heat.get(frame, [0.0] * len(self.bvh.joints))
            for frame in range(self.bvh.frame_count)
        }
        self.frame_residuals = residuals
        self.heatmap.set_values(residuals, self.current_frame)
        self.set_frame(self.current_frame)

    def toggle_pause(self):
        if self.pause_event.is_set():
            self.pause_event.clear()
            self.pause_button.setText("暂停")
            self._log("求解继续。")
        else:
            self.pause_event.set()
            self.pause_button.setText("继续")
            self._log("已在帧边界请求暂停。")

    def cancel_solve(self):
        self.cancel_event.set()
        self.pause_event.clear()
        self.status_label.setText("正在取消并提交最后完整区间…")
        self._log("已发出协作式取消。")

    def generate_demo(self):
        directory = QtWidgets.QFileDialog.getExistingDirectory(
            self, "选择样本输出目录"
        )
        if not directory:
            return
        try:
            result = generate_sample(
                directory,
                SampleSpec(frames=36, joints=8, conflict="none", seed=7),
                keep_baseline_session=False,
            )
            self.bvh_edit.setText(str(result["bvh_path"]))
            self.constraints_edit.setText(str(result["constraints_path"]))
            self.session_edit.setText(str(result["session_dir"]))
            self.load_bvh_file()
            self.load_constraint_file()
            self._log("样本已生成，可直接启动求解。")
        except Exception as exc:
            self._log(f"样本生成失败: {exc}")

    def _error(self, error: WorkbenchError):
        self._log(
            f"{error.code}: {error.message} "
            + json.dumps(error.details, ensure_ascii=False)
        )
        self.status_label.setText(error.code)

    def closeEvent(self, event):
        self.cancel_event.set()
        self.pause_event.clear()
        if self.worker is not None and self.worker.isRunning():
            self.worker.wait(4000)
        event.accept()


def main(argv=None) -> int:
    app = QtWidgets.QApplication(argv or [])
    app.setApplicationName("Motion Workbench")
    window = MainWindow()
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
