from __future__ import annotations

import sys
import threading
from pathlib import Path
from typing import Any

from PySide6.QtCore import QThread, Qt, Signal
from PySide6.QtGui import QColor, QFont
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QFileDialog,
    QFormLayout,
    QFrame,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from .engine import TranscodeEngine
from .errors import E_BUSY, E_SIDECAR_DRIFT, TaskError
from .models import TierParams, sha256_bytes
from .samples import generate_fixtures


class TierWorker(QThread):
    progress_event = Signal(dict)
    result_ready = Signal(dict)
    failed = Signal(dict)

    def __init__(
        self,
        engine: TranscodeEngine,
        wav_path: str,
        tier: TierParams,
        cancel_event: threading.Event,
    ):
        super().__init__()
        self.engine = engine
        self.wav_path = wav_path
        self.tier = tier
        self.cancel_event = cancel_event

    def run(self) -> None:
        try:
            result = self.engine.generate_tier(
                self.wav_path,
                self.tier,
                cancel_event=self.cancel_event,
                progress=self.progress_event.emit,
            )
            self.result_ready.emit(
                {
                    "run_id": result.run_id,
                    "run_dir": str(result.run_dir),
                    "cancelled": result.cancelled,
                    "resumed": result.resumed,
                    "resume_index": result.resume_index,
                    "tier_version_hash": result.tier_version_hash,
                    "generation": result.generation,
                    "generation_hash": result.generation_hash,
                    "total_slices": (
                        result.sidecar["total_slices"] if result.sidecar else None
                    ),
                    "reused_slices": result.reused_slices,
                    "recomputed_slices": result.recomputed_slices,
                    "reused_audio_slices": result.reused_audio_slices,
                    "regenerated_audio_slices": result.regenerated_audio_slices,
                    "sidecar_recomputed_slices": result.sidecar_recomputed_slices,
                }
            )
        except TaskError as exc:
            self.failed.emit(exc.as_dict())
        except Exception as exc:  # defensive UI boundary
            self.failed.emit(
                {
                    "code": "E_GUI_WORKER",
                    "message": f"{type(exc).__name__}: {exc}",
                }
            )


class AuditWorker(QThread):
    result_ready = Signal(dict)
    failed = Signal(dict)

    def __init__(self, engine: TranscodeEngine, wav_path: str, tier_id: str):
        super().__init__()
        self.engine = engine
        self.wav_path = wav_path
        self.tier_id = tier_id

    def run(self) -> None:
        try:
            report = self.engine.audit_published_tier(self.wav_path, self.tier_id)
            self.result_ready.emit(report)
        except TaskError as exc:
            self.failed.emit(exc.as_dict())


class SliceCell(QLabel):
    COLORS = {
        "pending": "#5f6672",
        "recomputed": "#2f8f5b",
        "reused": "#3f72af",
        "recovered": "#7462a8",
        "cancelled": "#b17a22",
        "drift": "#b73c4a",
    }

    def __init__(self, index: int):
        super().__init__(str(index))
        self.index = index
        self.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.setFixedSize(38, 24)
        self.setFont(QFont("monospace", 8))
        self.set_status("pending", "not generated")

    def set_status(self, status: str, tooltip: str) -> None:
        color = self.COLORS.get(status, "#5f6672")
        self.setStyleSheet(
            f"background:{color};color:#ffffff;border-radius:3px;"
            "border:1px solid rgba(255,255,255,0.15);"
        )
        self.setToolTip(f"slice {self.index}: {tooltip}")


