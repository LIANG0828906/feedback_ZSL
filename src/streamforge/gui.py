from __future__ import annotations

import json
import sys
import threading
from pathlib import Path
from typing import Any

from PySide6.QtCore import QObject, Qt, QTimer, Signal
from PySide6.QtGui import QColor, QCloseEvent, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QCheckBox,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .errors import BusinessError, ErrorCode
from .generator import atomic_write
from .models import canonical_json
from .pipeline import Pipeline, load_artifacts_from_output
from .playback import PlaybackConfig, load_bandwidth, simulate_playback
from .samples import generate_samples
from .scheduler import RunControl


class WorkerSignals(QObject):
    progress = Signal(dict)
    scheduler_event = Signal(dict)
    finished = Signal(dict)
    failed = Signal(dict)


class MemoryChart(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.samples: list[int] = []
        self.limit = 1
        self.setMinimumHeight(150)

    def set_data(self, allocated: int, peak: int, limit: int) -> None:
        self.samples.append(allocated)
        self.samples = self.samples[-240:]
        self.limit = max(1, limit)
        if peak > self.limit:
            self.limit = peak
        self.update()

    def paintEvent(self, event: Any) -> None:
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#111820"))
        painter.setPen(QPen(QColor("#304252"), 1))
        for row in range(1, 4):
            y = int(self.height() * row / 4)
            painter.drawLine(0, y, self.width(), y)
        if len(self.samples) < 2:
            painter.setPen(QColor("#9bb0c4"))
            painter.drawText(12, 24, "Memory samples will appear here")
            return
        painter.setPen(QPen(QColor("#43d7a1"), 2))
        width = max(1, self.width() - 1)
        height = max(1, self.height() - 8)
        path = []
        for index, value in enumerate(self.samples):
            x = int(index * width / max(1, len(self.samples) - 1))
            y = self.height() - 4 - int(min(1.0, value / self.limit) * height)
            path.append((x, y))
        for first, second in zip(path, path[1:]):
            painter.drawLine(first[0], first[1], second[0], second[1])
        painter.setPen(QColor("#d3e0ea"))
        painter.drawText(
            10,
            20,
            f"current {self.samples[-1]:,} B / ceiling {self.limit:,} B",
        )


class MainWindow(QMainWindow):
    def __init__(
        self,
        *,
        samples_root: Path,
        output_root: Path,
        seed: int,
    ) -> None:
        super().__init__()
        self.samples_root = samples_root.resolve()
        self.output_root = output_root.resolve()
        self.seed = seed
        self.worker: threading.Thread | None = None
        self.control: RunControl | None = None
        self.signals = WorkerSignals()
        self.signals.progress.connect(self._on_progress)
        self.signals.scheduler_event.connect(self._on_scheduler_event)
        self.signals.finished.connect(self._on_finished)
        self.signals.failed.connect(self._on_failed)
        self.setWindowTitle("StreamForge - Local Multi-bitrate Lab")
        self.resize(1120, 760)
        self._build_ui()
        self._load_trace()

    def _build_ui(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)
        form = QFormLayout()
        self.samples_edit = QLineEdit(str(self.samples_root))
        self.output_edit = QLineEdit(str(self.output_root))
        self.snapshot_edit = QLineEdit(str(self.output_root / "snapshot.json"))
        browse_samples = QPushButton("Browse")
        browse_output = QPushButton("Browse")
        browse_snapshot = QPushButton("Browse")
        browse_samples.clicked.connect(self._browse_samples)
        browse_output.clicked.connect(self._browse_output)
        browse_snapshot.clicked.connect(self._browse_snapshot)
        form.addRow("Samples", self._with_button(self.samples_edit, browse_samples))
        form.addRow("Output", self._with_button(self.output_edit, browse_output))
        form.addRow("Snapshot", self._with_button(self.snapshot_edit, browse_snapshot))
        layout.addLayout(form)

        controls = QHBoxLayout()
        self.generate_button = QPushButton("Generate Samples")
        self.start_button = QPushButton("Start")
        self.pause_button = QPushButton("Pause")
        self.cancel_button = QPushButton("Cancel")
        self.restore_checkbox = QCheckBox("Restore snapshot")
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)
        self.generate_button.clicked.connect(self._generate)
        self.start_button.clicked.connect(self._start)
        self.pause_button.clicked.connect(self._toggle_pause)
        self.cancel_button.clicked.connect(self._cancel)
        controls.addWidget(self.generate_button)
        controls.addWidget(self.start_button)
        controls.addWidget(self.pause_button)
        controls.addWidget(self.cancel_button)
        controls.addWidget(self.restore_checkbox)
        controls.addStretch(1)
        layout.addLayout(controls)

        self.table = QTableWidget(0, 4)
        self.table.setHorizontalHeaderLabels(["Tier", "Completed", "Total", "State"])
        self.table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        self.table.setMaximumHeight(190)
        layout.addWidget(self.table)

        self.chart = MemoryChart()
        layout.addWidget(self.chart)
        self.trace = QPlainTextEdit()
        self.trace.setReadOnly(True)
        self.trace.setStyleSheet(
            "QPlainTextEdit { background:#0b1117; color:#c6d5e2; "
            "font-family: Consolas, monospace; }"
        )
        layout.addWidget(self.trace, 1)
        self.status = QLabel("Idle")
        layout.addWidget(self.status)
        self.setCentralWidget(central)

    @staticmethod
    def _with_button(edit: QLineEdit, button: QPushButton) -> QWidget:
        widget = QWidget()
        row = QHBoxLayout(widget)
        row.setContentsMargins(0, 0, 0, 0)
        row.addWidget(edit, 1)
        row.addWidget(button)
        return widget

    def _browse_samples(self) -> None:
        selected = QFileDialog.getExistingDirectory(self, "Samples directory")
        if selected:
            self.samples_edit.setText(selected)

    def _browse_output(self) -> None:
        selected = QFileDialog.getExistingDirectory(self, "Output directory")
        if selected:
            self.output_edit.setText(selected)
            self.snapshot_edit.setText(str(Path(selected) / "snapshot.json"))

    def _browse_snapshot(self) -> None:
        selected, _ = QFileDialog.getOpenFileName(
            self, "Snapshot", str(self.output_root), "JSON (*.json)"
        )
        if selected:
            self.snapshot_edit.setText(selected)

    def _generate(self) -> None:
        try:
            paths = generate_samples(
                Path(self.samples_edit.text()),
                seed=self.seed,
                overwrite=True,
            )
            self._append(f"Generated samples in {paths['root']}")
        except Exception as exc:  # GUI boundary
            self._append(f"Generation failed: {exc}")

    def _start(self) -> None:
        if self.worker is not None and self.worker.is_alive():
            error = BusinessError(
                ErrorCode.BUSY,
                "a slicing run is already active",
            )
            self._append(json.dumps(error.to_dict(), sort_keys=True))
            return
        self.control = RunControl()
        self.chart.samples.clear()
        self.start_button.setEnabled(False)
        self.pause_button.setEnabled(True)
        self.cancel_button.setEnabled(True)
        self.status.setText("Running")
        sample_root = Path(self.samples_edit.text()).resolve()
        output_root = Path(self.output_edit.text()).resolve()
        restore = (
            Path(self.snapshot_edit.text())
            if self.restore_checkbox.isChecked()
            else None
        )
        control = self.control
        self.worker = threading.Thread(
            target=self._run_worker,
            args=(sample_root, output_root, restore, control),
            daemon=True,
            name="streamforge-gui-run",
        )
        self.worker.start()

    def _run_worker(
        self,
        sample_root: Path,
        output_root: Path,
        restore: Path | None,
        control: RunControl,
    ) -> None:
        try:
            config_path = sample_root / "track_config.json"
            budget_path = sample_root / "budgets.json"
            if not config_path.exists():
                generate_samples(sample_root, seed=self.seed, overwrite=False)
            pipeline = Pipeline(
                config_path=config_path,
                budget_path=budget_path,
                output_root=output_root,
                control=control,
                on_progress=self.signals.progress.emit,
                on_event=self.signals.scheduler_event.emit,
            )
            run_result = pipeline.run(restore_snapshot=restore)
            artifacts = load_artifacts_from_output(output_root, pipeline.project)
            playback = PlaybackConfig.from_dict(
                json.loads((sample_root / "playback.json").read_text(encoding="utf-8"))
            )
            playback_result = simulate_playback(
                tiers=pipeline.project.tiers,
                slices=artifacts,
                bandwidth=load_bandwidth(sample_root / "bandwidth.json"),
                playback=playback,
                budget=pipeline.project.budget,
            )
            playback_trace = output_root / "playback_trace.jsonl"
            lines = [canonical_json(event) for event in playback_result.events]
            atomic_write(
                playback_trace,
                b"\n".join(lines) + (b"\n" if lines else b""),
            )
            self.signals.finished.emit(
                {
                    "run": run_result.to_dict(),
                    "playback": {
                        "events": playback_result.events,
                        "errors": playback_result.errors,
                        "final_tier": playback_result.final_tier,
                        "switch_count": playback_result.switch_count,
                    },
                }
            )
        except BusinessError as exc:
            self.signals.failed.emit(exc.to_dict())
        except Exception as exc:  # GUI boundary
            self.signals.failed.emit(
                {
                    "code": ErrorCode.IO,
                    "details": {"error": repr(exc)},
                    "message": str(exc),
                }
            )

    def _on_progress(self, payload: dict[str, Any]) -> None:
        tiers = payload.get("tiers", {})
        self.table.setRowCount(len(tiers))
        for row, (tier_id, state) in enumerate(sorted(tiers.items())):
            values = [
                tier_id,
                str(state.get("completed", 0)),
                str(state.get("total", 0)),
                "HALTED" if state.get("halted") else "RUNNING",
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setFlags(item.flags() & ~Qt.ItemFlag.ItemIsEditable)
                self.table.setItem(row, column, item)
        self.chart.set_data(
            int(payload.get("allocated_bytes", 0)),
            int(payload.get("peak_bytes", 0)),
            self.chart.limit,
        )
        self.status.setText(
            f"Step {payload.get('step', 0)} | "
            f"allocated {int(payload.get('allocated_bytes', 0)):,} B | "
            f"peak {int(payload.get('peak_bytes', 0)):,} B"
        )

    def _on_finished(self, payload: dict[str, Any]) -> None:
        self._set_idle()
        self._load_trace()
        playback = payload.get("playback", {})
        for event in playback.get("events", []):
            self._append("playback " + json.dumps(event, sort_keys=True))
        run = payload.get("run", {})
        self.status.setText(
            f"Run {run.get('status')} | playback {playback.get('final_tier')} | "
            f"switches {playback.get('switch_count')}"
        )
        for error in playback.get("errors", []):
            self._append("playback error " + json.dumps(error, sort_keys=True))

    def _on_scheduler_event(self, payload: dict[str, Any]) -> None:
        event = payload.get("event")
        if event in {
            "budget_rejected",
            "memory_degraded",
            "starvation_warning",
            "task_waiting",
            "tier_halted",
        }:
            self._append("scheduler " + json.dumps(payload, sort_keys=True))

    def _on_failed(self, payload: dict[str, Any]) -> None:
        self._set_idle()
        self._append(json.dumps(payload, sort_keys=True))
        self.status.setText(str(payload.get("code", "failed")))

    def _set_idle(self) -> None:
        self.start_button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)
        self.pause_button.setText("Pause")

    def _toggle_pause(self) -> None:
        if self.control is None:
            return
        if self.control.paused:
            self.control.resume()
            self.pause_button.setText("Pause")
            self.status.setText("Running")
        else:
            self.control.pause()
            self.pause_button.setText("Resume")
            self.status.setText("Paused")

    def _cancel(self) -> None:
        if self.control is not None:
            self.control.cancel()
            self.status.setText("Cancelling cooperatively...")

    def _load_trace(self) -> None:
        self.trace.clear()
        for name in ("scheduler_trace.jsonl", "playback_trace.jsonl"):
            path = Path(self.output_edit.text()) / name
            if path.exists():
                self.trace.appendPlainText(f"--- {name} ---")
                self.trace.appendPlainText(path.read_text(encoding="utf-8").rstrip())

    def _append(self, text: str) -> None:
        self.trace.appendPlainText(text)

    def closeEvent(self, event: QCloseEvent) -> None:
        if self.control is not None:
            self.control.cancel()
        if self.worker is not None and self.worker.is_alive():
            self.worker.join(timeout=3.0)
            if self.worker.is_alive():
                self.status.setText(
                    "Waiting for cooperative cancellation and file-handle release..."
                )
                event.ignore()
                return
        event.accept()


def create_main_window(
    *,
    samples_root: Path,
    output_root: Path,
    seed: int = 20261121,
) -> MainWindow:
    return MainWindow(
        samples_root=samples_root,
        output_root=output_root,
        seed=seed,
    )


def launch_gui(*, samples_root: Path, output_root: Path, seed: int) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    window = create_main_window(
        samples_root=samples_root,
        output_root=output_root,
        seed=seed,
    )
    window.show()
    return app.exec()
