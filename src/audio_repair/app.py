"""Professional PySide6 desktop shell for resumable spectral repair."""

from __future__ import annotations

import hashlib
import sys
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QThread, QTimer, Qt, QUrl, Signal
from PySide6.QtGui import QColor, QFont, QPalette
from PySide6.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFrame,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QSizePolicy,
    QSlider,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .audio_io import AudioData, load_audio, write_wav_atomic
from .config import RepairConfig, SessionConfig, default_config
from .engine import ProgressEvent, RepairSession, RunStatus, SessionResult
from .errors import AppError
from .metrics import audio_stats
from .segments import SegmentRecord, SegmentStatus
from .ui.widgets import SpectrogramView, WaveformView

try:
    from PySide6.QtMultimedia import QAudioOutput, QMediaPlayer
except Exception:  # pragma: no cover - optional backend on stripped Linux builds
    QAudioOutput = None  # type: ignore[assignment]
    QMediaPlayer = None  # type: ignore[assignment]


STATUS_LABELS = {
    SegmentStatus.PENDING: "Pending",
    SegmentStatus.PROCESSING: "Processing",
    SegmentStatus.COMMITTED: "Committed",
    SegmentStatus.FAILED: "Failed",
}

STATUS_COLORS = {
    SegmentStatus.PENDING: QColor("#7d8ea3"),
    SegmentStatus.PROCESSING: QColor("#f0b44c"),
    SegmentStatus.COMMITTED: QColor("#36ddbb"),
    SegmentStatus.FAILED: QColor("#ff687d"),
}