class TierPanel(QWidget):
    def __init__(self, engine: TranscodeEngine, tier_id: str, defaults: dict[str, int]):
        super().__init__()
        self.engine = engine
        self.tier_id = tier_id
        self.worker: TierWorker | None = None
        self.audit_worker: AuditWorker | None = None
        self.cancel_event = threading.Event()
        self.cells: list[SliceCell] = []
        self.last_version_hash: str | None = None
        self.current_wav: str = ""

        layout = QVBoxLayout(self)
        parameters = QGroupBox("Tier parameters")
        form = QFormLayout(parameters)
        self.rate = QSpinBox()
        self.rate.setRange(8000, 192000)
        self.rate.setSingleStep(100)
        self.rate.setValue(defaults["rate"])
        self.filter_length = QSpinBox()
        self.filter_length.setRange(8, 1024)
        self.filter_length.setSingleStep(2)
        self.filter_length.setValue(defaults["filter"])
        self.quant = QComboBox()
        self.quant.addItems(["16", "24"])
        self.quant.setCurrentText(str(defaults["quant"]))
        self.slice_samples = QSpinBox()
        self.slice_samples.setRange(64, 8192)
        self.slice_samples.setSingleStep(64)
        self.slice_samples.setValue(defaults["slice"])
        form.addRow("Target sample rate", self.rate)
        form.addRow("Filter length", self.filter_length)
        form.addRow("Quantization bits", self.quant)
        form.addRow("Slice samples", self.slice_samples)
        layout.addWidget(parameters)

        controls = QHBoxLayout()
        self.start_button = QPushButton("Start / Resume")
        self.cancel_button = QPushButton("Cooperative cancel")
        self.audit_button = QPushButton("Consistency audit")
        self.cancel_button.setEnabled(False)
        controls.addWidget(self.start_button)
        controls.addWidget(self.cancel_button)
        controls.addWidget(self.audit_button)
        layout.addLayout(controls)

        self.status = QLabel("Waiting to start")
        self.status.setWordWrap(True)
        layout.addWidget(self.status)
        self.progress = QProgressBar()
        self.progress.setRange(0, 1)
        self.progress.setValue(0)
        layout.addWidget(self.progress)
        self.version_label = QLabel("Parameter version: -")
        self.version_label.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse)
        layout.addWidget(self.version_label)

        self.grid_host = QWidget()
        self.grid = QGridLayout(self.grid_host)
        self.grid.setSpacing(3)
        self.grid.setAlignment(Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignTop)
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setWidget(self.grid_host)
        scroll.setFrameShape(QFrame.Shape.StyledPanel)
        layout.addWidget(scroll, 1)

        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(400)
        self.log.setFixedHeight(118)
        layout.addWidget(self.log)

        self.start_button.clicked.connect(self.start)
        self.cancel_button.clicked.connect(self.cancel)
        self.audit_button.clicked.connect(self.audit)

    def set_wav(self, path: str) -> None:
        self.current_wav = path

    def _append(self, text: str) -> None:
        self.log.appendPlainText(text)

    def _configured_tier(self) -> TierParams:
        filter_length = self.filter_length.value()
        if filter_length % 2:
            filter_length += 1
            self.filter_length.setValue(filter_length)
        return self.engine.catalog.configure(
            self.tier_id,
            target_rate=self.rate.value(),
            filter_length=filter_length,
            quant_bits=int(self.quant.currentText()),
            slice_samples=self.slice_samples.value(),
        )

    def _ensure_cells(self, total: int) -> None:
        if len(self.cells) >= total:
            return
        for index in range(len(self.cells), total):
            cell = SliceCell(index)
            self.grid.addWidget(cell, index // 20, index % 20)
            self.cells.append(cell)
        self.progress.setRange(0, total)

    def start(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self._append("E_BUSY: this tier is already running")
            self.status.setText("E_BUSY: duplicate start for the same tier")
            return
        if not self.current_wav:
            self._append("Load a WAV fixture first")
            return
        try:
            tier = self._configured_tier()
        except TaskError as exc:
            self._append(str(exc))
            return
        self.version_label.setText(
            f"Parameter version: {tier.param_version}  "
            f"rate={tier.target_rate} L={tier.filter_length} "
            f"q={tier.quant_bits} slice={tier.slice_samples}"
        )
        self.cancel_event = threading.Event()
        self.status.setText("Starting...")
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.progress.setValue(0)
        self.worker = TierWorker(
            self.engine,
            self.current_wav,
            tier,
            self.cancel_event,
        )
        self.worker.progress_event.connect(self._on_progress)
        self.worker.result_ready.connect(self._on_result)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self._on_thread_finished)
        self.worker.start()

    def cancel(self) -> None:
        self.cancel_event.set()
        self.status.setText("Cancel requested; waiting for the current slice commit...")

    def audit(self) -> None:
        if not self.current_wav:
            self._append("Load a WAV fixture first")
            return
        if self.audit_worker is not None and self.audit_worker.isRunning():
            self._append("An audit is already running")
            return
        self.status.setText("Auditing every slice...")
        self.audit_worker = AuditWorker(
            self.engine,
            self.current_wav,
            self.tier_id,
        )
        self.audit_worker.result_ready.connect(self._on_audit_ok)
        self.audit_worker.failed.connect(self._on_audit_failed)
        self.audit_worker.start()

    def _on_progress(self, event: dict[str, Any]) -> None:
        kind = event.get("event")
        if kind == "recovery":
            total = int(event.get("total_slices", 0))
            start = int(event.get("resume_index", 0))
            self._ensure_cells(total)
            for index in range(min(start, len(self.cells))):
                self.cells[index].set_status("recovered", "verified committed prefix")
            self.progress.setValue(start)
            self.status.setText(f"Recovery scan passed: resuming at {start}/{total}")
            self._append(f"Recovery scan: committed prefix {start}/{total}")
            return
        if kind == "slice":
            total = int(event.get("total_slices", 0))
            index = int(event.get("slice_index", 0))
            self._ensure_cells(total)
            status = event.get("status", "pending")
            hash_prefix = str(event.get("content_hash", ""))[:10]
            phase = event.get("phase_remainder_num")
            self.cells[index].set_status(
                status,
                f"phase_remainder={phase} hash={hash_prefix}",
            )
            self.progress.setValue(index + 1)
            self.status.setText(
                f"Slice {index + 1}/{total} | {status} | phase={phase}"
            )
            return
        if kind == "cancelled":
            self.status.setText(
                f"Cancelled; {event.get('committed_slices')} committed slices are resumable"
            )
            for index, cell in enumerate(self.cells):
                if index >= int(event.get("committed_slices", 0)):
                    cell.set_status("cancelled", "not committed; resumable")
            return
        if kind == "complete":
            self.status.setText(
                f"Published generation={event.get('generation')} "
                f"hash={str(event.get('tier_version_hash', ''))[:12]}"
            )

    def _on_result(self, result: dict[str, Any]) -> None:
        self.start_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        if result.get("cancelled"):
            self.status.setText("Cooperatively cancelled; snapshot retained for resume")
            return
        self.last_version_hash = result.get("tier_version_hash")
        self.status.setText(
            "Complete | version="
            f"{str(result.get('tier_version_hash', ''))[:12]} · "
            f"audio reused {result.get('reused_audio_slices', 0)} | "
            f"audio regenerated {result.get('regenerated_audio_slices', 0)} | "
            f"sidecars rewritten {result.get('sidecar_recomputed_slices', 0)}"
        )
        for cell in self.cells:
            if cell.toolTip().startswith(f"slice {cell.index}: not generated"):
                cell.set_status("recomputed", "committed")
        self._append(
            f"Complete run={result.get('run_id')} generation={result.get('generation')} "
            f"version={result.get('tier_version_hash')}"
        )

    def _on_failed(self, error: dict[str, Any]) -> None:
        self.start_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        code = error.get("code", "E_UNKNOWN")
        message = error.get("message", "")
        self.status.setText(f"{code}: {message}")
        self._append(f"{code}: {message}")
        if code == E_BUSY:
            self.status.setText("E_BUSY: duplicate start for the same tier")

    def _on_audit_ok(self, report: dict[str, Any]) -> None:
        self.status.setText(
            f"Audit passed | generation={report.get('generation')} | "
            f"version={str(report.get('tier_version_hash', ''))[:12]}"
        )
        for cell in self.cells:
            if not cell.toolTip().startswith(f"slice {cell.index}: not generated"):
                cell.set_status("recomputed", "consistency audit passed")
        self._append("Consistency audit passed: every sidecar matches its buffer")

    def _on_audit_failed(self, error: dict[str, Any]) -> None:
        code = error.get("code", "E_UNKNOWN")
        self._append(f"Audit failed {code}: {error.get('message', '')}")
        details = error.get("details", {})
        report = details.get("report", details)
        drifting = report.get("drifting_slices", [])
        for item in drifting:
            index = int(item.get("index", -1))
            if 0 <= index < len(self.cells):
                stale = str(item.get("stale_side", "unknown"))
                self.cells[index].set_status(
                    "drift",
                    f"drift stale_side={stale} "
                    f"deviation_samples={item.get('deviation_samples')}",
                )
        self.status.setText(
            f"{code} | first={report.get('first_drift_slice')} "
            f"stale_side={report.get('stale_side')}"
        )

    def _on_thread_finished(self) -> None:
        if not self.cancel_event.is_set():
            self.start_button.setEnabled(True)
            self.cancel_button.setEnabled(False)


class ReviewWindow(QMainWindow):
    DEFAULT_TIERS = (
        ("review44", {"rate": 44100, "filter": 48, "quant": 16, "slice": 2048}),
        ("review24", {"rate": 48000, "filter": 64, "quant": 24, "slice": 2048}),
        ("surround", {"rate": 32000, "filter": 48, "quant": 24, "slice": 1024}),
    )

    def __init__(self, store_root: str, sample_root: str):
        super().__init__()
        self.store_root = Path(store_root)
        self.sample_root = Path(sample_root)
        self.engine = TranscodeEngine(self.store_root)
        self.setWindowTitle("Live Transcode Review")
        self.resize(1240, 850)
        central = QWidget()
        self.setCentralWidget(central)
        layout = QVBoxLayout(central)

        header = QGroupBox("Source material")
        header_layout = QHBoxLayout(header)
        self.wav_path = QLineEdit()
        self.wav_path.setPlaceholderText("Select a 48 kHz 16/24-bit mono or 5.1 WAV")
        browse = QPushButton("Browse")
        load = QPushButton("Load")
        make_samples = QPushButton("Generate fixtures")
        header_layout.addWidget(self.wav_path, 1)
        header_layout.addWidget(browse)
        header_layout.addWidget(load)
        header_layout.addWidget(make_samples)
        layout.addWidget(header)

        self.tabs = QTabWidget()
        self.panels: dict[str, TierPanel] = {}
        for tier_id, defaults in self.DEFAULT_TIERS:
            panel = TierPanel(self.engine, tier_id, defaults)
            self.panels[tier_id] = panel
            self.tabs.addTab(panel, tier_id)
        layout.addWidget(self.tabs, 1)

        self.global_log = QPlainTextEdit()
        self.global_log.setReadOnly(True)
        self.global_log.setMaximumBlockCount(300)
        self.global_log.setFixedHeight(90)
        layout.addWidget(self.global_log)

        browse.clicked.connect(self._browse)
        load.clicked.connect(self._load_current)
        make_samples.clicked.connect(self._generate_samples)
        if (self.sample_root / "mono_16bit_seed19093.wav").exists():
            self.wav_path.setText(str(self.sample_root / "mono_16bit_seed19093.wav"))

        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #1f232b; color: #e8ebf0; }
            QGroupBox { border: 1px solid #404754; border-radius: 5px; margin-top: 8px; }
            QGroupBox::title { subcontrol-origin: margin; left: 8px; padding: 0 4px; }
            QPushButton { background: #343c49; border: 1px solid #566173; border-radius: 4px; padding: 5px 9px; }
            QPushButton:hover { background: #414b5d; }
            QPushButton:disabled { color: #7e8795; }
            QLineEdit, QSpinBox, QComboBox, QPlainTextEdit {
                background: #151920; color: #e8ebf0; border: 1px solid #485163; border-radius: 3px; padding: 3px;
            }
            QProgressBar { border: 1px solid #485163; border-radius: 3px; text-align: center; }
            QProgressBar::chunk { background: #3f72af; }
            QTabWidget::pane { border: 1px solid #404754; }
            """
        )

    def _browse(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self, "Select WAV", str(self.sample_root), "WAV (*.wav)"
        )
        if path:
            self.wav_path.setText(path)
            self.wav_path.setCursorPosition(0)

    def _load_current(self) -> None:
        path = self.wav_path.text().strip()
        if not path:
            return
        for panel in self.panels.values():
            panel.set_wav(path)
        self.global_log.appendPlainText(f"Loaded: {path}")

    def _generate_samples(self) -> None:
        try:
            fixtures = generate_fixtures(self.sample_root)
        except Exception as exc:
            QMessageBox.critical(self, "Fixture generation failed", str(exc))
            return
        first = fixtures[0]["path"]
        self.wav_path.setText(first)
        self._load_current()
        self.global_log.appendPlainText(
            f"Generated {len(fixtures)} deterministic fixtures in {self.sample_root}"
        )

    def closeEvent(self, event) -> None:  # noqa: N802 - Qt API
        for panel in self.panels.values():
            panel.cancel_event.set()
            if panel.worker is not None and panel.worker.isRunning():
                panel.worker.wait(3000)
            if panel.audit_worker is not None and panel.audit_worker.isRunning():
                panel.audit_worker.wait(1000)
        event.accept()


def run_gui(*, store_root: str = "artifacts/review_store", sample_root: str = "artifacts/samples") -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    window = ReviewWindow(store_root, sample_root)
    window.show()
    return app.exec()

