"""PyQt5 desktop workbench for registration and panorama stitching."""

from __future__ import annotations

import json
import sys
import threading
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from PyQt5.QtCore import Qt, QThread, pyqtSignal
from PyQt5.QtGui import QColor, QImage, QPixmap
from PyQt5.QtWidgets import (
    QAction,
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
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
    QScrollArea,
    QSpinBox,
    QSplitter,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .config import BlendConfig, FeatureConfig, HomographyConfig, PipelineConfig, RuntimeConfig, StrategyConfig
from .dataset import Dataset, load_dataset
from .diagnostics import (
    match_diagnostic,
    registration_overlay,
    seam_overlay,
)
from .errors import WorkbenchError
from .geometry import invert_homography
from .pipeline import RunResult, StitchPipeline
from .seam import SeamPath


class PipelineWorker(QThread):
    event = pyqtSignal(object)
    completed = pyqtSignal(object)
    failed = pyqtSignal(object)

    def __init__(
        self,
        dataset_path: str,
        config: PipelineConfig,
        output_dir: str,
        resume: bool,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.dataset_path = dataset_path
        self.config = config
        self.output_dir = output_dir
        self.resume = resume
        self.pause_event = threading.Event()
        self.cancel_event = threading.Event()

    def request_pause(self) -> None:
        self.pause_event.set()

    def request_cancel(self) -> None:
        self.cancel_event.set()

    def run(self) -> None:
        try:
            dataset = load_dataset(self.dataset_path)
            pipeline = StitchPipeline(
                dataset,
                self.config,
                self.output_dir,
                resume=self.resume,
            )
            result = pipeline.run(
                pause_event=self.pause_event,
                cancel_event=self.cancel_event,
                progress=lambda payload: self.event.emit(payload),
            )
            self.completed.emit(result)
        except WorkbenchError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:  # pragma: no cover - GUI safety boundary
            self.failed.emit(
                {
                    "code": "E_UNEXPECTED",
                    "message": str(exc),
                }
            )


def _bgr_to_pixmap(image: np.ndarray) -> QPixmap:
    rgb = cv2.cvtColor(image, cv2.COLOR_BGR2RGB)
    height, width, channels = rgb.shape
    image_qt = QImage(
        rgb.data,
        width,
        height,
        channels * width,
        QImage.Format_RGB888,
    ).copy()
    return QPixmap.fromImage(image_qt)


class ImagePane(QScrollArea):
    def __init__(self) -> None:
        super().__init__()
        self.setWidgetResizable(True)
        self.setAlignment(Qt.AlignCenter)
        self.label = QLabel("No image loaded")
        self.label.setAlignment(Qt.AlignCenter)
        self.label.setStyleSheet("color:#8a95a7; padding:24px;")
        self.setWidget(self.label)

    def set_image(self, image: np.ndarray | None, message: str = "No image") -> None:
        if image is None:
            self.label.setPixmap(QPixmap())
            self.label.setText(message)
            return
        pixmap = _bgr_to_pixmap(image)
        available = self.viewport().size()
        maximum = pixmap.size()
        if maximum.width() > max(200, available.width() - 12) or maximum.height() > max(
            200, available.height() - 12
        ):
            maximum.scale(
                max(200, available.width() - 12),
                max(200, available.height() - 12),
                Qt.KeepAspectRatio,
            )
            pixmap = pixmap.scaled(
                maximum, Qt.KeepAspectRatio, Qt.SmoothTransformation
            )
        self.label.setText("")
        self.label.setPixmap(pixmap)

    def set_pixmap_file(self, path: Path, message: str) -> None:
        if not path.is_file():
            self.set_image(None, message)
            return
        pixmap = QPixmap(str(path))
        if pixmap.isNull():
            self.set_image(None, message)
            return
        available = self.viewport().size()
        if pixmap.width() > max(200, available.width() - 12) or pixmap.height() > max(
            200, available.height() - 12
        ):
            pixmap = pixmap.scaled(
                max(200, available.width() - 12),
                max(200, available.height() - 12),
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation,
            )
        self.label.setText("")
        self.label.setPixmap(pixmap)


class MainWindow(QMainWindow):
    def __init__(self, initial_dataset: str | None = None) -> None:
        super().__init__()
        self.setWindowTitle("PanoWorkbench - Deterministic Registration Studio")
        self.resize(1500, 940)
        self.dataset: Dataset | None = None
        self.result: RunResult | None = None
        self.worker: PipelineWorker | None = None
        self._diagnostic_cache: dict[str, np.ndarray] = {}
        self._build_ui()
        self._apply_style()
        self._set_running(False)
        if initial_dataset:
            self.load_dataset_path(initial_dataset)

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background:#111722; color:#e6edf7; }
            QGroupBox { border:1px solid #293447; border-radius:7px; margin-top:9px; padding-top:8px; font-weight:600; }
            QGroupBox::title { subcontrol-origin: margin; left:10px; padding:0 5px; color:#7dd3fc; }
            QPushButton { background:#1f6feb; border:none; border-radius:5px; padding:7px 12px; color:white; font-weight:600; }
            QPushButton:hover { background:#388bfd; }
            QPushButton:disabled { background:#273244; color:#6e7b90; }
            QPushButton#danger { background:#b42318; }
            QPushButton#secondary { background:#2b384c; }
            QLineEdit, QSpinBox, QDoubleSpinBox, QComboBox { background:#0b1019; border:1px solid #30405a; border-radius:4px; padding:4px; color:#e6edf7; }
            QTableWidget { background:#0b1019; alternate-background-color:#111a28; gridline-color:#263246; border:1px solid #293447; }
            QHeaderView::section { background:#1a2433; color:#9fb0c7; border:none; padding:5px; }
            QTabWidget::pane { border:1px solid #293447; }
            QTabBar::tab { background:#172131; color:#a9b7ca; padding:8px 14px; border-top-left-radius:5px; border-top-right-radius:5px; }
            QTabBar::tab:selected { background:#1f6feb; color:white; }
            QPlainTextEdit { background:#090d14; color:#b9c7da; border:1px solid #293447; font-family:Consolas,monospace; }
            QProgressBar { border:1px solid #30405a; border-radius:4px; text-align:center; background:#0b1019; }
            QProgressBar::chunk { background:#1f6feb; }
            """
        )

    def _build_ui(self) -> None:
        open_action = QAction("Open dataset", self)
        open_action.setShortcut("Ctrl+O")
        open_action.triggered.connect(self.choose_dataset)
        exit_action = QAction("Exit", self)
        exit_action.setShortcut("Ctrl+Q")
        exit_action.triggered.connect(self.close)
        file_menu = self.menuBar().addMenu("File")
        file_menu.addAction(open_action)
        file_menu.addSeparator()
        file_menu.addAction(exit_action)

        splitter = QSplitter(Qt.Horizontal)
        self.setCentralWidget(splitter)
        left = QWidget()
        left_layout = QVBoxLayout(left)
        left_layout.setContentsMargins(8, 8, 8, 8)
        left_layout.setSpacing(7)
        splitter.addWidget(left)

        path_group = QGroupBox("Dataset and output")
        path_form = QFormLayout(path_group)
        dataset_row = QHBoxLayout()
        self.dataset_edit = QLineEdit()
        self.dataset_edit.setReadOnly(True)
        browse_dataset = QPushButton("Browse")
        browse_dataset.setObjectName("secondary")
        browse_dataset.clicked.connect(self.choose_dataset)
        dataset_row.addWidget(self.dataset_edit, 1)
        dataset_row.addWidget(browse_dataset)
        path_form.addRow("Dataset", dataset_row)
        output_row = QHBoxLayout()
        self.output_edit = QLineEdit()
        browse_output = QPushButton("Browse")
        browse_output.setObjectName("secondary")
        browse_output.clicked.connect(self.choose_output)
        output_row.addWidget(self.output_edit, 1)
        output_row.addWidget(browse_output)
        path_form.addRow("Output", output_row)
        self.resume_check = QCheckBox("Resume version-matched checkpoint")
        self.resume_check.setChecked(True)
        path_form.addRow("Checkpoint", self.resume_check)
        left_layout.addWidget(path_group)

        parameter_group = QGroupBox("Algorithm parameters")
        parameter_form = QFormLayout(parameter_group)
        self.mode_combo = QComboBox()
        self.mode_combo.addItems(["auto", "full", "restricted"])
        parameter_form.addRow("Strategy", self.mode_combo)
        self.feature_spin = QSpinBox()
        self.feature_spin.setRange(32, 50000)
        self.feature_spin.setValue(5000)
        parameter_form.addRow("Feature budget", self.feature_spin)
        self.octave_spin = QSpinBox()
        self.octave_spin.setRange(1, 8)
        self.octave_spin.setValue(4)
        parameter_form.addRow("Scale layers", self.octave_spin)
        self.contrast_spin = QDoubleSpinBox()
        self.contrast_spin.setRange(0.00001, 0.5)
        self.contrast_spin.setDecimals(5)
        self.contrast_spin.setSingleStep(0.001)
        self.contrast_spin.setValue(0.012)
        parameter_form.addRow("Response threshold", self.contrast_spin)
        self.ratio_spin = QDoubleSpinBox()
        self.ratio_spin.setRange(0.40, 0.99)
        self.ratio_spin.setDecimals(3)
        self.ratio_spin.setValue(0.76)
        parameter_form.addRow("Ratio test", self.ratio_spin)
        self.min_matches_spin = QSpinBox()
        self.min_matches_spin.setRange(4, 5000)
        self.min_matches_spin.setValue(12)
        parameter_form.addRow("Minimum matches", self.min_matches_spin)
        self.reproj_spin = QDoubleSpinBox()
        self.reproj_spin.setRange(0.05, 50.0)
        self.reproj_spin.setDecimals(2)
        self.reproj_spin.setValue(3.0)
        parameter_form.addRow("RANSAC threshold", self.reproj_spin)
        self.consensus_spin = QDoubleSpinBox()
        self.consensus_spin.setRange(0.05, 50.0)
        self.consensus_spin.setDecimals(2)
        self.consensus_spin.setValue(5.0)
        parameter_form.addRow("Consensus envelope", self.consensus_spin)
        self.iterations_spin = QSpinBox()
        self.iterations_spin.setRange(50, 200000)
        self.iterations_spin.setValue(2400)
        parameter_form.addRow("RANSAC iterations", self.iterations_spin)
        self.min_inliers_spin = QSpinBox()
        self.min_inliers_spin.setRange(4, 100000)
        self.min_inliers_spin.setValue(10)
        parameter_form.addRow("Minimum inliers", self.min_inliers_spin)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 32)
        self.workers_spin.setValue(4)
        parameter_form.addRow("Max workers", self.workers_spin)
        self.cache_spin = QSpinBox()
        self.cache_spin.setRange(1, 128)
        self.cache_spin.setValue(8)
        parameter_form.addRow("Image buffers", self.cache_spin)
        left_layout.addWidget(parameter_group)

        buttons = QHBoxLayout()
        self.run_button = QPushButton("Run / Resume")
        self.run_button.clicked.connect(self.start_run)
        self.pause_button = QPushButton("Pause")
        self.pause_button.setObjectName("secondary")
        self.pause_button.clicked.connect(self.pause_run)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setObjectName("danger")
        self.cancel_button.clicked.connect(self.cancel_run)
        self.retry_button = QPushButton("Retry failed")
        self.retry_button.setObjectName("secondary")
        self.retry_button.clicked.connect(self.retry_failed)
        buttons.addWidget(self.run_button)
        buttons.addWidget(self.pause_button)
        buttons.addWidget(self.cancel_button)
        buttons.addWidget(self.retry_button)
        left_layout.addLayout(buttons)

        self.dataset_label = QLabel("No dataset")
        self.dataset_label.setStyleSheet("color:#9fb0c7;")
        left_layout.addWidget(self.dataset_label)
        self.pair_table = QTableWidget(0, 7)
        self.pair_table.setHorizontalHeaderLabels(
            ["Pair", "Images", "State", "Matches", "Inliers", "Median px", "Truth px"]
        )
        self.pair_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeToContents
        )
        self.pair_table.horizontalHeader().setStretchLastSection(True)
        self.pair_table.setSelectionBehavior(QTableWidget.SelectRows)
        self.pair_table.setSelectionMode(QTableWidget.SingleSelection)
        self.pair_table.setAlternatingRowColors(True)
        self.pair_table.itemSelectionChanged.connect(self.on_pair_selected)
        left_layout.addWidget(self.pair_table, 1)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.setContentsMargins(8, 8, 8, 8)
        splitter.addWidget(right)
        splitter.setStretchFactor(0, 0)
        splitter.setStretchFactor(1, 1)
        splitter.setSizes([660, 840])

        self.tabs = QTabWidget()
        self.panorama_pane = ImagePane()
        self.matches_pane = ImagePane()
        self.overlay_pane = ImagePane()
        self.seam_pane = ImagePane()
        self.heatmap_pane = ImagePane()
        self.log_view = QPlainTextEdit()
        self.log_view.setReadOnly(True)
        self.tabs.addTab(self.panorama_pane, "Panorama")
        self.tabs.addTab(self.matches_pane, "Feature matches")
        self.tabs.addTab(self.overlay_pane, "Registration overlay")
        self.tabs.addTab(self.seam_pane, "Seam lines")
        self.tabs.addTab(self.heatmap_pane, "Error heatmap")
        self.tabs.addTab(self.log_view, "Event log")
        right_layout.addWidget(self.tabs, 1)

        status_widget = QWidget()
        status_layout = QHBoxLayout(status_widget)
        status_layout.setContentsMargins(0, 0, 0, 0)
        self.status_label = QLabel("Ready")
        self.progress = QProgressBar()
        self.progress.setRange(0, 1)
        self.progress.setValue(0)
        status_layout.addWidget(self.status_label, 1)
        status_layout.addWidget(self.progress, 1)
        right_layout.addWidget(status_widget)

    def log(self, message: str) -> None:
        self.log_view.appendPlainText(message)

    def choose_dataset(self) -> None:
        path = QFileDialog.getExistingDirectory(self, "Choose generated dataset")
        if path:
            self.load_dataset_path(path)

    def choose_output(self) -> None:
        path = QFileDialog.getExistingDirectory(self, "Choose output directory")
        if path:
            self.output_edit.setText(path)

    def load_dataset_path(self, path: str) -> None:
        try:
            dataset = load_dataset(path)
        except WorkbenchError as exc:
            QMessageBox.critical(self, "Dataset error", str(exc))
            self.log(str(exc))
            return
        self.dataset = dataset
        self.dataset_edit.setText(str(dataset.root))
        if not self.output_edit.text().strip():
            self.output_edit.setText(str(dataset.root.parent / f"{dataset.root.name}_output"))
        canvas = f"{dataset.theoretical_width}x{dataset.theoretical_height}"
        self.dataset_label.setText(
            f"{dataset.name}: {dataset.image_count} images | truth canvas {canvas} | "
            f"seed {dataset.seed}"
        )
        self.status_label.setText("Dataset validated")
        self.log(f"Loaded {dataset.name}: {dataset.image_count} images, canvas {canvas}")
        for error in dataset.errors:
            self.log(f"Validation: {error}")
        self._diagnostic_cache.clear()

    def _config(self) -> PipelineConfig:
        return PipelineConfig(
            features=FeatureConfig(
                max_features=self.feature_spin.value(),
                octave_layers=self.octave_spin.value(),
                contrast_threshold=self.contrast_spin.value(),
                ratio_test=self.ratio_spin.value(),
                min_matches=self.min_matches_spin.value(),
            ),
            homography=HomographyConfig(
                reproj_threshold=self.reproj_spin.value(),
                consensus_threshold=self.consensus_spin.value(),
                max_iterations=self.iterations_spin.value(),
                min_inliers=self.min_inliers_spin.value(),
            ),
            strategy=StrategyConfig(mode=self.mode_combo.currentText()),
            blend=BlendConfig(),
            runtime=RuntimeConfig(
                max_workers=self.workers_spin.value(),
                max_cached_images=self.cache_spin.value(),
            ),
        ).validate()

    def _set_running(self, running: bool) -> None:
        self.run_button.setEnabled(not running)
        self.retry_button.setEnabled(not running)
        self.pause_button.setEnabled(running)
        self.cancel_button.setEnabled(running)

    def start_run(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            return
        dataset_path = self.dataset_edit.text().strip()
        output_path = self.output_edit.text().strip()
        if not dataset_path:
            QMessageBox.warning(self, "Missing dataset", "Choose a generated dataset first.")
            return
        if not output_path:
            QMessageBox.warning(self, "Missing output", "Choose an output directory first.")
            return
        try:
            config = self._config()
        except WorkbenchError as exc:
            QMessageBox.critical(self, "Parameter error", str(exc))
            return
        self._diagnostic_cache.clear()
        self.worker = PipelineWorker(
            dataset_path,
            config,
            output_path,
            self.resume_check.isChecked(),
            self,
        )
        self.worker.event.connect(self.on_worker_event)
        self.worker.completed.connect(self.on_worker_completed)
        self.worker.failed.connect(self.on_worker_failed)
        self.worker.finished.connect(lambda: self._set_running(False))
        self.progress.setRange(0, max(1, self._total_pairs_guess()))
        self.progress.setValue(0)
        self.status_label.setText("Running")
        self.log("Run started")
        self._set_running(True)
        self.worker.start()

    def _total_pairs_guess(self) -> int:
        if self.dataset is None:
            return 1
        count = self.dataset.image_count
        return max(1, count * (count - 1) // 2)

    def pause_run(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.request_pause()
            self.status_label.setText("Pause requested after current commit")
            self.log("Pause requested; committed pairs remain checkpointed")

    def cancel_run(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.request_cancel()
            self.status_label.setText("Cancel requested")
            self.log("Cancel requested; no unsaved pair will be reported as committed")

    def retry_failed(self) -> None:
        if self.result is None:
            self.start_run()
            return
        failed = [
            pair
            for pair in self.result.report.get("pairs", [])
            if pair.get("state") == "failed"
        ]
        if not failed:
            QMessageBox.information(self, "No failed pairs", "There are no failed pairs to retry.")
            return
        self.resume_check.setChecked(True)
        self.log(f"Retrying {len(failed)} failed pair(s) from persisted committed state")
        self.start_run()

    def on_worker_event(self, payload: dict[str, Any]) -> None:
        stage = payload.get("stage")
        committed = payload.get("committed", 0)
        total = payload.get("total", 1)
        if total:
            self.progress.setRange(0, max(1, total))
            self.progress.setValue(committed)
        if stage == "pair":
            pair_id = payload.get("pair_id")
            state = payload.get("state")
            message = f"{pair_id} {state}"
            if payload.get("error"):
                message += f" {payload['error']['code']}: {payload['error']['message']}"
            self.log(message)
            self.status_label.setText(f"{committed}/{total} pairs committed")
        elif stage in {"start", "checkpoint", "pause", "cancel", "panorama", "complete"}:
            message = payload.get("message") or payload.get("state") or stage
            self.log(f"{stage}: {message}")
            self.status_label.setText(message)
        elif stage == "warning":
            self.log(f"warning: {payload.get('message', '')}")

    def on_worker_completed(self, result: RunResult) -> None:
        self.result = result
        self._populate_report()
        self.status_label.setText(
            f"{result.status}: {result.report['progress']['committed_pairs']}/"
            f"{result.report['progress']['total_pairs']} pairs"
        )
        self.log(
            f"Run finished with status {result.status}; "
            f"panorama_complete={result.report['progress']['panorama_complete']}"
        )
        if result.status != "completed":
            self.log("Resume with the same parameter version to continue from committed pairs.")

    def on_worker_failed(self, error: dict[str, Any]) -> None:
        self.status_label.setText("Failed")
        self.log(f"{error.get('code')}: {error.get('message')}")
        QMessageBox.critical(
            self,
            "Workbench error",
            f"{error.get('code')}: {error.get('message')}",
        )

    def _populate_report(self) -> None:
        if self.result is None:
            return
        report = self.result.report
        pairs = report.get("pairs", [])
        self.pair_table.setRowCount(len(pairs))
        for row, pair in enumerate(pairs):
            result = pair.get("result") or {}
            state = pair.get("state", "")
            values = [
                pair.get("pair_id", ""),
                f"{pair.get('image_a')} - {pair.get('image_b')}",
                state,
                str(result.get("matches", pair.get("matches", ""))),
                str(result.get("inliers", "")),
                (
                    f"{result.get('residual_median', 0.0):.3f}"
                    if result
                    else ""
                ),
                (
                    f"{result.get('truth_corner_error', 0.0):.3f}"
                    if result and result.get("truth_corner_error") is not None
                    else ""
                ),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column in {3, 4, 5, 6}:
                    item.setTextAlignment(Qt.AlignRight | Qt.AlignVCenter)
                if state == "committed":
                    item.setBackground(QColor("#14351f"))
                elif state == "failed":
                    item.setBackground(QColor("#431b1b"))
                elif state == "registering":
                    item.setBackground(QColor("#3b3215"))
                self.pair_table.setItem(row, column, item)
        panorama = report.get("panorama") or {}
        if panorama.get("state") == "committed":
            path = self.result.output_dir / panorama.get("output_path", "panorama.png")
            self.panorama_pane.set_pixmap_file(
                path,
                f"Panorama unavailable; dimensions {panorama.get('width')}x{panorama.get('height')}",
            )
            self.heatmap_pane.set_pixmap_file(
                self.result.output_dir / "registration_error_heatmap.png",
                "No registration error heatmap",
            )
            self._show_seams(None)
        first_row = next(
            (index for index, pair in enumerate(pairs) if pair.get("state") == "failed"),
            next(
                (index for index, pair in enumerate(pairs) if pair.get("state") == "committed"),
                0 if pairs else -1,
            ),
        )
        if first_row >= 0:
            self.pair_table.selectRow(first_row)

    def on_pair_selected(self) -> None:
        rows = self.pair_table.selectionModel().selectedRows()
        if not rows or self.result is None:
            return
        row = rows[0].row()
        pairs = self.result.report.get("pairs", [])
        if row >= len(pairs):
            return
        pair = pairs[row]
        if pair.get("state") != "committed" or not pair.get("result"):
            self.matches_pane.set_image(None, f"{pair.get('pair_id')} is {pair.get('state')}")
            self.overlay_pane.set_image(None, "Pair is not committed")
            error = pair.get("error")
            if error:
                self.log(f"{pair.get('pair_id')}: {error.get('code')} {error.get('message')}")
            return
        self._show_pair_diagnostics(pair)
        self._show_seams(pair.get("pair_id"))

    def _diagnostic_feature_config(self) -> FeatureConfig:
        if self.result is not None:
            parameters = self.result.report.get("parameters", {}).get("features")
            strategy = self.result.report.get("strategy", {})
            if parameters:
                config = FeatureConfig(**parameters)
                return config.with_budget(
                    int(strategy.get("actual_feature_budget", config.max_features))
                )
        return self._config().features

    def _show_pair_diagnostics(self, pair: dict[str, Any]) -> None:
        assert self.dataset is not None
        cache_key = f"pair:{pair['pair_id']}"
        if cache_key not in self._diagnostic_cache:
            try:
                image_a = int(pair["image_a"])
                image_b = int(pair["image_b"])
                config = self._diagnostic_feature_config()
                visual = match_diagnostic(self.dataset, image_a, image_b, config)
                h_ab = np.asarray(
                    pair["result"]["homography_a_to_b"], dtype=np.float64
                ).reshape(3, 3)
                overlay = registration_overlay(
                    self.dataset,
                    image_a,
                    image_b,
                    invert_homography(h_ab),
                )
                self._diagnostic_cache[cache_key] = visual
                self._diagnostic_cache[f"overlay:{pair['pair_id']}"] = overlay
            except (WorkbenchError, KeyError, ValueError) as exc:
                self.matches_pane.set_image(None, f"Diagnostic unavailable: {exc}")
                self.overlay_pane.set_image(None, "Diagnostic unavailable")
                return
        self.matches_pane.set_image(self._diagnostic_cache[cache_key])
        self.overlay_pane.set_image(self._diagnostic_cache[f"overlay:{pair['pair_id']}"])

    def _seams_from_disk(self) -> list[tuple[str, SeamPath]]:
        if self.result is None:
            return []
        path = self.result.output_dir / "seams.json"
        if not path.is_file():
            return []
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
            return [
                (
                    item["pair_id"],
                    SeamPath(
                        orientation=item["orientation"],
                        points_xy=tuple(
                            (int(point[0]), int(point[1]))
                            for point in item["points_xy"]
                        ),
                        bbox=tuple(int(value) for value in item["bbox"]),
                        total_cost=float(item["total_cost"]),
                    ),
                )
                for item in payload.get("seams", [])
            ]
        except (OSError, KeyError, TypeError, ValueError):
            return []

    def _show_seams(self, only_pair_id: str | None) -> None:
        if self.result is None:
            return
        panorama_path = self.result.output_dir / "panorama.png"
        panorama = cv2.imread(str(panorama_path), cv2.IMREAD_COLOR)
        if panorama is None:
            self.seam_pane.set_image(None, "Panorama unavailable for seam overlay")
            return
        seams = list(self.result.seams) if self.result.seams else self._seams_from_disk()
        if not seams:
            self.seam_pane.set_image(None, "No seam records")
            return
        visual = seam_overlay(panorama, seams, only_pair_id=only_pair_id)
        self.seam_pane.set_image(visual)

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None and self.worker.isRunning():
            answer = QMessageBox.question(
                self,
                "Run in progress",
                "Cancel the run and exit? Only committed pairs are persisted.",
                QMessageBox.Yes | QMessageBox.No,
                QMessageBox.No,
            )
            if answer != QMessageBox.Yes:
                event.ignore()
                return
            self.worker.request_cancel()
            self.worker.wait(15000)
        event.accept()


def launch_gui(initial_dataset: str | None = None) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    app.setApplicationName("PanoWorkbench")
    window = MainWindow(initial_dataset)
    window.show()
    return int(app.exec_())


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(launch_gui(sys.argv[1] if len(sys.argv) > 1 else None))