class EngineWorker(QThread):
    progress = Signal(object)
    segment = Signal(object)
    completed = Signal(object)
    failed = Signal(object)

    def __init__(
        self,
        session: RepairSession,
        *,
        retry_failed: bool = False,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.session = session
        self.retry_failed = retry_failed

    def run(self) -> None:
        self.session.set_callbacks(
            progress_callback=self.progress.emit,
            segment_callback=self.segment.emit,
        )
        try:
            result = self.session.run(retry_failed=self.retry_failed)
        except AppError as exc:
            self.failed.emit(exc.to_dict())
        except BaseException as exc:  # noqa: BLE001 - surface worker faults in UI
            self.failed.emit(
                {
                    "ok": False,
                    "code": "E_INTERNAL",
                    "message": "processing worker terminated",
                    "details": {"reason": str(exc)},
                }
            )
        else:
            self.completed.emit(result)


class MainWindow(QMainWindow):
    def __init__(self) -> None:
        super().__init__()
        self.setWindowTitle("Resona Restore · Segment-aware Audio Repair")
        self.resize(1540, 940)
        self.setMinimumSize(1120, 720)
        self.audio: AudioData | None = None
        self.session: RepairSession | None = None
        self.worker: EngineWorker | None = None
        self.current_segment = 0
        self.preview_output: np.ndarray | None = None
        self.preview_index: int | None = None
        self._player: Any = None
        self._audio_output: Any = None
        self._build_ui()
        self._apply_style()
        self._set_running_state(False)
        QTimer.singleShot(120, self._load_default_sample)

    def _build_ui(self) -> None:
        root = QWidget()
        self.setCentralWidget(root)
        root_layout = QVBoxLayout(root)
        root_layout.setContentsMargins(16, 14, 16, 14)
        root_layout.setSpacing(10)

        header = QHBoxLayout()
        brand = QLabel("RESONA")
        brand.setObjectName("brand")
        subtitle = QLabel("SEGMENT-AWARE SPECTRAL RESTORATION")
        subtitle.setObjectName("subtitle")
        header.addWidget(brand)
        header.addWidget(subtitle)
        header.addStretch(1)
        self.session_badge = QLabel("NO SESSION")
        self.session_badge.setObjectName("badge")
        header.addWidget(self.session_badge)
        root_layout.addLayout(header)

        body = QSplitter(Qt.Orientation.Horizontal)
        body.setChildrenCollapsible(False)
        root_layout.addWidget(body, 1)
        body.addWidget(self._build_left_panel())
        body.addWidget(self._build_right_panel())
        body.setSizes([360, 1170])

    def _build_left_panel(self) -> QWidget:
        panel = QWidget()
        panel.setObjectName("panel")
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(14, 14, 14, 14)
        layout.setSpacing(10)

        open_row = QHBoxLayout()
        self.open_button = QPushButton("Open local WAV")
        self.open_button.clicked.connect(self._choose_file)
        self.sample_combo = QComboBox()
        self.sample_combo.addItems(["Music fixture", "Speech fixture", "Boundary fixture"])
        self.sample_button = QPushButton("Load fixture")
        self.sample_button.clicked.connect(self._load_selected_sample)
        open_row.addWidget(self.open_button)
        open_row.addWidget(self.sample_combo)
        open_row.addWidget(self.sample_button)
        layout.addLayout(open_row)

        self.file_label = QLabel("No audio loaded")
        self.file_label.setObjectName("fileLabel")
        self.file_label.setWordWrap(True)
        layout.addWidget(self.file_label)

        self.meta_label = QLabel("")
        self.meta_label.setObjectName("metaLabel")
        layout.addWidget(self.meta_label)

        segment_header = QHBoxLayout()
        segment_title = QLabel("SEGMENT LEDGER")
        segment_title.setObjectName("sectionTitle")
        self.load_info_label = QLabel("")
        self.load_info_label.setObjectName("metaLabel")
        segment_header.addWidget(segment_title)
        segment_header.addStretch(1)
        segment_header.addWidget(self.load_info_label)
        layout.addLayout(segment_header)

        self.table = QTableWidget(0, 4)
        self.table.setHorizontalHeaderLabels(["#", "Sample range", "State", "Cfg"])
        self.table.verticalHeader().setVisible(False)
        self.table.setSelectionBehavior(QTableWidget.SelectionBehavior.SelectRows)
        self.table.setSelectionMode(QTableWidget.SelectionMode.SingleSelection)
        self.table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        self.table.setAlternatingRowColors(True)
        self.table.horizontalHeader().setSectionResizeMode(0, QHeaderView.ResizeMode.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(1, QHeaderView.ResizeMode.Stretch)
        self.table.horizontalHeader().setSectionResizeMode(2, QHeaderView.ResizeMode.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(3, QHeaderView.ResizeMode.ResizeToContents)
        self.table.itemSelectionChanged.connect(self._on_segment_selected)
        layout.addWidget(self.table, 1)

        progress_header = QHBoxLayout()
        self.progress_label = QLabel("Committed 0 / 0")
        self.progress_label.setObjectName("sectionTitle")
        self.resume_label = QLabel("")
        self.resume_label.setObjectName("metaLabel")
        progress_header.addWidget(self.progress_label)
        progress_header.addStretch(1)
        progress_header.addWidget(self.resume_label)
        layout.addLayout(progress_header)
        self.progress_bar = QProgressBar()
        self.progress_bar.setRange(0, 1)
        self.progress_bar.setValue(0)
        layout.addWidget(self.progress_bar)

        path_title = QLabel("WORKSPACE PATHS")
        path_title.setObjectName("sectionTitle")
        layout.addWidget(path_title)
        self.path_label = QLabel("checkpoints: -\nexports: -")
        self.path_label.setObjectName("pathLabel")
        self.path_label.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse)
        layout.addWidget(self.path_label)
        return panel

    def _build_right_panel(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(0, 0, 0, 0)
        layout.setSpacing(10)

        controls = QGroupBox("DSP PARAMETERS · APPLY TO UNCOMMITTED SEGMENTS ONLY")
        grid = QGridLayout(controls)
        grid.setContentsMargins(14, 20, 14, 12)
        grid.setHorizontalSpacing(10)
        grid.setVerticalSpacing(8)

        self.profile_combo = QComboBox()
        self.profile_combo.addItems(["music", "speech"])
        self.profile_combo.currentTextChanged.connect(self._apply_profile_defaults)
        grid.addWidget(QLabel("Preset"), 0, 0)
        grid.addWidget(self.profile_combo, 0, 1)

        self.strength_slider = QSlider(Qt.Orientation.Horizontal)
        self.strength_slider.setRange(0, 400)
        self.strength_slider.setValue(135)
        self.strength_spin = QDoubleSpinBox()
        self.strength_spin.setRange(0.0, 4.0)
        self.strength_spin.setSingleStep(0.05)
        self.strength_spin.setDecimals(2)
        self.strength_spin.setValue(1.35)
        self.strength_slider.valueChanged.connect(
            lambda value: self.strength_spin.setValue(value / 100.0)
        )
        self.strength_spin.valueChanged.connect(
            lambda value: self.strength_slider.setValue(int(round(value * 100)))
        )
        self.strength_spin.valueChanged.connect(self._push_config_to_session)
        grid.addWidget(QLabel("Strength"), 0, 2)
        grid.addWidget(self.strength_slider, 0, 3, 1, 2)
        grid.addWidget(self.strength_spin, 0, 5)

        self.frame_combo = QComboBox()
        for size in (256, 512, 1024, 2048, 4096, 8192):
            self.frame_combo.addItem(str(size), size)
        self.frame_combo.setCurrentText("2048")
        self.hop_spin = QSpinBox()
        self.hop_spin.setRange(1, 8192)
        self.hop_spin.setValue(512)
        self.window_combo = QComboBox()
        self.window_combo.addItems(["hann", "hamming", "blackman", "rectangular"])
        self.frame_combo.currentIndexChanged.connect(self._push_config_to_session)
        self.hop_spin.valueChanged.connect(self._push_config_to_session)
        self.window_combo.currentTextChanged.connect(self._push_config_to_session)
        grid.addWidget(QLabel("Frame"), 1, 0)
        grid.addWidget(self.frame_combo, 1, 1)
        grid.addWidget(QLabel("Hop"), 1, 2)
        grid.addWidget(self.hop_spin, 1, 3)
        grid.addWidget(QLabel("Window"), 1, 4)
        grid.addWidget(self.window_combo, 1, 5)

        self.floor_spin = QDoubleSpinBox()
        self.floor_spin.setRange(0.0, 0.9)
        self.floor_spin.setSingleStep(0.01)
        self.floor_spin.setDecimals(2)
        self.floor_spin.setValue(0.04)
        self.noise_frames_spin = QSpinBox()
        self.noise_frames_spin.setRange(1, 512)
        self.noise_frames_spin.setValue(12)
        self.noise_mode_combo = QComboBox()
        self.noise_mode_combo.addItems(["leading", "silence"])
        self.floor_spin.valueChanged.connect(self._push_config_to_session)
        self.noise_frames_spin.valueChanged.connect(self._push_config_to_session)
        self.noise_mode_combo.currentTextChanged.connect(self._push_config_to_session)
        grid.addWidget(QLabel("Gain floor"), 2, 0)
        grid.addWidget(self.floor_spin, 2, 1)
        grid.addWidget(QLabel("Noise frames"), 2, 2)
        grid.addWidget(self.noise_frames_spin, 2, 3)
        grid.addWidget(QLabel("Noise mode"), 2, 4)
        grid.addWidget(self.noise_mode_combo, 2, 5)

        self.segment_spin = QSpinBox()
        self.segment_spin.setRange(256, 8_000_000)
        self.segment_spin.setValue(16384)
        self.segment_spin.setSingleStep(1024)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 8)
        self.workers_spin.setValue(4)
        self.memory_spin = QSpinBox()
        self.memory_spin.setRange(4, 1024)
        self.memory_spin.setValue(64)
        self.memory_spin.setSuffix(" MiB")
        self.segment_spin.setToolTip("Locked after loading; affects snapshot boundaries")
        grid.addWidget(QLabel("Segment"), 3, 0)
        grid.addWidget(self.segment_spin, 3, 1)
        grid.addWidget(QLabel("Workers"), 3, 2)
        grid.addWidget(self.workers_spin, 3, 3)
        grid.addWidget(QLabel("Mem cap"), 3, 4)
        grid.addWidget(self.memory_spin, 3, 5)
        grid.setColumnStretch(6, 1)
        layout.addWidget(controls)

        actions = QFrame()
        actions.setObjectName("actionBar")
        action_layout = QHBoxLayout(actions)
        action_layout.setContentsMargins(8, 6, 8, 6)
        action_layout.setSpacing(8)
        self.start_button = QPushButton("Start / Resume")
        self.start_button.setObjectName("primary")
        self.start_button.clicked.connect(self._start_processing)
        self.pause_button = QPushButton("Pause")
        self.pause_button.clicked.connect(self._pause)
        self.resume_button = QPushButton("Resume")
        self.resume_button.clicked.connect(self._resume)
        self.stop_button = QPushButton("Stop")
        self.stop_button.clicked.connect(self._stop)
        self.retry_button = QPushButton("Retry failed")
        self.retry_button.clicked.connect(self._retry_failed)
        self.preview_button = QPushButton("Preview")
        self.preview_button.clicked.connect(self._preview_current)
        self.audition_original_button = QPushButton("Original")
        self.audition_original_button.clicked.connect(lambda: self._audition(False))
        self.audition_repaired_button = QPushButton("Repaired")
        self.audition_repaired_button.clicked.connect(lambda: self._audition(True))
        self.export_button = QPushButton("Export WAV")
        self.export_button.clicked.connect(self._export)
        self.reset_button = QPushButton("Reset full")
        self.reset_button.clicked.connect(self._reset_and_process)
        for button in (
            self.start_button,
            self.pause_button,
            self.resume_button,
            self.stop_button,
            self.retry_button,
            self.preview_button,
            self.audition_original_button,
            self.audition_repaired_button,
            self.export_button,
        ):
            action_layout.addWidget(button)
        action_layout.addStretch(1)
        action_layout.addWidget(self.reset_button)
        layout.addWidget(actions)

        visuals = QSplitter(Qt.Orientation.Vertical)
        visuals.setChildrenCollapsible(False)
        self.waveform_view = WaveformView()
        self.spectrum_view = SpectrogramView()
        visuals.addWidget(self.waveform_view)
        visuals.addWidget(self.spectrum_view)
        visuals.setSizes([310, 300])
        layout.addWidget(visuals, 1)

        log_header = QHBoxLayout()
        log_title = QLabel("ACTIVITY / AUDIT")
        log_title.setObjectName("sectionTitle")
        self.status_label = QLabel("Ready")
        self.status_label.setObjectName("statusLabel")
        log_header.addWidget(log_title)
        log_header.addStretch(1)
        log_header.addWidget(self.status_label)
        layout.addLayout(log_header)
        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(600)
        self.log.setFixedHeight(106)
        layout.addWidget(self.log)
        return panel

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            * {
                font-family: "Segoe UI", "Microsoft YaHei UI", "Noto Sans CJK SC",
                             "DejaVu Sans", sans-serif;
                font-size: 12px;
                color: #c8d5e3;
            }
            QMainWindow, QWidget { background: #0b1016; }
            #panel, QGroupBox, #actionBar {
                background: #111821;
                border: 1px solid #253140;
                border-radius: 7px;
            }
            QGroupBox {
                margin-top: 5px;
                padding-top: 8px;
                font-weight: 600;
                color: #9fb2c7;
            }
            QGroupBox::title {
                subcontrol-origin: margin;
                left: 12px;
                padding: 0 4px;
            }
            #brand {
                font-size: 25px;
                font-weight: 800;
                letter-spacing: 4px;
                color: #e6f2ff;
            }
            #subtitle {
                font-size: 10px;
                letter-spacing: 2px;
                color: #4d9f91;
                padding-left: 9px;
            }
            #badge {
                background: #10221f;
                color: #36ddbb;
                border: 1px solid #24554d;
                border-radius: 10px;
                padding: 4px 10px;
                font-size: 10px;
                font-weight: 700;
            }
            #sectionTitle {
                color: #6f8ca8;
                font-size: 10px;
                font-weight: 700;
                letter-spacing: 1px;
            }
            #fileLabel {
                color: #e6f2ff;
                font-size: 13px;
                font-weight: 600;
            }
            #metaLabel, #pathLabel, #statusLabel {
                color: #7e91a6;
                font-size: 10px;
            }
            QPushButton {
                background: #182330;
                border: 1px solid #2a394a;
                border-radius: 5px;
                padding: 7px 11px;
                color: #c9d7e6;
            }
            QPushButton:hover { background: #203142; border-color: #3d5b72; }
            QPushButton:pressed { background: #0e1720; }
            QPushButton:disabled { color: #526172; background: #111821; border-color: #1d2732; }
            QPushButton#primary {
                background: #167e6b;
                border-color: #36ddbb;
                color: #f1fffc;
                font-weight: 700;
            }
            QPushButton#primary:hover { background: #1b9880; }
            QPushButton#primary:disabled {
                background: #12211e;
                border-color: #24443e;
                color: #52756e;
            }
            QLineEdit, QComboBox, QSpinBox, QDoubleSpinBox {
                background: #0d141c;
                border: 1px solid #2a394a;
                border-radius: 4px;
                padding: 5px 7px;
                min-height: 19px;
                selection-background-color: #1c7f6e;
            }
            QComboBox::drop-down { border: 0; width: 18px; }
            QTableWidget {
                background: #0d141c;
                alternate-background-color: #101923;
                border: 1px solid #253140;
                gridline-color: #1c2733;
                selection-background-color: #193b38;
                selection-color: #e8fffa;
            }
            QHeaderView::section {
                background: #141e29;
                color: #71879d;
                border: 0;
                border-bottom: 1px solid #2a394a;
                padding: 6px;
                font-size: 10px;
                font-weight: 700;
            }
            QProgressBar {
                background: #0b1117;
                border: 1px solid #263442;
                border-radius: 4px;
                height: 12px;
                text-align: center;
                color: #9fb2c7;
                font-size: 9px;
            }
            QProgressBar::chunk {
                background: #1ea98f;
                border-radius: 3px;
            }
            QSlider::groove:horizontal {
                height: 4px;
                background: #22303f;
                border-radius: 2px;
            }
            QSlider::handle:horizontal {
                background: #36ddbb;
                width: 14px;
                margin: -5px 0;
                border-radius: 7px;
            }
            QPlainTextEdit {
                background: #080d12;
                border: 1px solid #202b37;
                border-radius: 4px;
                color: #8da1b6;
                font-family: "Cascadia Mono", "Consolas", "DejaVu Sans Mono", monospace;
                font-size: 10px;
            }
            QSplitter::handle { background: #0b1016; width: 8px; height: 8px; }
            QScrollBar:vertical {
                background: #0d141c; width: 10px; margin: 0;
            }
            QScrollBar::handle:vertical {
                background: #2b3a49; min-height: 24px; border-radius: 5px;
            }
            QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical { height: 0; }
            """
        )

    def _workspace_root(self) -> Path:
        return Path.cwd()

    def _sample_paths(self) -> dict[str, Path]:
        sample_dir = self._workspace_root() / "artifacts" / "samples"
        return {
            "music": sample_dir / "music_noisy_stereo.wav",
            "speech": sample_dir / "speech_noisy_mono.wav",
            "edge": sample_dir / "boundary_stereo_tail1.wav",
        }

    def _load_default_sample(self) -> None:
        path = self._sample_paths()["music"]
        if path.exists() and self.audio is None:
            self.load_audio_path(path)

    def _choose_file(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self, "Open local WAV", str(self._workspace_root()), "WAV audio (*.wav)"
        )
        if path:
            self.load_audio_path(Path(path))

    def _load_selected_sample(self) -> None:
        keys = ["music", "speech", "edge"]
        path = self._sample_paths()[keys[self.sample_combo.currentIndex()]]
        if not path.exists():
            QMessageBox.warning(
                self,
                "Fixture not found",
                f"Could not find {path}. Run scripts/generate_samples.py first.",
            )
            return
        self.load_audio_path(path)

    def load_audio_path(self, path: Path) -> bool:
        if self.worker is not None:
            QMessageBox.warning(
                self,
                "Job running",
                "Stop the active job before loading another audio file.",
            )
            return False
        try:
            audio = load_audio(path)
        except AppError as exc:
            self._show_error(exc.to_dict())
            return False
        self.audio = audio
        self.profile_combo.blockSignals(True)
        profile = "speech" if "speech" in path.name.lower() else "music"
        self.profile_combo.setCurrentText(profile)
        self.profile_combo.blockSignals(False)
        self._apply_profile_defaults(profile)
        base = default_config(profile)
        segment_samples = 4096 if "boundary" in path.name else 16384
        self.segment_spin.setValue(segment_samples)
        checkpoint = (
            self._workspace_root()
            / "artifacts"
            / "ui_checkpoints"
            / f"{audio.source_sha256[:12]}_{segment_samples}"
        )
        try:
            session = RepairSession(
                audio,
                base,
                SessionConfig(
                    segment_samples=segment_samples,
                    max_workers=self.workers_spin.value(),
                    max_buffer_bytes=self.memory_spin.value() * 1024 * 1024,
                ),
                checkpoint,
                clear_checkpoint=False,
            )
        except AppError as exc:
            self._show_error(exc.to_dict())
            return False
        self.session = session
        self.preview_output = None
        self.preview_index = None
        self.file_label.setText(path.name)
        self.meta_label.setText(
            f"{audio.sample_rate:,} Hz · {audio.channels} ch · "
            f"{audio.total_samples:,} samples · {audio.duration_seconds:.3f} s"
        )
        self.segment_spin.setEnabled(False)
        self.path_label.setText(
            f"checkpoints: {checkpoint}\n"
            f"exports: {self._workspace_root() / 'artifacts' / 'ui_exports'}"
        )
        self._populate_table()
        self._update_progress()
        info = session.load_info
        if info.state == "invalidated":
            self._log(
                f"Snapshot version/identity mismatch; whole snapshot invalidated: {info.reason}",
                "WARN",
            )
            self.load_info_label.setText("SNAPSHOT INVALIDATED")
        elif session.load_info.loaded_count:
            self.load_info_label.setText(f"RESUMED {session.load_info.loaded_count}")
            self._log(
                f"Loaded checkpoint: resumed {session.load_info.loaded_count} committed segments",
                "RESUME",
            )
        else:
            self.load_info_label.setText("FRESH SNAPSHOT")
            self._log(f"Fresh snapshot: {len(session.specs)} segments", "READY")
        self.session_badge.setText(f"{len(session.specs)} SEGMENTS")
        self._select_row(0)
        self._set_running_state(False)
        return True

    def _populate_table(self) -> None:
        assert self.session is not None
        records = self.session.state_snapshot()
        self.table.setRowCount(len(records))
        for row, record in enumerate(records.values()):
            spec = record.spec
            index_item = QTableWidgetItem(f"{spec.index:02d}")
            index_item.setData(Qt.ItemDataRole.UserRole, spec.index)
            range_item = QTableWidgetItem(
                f"{spec.start_sample:,}–{spec.end_sample - 1:,}"
            )
            status_item = QTableWidgetItem(STATUS_LABELS[record.status])
            status_item.setForeground(STATUS_COLORS[record.status])
            config_item = QTableWidgetItem(
                (record.config_fingerprint or "-")[:8]
            )
            config_item.setToolTip(record.config_fingerprint or "Not committed")
            for column, item in enumerate(
                (index_item, range_item, status_item, config_item)
            ):
                self.table.setItem(row, column, item)

    def _record_for_row(self, row: int) -> SegmentRecord | None:
        if self.session is None or row < 0:
            return None
        records = self.session.state_snapshot()
        return records.get(row)

    def _update_record(self, record: SegmentRecord) -> None:
        if record.spec.index >= self.table.rowCount():
            return
        row = record.spec.index
        status_item = self.table.item(row, 2)
        config_item = self.table.item(row, 3)
        if status_item is not None:
            status_item.setText(STATUS_LABELS[record.status])
            status_item.setForeground(STATUS_COLORS[record.status])
            if record.error:
                status_item.setToolTip(record.error.get("message", ""))
        if config_item is not None:
            config_item.setText((record.config_fingerprint or "-")[:8])
            config_item.setToolTip(record.config_fingerprint or "Not committed")
        self._update_progress()
        if row == self.current_segment and record.status == SegmentStatus.COMMITTED:
            self._show_segment(row, force_reload=True)

    def _update_progress(self) -> None:
        if self.session is None:
            return
        committed, total = self.session.state_machine.progress()
        self.progress_label.setText(f"Committed {committed} / {total}")
        self.progress_bar.setRange(0, max(1, total))
        self.progress_bar.setValue(committed)
        self.progress_bar.setFormat(f"{committed}/{total}")
        self.resume_label.setText(
            f"resumed {self.session.load_info.loaded_count}"
            if self.session.load_info.loaded_count
            else ""
        )

    def _on_segment_selected(self) -> None:
        rows = self.table.selectionModel().selectedRows()
        if not rows:
            return
        row = rows[0].row()
        if row == self.current_segment and self.preview_index is not None:
            return
        self.current_segment = row
        self.preview_output = None
        self.preview_index = None
        self._show_segment(row)

    def _select_row(self, row: int) -> None:
        if 0 <= row < self.table.rowCount():
            self.table.selectRow(row)
            self.current_segment = row
            self.preview_output = None
            self.preview_index = None
            self._show_segment(row)

    def _show_segment(
        self,
        index: int,
        *,
        force_reload: bool = False,
        repaired_override: np.ndarray | None = None,
    ) -> None:
        assert self.audio is not None and self.session is not None
        if index < 0 or index >= len(self.session.specs):
            return
        spec = self.session.specs[index]
        original = self.audio.slice(spec.start_sample, spec.end_sample)
        repaired = repaired_override
        record = self.session.state_machine.get(index)
        if repaired is None and record.status == SegmentStatus.COMMITTED:
            try:
                repaired = self.session.store.load_segment(index).output
            except AppError as exc:
                self._show_error(exc.to_dict())
                repaired = None
            self.preview_output = repaired
            self.preview_index = index
        elif (
            repaired is None
            and self.preview_index == index
            and self.preview_output is not None
        ):
            repaired = self.preview_output
        config = self.session.current_config()
        range_text = (
            f"{spec.start_sample:,}–{spec.end_sample - 1:,} · "
            f"{record.status.value}"
        )
        self.waveform_view.set_audio(
            original,
            repaired,
            self.audio.sample_rate,
            title=f"Segment {index:02d} · waveform",
            range_text=range_text,
        )
        self.spectrum_view.set_audio(
            original,
            repaired,
            self.audio.sample_rate,
            title=f"Segment {index:02d} · STFT before / after",
            config=config,
        )

    def _current_config(self) -> RepairConfig:
        return RepairConfig(
            frame_size=int(self.frame_combo.currentData()),
            hop_size=self.hop_spin.value(),
            window=self.window_combo.currentText(),
            strength=self.strength_spin.value(),
            floor=self.floor_spin.value(),
            noise_frames=self.noise_frames_spin.value(),
            noise_mode=self.noise_mode_combo.currentText(),
            freq_smooth=5 if self.profile_combo.currentText() == "music" else 7,
            temporal_smooth=0.25 if self.profile_combo.currentText() == "music" else 0.5,
        ).validate()

    def _apply_profile_defaults(self, profile: str) -> None:
        config = default_config(profile if profile in ("music", "speech") else "music")
        self.frame_combo.blockSignals(True)
        self.hop_spin.blockSignals(True)
        self.window_combo.blockSignals(True)
        self.strength_spin.blockSignals(True)
        self.strength_slider.blockSignals(True)
        self.floor_spin.blockSignals(True)
        self.noise_frames_spin.blockSignals(True)
        self.noise_mode_combo.blockSignals(True)
        self.frame_combo.setCurrentText(str(config.frame_size))
        self.hop_spin.setValue(config.hop_size)
        self.window_combo.setCurrentText(config.window)
        self.strength_spin.setValue(config.strength)
        self.strength_slider.setValue(int(config.strength * 100))
        self.floor_spin.setValue(config.floor)
        self.noise_frames_spin.setValue(config.noise_frames)
        self.noise_mode_combo.setCurrentText(config.noise_mode)
        self.frame_combo.blockSignals(False)
        self.hop_spin.blockSignals(False)
        self.window_combo.blockSignals(False)
        self.strength_spin.blockSignals(False)
        self.strength_slider.blockSignals(False)
        self.floor_spin.blockSignals(False)
        self.noise_frames_spin.blockSignals(False)
        self.noise_mode_combo.blockSignals(False)
        self._push_config_to_session()

    def _push_config_to_session(self) -> None:
        if self.session is None:
            return
        try:
            config = self._current_config()
        except AppError as exc:
            self._status_label(exc.message, error=True)
            return
        self.session.set_repair_config(config)
        self._log(
            f"Parameters updated for uncommitted segments only · strength={config.strength:.2f} "
            f"frame={config.frame_size} hop={config.hop_size}",
            "CONFIG",
        )
        if self.worker is not None:
            self._status_label("Parameter change applies to the next uncommitted batch")

    def _session_config(self) -> SessionConfig:
        return SessionConfig(
            segment_samples=self.segment_spin.value(),
            max_workers=self.workers_spin.value(),
            max_buffer_bytes=self.memory_spin.value() * 1024 * 1024,
        ).validate(self.audio.channels if self.audio else 1)

    def _start_processing(self) -> None:
        if self.session is None:
            QMessageBox.information(
                self, "No audio", "Load a local WAV file or a deterministic fixture first."
            )
            return
        if self.worker is not None:
            return
        try:
            self.session.set_repair_config(self._current_config())
            resources = self._session_config()
            self.session.set_resource_limits(
                resources.max_workers, resources.max_buffer_bytes
            )
        except AppError as exc:
            self._show_error(exc.to_dict())
            return
        self._set_running_state(True)
        self._status_label("Processing · checkpoints commit serially")
        self._log("Started segmented spectral repair", "RUN")
        self.worker = EngineWorker(self.session, parent=self)
        self.worker.progress.connect(self._on_progress)
        self.worker.segment.connect(self._update_record)
        self.worker.completed.connect(self._on_worker_completed)
        self.worker.failed.connect(self._on_worker_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.worker.start()

    def _pause(self) -> None:
        if self.session is not None and self.worker is not None:
            self.session.request_pause()
            self.start_button.setEnabled(False)
            self.pause_button.setEnabled(False)
            self.resume_button.setEnabled(True)
            self._status_label("Pause requested · will stop at a committed boundary")
            self._log("Pause requested: draining the current batch", "PAUSE")

    def _resume(self) -> None:
        if self.session is not None:
            self.session.request_resume()
            self.resume_button.setEnabled(False)
            self.pause_button.setEnabled(True)
            self._status_label("Resuming")
            self._log("Resuming from the durable segment boundary", "RESUME")

    def _stop(self) -> None:
        if self.session is not None:
            self.session.request_stop()
            self._status_label("Stop requested · durable commits only")
            self._log("Stop requested: no complete exit report will be produced", "STOP")
            self.stop_button.setEnabled(False)
            self.pause_button.setEnabled(False)
            self.resume_button.setEnabled(False)

    def _retry_failed(self) -> None:
        if self.session is None or self.worker is not None:
            return
        try:
            self.session.set_repair_config(self._current_config())
        except AppError as exc:
            self._show_error(exc.to_dict())
            return
        self._set_running_state(True)
        self._status_label("Retrying failed segments")
        self.worker = EngineWorker(self.session, retry_failed=True, parent=self)
        self.worker.progress.connect(self._on_progress)
        self.worker.segment.connect(self._update_record)
        self.worker.completed.connect(self._on_worker_completed)
        self.worker.failed.connect(self._on_worker_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.worker.start()

    def _on_progress(self, event: ProgressEvent) -> None:
        self._update_progress()
        if event.status == RunStatus.PAUSED:
            self._status_label("Paused · no segment is processing")
            self.resume_button.setEnabled(True)
            self.pause_button.setEnabled(False)
        elif event.status == RunStatus.RUNNING:
            self._status_label(f"Processing · {event.committed}/{event.total} committed")
        if event.failed_indices:
            self.retry_button.setEnabled(True)

    def _on_worker_completed(self, result: SessionResult) -> None:
        if result.complete:
            self._status_label("All segments committed · export is ready")
            self._log(
                f"Completed {result.committed}/{result.total} segments · "
                f"{result.computed_this_run} computed this run · {result.resumed} resumed",
                "DONE",
            )
        elif result.status == RunStatus.STOPPED:
            self._status_label("Stopped · no complete exit report")
            self._log(
                f"Stopped: only {result.committed}/{result.total} committed segments are recoverable; "
                "no complete report was generated",
                "STOPPED",
            )
        else:
            self._status_label(result.status.value)
            self._log(result.message, result.status.value)

    def _on_worker_failed(self, payload: dict[str, Any]) -> None:
        self._show_error(payload)

    def _on_worker_finished(self) -> None:
        self._set_running_state(False)
        self._update_progress()
        if self.session is not None:
            failures = self.session.state_machine.indexes_with({SegmentStatus.FAILED})
            self.retry_button.setEnabled(bool(failures))
        worker = self.worker
        self.worker = None
        if worker is not None:
            worker.deleteLater()

    def _set_running_state(self, running: bool) -> None:
        pending: list[int] = []
        failed: list[int] = []
        complete = False
        if self.session is not None:
            pending = self.session.state_machine.indexes_with({SegmentStatus.PENDING})
            failed = self.session.state_machine.indexes_with({SegmentStatus.FAILED})
            complete = not pending and not failed
        self.start_button.setEnabled(
            not running and self.audio is not None and bool(pending)
        )
        self.pause_button.setEnabled(running)
        self.resume_button.setEnabled(False)
        self.stop_button.setEnabled(running)
        self.retry_button.setEnabled(not running and bool(failed))
        self.preview_button.setEnabled(not running and self.audio is not None)
        self.audition_original_button.setEnabled(self.audio is not None)
        self.audition_repaired_button.setEnabled(not running and self.audio is not None)
        self.export_button.setEnabled(
            not running and self.audio is not None and complete
        )
        self.reset_button.setEnabled(not running and self.audio is not None)
        self.profile_combo.setEnabled(not running)
        self.strength_spin.setEnabled(True)
        self.strength_slider.setEnabled(True)
        self.frame_combo.setEnabled(True)
        self.hop_spin.setEnabled(True)
        self.window_combo.setEnabled(True)
        self.floor_spin.setEnabled(True)
        self.noise_frames_spin.setEnabled(True)
        self.noise_mode_combo.setEnabled(True)
        self.workers_spin.setEnabled(not running)
        self.memory_spin.setEnabled(not running)

    def _preview_current(self) -> None:
        if self.session is None:
            return
        try:
            config = self._current_config()
            original, repaired, result = self.session.preview_segment(
                self.current_segment, config
            )
        except AppError as exc:
            self._show_error(exc.to_dict())
            return
        self.preview_output = repaired
        self.preview_index = self.current_segment
        self._show_segment(self.current_segment, repaired_override=repaired)
        self._status_label(
            f"Preview segment {self.current_segment:02d} · not persisted · "
            f"gain {result.mean_gain:.3f} · {result.attenuation_db:.1f} dB"
            if np.isfinite(result.mean_gain)
            else f"Preview segment {self.current_segment:02d} · committed snapshot",
        )

    def _audition(self, repaired: bool) -> None:
        if self.audio is None or self.session is None:
            return
        if QMediaPlayer is None or QAudioOutput is None:
            self._show_error(
                {
                    "code": "E_INTERNAL",
                    "message": "This PySide6 installation has no usable QtMultimedia backend",
                }
            )
            return
        index = self.current_segment
        spec = self.session.specs[index]
        if repaired:
            record = self.session.state_machine.get(index)
            if record.status == SegmentStatus.COMMITTED:
                data = self.session.store.load_segment(index).output
            else:
                try:
                    _original, data, _result = self.session.preview_segment(
                        index, self._current_config()
                    )
                except AppError as exc:
                    self._show_error(exc.to_dict())
                    return
        else:
            data = self.audio.slice(spec.start_sample, spec.end_sample)
        audition_dir = self._workspace_root() / "artifacts" / "audition"
        audition_dir.mkdir(parents=True, exist_ok=True)
        target = audition_dir / (
            f"segment_{index:04d}_{'repaired' if repaired else 'original'}.wav"
        )
        try:
            write_wav_atomic(target, data, self.audio.sample_rate)
        except AppError as exc:
            self._show_error(exc.to_dict())
            return
        if self._player is None:
            self._player = QMediaPlayer(self)
            self._audio_output = QAudioOutput(self)
            self._player.setAudioOutput(self._audio_output)
            self._audio_output.setVolume(0.75)
        self._player.setSource(QUrl.fromLocalFile(str(target)))
        self._player.play()
        self._status_label(
            f"Audition segment {index:02d} · {'repaired' if repaired else 'original'}"
        )

    def _export(self) -> None:
        if self.session is None:
            return
        try:
            self.session.collect_outputs()
        except AppError as exc:
            self._show_error(exc.to_dict())
            return
        export_dir = self._workspace_root() / "artifacts" / "ui_exports"
        export_dir.mkdir(parents=True, exist_ok=True)
        stem = Path(self.audio.source_path).stem if self.audio else "audio"
        suggested = export_dir / f"{stem}_repaired.wav"
        path, _ = QFileDialog.getSaveFileName(
            self, "Export repaired WAV", str(suggested), "WAV audio (*.wav)"
        )
        if not path:
            return
        try:
            stats = self.session.export(path)
        except AppError as exc:
            self._show_error(exc.to_dict())
            return
        self._status_label("Export complete")
        self._log(
            f"Exported {path} · peak {stats.peak_dbfs:.2f} dBFS · "
            f"RMS {stats.rms_dbfs:.2f} dBFS · LUFS {stats.integrated_lufs:.2f}",
            "EXPORT",
        )

    def _reset_and_process(self) -> None:
        if self.audio is None:
            return
        answer = QMessageBox.question(
            self,
            "Reset checkpoint",
            "This deletes the current checkpoint directory and recomputes every "
            "segment from scratch. Continue?",
            QMessageBox.StandardButton.Yes | QMessageBox.StandardButton.No,
        )
        if answer != QMessageBox.StandardButton.Yes:
            return
        try:
            session_config = self._session_config()
            session = RepairSession(
                self.audio,
                self._current_config(),
                session_config,
                self.session.checkpoint_dir,
                clear_checkpoint=True,
            )
        except (AppError, IndexError) as exc:
            self._show_error(
                exc.to_dict()
                if isinstance(exc, AppError)
                else {"code": "E_IO", "message": str(exc)}
            )
            return
        self.session = session
        self.preview_output = None
        self.preview_index = None
        self.segment_spin.setEnabled(False)
        self._populate_table()
        self._update_progress()
        self.load_info_label.setText("FRESH SNAPSHOT")
        self._log("Historical snapshot cleared; full recompute started", "RESET")
        self._start_processing()

    def _show_error(self, payload: dict[str, Any]) -> None:
        code = payload.get("code", "E_INTERNAL")
        message = payload.get("message", "unknown error")
        self._status_label(f"{code} · {message}", error=True)
        self._log(f"{code}: {message} {payload.get('details', '')}", "ERROR")
        QMessageBox.critical(self, f"Business error {code}", message)

    def _status_label(self, text: str, *, error: bool = False) -> None:
        self.status_label.setText(text)
        self.status_label.setStyleSheet(
            "color: #ff687d; font-size: 10px;"
            if error
            else "color: #7e91a6; font-size: 10px;"
        )

    def _log(self, message: str, tag: str = "INFO") -> None:
        self.log.appendPlainText(f"[{tag:<8}] {message}")

    def closeEvent(self, event: Any) -> None:  # noqa: N802 - Qt API
        if self.worker is not None:
            answer = QMessageBox.question(
                self,
                "Job still running",
                "Exiting stops scheduling and keeps only durable committed segments. "
                "No complete exit report will be generated. Stop and exit?",
                QMessageBox.StandardButton.Yes | QMessageBox.StandardButton.No,
            )
            if answer != QMessageBox.StandardButton.Yes:
                event.ignore()
                return
            self.session.request_stop() if self.session else None
            if not self.worker.wait(10000):
                QMessageBox.warning(
                    self,
                    "Batch is still draining",
                    "Stop was requested but the current batch has not finished. Wait before "
                    "closing, or force-terminate the process; restart will recover only "
                    "committed segments.",
                )
                event.ignore()
                return
        event.accept()


def _application() -> QApplication:
    app = QApplication.instance()
    if app is None:
        app = QApplication(sys.argv)
    app.setStyle("Fusion")
    palette = QPalette()
    palette.setColor(QPalette.ColorRole.Window, QColor("#0b1016"))
    palette.setColor(QPalette.ColorRole.WindowText, QColor("#c8d5e3"))
    palette.setColor(QPalette.ColorRole.Base, QColor("#0d141c"))
    palette.setColor(QPalette.ColorRole.Text, QColor("#c8d5e3"))
    palette.setColor(QPalette.ColorRole.Button, QColor("#182330"))
    palette.setColor(QPalette.ColorRole.ButtonText, QColor("#c9d7e6"))
    palette.setColor(QPalette.ColorRole.Highlight, QColor("#1c7f6e"))
    palette.setColor(QPalette.ColorRole.HighlightedText, QColor("#e8fffa"))
    app.setPalette(palette)
    app.setFont(QFont("DejaVu Sans", 9))
    return app


def main() -> int:
    app = _application()
    window = MainWindow()
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())

