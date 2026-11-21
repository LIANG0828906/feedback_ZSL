"""Optional PyQt5 desktop interface.

The numerical engine and CLI do not import PyQt5. This keeps headless tests
deterministic while the same session API powers the Windows desktop UI.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .engine import FittingConfig, FittingSession
from .errors import E_BUSY, E_STALE_HANDLE, AppError
from .params import GROUPS
from .scene import Scene, load_scene


def _import_qt() -> tuple[Any, ...]:
    try:
        from PyQt5 import QtCore, QtGui, QtWidgets
    except ImportError as exc:
        raise RuntimeError(
            "PyQt5 is not installed; run `pip install PyQt5` on the desktop"
        ) from exc
    return QtCore, QtGui, QtWidgets


class LossPlot:
    @staticmethod
    def build(QtCore: Any, QtWidgets: Any, QtGui: Any) -> type:
        class _LossPlot(QtWidgets.QWidget):
            def __init__(self) -> None:
                super().__init__()
                self.values: list[float] = []
                self.setMinimumHeight(150)

            def set_values(self, values: list[float]) -> None:
                self.values = list(values)
                self.update()

            def paintEvent(self, event: Any) -> None:
                painter = QtGui.QPainter(self)
                painter.fillRect(self.rect(), QtGui.QColor("#101820"))
                painter.setPen(QtGui.QPen(QtGui.QColor("#314150"), 1))
                painter.drawRect(self.rect().adjusted(1, 1, -2, -2))
                if not self.values:
                    painter.setPen(QtGui.QColor("#8fa3b8"))
                    painter.drawText(
                        self.rect(), QtCore.Qt.AlignCenter, "No committed loss"
                    )
                    return
                margin = 14
                width = max(1, self.width() - 2 * margin)
                height = max(1, self.height() - 2 * margin)
                maximum = max(self.values)
                minimum = min(self.values)
                span = maximum - minimum
                if span <= 1.0e-15:
                    span = 1.0
                count = len(self.values)
                points = []
                for index, value in enumerate(self.values):
                    x = margin + (width * index / max(1, count - 1))
                    y = margin + height * (1.0 - (value - minimum) / span)
                    points.append(QtCore.QPointF(x, y))
                painter.setPen(QtGui.QPen(QtGui.QColor("#64d2a6"), 2))
                if len(points) == 1:
                    painter.drawEllipse(points[0], 3, 3)
                else:
                    painter.drawPolyline(QtGui.QPolygonF(points))
                painter.setPen(QtGui.QColor("#8fa3b8"))
                painter.drawText(
                    margin,
                    margin - 3,
                    f"last {self.values[-1]:.8g}  min {minimum:.8g}  max {maximum:.8g}",
                )

        return _LossPlot


def run_gui(scene_path: str | None = None, checkpoint_dir: str | None = None) -> int:
    try:
        QtCore, QtGui, QtWidgets = _import_qt()
    except RuntimeError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, sort_keys=True))
        return 2

    LossPlotWidget = LossPlot.build(QtCore, QtWidgets, QtGui)

    class MainWindow(QtWidgets.QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("Inverse Imaging Workstation")
            self.resize(1180, 780)
            self.scene: Scene | None = None
            self.session: FittingSession | None = None
            self.current_handle: str | None = None
            self.timer = QtCore.QTimer(self)
            self.timer.setInterval(200)
            self.timer.timeout.connect(self.poll_session)
            self.timer.start()

            central = QtWidgets.QWidget()
            self.setCentralWidget(central)
            root = QtWidgets.QVBoxLayout(central)
            root.addWidget(self.build_scene_row())
            root.addWidget(self.build_fit_row())
            split = QtWidgets.QSplitter(QtCore.Qt.Horizontal)
            split.addWidget(self.build_parameter_panel())
            split.addWidget(self.build_monitor_panel())
            split.setStretchFactor(0, 1)
            split.setStretchFactor(1, 2)
            root.addWidget(split, 1)
            self.statusBar().showMessage("Load a local scene JSON to begin.")
            if scene_path:
                self.scene_edit.setText(scene_path)
                self.load_scene()
            if checkpoint_dir:
                self.checkpoint_edit.setText(checkpoint_dir)

        def build_scene_row(self) -> Any:
            group = QtWidgets.QGroupBox("Local scene")
            layout = QtWidgets.QHBoxLayout(group)
            self.scene_edit = QtWidgets.QLineEdit()
            self.scene_edit.setPlaceholderText("scene.json")
            browse = QtWidgets.QPushButton("Browse")
            browse.clicked.connect(self.browse_scene)
            load = QtWidgets.QPushButton("Load")
            load.clicked.connect(self.load_scene)
            self.checkpoint_edit = QtWidgets.QLineEdit()
            self.checkpoint_edit.setPlaceholderText("checkpoint directory")
            layout.addWidget(QtWidgets.QLabel("Scene"))
            layout.addWidget(self.scene_edit, 3)
            layout.addWidget(browse)
            layout.addWidget(load)
            layout.addWidget(QtWidgets.QLabel("Checkpoint"))
            layout.addWidget(self.checkpoint_edit, 2)
            return group

        def build_fit_row(self) -> Any:
            group = QtWidgets.QGroupBox("Group-scheduled fitting")
            layout = QtWidgets.QHBoxLayout(group)
            self.steps_spin = QtWidgets.QSpinBox()
            self.steps_spin.setRange(0, 100000)
            self.steps_spin.setValue(8)
            self.lr_spin = QtWidgets.QDoubleSpinBox()
            self.lr_spin.setRange(0.0, 10.0)
            self.lr_spin.setDecimals(6)
            self.lr_spin.setSingleStep(0.001)
            self.lr_spin.setValue(0.02)
            self.parallel_spin = QtWidgets.QSpinBox()
            self.parallel_spin.setRange(1, 64)
            self.parallel_spin.setValue(2)
            self.chunk_spin = QtWidgets.QSpinBox()
            self.chunk_spin.setRange(1, 512)
            self.chunk_spin.setValue(8)
            self.groups_edit = QtWidgets.QLineEdit(",".join(GROUPS))
            self.start_button = QtWidgets.QPushButton("Start")
            self.start_button.clicked.connect(self.start_fit)
            self.cancel_button = QtWidgets.QPushButton("Cooperative cancel")
            self.cancel_button.clicked.connect(self.cancel_fit)
            self.modify_button = QtWidgets.QPushButton("Modify group")
            self.modify_button.clicked.connect(self.modify_group)
            for label, widget in (
                ("Steps", self.steps_spin),
                ("LR", self.lr_spin),
                ("Workers", self.parallel_spin),
                ("Chunk", self.chunk_spin),
                ("Schedule", self.groups_edit),
            ):
                layout.addWidget(QtWidgets.QLabel(label))
                layout.addWidget(widget)
            layout.addWidget(self.start_button)
            layout.addWidget(self.cancel_button)
            layout.addWidget(self.modify_button)
            return group

        def build_parameter_panel(self) -> Any:
            group = QtWidgets.QGroupBox("Parameters (committed snapshot)")
            layout = QtWidgets.QVBoxLayout(group)
            self.parameter_tree = QtWidgets.QTreeWidget()
            self.parameter_tree.setColumnCount(3)
            self.parameter_tree.setHeaderLabels(["Group / key", "Value", ""])
            self.parameter_tree.header().setSectionResizeMode(
                0, QtWidgets.QHeaderView.Stretch
            )
            layout.addWidget(self.parameter_tree)
            self.degradation_label = QtWidgets.QLabel("No downgrade")
            self.degradation_label.setWordWrap(True)
            self.degradation_label.setStyleSheet(
                "color: #e2a45d; font-weight: bold; padding: 4px;"
            )
            layout.addWidget(self.degradation_label)
            return group

        def build_monitor_panel(self) -> Any:
            group = QtWidgets.QGroupBox("Published state")
            layout = QtWidgets.QVBoxLayout(group)
            self.stats_label = QtWidgets.QLabel("No checkpoint")
            self.stats_label.setWordWrap(True)
            layout.addWidget(self.stats_label)
            self.plot = LossPlotWidget()
            layout.addWidget(self.plot)
            self.queue_label = QtWidgets.QLabel("Waiting queue: empty")
            self.queue_label.setWordWrap(True)
            layout.addWidget(self.queue_label)
            self.log = QtWidgets.QPlainTextEdit()
            self.log.setReadOnly(True)
            self.log.setMaximumBlockCount(400)
            layout.addWidget(self.log, 1)
            return group

        def browse_scene(self) -> None:
            selected, _ = QtWidgets.QFileDialog.getOpenFileName(
                self, "Open scene", str(Path.cwd()), "JSON (*.json)"
            )
            if selected:
                self.scene_edit.setText(selected)
                self.load_scene()

        def load_scene(self) -> None:
            try:
                self.scene = load_scene(self.scene_edit.text().strip())
                if not self.checkpoint_edit.text().strip():
                    self.checkpoint_edit.setText(
                        str(self.scene.path.parent / "checkpoint")
                    )
                self.session = FittingSession(
                    self.scene,
                    FittingConfig(
                        steps=self.steps_spin.value(),
                        learning_rate=self.lr_spin.value(),
                        active_groups=self.parse_groups(),
                        chunk_size=self.chunk_spin.value(),
                        parallel=self.parallel_spin.value(),
                        checkpoint_dir=self.checkpoint_edit.text().strip(),
                    ),
                )
                self.current_handle = None
                self.populate_parameters(self.scene.params.nested())
                self.statusBar().showMessage(f"Loaded {self.scene.scene_id[:16]}")
                self.log.appendPlainText(f"Loaded scene {self.scene.path}")
            except (AppError, OSError, ValueError) as exc:
                self.show_error(exc)

        def parse_groups(self) -> tuple[str, ...]:
            groups = tuple(
                part.strip() for part in self.groups_edit.text().split(",") if part.strip()
            )
            if not groups or set(groups) - set(GROUPS):
                raise ValueError("schedule must contain camera,geometry,material,light")
            return groups

        def start_fit(self) -> None:
            try:
                if self.session is None:
                    self.load_scene()
                if self.session is None or self.scene is None:
                    return
                if self.session.status in ("starting", "running", "cancelling"):
                    raise AppError(E_BUSY, "fitting session is already running", "session")
                self.session = FittingSession(
                    self.scene,
                    FittingConfig(
                    steps=self.steps_spin.value(),
                    learning_rate=self.lr_spin.value(),
                    active_groups=self.parse_groups(),
                    chunk_size=self.chunk_spin.value(),
                    parallel=self.parallel_spin.value(),
                    checkpoint_dir=self.checkpoint_edit.text().strip(),
                    verify_incremental=True,
                    ),
                )
                self.session.start()
                self.statusBar().showMessage("Fitting started")
            except (AppError, ValueError) as exc:
                self.show_error(exc)

        def cancel_fit(self) -> None:
            if self.session is not None:
                self.session.cancel()
                self.statusBar().showMessage("Cancellation requested")

        def modify_group(self) -> None:
            if self.session is None:
                return
            group, accepted = QtWidgets.QInputDialog.getItem(
                self, "Modify group", "Parameter group", list(GROUPS), 0, False
            )
            if not accepted:
                return
            template = {
                key: self.session.scene.params.values[group][key]
                for key in self.session.scene.params.keys(group)
            }
            text, accepted = QtWidgets.QInputDialog.getMultiLineText(
                self,
                f"Modify {group}",
                "Flat JSON object key -> number",
                json.dumps(template, indent=2, sort_keys=True),
            )
            if not accepted:
                return
            try:
                updates = json.loads(text)
                if not isinstance(updates, dict):
                    raise ValueError("expected a JSON object")
                self.session.update_group(group, updates)
                self.populate_parameters(self.session.scene.params.nested())
                self.log.appendPlainText(
                    f"Modified {group}; start a fresh checkpoint directory to fit edits."
                )
            except (ValueError, AppError, json.JSONDecodeError) as exc:
                self.show_error(exc)

        def poll_session(self) -> None:
            if self.session is None:
                return
            latest = self.session.latest_snapshot
            if latest is None:
                if self.session.status == "running":
                    self.statusBar().showMessage("Computing first checkpoint...")
                return
            if latest.handle == self.current_handle:
                return
            try:
                snapshot = self.session.query(latest.handle)
            except AppError as exc:
                if exc.code == E_STALE_HANDLE:
                    self.log.appendPlainText(f"E_STALE_HANDLE: {exc.message}")
                return
            self.current_handle = snapshot.handle
            self.populate_parameters(snapshot.params)
            history = list(snapshot.history)
            self.plot.set_values([float(item["loss"]) for item in history])
            last = history[-1] if history else {}
            self.stats_label.setText(
                "step={step}  loss={loss:.9g}  cumulative={cumulative:.9g}\n"
                "affected={affected}  reused={reused}  recomputed={recomputed}\n"
                "gradient norms: {norms}\n"
                "reduce hash: {reduce_hash}".format(
                    step=snapshot.step,
                    loss=snapshot.loss,
                    cumulative=snapshot.cumulative_loss,
                    affected=last.get("affected_node_count", "-"),
                    reused=last.get("reused_node_count", "-"),
                    recomputed=last.get("recomputed_node_count", "-"),
                    norms=", ".join(
                        f"{name}={value:.5g}"
                        for name, value in snapshot.gradient_norms.items()
                    ),
                    reduce_hash=snapshot.reduce_hash[:16],
                )
            )
            downgrades = self.session.degradations
            if downgrades:
                self.degradation_label.setText(
                    f"Last downgrade step {downgrades[-1]['step']}: "
                    f"{downgrades[-1]['reason']}"
                )
                self.log.appendPlainText(
                    f"Degraded step {downgrades[-1]['step']}: {downgrades[-1]['reason']}"
                )
            queue = self.session.queue_snapshot()
            self.queue_label.setText(
                "Waiting queue: empty"
                if not queue
                else "Waiting queue: "
                + ", ".join(str(item.get("name", "?")) for item in queue)
            )
            self.statusBar().showMessage(
                f"Published step {snapshot.step}; session={self.session.status}"
            )

        def populate_parameters(self, nested: dict[str, Any]) -> None:
            self.parameter_tree.clear()
            for group in GROUPS:
                group_item = QtWidgets.QTreeWidgetItem([group, "", ""])
                self.parameter_tree.addTopLevelItem(group_item)
                self._append_parameter_rows(group_item, group, nested.get(group, {}))
                group_item.setExpanded(group in ("camera", "light"))

        def _append_parameter_rows(
            self, parent: Any, prefix: str, value: Any
        ) -> None:
            if isinstance(value, dict):
                for key in sorted(value):
                    child = QtWidgets.QTreeWidgetItem([str(key), "", ""])
                    parent.addChild(child)
                    self._append_parameter_rows(child, f"{prefix}.{key}", value[key])
            elif isinstance(value, list):
                for index, item in enumerate(value):
                    child = QtWidgets.QTreeWidgetItem([f"[{index}]", "", ""])
                    parent.addChild(child)
                    self._append_parameter_rows(child, f"{prefix}.{index}", item)
            else:
                parent.setText(0, parent.text(0))
                parent.setText(1, f"{float(value):.10g}")

        def show_error(self, exc: BaseException) -> None:
            if isinstance(exc, AppError):
                message = f"{exc.code}: {exc.message}"
                if exc.field:
                    message += f" (field={exc.field})"
            else:
                message = str(exc)
            self.log.appendPlainText(message)
            self.statusBar().showMessage(message)

        def closeEvent(self, event: Any) -> None:
            if self.session is not None:
                self.session.cancel()
            event.accept()

    application = QtWidgets.QApplication([])
    application.setApplicationName("Inverse Imaging Workstation")
    window = MainWindow()
    window.show()
    return int(application.exec_())

