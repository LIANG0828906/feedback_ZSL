"""PySide6 application shell for formation diagnostics."""

from __future__ import annotations

import json
import sys
import time
from pathlib import Path
from typing import Any, Callable

from PySide6.QtCore import QThread, Qt, Signal
from PySide6.QtGui import QAction, QColor, QFont, QPalette
from PySide6.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QCheckBox,
    QComboBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSlider,
    QSpinBox,
    QSplitter,
    QStatusBar,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from ..engine import FormationEngine, InvalidationManifest
from ..errors import ErrorCode, WorkbenchError
from ..smoothing import traversed_cells
from ..validation import load_scene
from .canvas import TacticalCanvas


class EngineTask(QThread):
    succeeded = Signal(str, object)
    failed = Signal(str, object)

    def __init__(
        self,
        name: str,
        operation: Callable[[], Any],
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.name = name
        self.operation = operation

    def run(self) -> None:
        try:
            self.succeeded.emit(self.name, self.operation())
        except BaseException as exc:
            self.failed.emit(self.name, exc)


class MainWindow(QMainWindow):
    def __init__(
        self,
        engine: FormationEngine,
        *,
        scene_path: Path | None = None,
        checkpoint_path: Path | None = None,
    ) -> None:
        super().__init__()
        self.engine = engine
        self.scene_path = scene_path
        self.checkpoint_path = checkpoint_path
        self._task: EngineTask | None = None
        self._selected_member: str | None = None
        self._selected_cell = (1, 1)
        self._pending_manifest: InvalidationManifest | None = None

        self.setWindowTitle("Formation Coupling Lab")
        self.resize(1440, 880)
        self._apply_theme()
        self._build_ui()
        self._refresh_all()
        self._log(
            "Workbench ready. Click a grid cell to inspect or edit. "
            "All operations run locally."
        )

    def _apply_theme(self) -> None:
        app = QApplication.instance()
        if app is not None:
            app.setStyle("Fusion")
            palette = QPalette()
            palette.setColor(QPalette.ColorRole.Window, QColor("#0d1117"))
            palette.setColor(QPalette.ColorRole.WindowText, QColor("#d8e1ea"))
            palette.setColor(QPalette.ColorRole.Base, QColor("#111820"))
            palette.setColor(QPalette.ColorRole.AlternateBase, QColor("#151e27"))
            palette.setColor(QPalette.ColorRole.Text, QColor("#d8e1ea"))
            palette.setColor(QPalette.ColorRole.Button, QColor("#1d2833"))
            palette.setColor(QPalette.ColorRole.ButtonText, QColor("#d8e1ea"))
            palette.setColor(QPalette.ColorRole.Highlight, QColor("#2f6f8f"))
            palette.setColor(QPalette.ColorRole.HighlightedText, QColor("#ffffff"))
            app.setPalette(palette)
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #0d1117; color: #d8e1ea; }
            QGroupBox {
                border: 1px solid #263341; border-radius: 7px;
                margin-top: 11px; padding-top: 9px; font-weight: 600;
            }
            QGroupBox::title { subcontrol-origin: margin; left: 9px; padding: 0 4px; color: #8fc7df; }
            QPushButton {
                background: #1d2a36; border: 1px solid #32485c;
                border-radius: 4px; padding: 6px 10px;
            }
            QPushButton:hover { background: #263a4b; border-color: #4f7892; }
            QPushButton:pressed { background: #14202a; }
            QPushButton:disabled { color: #6c7885; background: #151c23; }
            QLineEdit, QSpinBox, QComboBox {
                background: #111a22; border: 1px solid #2b3d4d;
                border-radius: 4px; padding: 4px 5px;
            }
            QTableWidget {
                background: #101820; alternate-background-color: #141e27;
                gridline-color: #253240; selection-background-color: #244b61;
            }
            QHeaderView::section {
                background: #1b2834; color: #a9bfd0; border: none;
                border-right: 1px solid #293b4b; padding: 5px;
            }
            QTabWidget::pane { border: 1px solid #263341; }
            QTabBar::tab { background: #16212b; padding: 7px 12px; }
            QTabBar::tab:selected { background: #24384a; color: #ffffff; }
            QTextEdit { background: #0f161d; border: 1px solid #263341; }
            QSlider::groove:horizontal { background: #1b2732; height: 5px; border-radius: 2px; }
            QSlider::handle:horizontal { background: #52a6ca; width: 13px; margin: -5px 0; border-radius: 6px; }
            """
        )

    def _build_ui(self) -> None:
        splitter = QSplitter(Qt.Orientation.Horizontal)
        self.canvas = TacticalCanvas(self.engine)
        self.canvas.cell_selected.connect(self._on_cell_selected)
        splitter.addWidget(self.canvas)

        side = QWidget()
        side.setMinimumWidth(450)
        side.setMaximumWidth(650)
        layout = QVBoxLayout(side)
        layout.setContentsMargins(8, 8, 8, 8)

        self._scene_label = QLabel()
        self._scene_label.setWordWrap(True)
        self._scene_label.setFont(QFont("Consolas", 9))
        layout.addWidget(self._scene_label)

        edit_group = QGroupBox("Incremental edit")
        edit_layout = QFormLayout(edit_group)
        self._selected_cell_label = QLabel("(1, 1)")
        edit_layout.addRow("Selected cell", self._selected_cell_label)
        self._cost_spin = QSpinBox()
        self._cost_spin.setRange(-1, 1_000_000)
        self._cost_spin.setValue(1)
        edit_layout.addRow("Passable cost", self._cost_spin)
        self._apply_cost_button = QPushButton("Apply cost -> incremental")
        self._apply_cost_button.clicked.connect(self._apply_cost)
        edit_layout.addRow(self._apply_cost_button)
        self._blockade_combo = QComboBox()
        self._blockade_combo.currentIndexChanged.connect(self._on_blockade_changed)
        edit_layout.addRow("Blockade", self._blockade_combo)
        self._blockade_active = QCheckBox("Active")
        edit_layout.addRow(self._blockade_active)
        self._apply_blockade_button = QPushButton("Apply blockade -> incremental")
        self._apply_blockade_button.clicked.connect(self._apply_blockade)
        edit_layout.addRow(self._apply_blockade_button)
        layout.addWidget(edit_group)

        action_group = QGroupBox("Recompute / persistence")
        action_layout = QHBoxLayout(action_group)
        self._build_button = QPushButton("Full build")
        self._build_button.clicked.connect(self._full_build)
        self._compare_button = QPushButton("Compare full")
        self._compare_button.clicked.connect(self._compare)
        self._retry_button = QPushButton("Retry member")
        self._retry_button.clicked.connect(self._retry_member)
        self._save_button = QPushButton("Save checkpoint")
        self._save_button.clicked.connect(self._save_checkpoint)
        self._load_button = QPushButton("Load / rebuild")
        self._load_button.clicked.connect(self._load_checkpoint)
        self._export_button = QPushButton("Export JSON")
        self._export_button.clicked.connect(self._export)
        for button in (
            self._build_button,
            self._compare_button,
            self._retry_button,
            self._save_button,
            self._load_button,
            self._export_button,
        ):
            action_layout.addWidget(button)
        layout.addWidget(action_group)

        tick_group = QGroupBox("Tick inspector")
        tick_layout = QVBoxLayout(tick_group)
        self._tick_label = QLabel("tick 0")
        self._tick_slider = QSlider(Qt.Orientation.Horizontal)
        self._tick_slider.setRange(0, self.engine.scene.simulation.max_ticks)
        self._tick_slider.valueChanged.connect(self._on_tick_changed)
        tick_layout.addWidget(self._tick_label)
        tick_layout.addWidget(self._tick_slider)
        layout.addWidget(tick_group)

        self._tabs = QTabWidget()
        members_tab = QWidget()
        members_layout = QVBoxLayout(members_tab)
        self._member_table = QTableWidget(0, 7)
        self._member_table.setHorizontalHeaderLabels(
            ["Member", "Status", "Path", "Hit", "ms", "Sim", "Slot dev"]
        )
        self._member_table.verticalHeader().setVisible(False)
        self._member_table.setSelectionBehavior(
            QAbstractItemView.SelectionBehavior.SelectRows
        )
        self._member_table.setSelectionMode(
            QAbstractItemView.SelectionMode.SingleSelection
        )
        self._member_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        self._member_table.itemSelectionChanged.connect(self._on_member_selected)
        members_layout.addWidget(self._member_table)
        self._tabs.addTab(members_tab, "Members")

        invalid_tab = QWidget()
        invalid_layout = QVBoxLayout(invalid_tab)
        self._invalid_table = QTableWidget(0, 5)
        self._invalid_table.setHorizontalHeaderLabels(
            ["Member", "Scope", "Reason", "Reused", "Intervals"]
        )
        self._invalid_table.verticalHeader().setVisible(False)
        self._invalid_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        invalid_layout.addWidget(self._invalid_table)
        self._tabs.addTab(invalid_tab, "Invalidation")

        log_tab = QWidget()
        log_layout = QVBoxLayout(log_tab)
        self._log_view = QTextEdit()
        self._log_view.setReadOnly(True)
        self._log_view.setFont(QFont("Consolas", 9))
        log_layout.addWidget(self._log_view)
        self._tabs.addTab(log_tab, "Event log")
        layout.addWidget(self._tabs, 1)

        splitter.addWidget(side)
        splitter.setStretchFactor(0, 3)
        splitter.setStretchFactor(1, 2)
        self.setCentralWidget(splitter)
        self.setStatusBar(QStatusBar())
        self.statusBar().showMessage("Ready")

        export_action = QAction("Export trajectory", self)
        export_action.triggered.connect(self._export)
        self.addAction(export_action)

    def _start_task(
        self,
        name: str,
        operation: Callable[[], Any],
        *,
        blocked_message: str = "Working...",
    ) -> None:
        if self._task is not None and self._task.isRunning():
            self._log("An operation is already running; wait for it to finish.")
            return
        self._set_busy(True)
        self.statusBar().showMessage(blocked_message)
        self._task = EngineTask(name, operation, self)
        self._task.succeeded.connect(self._task_succeeded)
        self._task.failed.connect(self._task_failed)
        self._task.finished.connect(lambda: self._set_busy(False))
        self._task.start()

    def _set_busy(self, busy: bool) -> None:
        for button in (
            self._build_button,
            self._compare_button,
            self._retry_button,
            self._save_button,
            self._load_button,
            self._export_button,
            self._apply_cost_button,
            self._apply_blockade_button,
        ):
            button.setEnabled(not busy)
        if not busy:
            self.statusBar().showMessage("Ready")

    def _task_succeeded(self, name: str, result: object) -> None:
        if name == "compare":
            comparison = result
            self._log(f"Full comparison passed: {json.dumps(comparison.to_dict())}")
            QMessageBox.information(
                self,
                "Full consistency",
                (
                    "Incremental cache matches empty-cache full rebuild.\n\n"
                    f"Members: {len(comparison.checked_members)}\n"
                    f"Ticks: {comparison.checked_ticks}\n"
                    f"Path cells: {comparison.path_cells}\n"
                    f"Waypoints: {comparison.waypoints}"
                ),
            )
        elif name in {"build", "restore"}:
            self._log(f"{name}: {json.dumps(result, ensure_ascii=True)[:1200]}")
        elif name == "retry":
            self._log(f"Retry manifest: {json.dumps(result.to_dict(), ensure_ascii=True)}")
        elif name == "save":
            self._log(f"Checkpoint save: {json.dumps(result, ensure_ascii=True)}")
        elif name == "export":
            self._log(f"Exported trajectory: {result}")
        elif name == "edit-cost":
            self._pending_manifest = result
            self._log(f"Incremental cost edit: {json.dumps(result.to_dict(), ensure_ascii=True)}")
        elif name == "edit-blockade":
            self._pending_manifest = result
            self._log(
                f"Incremental blockade edit: {json.dumps(result.to_dict(), ensure_ascii=True)}"
            )
        self._refresh_all()

    def _task_failed(self, name: str, exc: object) -> None:
        if isinstance(exc, WorkbenchError):
            payload = exc.to_dict()
            message = (
                f"{exc.code.value}: {exc.message}\n"
                f"field={exc.field} coord={exc.coord} member={exc.member_id} "
                f"tick={exc.tick} pair={exc.pair}"
            )
        else:
            payload = {"message": f"{type(exc).__name__}: {exc}"}
            message = payload["message"]
        self._log(f"{name} failed: {json.dumps(payload, ensure_ascii=True)}")
        QMessageBox.critical(self, f"{name} failed", message)
        self._refresh_all()

    def _refresh_all(self) -> None:
        scene = self.engine.scene
        self._scene_label.setText(
            (
                f"{scene.name}\n"
                f"map {scene.map.width}x{scene.map.height}  "
                f"hash {scene.map.content_hash()[:12]}  "
                f"rev {self.engine.revision}\n"
                f"formation {scene.formation.version}  "
                f"leader {scene.formation.leader}\n"
                f"workers {scene.simulation.max_parallel_workers}  "
                f"buffer {scene.simulation.per_worker_buffer}"
            )
        )
        current_blockade = self._blockade_combo.currentData()
        self._blockade_combo.blockSignals(True)
        self._blockade_combo.clear()
        for blockade in scene.map.blockades:
            self._blockade_combo.addItem(
                f"{blockade.blockade_id} [{blockade.terrain}]",
                blockade.blockade_id,
            )
        if current_blockade is not None:
            index = self._blockade_combo.findData(current_blockade)
            if index >= 0:
                self._blockade_combo.setCurrentIndex(index)
        self._blockade_combo.blockSignals(False)
        if self._blockade_combo.currentData() is not None:
            selected = next(
                item
                for item in scene.map.blockades
                if item.blockade_id == self._blockade_combo.currentData()
            )
            self._blockade_active.setChecked(selected.active)

        states = self.engine.member_states()
        paths = self.engine.paths()
        traces = self.engine.traces()
        self._member_table.setRowCount(len(states))
        for row, (member_id, state) in enumerate(sorted(states.items())):
            path = paths.get(member_id)
            trace = traces.get(member_id)
            deviation = (
                trace.ticks[min(self._tick_slider.value(), len(trace.ticks) - 1)].slot_deviation
                if trace and trace.ticks
                else 0
            )
            values = [
                member_id,
                state["status"],
                f"{len(path.raw.cells)} cells" if path is not None else "-",
                "HIT" if state["path_cache_hit"] else "MISS",
                f"{state['path_ms']:.2f}",
                "HIT" if state["simulation_cache_hit"] else "MISS",
                str(deviation),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column == 1:
                    if value == "ok":
                        item.setForeground(QColor("#80ed99"))
                    elif value == "failed":
                        item.setForeground(QColor("#ff6b7d"))
                if column == 3:
                    item.setForeground(
                        QColor("#80ed99") if value == "HIT" else QColor("#ffd166")
                    )
                self._member_table.setItem(row, column, item)
        if self._member_table.currentRow() < 0 and self._member_table.rowCount():
            self._member_table.selectRow(0)

        manifest = self.engine.last_manifest or self._pending_manifest
        if manifest is not None:
            rows: list[tuple[str, str, str, str, str]] = []
            reasons = {item["member_id"]: item for item in manifest.reasons}
            for member_id in sorted(states):
                reason = reasons.get(member_id)
                if reason is None:
                    rows.append(
                        (
                            member_id,
                            "cache",
                            "unaffected_member_reuse",
                            "YES",
                            "all intervals reused",
                        )
                    )
                else:
                    member_intervals = [
                        interval
                        for interval in manifest.invalidated_intervals
                        if interval.member_id == member_id
                    ]
                    rows.append(
                        (
                            member_id,
                            str(reason["scope"]),
                            str(reason["reason"]),
                            "NO" if not reason.get("cache_reused") else "PARTIAL",
                            (
                                f"{member_intervals[0].tick_start}-"
                                f"{member_intervals[-1].tick_end} "
                                f"({len(member_intervals)} intervals)"
                                if member_intervals
                                else "-"
                            ),
                        )
                    )
            self._invalid_table.setRowCount(len(rows))
            for row, values in enumerate(rows):
                for column, value in enumerate(values):
                    item = QTableWidgetItem(value)
                    if column == 3:
                        item.setForeground(
                            QColor("#80ed99") if value == "YES" else QColor("#ffb454")
                        )
                    self._invalid_table.setItem(row, column, item)
            invalid_cells: set[tuple[int, int]] = set(manifest.changed_cells)
            self.canvas.set_invalidated_cells(invalid_cells)
            self.canvas.set_invalidated_members(
                {str(item["member_id"]) for item in manifest.reasons}
            )
        else:
            self._invalid_table.setRowCount(0)
            self.canvas.set_invalidated_cells(set())
            self.canvas.set_invalidated_members(set())
        self.canvas.update()

    def _log(self, message: str) -> None:
        stamp = time.strftime("%H:%M:%S")
        self._log_view.append(f"[{stamp}] {message}")

    def _on_cell_selected(self, x: int, y: int) -> None:
        self._selected_cell = (x, y)
        scene = self.engine.scene
        self._selected_cell_label.setText(f"({x}, {y})")
        self._cost_spin.setValue(scene.map.cost_at((x, y)))
        for blockade in scene.map.blockades:
            if (x, y) in blockade.cells:
                index = self._blockade_combo.findData(blockade.blockade_id)
                if index >= 0:
                    self._blockade_combo.setCurrentIndex(index)
                    self._blockade_active.setChecked(blockade.active)
                break

    def _on_blockade_changed(self) -> None:
        blockade_id = self._blockade_combo.currentData()
        if blockade_id is None:
            return
        for blockade in self.engine.scene.map.blockades:
            if blockade.blockade_id == blockade_id:
                self._blockade_active.setChecked(blockade.active)
                break

    def _on_tick_changed(self, value: int) -> None:
        self._tick_label.setText(f"tick {value}")
        self.canvas.set_tick(value)
        self._refresh_all()

    def _on_member_selected(self) -> None:
        row = self._member_table.currentRow()
        if row < 0:
            self._selected_member = None
        else:
            item = self._member_table.item(row, 0)
            self._selected_member = item.text() if item is not None else None
        self.canvas.set_selected_member(self._selected_member)

    def _apply_cost(self) -> None:
        coord = self._selected_cell
        cost = self._cost_spin.value()
        self.canvas.set_selected_cell(coord)
        self._start_task(
            "edit-cost",
            lambda: self.engine.apply_cell_cost(coord, cost),
            blocked_message="Applying cell-cost dependency update...",
        )

    def _apply_blockade(self) -> None:
        blockade_id = self._blockade_combo.currentData()
        if blockade_id is None:
            return
        active = self._blockade_active.isChecked()
        self._start_task(
            "edit-blockade",
            lambda: self.engine.apply_blockade(str(blockade_id), active),
            blocked_message="Applying blockade dependency update...",
        )

    def _full_build(self) -> None:
        self._start_task(
            "build",
            lambda: self.engine.build_all(parallel=True, save_checkpoint=True),
            blocked_message="Running full rebuild...",
        )

    def _compare(self) -> None:
        self._start_task(
            "compare",
            lambda: self.engine.compare_with_full(parallel=True),
            blocked_message="Comparing incremental cache with empty-cache rebuild...",
        )

    def _retry_member(self) -> None:
        row = self._member_table.currentRow()
        if row < 0:
            self._log("Select a member before retrying.")
            return
        item = self._member_table.item(row, 0)
        if item is None:
            return
        member_id = item.text()
        self._start_task(
            "retry",
            lambda: self.engine.retry_member(member_id),
            blocked_message=f"Retrying {member_id}...",
        )

    def _save_checkpoint(self) -> None:
        if self.engine.checkpoint_path is None:
            QMessageBox.warning(
                self,
                "No checkpoint path",
                "This session has no checkpoint path configured.",
            )
            return
        self._start_task(
            "save",
            self.engine.save_checkpoint,
            blocked_message="Writing atomic checkpoint...",
        )

    def _load_checkpoint(self) -> None:
        self._start_task(
            "restore",
            self.engine.load_or_rebuild,
            blocked_message="Validating checkpoint versions...",
        )

    def _export(self) -> None:
        suggested = Path("artifacts") / "trajectory_export.json"
        destination, _ = QFileDialog.getSaveFileName(
            self,
            "Export trajectory JSON",
            str(suggested),
            "JSON (*.json)",
        )
        if not destination:
            return
        self._start_task(
            "export",
            lambda: str(self.engine.export_json(destination)),
            blocked_message="Exporting trajectory...",
        )

    def closeEvent(self, event) -> None:  # type: ignore[override]
        if self._task is not None and self._task.isRunning():
            self._task.requestInterruption()
            self._task.wait(250)
        super().closeEvent(event)


def run_gui(
    *,
    scene_path: str | Path = "samples/formation_scene.json",
    checkpoint_path: str | Path = "artifacts/formation_checkpoint.json",
) -> int:
    scene = load_scene(scene_path)
    engine = FormationEngine(scene, checkpoint_path=checkpoint_path)
    engine.load_or_rebuild()
    app = QApplication.instance() or QApplication(sys.argv)
    window = MainWindow(
        engine,
        scene_path=Path(scene_path),
        checkpoint_path=Path(checkpoint_path),
    )
    window.show()
    return app.exec()
