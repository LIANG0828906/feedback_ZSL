"""Professional PySide6 workbench for SpectraForge."""

from __future__ import annotations

import copy
import json
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QThread, Qt, Signal
from PySide6.QtGui import QBrush, QColor, QFont
from PySide6.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFrame,
    QGridLayout,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QVBoxLayout,
    QWidget,
    QHeaderView,
)

from ..checkpoint import CheckpointManager
from ..compare import compare_outputs
from ..errors import WorkbenchError
from ..export import export_output
from ..fusion import fuse_exposures
from ..metadata import load_scene
from ..parameters import parameter_version, validate_parameters
from ..pipeline import assemble_output, prepare_scene
from ..sample_generator import generate_all
from ..scheduler import ProcessingJob, ResourceConfig
from ..segmentation import make_segments, segmentation_spec
from ..state import SegmentState
from ..util import read_json
from .theme import APP_QSS, STATE_COLORS
from .widgets import (
    ChromaticityWidget,
    DifferenceHeatmapWidget,
    HistogramWidget,
    ImagePanel,
    SegmentDetailWidget,
    ToneCurveWidget,
    tone_to_uint8,
)


class JobThread(QThread):
    event = Signal(object)
    result_ready = Signal(object)
    failure = Signal(object)

    def __init__(self, job: ProcessingJob, parent: QWidget | None = None):
        super().__init__(parent)
        self.job = job

    def run(self) -> None:
        try:
            result = self.job.run(retry_failed=True)
        except WorkbenchError as exc:
            self.failure.emit(exc.to_dict())
        except Exception as exc:
            self.failure.emit(
                WorkbenchError(
                    "E_INTERNAL",
                    "Background processing thread failed",
                    {"reason": str(exc), "type": type(exc).__name__},
                ).to_dict()
            )
        else:
            self.result_ready.emit(result)


class CompareThread(QThread):
    result_ready = Signal(object)
    failure = Signal(object)

    def __init__(
        self,
        scene_path: Path,
        workspace: Path,
        params: dict[str, Any],
        structure: dict[str, Any],
        current_output: Any,
        parent: QWidget | None = None,
    ):
        super().__init__(parent)
        self.scene_path = scene_path
        self.workspace = workspace
        self.params = copy.deepcopy(params)
        self.structure = dict(structure)
        self.current_output = current_output

    def run(self) -> None:
        manager = None
        try:
            scene = load_scene(self.scene_path)
            segments = make_segments(
                scene.width,
                scene.height,
                scene.band_count,
                split_mode=self.structure["split_mode"],
                tile_width=self.structure["tile_width"],
                tile_height=self.structure["tile_height"],
            )
            config = segmentation_spec(
                scene.width,
                scene.height,
                scene.band_count,
                split_mode=self.structure["split_mode"],
                tile_width=self.structure["tile_width"],
                tile_height=self.structure["tile_height"],
                interpolation=self.params["interpolation"],
            )
            root = self.workspace / "ui_compare" / (
                datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ") + "-" + uuid.uuid4().hex[:6]
            )
            manager = CheckpointManager(root, scene, segments, config)
            job = ProcessingJob(scene, manager, resource=ResourceConfig(1, 64), params=self.params)
            job.run()
            reference = assemble_output(scene, manager, require_complete=True)
            report = compare_outputs(self.current_output, reference)
            if report.equal:
                diff = np.zeros_like(self.current_output.radiance, dtype=np.float32)
            else:
                diff = np.max(
                    np.abs(
                        self.current_output.linear_rgb.astype(np.float64)
                        - reference.linear_rgb.astype(np.float64)
                    ),
                    axis=2,
                ).astype(np.float32)
            self.result_ready.emit(
                {
                    "report": report.to_dict(),
                    "diff": diff,
                    "reference_summary": reference.compact_summary(),
                    "workspace": str(root),
                }
            )
        except WorkbenchError as exc:
            self.failure.emit(exc.to_dict())
        except Exception as exc:
            self.failure.emit(
                WorkbenchError(
                    "E_INTERNAL",
                    "Comparison thread failed",
                    {"reason": str(exc), "type": type(exc).__name__},
                ).to_dict()
            )
        finally:
            if manager is not None:
                manager.close()


class MainWindow(QMainWindow):
    def __init__(
        self,
        samples: Path,
        workspace: Path,
        initial_scene: str | None = None,
        parent: QWidget | None = None,
    ):
        super().__init__(parent)
        self.samples = samples
        self.workspace = workspace
        self.workspace.mkdir(parents=True, exist_ok=True)
        self.catalog: dict[str, Any] = {}
        self.scene: Any = None
        self.prepared: Any = None
        self.manager: CheckpointManager | None = None
        self.job: ProcessingJob | None = None
        self.job_thread: JobThread | None = None
        self.compare_thread: CompareThread | None = None
        self.output: Any = None
        self._job_running = False
        self._compare_running = False
        self._loading_controls = False
        self._segment_rows: dict[int, int] = {}
        self._initial_scene = initial_scene
        self._build_ui()
        self._load_catalog(initial_scene)

    # ------------------------------------------------------------------ UI
    def _build_ui(self) -> None:
        self.setWindowTitle("SpectraForge · Multispectral HDR Workbench")
        self.resize(1600, 1000)
        self.setMinimumSize(1240, 800)
        root = QWidget()
        root.setObjectName("appRoot")
        root_layout = QVBoxLayout(root)
        root_layout.setContentsMargins(0, 0, 0, 0)
        root_layout.setSpacing(0)
        root_layout.addWidget(self._build_header())
        body = QSplitter(Qt.Horizontal)
        body.setChildrenCollapsible(False)
        body.addWidget(self._build_left_panel())
        body.addWidget(self._build_center())
        body.addWidget(self._build_right_panel())
        body.setStretchFactor(0, 0)
        body.setStretchFactor(1, 1)
        body.setStretchFactor(2, 0)
        body.setSizes([350, 850, 400])
        root_layout.addWidget(body, 1)
        root_layout.addWidget(self._build_footer())
        self.setCentralWidget(root)

    def _build_header(self) -> QWidget:
        header = QFrame()
        header.setObjectName("header")
        header.setFixedHeight(78)
        layout = QHBoxLayout(header)
        layout.setContentsMargins(20, 10, 20, 10)
        layout.setSpacing(18)
        brand_box = QVBoxLayout()
        brand_box.setSpacing(0)
        brand = QLabel("SPECTRAFORGE")
        brand.setObjectName("brand")
        subtitle = QLabel("MULTISPECTRAL / HDR COMPOSITING WORKBENCH")
        subtitle.setObjectName("brandAccent")
        brand_box.addWidget(brand)
        brand_box.addWidget(subtitle)
        layout.addLayout(brand_box)
        layout.addSpacing(10)
        self.scene_combo = QComboBox()
        self.scene_combo.setMinimumWidth(255)
        self.scene_combo.setToolTip("Deterministic local scene")
        self.scene_combo.currentIndexChanged.connect(self._on_scene_changed)
        layout.addWidget(self.scene_combo)
        self.scene_chip = QLabel("NO SCENE")
        self.scene_chip.setStyleSheet(
            "color:#9fe9e6;background:#10282b;border:1px solid #24575b;"
            "border-radius:11px;padding:3px 9px;font-family:'Cascadia Mono';font-size:9px;"
        )
        layout.addWidget(self.scene_chip)
        layout.addStretch(1)
        self.workspace_chip = QLabel(f"WORKSPACE  {self.workspace}")
        self.workspace_chip.setStyleSheet("color:#687d91;font-family:'Cascadia Mono';font-size:9px;")
        layout.addWidget(self.workspace_chip)
        self.header_status = QLabel("READY")
        self.header_status.setStyleSheet(
            "color:#36d6a0;background:#10261e;border:1px solid #245743;"
            "border-radius:11px;padding:3px 10px;font-family:'Cascadia Mono';font-size:9px;"
        )
        layout.addWidget(self.header_status)
        return header

    def _section(self, parent_layout: QVBoxLayout, title: str) -> QGridLayout:
        label = QLabel(title)
        label.setObjectName("sectionTitle")
        parent_layout.addWidget(label)
        grid = QGridLayout()
        grid.setHorizontalSpacing(8)
        grid.setVerticalSpacing(7)
        grid.setColumnStretch(1, 1)
        parent_layout.addLayout(grid)
        return grid

    def _control_label(self, text: str) -> QLabel:
        label = QLabel(text)
        label.setStyleSheet("color:#899db2;font-size:11px;")
        return label

    def _build_left_panel(self) -> QWidget:
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setMinimumWidth(330)
        scroll.setMaximumWidth(390)
        panel = QFrame()
        panel.setObjectName("panel")
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(14, 14, 14, 14)
        layout.setSpacing(12)

        self.scene_info = QLabel("Load a deterministic scene to begin.")
        self.scene_info.setWordWrap(True)
        self.scene_info.setStyleSheet(
            "color:#93a8bd;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:8px;font-family:'Cascadia Mono';font-size:10px;"
        )
        layout.addWidget(self.scene_info)

        self.param_state = QLabel("PARAM —")
        self.param_state.setWordWrap(True)
        self.param_state.setStyleSheet(
            "color:#7f93a8;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:7px;font-family:'Cascadia Mono';font-size:9px;"
        )
        layout.addWidget(self.param_state)
        self.process_button = QPushButton("PROCESS / RESUME")
        self.process_button.setObjectName("primary")
        self.process_button.clicked.connect(self.start_processing)
        self.pause_button = QPushButton("PAUSE")
        self.pause_button.clicked.connect(self.pause_processing)
        self.pause_button.setEnabled(False)
        self.reprocess_button = QPushButton("REPROCESS ALL")
        self.reprocess_button.setObjectName("danger")
        self.reprocess_button.clicked.connect(self.reprocess_all)
        button_row = QHBoxLayout()
        button_row.addWidget(self.process_button, 1)
        button_row.addWidget(self.pause_button)
        layout.addLayout(button_row)
        layout.addWidget(self.reprocess_button)
        self.export_button = QPushButton("EXPORT COMMITTED COMPOSITE")
        self.export_button.setObjectName("accent")
        self.export_button.clicked.connect(self.export_results)
        layout.addWidget(self.export_button)
        self.compare_button = QPushButton("FULL-RECOMPUTE COMPARE")
        self.compare_button.clicked.connect(self.compare_full)
        layout.addWidget(self.compare_button)

        seg = self._section(layout, "SEGMENTATION / RESOURCES")
        self.split_combo = QComboBox()
        self.split_combo.addItems(["tiles", "bands"])
        self.interp_combo = QComboBox()
        self.interp_combo.addItems(["bilinear", "nearest", "bicubic"])
        self.tile_spin = QSpinBox()
        self.tile_spin.setRange(8, 256)
        self.tile_spin.setSingleStep(8)
        self.tile_spin.setValue(32)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 32)
        self.workers_spin.setValue(4)
        self.buffer_spin = QSpinBox()
        self.buffer_spin.setRange(1, 4096)
        self.buffer_spin.setValue(64)
        self.buffer_spin.setSuffix(" MB")
        for row, (text, widget) in enumerate(
            [
                ("Split", self.split_combo),
                ("Interp", self.interp_combo),
                ("Tile", self.tile_spin),
                ("Workers", self.workers_spin),
                ("Buffer", self.buffer_spin),
            ]
        ):
            seg.addWidget(self._control_label(text), row, 0)
            seg.addWidget(widget, row, 1)
        self.structure_button = QPushButton("APPLY STRUCTURE / RELOAD SNAPSHOT")
        self.structure_button.clicked.connect(self._apply_structure)
        seg.addWidget(self.structure_button, 5, 0, 1, 2)
        structure_note = QLabel("Structural edits create a separate checkpoint snapshot and never overwrite old segments.")
        structure_note.setWordWrap(True)
        structure_note.setStyleSheet("color:#60758a;font-size:10px;")
        seg.addWidget(structure_note, 6, 0, 1, 2)

        exposure = self._section(layout, "EXPOSURE FUSION")
        self.exposure_host = QWidget()
        self.exposure_layout = QVBoxLayout(self.exposure_host)
        self.exposure_layout.setContentsMargins(0, 0, 0, 0)
        self.exposure_layout.setSpacing(2)
        exposure.addWidget(self.exposure_host, 0, 0, 1, 2)
        self.weight_spin = self._double_spin(0.25, 12.0, 2.0, 2, 0.25)
        self.bias_spin = self._double_spin(0.0, 4.0, 1.0, 2, 0.1)
        self.black_spin = self._double_spin(0.0, 0.1, 0.002, 4, 0.001)
        self.sat_spin = self._double_spin(0.5, 1.0, 0.995, 3, 0.005)
        self.under_spin = self._double_spin(0.0, 0.2, 0.008, 3, 0.001)
        for row, (text, widget) in enumerate(
            [
                ("Weight power", self.weight_spin),
                ("Exposure bias", self.bias_spin),
                ("Black level", self.black_spin),
                ("Saturation", self.sat_spin),
                ("Under-exposure", self.under_spin),
            ],
            start=1,
        ):
            exposure.addWidget(self._control_label(text), row, 0)
            exposure.addWidget(widget, row, 1)

        color = self._section(layout, "CHROMA RECONSTRUCTION")
        self.gamut_combo = QComboBox()
        self.gamut_combo.addItems(["sRGB", "DisplayP3", "Rec2020"])
        self.intent_combo = QComboBox()
        self.intent_combo.addItems(["compress", "clip", "strict"])
        color.addWidget(self._control_label("Target gamut"), 0, 0)
        color.addWidget(self.gamut_combo, 0, 1)
        color.addWidget(self._control_label("Intent"), 1, 0)
        color.addWidget(self.intent_combo, 1, 1)

        tone = self._section(layout, "TONE / DISPLAY")
        self.tone_combo = QComboBox()
        self.tone_combo.addItems(["reinhard", "aces", "filmic"])
        self.tone_exposure_spin = self._double_spin(-8.0, 8.0, 0.0, 2, 0.1)
        self.tone_key_spin = self._double_spin(0.05, 8.0, 0.68, 2, 0.05)
        self.tone_white_spin = self._double_spin(0.1, 100.0, 8.0, 2, 0.5)
        self.saturation_spin = self._double_spin(0.0, 2.5, 1.0, 2, 0.05)
        self.gamma_spin = self._double_spin(1.0, 3.0, 2.2, 2, 0.05)
        for row, (text, widget) in enumerate(
            [
                ("Curve", self.tone_combo),
                ("EV", self.tone_exposure_spin),
                ("Key", self.tone_key_spin),
                ("White", self.tone_white_spin),
                ("Saturation", self.saturation_spin),
                ("Display γ", self.gamma_spin),
            ]
        ):
            tone.addWidget(self._control_label(text), row, 0)
            tone.addWidget(widget, row, 1)

        self.generate_button = QPushButton("REGENERATE DETERMINISTIC SAMPLES")
        self.generate_button.clicked.connect(self.regenerate_samples)
        layout.addWidget(self.generate_button)
        layout.addStretch(1)
        self._param_widgets = [
            self.weight_spin,
            self.bias_spin,
            self.black_spin,
            self.sat_spin,
            self.under_spin,
            self.gamut_combo,
            self.intent_combo,
            self.tone_combo,
            self.tone_exposure_spin,
            self.tone_key_spin,
            self.tone_white_spin,
            self.saturation_spin,
            self.gamma_spin,
        ]
        for widget in self._param_widgets:
            if isinstance(widget, QComboBox):
                widget.currentTextChanged.connect(self._on_parameters_changed)
            else:
                widget.valueChanged.connect(self._on_parameters_changed)
        scroll.setWidget(panel)
        return scroll

    def _double_spin(
        self,
        low: float,
        high: float,
        value: float,
        decimals: int,
        step: float,
    ) -> QDoubleSpinBox:
        widget = QDoubleSpinBox()
        widget.setRange(low, high)
        widget.setDecimals(decimals)
        widget.setSingleStep(step)
        widget.setValue(value)
        return widget

    def _build_center(self) -> QWidget:
        panel = QFrame()
        panel.setObjectName("panel")
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(10, 10, 10, 10)
        layout.setSpacing(8)
        title_row = QHBoxLayout()
        title = QLabel("COMPOSITE VIEWPORT")
        title.setObjectName("sectionTitle")
        self.view_status = QLabel("No committed buffers")
        self.view_status.setStyleSheet("color:#687d91;font-family:'Cascadia Mono';font-size:9px;")
        title_row.addWidget(title)
        title_row.addStretch(1)
        title_row.addWidget(self.view_status)
        layout.addLayout(title_row)
        self.preview_tabs = QTabWidget()
        overview = QWidget()
        grid = QGridLayout(overview)
        grid.setContentsMargins(5, 5, 5, 5)
        grid.setSpacing(8)
        self.hdr_panel = ImagePanel("FUSED HDR", "reliability-weighted exposure fusion")
        self.composite_panel = ImagePanel("COMPOSITE", "chroma × HDR luminance → tone map")
        self.chroma_panel = ImagePanel("CHROMA", "white-adapted target RGB, Y-normalized")
        self.selected_panel = ImagePanel("SELECTED SEGMENT", "click a committed segment")
        grid.addWidget(self.hdr_panel, 0, 0)
        grid.addWidget(self.composite_panel, 0, 1)
        grid.addWidget(self.chroma_panel, 1, 0)
        grid.addWidget(self.selected_panel, 1, 1)
        self.preview_tabs.addTab(overview, "Composite")
        bands = QWidget()
        bands_layout = QVBoxLayout(bands)
        bands_layout.setContentsMargins(5, 5, 5, 5)
        band_top = QHBoxLayout()
        self.band_hdr_panel = ImagePanel("HDR FUSION OVERVIEW", "all selected exposures")
        self.band_hdr_panel.setMaximumWidth(270)
        self.band_hdr_panel.setMaximumHeight(155)
        self.band_hdr_info = QLabel(
            "EXPOSURE FUSION\n\n"
            "Reliability weights suppress saturated and underexposed samples, "
            "then inverse camera response linearizes every selected exposure.\n\n"
            "Waveband thumbnails below are geometrically aligned with the declared "
            "subpixel offsets before spectral reconstruction."
        )
        self.band_hdr_info.setWordWrap(True)
        self.band_hdr_info.setStyleSheet(
            "color:#8196aa;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:12px;font-family:'Cascadia Mono';font-size:9px;"
        )
        band_top.addWidget(self.band_hdr_panel)
        band_top.addWidget(self.band_hdr_info, 1)
        bands_layout.addLayout(band_top)
        self.band_scroll = QScrollArea()
        self.band_scroll.setWidgetResizable(True)
        self.band_host = QWidget()
        self.band_grid = QGridLayout(self.band_host)
        self.band_grid.setContentsMargins(0, 0, 0, 0)
        self.band_grid.setSpacing(8)
        self.band_scroll.setWidget(self.band_host)
        bands_layout.addWidget(self.band_scroll, 1)
        self.preview_tabs.addTab(bands, "Band Array")
        self.segment_detail = SegmentDetailWidget()
        self.preview_tabs.addTab(self.segment_detail, "Segment")
        layout.addWidget(self.preview_tabs, 1)
        segment_header = QHBoxLayout()
        seg_title = QLabel("SEGMENT LEDGER / ATOMIC CHECKPOINTS")
        seg_title.setObjectName("sectionTitle")
        self.segment_summary = QLabel("0 / 0 committed")
        self.segment_summary.setStyleSheet("color:#71879c;font-family:'Cascadia Mono';font-size:9px;")
        segment_header.addWidget(seg_title)
        segment_header.addStretch(1)
        segment_header.addWidget(self.segment_summary)
        layout.addLayout(segment_header)
        self.table = QTableWidget(0, 7)
        self.table.setHorizontalHeaderLabels(
            ["SEG", "SCOPE", "STATE", "PARAM", "PEAK", "GAMUT", "HASH"]
        )
        self.table.verticalHeader().setVisible(False)
        self.table.setAlternatingRowColors(True)
        self.table.setSelectionBehavior(QAbstractItemView.SelectRows)
        self.table.setSelectionMode(QAbstractItemView.SingleSelection)
        self.table.setEditTriggers(QAbstractItemView.NoEditTriggers)
        self.table.horizontalHeader().setSectionResizeMode(0, QHeaderView.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(1, QHeaderView.Stretch)
        self.table.horizontalHeader().setSectionResizeMode(2, QHeaderView.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(3, QHeaderView.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(4, QHeaderView.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(5, QHeaderView.ResizeToContents)
        self.table.horizontalHeader().setSectionResizeMode(6, QHeaderView.Stretch)
        self.table.setMinimumHeight(205)
        self.table.setMaximumHeight(285)
        self.table.currentCellChanged.connect(self._show_selected_segment)
        layout.addWidget(self.table)
        return panel

    def _build_right_panel(self) -> QWidget:
        panel = QFrame()
        panel.setObjectName("panel")
        panel.setMinimumWidth(360)
        panel.setMaximumWidth(455)
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(10, 10, 10, 10)
        tabs = QTabWidget()
        wave = QWidget()
        wave_layout = QVBoxLayout(wave)
        wave_layout.setContentsMargins(4, 4, 4, 4)
        self.histogram = HistogramWidget()
        self.tone_curve = ToneCurveWidget()
        self.signal_stats = QLabel("Waiting for processed radiance buffers.")
        self.signal_stats.setWordWrap(True)
        self.signal_stats.setStyleSheet(
            "color:#8196aa;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:8px;font-family:'Cascadia Mono';font-size:9px;"
        )
        wave_layout.addWidget(self.histogram)
        wave_layout.addWidget(self.tone_curve)
        wave_layout.addWidget(self.signal_stats)
        wave_layout.addStretch(1)
        tabs.addTab(wave, "Signal")
        chroma = QWidget()
        chroma_layout = QVBoxLayout(chroma)
        chroma_layout.setContentsMargins(4, 4, 4, 4)
        self.chromaticity = ChromaticityWidget()
        self.gamut_stats = QLabel("No chroma scope data.")
        self.gamut_stats.setWordWrap(True)
        self.gamut_stats.setStyleSheet(
            "color:#8196aa;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:8px;font-family:'Cascadia Mono';font-size:9px;"
        )
        chroma_layout.addWidget(self.chromaticity)
        chroma_layout.addWidget(self.gamut_stats)
        chroma_layout.addStretch(1)
        tabs.addTab(chroma, "Chroma")
        diff = QWidget()
        diff_layout = QVBoxLayout(diff)
        diff_layout.setContentsMargins(4, 4, 4, 4)
        self.heatmap = DifferenceHeatmapWidget()
        self.compare_report = QLabel("Three-path / full-recompute comparison has not run yet.")
        self.compare_report.setWordWrap(True)
        self.compare_report.setStyleSheet(
            "color:#8196aa;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:8px;font-family:'Cascadia Mono';font-size:9px;"
        )
        self.compare_tab_button = QPushButton("RUN FULL-RECOMPUTE COMPARISON")
        self.compare_tab_button.clicked.connect(self.compare_full)
        diff_layout.addWidget(self.heatmap)
        diff_layout.addWidget(self.compare_tab_button)
        diff_layout.addWidget(self.compare_report)
        diff_layout.addStretch(1)
        tabs.addTab(diff, "Difference")
        pipeline = QWidget()
        pipeline_layout = QVBoxLayout(pipeline)
        pipeline_layout.setContentsMargins(10, 10, 10, 10)
        self.pipeline_stats = QLabel("Pipeline diagnostics will appear here.")
        self.pipeline_stats.setWordWrap(True)
        self.pipeline_stats.setTextInteractionFlags(Qt.TextSelectableByMouse)
        self.pipeline_stats.setStyleSheet(
            "color:#8ca0b4;background:#0c131a;border:1px solid #1d2b38;"
            "border-radius:5px;padding:10px;font-family:'Cascadia Mono';font-size:10px;"
        )
        pipeline_layout.addWidget(self.pipeline_stats)
        pipeline_layout.addStretch(1)
        tabs.addTab(pipeline, "Pipeline")
        layout.addWidget(tabs, 1)
        self.error_banner = QLabel()
        self.error_banner.setWordWrap(True)
        self.error_banner.setStyleSheet(
            "color:#ffcbd1;background:#35161c;border:1px solid #823846;"
            "border-radius:5px;padding:8px;font-family:'Cascadia Mono';font-size:9px;"
        )
        self.error_banner.hide()
        layout.addWidget(self.error_banner)
        return panel

    def _build_footer(self) -> QWidget:
        footer = QFrame()
        footer.setFixedHeight(44)
        footer.setStyleSheet("background:#0d141c;border-top:1px solid #1b2936;")
        layout = QHBoxLayout(footer)
        layout.setContentsMargins(16, 7, 16, 7)
        self.progress = QProgressBar()
        self.progress.setMaximum(1)
        self.progress.setValue(0)
        self.progress.setFixedWidth(290)
        self.progress_label = QLabel("0 / 0 committed")
        self.progress_label.setStyleSheet("color:#8da1b5;font-family:'Cascadia Mono';font-size:9px;")
        self.param_footer = QLabel("PARAM —")
        self.param_footer.setStyleSheet("color:#657a8f;font-family:'Cascadia Mono';font-size:9px;")
        self.shortcut_hint = QLabel("Pause drains in-flight commits · forced termination restores only persisted, version-matched segments")
        self.shortcut_hint.setStyleSheet("color:#586d81;font-size:9px;")
        layout.addWidget(self.progress)
        layout.addWidget(self.progress_label)
        layout.addSpacing(14)
        layout.addWidget(self.param_footer)
        layout.addStretch(1)
        layout.addWidget(self.shortcut_hint)
        return footer

    # --------------------------------------------------------------- catalog
    def _load_catalog(self, initial_scene: str | None = None) -> None:
        catalog_path = self.samples / "catalog.json"
        if not catalog_path.exists():
            self.show_error(
                {
                    "code": "E_META",
                    "message": "Sample catalog is missing. Generate deterministic samples first.",
                    "details": {"path": str(catalog_path)},
                }
            )
            return
        self.catalog = read_json(catalog_path)
        self._loading_controls = True
        self.scene_combo.clear()
        for entry in self.catalog.get("scenes", []):
            self.scene_combo.addItem(f"{entry['title']}  ·  {entry['scene_id']}", entry["scene_id"])
        self._loading_controls = False
        if self.scene_combo.count():
            selected = 0
            if initial_scene:
                for index in range(self.scene_combo.count()):
                    if initial_scene in {self.scene_combo.itemData(index), self.scene_combo.itemText(index)}:
                        selected = index
                        break
            self._loading_controls = True
            self.scene_combo.setCurrentIndex(selected)
            self._loading_controls = False
            self._load_scene_by_index(selected)

    def _on_scene_changed(self, index: int) -> None:
        if self._loading_controls or index < 0:
            return
        if self._job_running:
            self.show_error({"code": "E_STATE", "message": "Cannot switch scenes while segments are processing."})
            self._loading_controls = True
            self.scene_combo.setCurrentIndex(self.scene_combo.findData(self.scene.metadata.scene_id))
            self._loading_controls = False
            return
        self._load_scene_by_index(index)

    def _load_scene_by_index(self, index: int) -> None:
        entry = self.catalog.get("scenes", [])
        if index < 0 or index >= len(entry):
            return
        if self.manager is not None:
            self.manager.close()
            self.manager = None
        scene_path = (self.samples / entry[index]["path"]).resolve()
        try:
            self.scene = load_scene(scene_path)
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        self.output = None
        self.prepared = None
        self._loading_controls = True
        self._configure_exposure_checks(self.scene.metadata.default_parameters["exposure_indices"])
        defaults = self.scene.metadata.default_parameters
        self.interp_combo.setCurrentText(str(defaults["interpolation"]))
        self.gamut_combo.setCurrentText(str(defaults["target_gamut"]))
        self.intent_combo.setCurrentText(str(defaults["gamut_intent"]))
        self.tone_combo.setCurrentText(str(defaults["tone_curve"]))
        self.tone_exposure_spin.setValue(float(defaults["tone_exposure"]))
        self.tone_key_spin.setValue(float(defaults["tone_key"]))
        self.tone_white_spin.setValue(float(defaults["tone_white"]))
        self.saturation_spin.setValue(float(defaults["saturation"]))
        self.gamma_spin.setValue(float(defaults["display_gamma"]))
        self.weight_spin.setValue(float(defaults["weight_power"]))
        self.bias_spin.setValue(float(defaults["exposure_bias"]))
        self.black_spin.setValue(float(defaults["black_level"]))
        self.sat_spin.setValue(float(defaults["saturation_threshold"]))
        self.under_spin.setValue(float(defaults["underexposure_threshold"]))
        self._loading_controls = False
        self.scene_chip.setText(
            f"{self.scene.band_count} BANDS  /  {self.scene.exposure_count} EXP  /  {self.scene.width}×{self.scene.height}"
        )
        self.scene_info.setText(
            f"{self.scene.metadata.title.upper()}\n"
            f"{self.scene.metadata.scene_id}\n"
            f"seed {self.scene.metadata.seed}  ·  {self.scene.metadata.pixel_pitch_um:g} µm pitch\n"
            f"white D50 → {defaults['target_gamut']}  ·  {_short_hash(self.scene.metadata.metadata_hash)}"
        )
        try:
            self._build_manager()
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            self.manager = None
        self._build_band_previews()
        self._refresh_from_snapshot(initial=True)
        self._on_parameters_changed()
        self.header_status.setText("READY")
        self.header_status.setStyleSheet(
            "color:#36d6a0;background:#10261e;border:1px solid #245743;"
            "border-radius:11px;padding:3px 10px;font-family:'Cascadia Mono';font-size:9px;"
        )

    def _configure_exposure_checks(self, selected: list[int]) -> None:
        while self.exposure_layout.count():
            item = self.exposure_layout.takeAt(0)
            widget = item.widget()
            if widget is not None:
                widget.deleteLater()
        self.exposure_checks: list[QCheckBox] = []
        for exposure in self.scene.metadata.exposures:
            check = QCheckBox(
                f"EXP {exposure.index + 1:02d}   {exposure.exposure_time:.5g} s   ·   {exposure.bit_depth}-bit"
            )
            check.setChecked(exposure.index in selected)
            check.toggled.connect(self._on_parameters_changed)
            self.exposure_layout.addWidget(check)
            self.exposure_checks.append(check)

    # ------------------------------------------------------------ structural
    def _structure(self) -> dict[str, Any]:
        return {
            "split_mode": self.split_combo.currentText(),
            "tile_width": self.tile_spin.value(),
            "tile_height": self.tile_spin.value(),
            "interpolation": self.interp_combo.currentText(),
        }

    def _build_manager(self) -> None:
        if self.scene is None:
            return
        structure = self._structure()
        segments = make_segments(
            self.scene.width,
            self.scene.height,
            self.scene.band_count,
            split_mode=structure["split_mode"],
            tile_width=structure["tile_width"],
            tile_height=structure["tile_height"],
        )
        config = segmentation_spec(
            self.scene.width,
            self.scene.height,
            self.scene.band_count,
            split_mode=structure["split_mode"],
            tile_width=structure["tile_width"],
            tile_height=structure["tile_height"],
            interpolation=structure["interpolation"],
        )
        self.manager = CheckpointManager(self.workspace, self.scene, segments, config)
        self._populate_table()
        if self.manager.invalidated_reason:
            self.show_error(
                {
                    "code": self.manager.invalidated_reason.get("code", "E_STATE"),
                    "message": "Old checkpoint snapshot failed version/content validation and was invalidated as a whole.",
                    "details": self.manager.invalidated_reason.get("details", {}),
                }
            )

    def _apply_structure(self) -> None:
        if self._job_running:
            self.show_error({"code": "E_STATE", "message": "Cannot reload a structural snapshot while processing."})
            return
        if self.manager is not None:
            self.manager.close()
        try:
            self._build_manager()
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            self.manager = None
        self._build_band_previews()
        self._refresh_from_snapshot(initial=True)
        self._on_parameters_changed()

    # -------------------------------------------------------------- params
    def current_params(self) -> dict[str, Any]:
        if self.scene is None:
            raise WorkbenchError("E_STATE", "No scene is loaded")
        params = copy.deepcopy(self.scene.metadata.default_parameters)
        params.update(
            {
                "interpolation": self.interp_combo.currentText(),
                "exposure_indices": [
                    index for index, check in enumerate(self.exposure_checks) if check.isChecked()
                ],
                "weight_power": self.weight_spin.value(),
                "exposure_bias": self.bias_spin.value(),
                "black_level": self.black_spin.value(),
                "saturation_threshold": self.sat_spin.value(),
                "underexposure_threshold": self.under_spin.value(),
                "target_gamut": self.gamut_combo.currentText(),
                "gamut_intent": self.intent_combo.currentText(),
                "tone_curve": self.tone_combo.currentText(),
                "tone_exposure": self.tone_exposure_spin.value(),
                "tone_key": self.tone_key_spin.value(),
                "tone_white": self.tone_white_spin.value(),
                "saturation": self.saturation_spin.value(),
                "display_gamma": self.gamma_spin.value(),
            }
        )
        return validate_parameters(params, self.scene.exposure_count)

    def _on_parameters_changed(self, *_args: Any) -> None:
        if self._loading_controls or self.scene is None:
            return
        try:
            params = self.current_params()
            version = parameter_version(params, self.scene.exposure_count)
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        self.param_state.setText(
            f"PARAM {_short_hash(version)}\n"
            f"exposure subset {params['exposure_indices']}\n"
            f"{params['target_gamut']} / {params['gamut_intent']} / {params['tone_curve']}"
        )
        self.param_footer.setText(f"PARAM {_short_hash(version)}")
        self.tone_curve.set_params(params)
        if self._job_running and self.job is not None:
            try:
                self.job.set_parameters(params)
            except WorkbenchError as exc:
                self.show_error(exc.to_dict())
        if self.output is not None:
            self.chromaticity.set_data(self.output.rgb_chroma, params["target_gamut"])
        if self.manager is not None and self.manager.progress()["committed"] > 0:
            self.param_state.setText(
                self.param_state.text() + "\nCommitted segment params are locked; edits affect uncommitted segments only."
            )
        self._refresh_raw_hdr()

    def _refresh_raw_hdr(self) -> None:
        if self.scene is None or self._job_running:
            return
        try:
            params = self.current_params()
            result = fuse_exposures(
                self.scene.exposure_raw,
                self.scene.metadata.exposures,
                params,
                self.scene.metadata.camera_response,
            )
            preview = tone_to_uint8(result.radiance)
            self.band_hdr_panel.set_array(
                np.repeat(preview[..., np.newaxis], 3, axis=2),
                caption=(
                    f"peak {float(result.radiance.max()):.4g}  ·  "
                    f"exposures {params['exposure_indices']}"
                ),
            )
            if self.output is None:
                self.hdr_panel.set_array(np.repeat(preview[..., np.newaxis], 3, axis=2))
                self.composite_panel.set_message("Run or resume the segment pipeline")
                self.chroma_panel.set_message("Committed chroma buffers will appear here")
        except WorkbenchError:
            pass

    # ----------------------------------------------------------- previews
    def _build_band_previews(self) -> None:
        if self.scene is None:
            return
        while self.band_grid.count():
            item = self.band_grid.takeAt(0)
            widget = item.widget()
            if widget is not None:
                widget.deleteLater()
        try:
            self.prepared = prepare_scene(self.scene, self.interp_combo.currentText())
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        for index in range(self.scene.band_count):
            preview = tone_to_uint8(self.prepared.aligned_bands[index])
            panel = ImagePanel(
                f"BAND {index:02d}",
                f"{self.scene.metadata.bands[index].center_nm:.1f} nm  ·  σ "
                f"{self.scene.metadata.bands[index].sigma_nm:g} nm",
            )
            panel.set_array(np.repeat(preview[..., np.newaxis], 3, axis=2))
            self.band_grid.addWidget(panel, index // 4, index % 4)
        self._refresh_raw_hdr()

    def _set_output_previews(self, output: Any) -> None:
        if output is None:
            return
        hdr = tone_to_uint8(output.radiance)
        self.hdr_panel.set_array(
            np.repeat(hdr[..., np.newaxis], 3, axis=2),
            caption=f"peak {output.peak_radiance:.5g} @ {output.peak_location[0]},{output.peak_location[1]}",
        )
        self.band_hdr_panel.set_array(
            np.repeat(hdr[..., np.newaxis], 3, axis=2),
            caption=f"peak {output.peak_radiance:.5g}",
        )
        self.composite_panel.set_array(output.display, caption=f"{output.display.shape[1]}×{output.display.shape[0]}")
        chroma = output.rgb_chroma.astype(np.float64)
        maximum = np.maximum(chroma.max(axis=2, keepdims=True), 1e-9)
        chroma_preview = np.rint(
            np.power(np.clip(chroma / maximum, 0.0, 1.0), 1.0 / 2.2) * 255.0
        ).astype(np.uint8)
        self.chroma_panel.set_array(chroma_preview)
        self.histogram.set_values(output.radiance)
        self.chromaticity.set_data(output.rgb_chroma, self.gamut_combo.currentText())
        self.view_status.setText(
            f"{'COMPLETE' if output.complete else 'PARTIAL'}  ·  "
            f"{len(output.param_versions)} param snapshot(s)"
        )
        self.signal_stats.setText(
            f"peak radiance   {output.peak_radiance:.6g}\n"
            f"peak coordinate {output.peak_location}\n"
            f"gamut pixels    {output.gamut_count}\n"
            f"gamut channels  {int(output.gamut_channel_mask.sum())}\n"
            f"display size    {output.display.shape[1]}×{output.display.shape[0]}"
        )
        self.gamut_stats.setText(
            f"target {self.gamut_combo.currentText()}\n"
            f"intent {self.intent_combo.currentText()}\n"
            f"out-of-gamut pixels {output.gamut_count}\n"
            f"channels {int(output.gamut_channel_mask.sum())}\n"
            f"first {output.gamut_coordinates[0] if output.gamut_coordinates else 'none'}"
        )

    # ----------------------------------------------------------- snapshot
    def _populate_table(self) -> None:
        self.table.setRowCount(0)
        self._segment_rows.clear()
        if self.manager is None:
            return
        for row, spec in enumerate(self.manager.segments):
            self.table.insertRow(row)
            self._segment_rows[spec.index] = row
            seg_item = QTableWidgetItem(f"{spec.index:03d}")
            seg_item.setData(Qt.UserRole, spec.index)
            self.table.setItem(row, 0, seg_item)
            self.table.setItem(row, 1, QTableWidgetItem(spec.label))
            for column in range(2, 7):
                self.table.setItem(row, column, QTableWidgetItem("—"))
            self._update_table_record(spec.index)
        self.table.resizeRowsToContents()
        self._update_progress()

    def _update_table_record(self, index: int) -> None:
        if self.manager is None or index not in self._segment_rows:
            return
        record = self.manager.state.records[index]
        row = self._segment_rows[index]
        status = QTableWidgetItem(record.state.value.upper())
        status.setForeground(QBrush(QColor(STATE_COLORS.get(record.state.value, "#dce7f3"))))
        font = QFont("Segoe UI", 9, QFont.DemiBold)
        status.setFont(font)
        self.table.setItem(row, 2, status)
        param = QTableWidgetItem(_short_hash(record.param_version) if record.param_version else "—")
        self.table.setItem(row, 3, param)
        peak = record.stats.get("peak_radiance")
        self.table.setItem(row, 4, QTableWidgetItem(f"{float(peak):.4g}" if peak is not None else "—"))
        gamut = record.stats.get("gamut_pixel_count")
        self.table.setItem(row, 5, QTableWidgetItem(str(gamut) if gamut is not None else "—"))
        self.table.setItem(row, 6, QTableWidgetItem(_short_hash(record.content_hash) if record.content_hash else "—"))
        if record.error:
            self.table.item(row, 2).setToolTip(json.dumps(record.error, ensure_ascii=False, indent=2))

    def _refresh_from_snapshot(self, initial: bool = False) -> None:
        if self.manager is None:
            return
        self._update_progress()
        for index in self.manager.state.records:
            self._update_table_record(index)
        committed = self.manager.progress()["committed"]
        if committed:
            try:
                self.output = assemble_output(self.scene, self.manager, require_complete=False)
                self._set_output_previews(self.output)
            except WorkbenchError as exc:
                self.show_error(exc.to_dict())
        else:
            self.output = None
            self.composite_panel.set_message("No committed composite segments")
            self.chroma_panel.set_message("No committed chroma buffers")
            self.histogram.set_values(np.zeros(2))
            self.chromaticity.set_data(None, self.gamut_combo.currentText())
        self._update_pipeline_stats(initial)
        self.table.blockSignals(True)
        if self.table.rowCount() and self.table.currentRow() < 0:
            self.table.selectRow(0)
        self.table.blockSignals(False)
        if self.table.currentRow() >= 0:
            self._show_selected_segment(self.table.currentRow(), 0)

    def _update_progress(self) -> None:
        if self.manager is None:
            return
        progress = self.manager.progress()
        self.progress.setMaximum(max(1, progress["total"]))
        self.progress.setValue(progress["committed"])
        self.progress_label.setText(f"{progress['committed']} / {progress['total']} committed")
        self.segment_summary.setText(
            f"{progress['committed']} / {progress['total']} committed  ·  "
            f"{progress['failed']} failed  ·  {progress['pending']} pending"
        )

    def _update_pipeline_stats(self, initial: bool = False) -> None:
        if self.manager is None or self.scene is None:
            return
        progress = self.manager.progress()
        alignment = self.prepared.alignment_report if self.prepared else {}
        invalid = self.manager.invalidated_reason
        self.pipeline_stats.setText(
            f"SCENE      {self.scene.metadata.scene_id}\n"
            f"SNAPSHOT   {_short_hash(self.manager.snapshot_key)}\n"
            f"CHECKPOINT {self.manager.snapshot_dir}\n\n"
            f"SEGMENTS   {progress['committed']} committed / {progress['total']}\n"
            f"FAILED     {progress['failed']}\n"
            f"PENDING    {progress['pending']}\n\n"
            f"ALIGNMENT  mean structural residual "
            f"{alignment.get('mean_structural_residual', 0.0):.6g}\n"
            f"EDGE       {alignment.get('mean_edge_residual', 0.0):.6g}\n\n"
            f"METADATA   {self.scene.metadata.metadata_version}\n"
            f"META HASH  {_short_hash(self.scene.metadata.metadata_hash)}\n"
            f"ALGORITHM  {self.manager.manifest.get('algorithm_version')}\n"
            f"PARAM SNAP {len(self.manager.manifest.get('parameter_snapshots', {}))}\n"
            f"INVALID    {invalid['code'] if invalid else 'none'}"
        )

    # ---------------------------------------------------------- processing
    def start_processing(self) -> None:
        if self.scene is None or self.manager is None or self._job_running:
            return
        structure = self._structure()
        expected_config = segmentation_spec(
            self.scene.width,
            self.scene.height,
            self.scene.band_count,
            split_mode=structure["split_mode"],
            tile_width=structure["tile_width"],
            tile_height=structure["tile_height"],
            interpolation=structure["interpolation"],
        )
        expected_config["segments"] = [
            segment.to_dict()
            for segment in make_segments(
                self.scene.width,
                self.scene.height,
                self.scene.band_count,
                split_mode=structure["split_mode"],
                tile_width=structure["tile_width"],
                tile_height=structure["tile_height"],
            )
        ]
        if self.manager.segmentation_config != expected_config:
            self.manager.close()
            try:
                self._build_manager()
            except WorkbenchError as exc:
                self.show_error(exc.to_dict())
                self.manager = None
                return
            self._build_band_previews()
            self._refresh_from_snapshot(initial=True)
            self.show_info("Structure changed; switched to an independent checkpoint snapshot.")
        params = self.current_params()
        progress = self.manager.progress()
        if progress["pending"] == 0 and progress["failed"] == 0:
            self.show_info(
                "All segments are committed. Locked params are preserved; use REPROCESS ALL to apply new params."
            )
            return
        self.error_banner.hide()
        self.job = ProcessingJob(
            self.scene,
            self.manager,
            resource=ResourceConfig(self.workers_spin.value(), self.buffer_spin.value()),
            params=params,
        )
        self.job_thread = JobThread(self.job, self)
        self._job_running = True
        self.job_thread.event.connect(self._on_job_event)
        self.job_thread.result_ready.connect(self._on_job_finished)
        self.job_thread.failure.connect(self._on_job_failure)
        self._set_running_ui(True)
        self.header_status.setText("PROCESSING")
        self.header_status.setStyleSheet(
            "color:#ffca7a;background:#2b2111;border:1px solid #705528;"
            "border-radius:11px;padding:3px 10px;font-family:'Cascadia Mono';font-size:9px;"
        )
        self.job_thread.start()

    def pause_processing(self) -> None:
        if self.job is not None and self._job_running:
            self.job.pause()
            self.header_status.setText("PAUSING")
            self.pause_button.setEnabled(False)
            self.show_info("Pause requested; in-flight segments will finish and commit before scheduling stops.")

    def _set_running_ui(self, running: bool) -> None:
        self.process_button.setEnabled(not running)
        self.pause_button.setEnabled(running)
        self.reprocess_button.setEnabled(not running)
        self.export_button.setEnabled(not running)
        self.compare_button.setEnabled(not running)
        self.structure_button.setEnabled(not running)
        self.scene_combo.setEnabled(not running)
        for widget in (
            self.split_combo,
            self.tile_spin,
            self.interp_combo,
            self.workers_spin,
            self.buffer_spin,
        ):
            widget.setEnabled(not running)

    def _on_job_event(self, event: dict[str, Any]) -> None:
        event_type = event.get("type")
        if event_type in {"segment_state", "progress"}:
            index = event.get("segment_index")
            if index is not None:
                self._update_table_record(int(index))
            self._update_progress()
            if event_type == "segment_state" and event.get("state") == "failed":
                self.show_error(event.get("error") or {"code": "E_INTERNAL", "message": "Segment failed"})
            if event_type == "segment_state" and event.get("state") == "committed":
                progress = self.manager.progress()
                stride = max(1, progress["total"] // 4)
                if progress["committed"] % stride == 0 or progress["committed"] == progress["total"]:
                    self._refresh_from_snapshot()
        elif event_type == "run_started":
            self.progress.setMaximum(max(1, event["progress"]["total"]))
            self.progress.setValue(event["progress"]["committed"])
            alignment = event.get("alignment", {})
            self.header_status.setText(
                f"PROCESSING  ·  residual {alignment.get('mean_structural_residual', 0.0):.3g}"
            )

    def _on_job_finished(self, result: dict[str, Any]) -> None:
        self._job_running = False
        self._set_running_ui(False)
        if result.get("ok"):
            self.error_banner.hide()
        if result.get("ok"):
            self.header_status.setText("COMPLETE")
            self.header_status.setStyleSheet(
                "color:#36d6a0;background:#10261e;border:1px solid #245743;"
                "border-radius:11px;padding:3px 10px;font-family:'Cascadia Mono';font-size:9px;"
            )
        elif result.get("paused"):
            self.header_status.setText("PAUSED")
            self.header_status.setStyleSheet(
                "color:#ffca7a;background:#2b2111;border:1px solid #705528;"
                "border-radius:11px;padding:3px 10px;font-family:'Cascadia Mono';font-size:9px;"
            )
        else:
            self.header_status.setText("INCOMPLETE")
        self._refresh_from_snapshot()
        metrics = result.get("resource_metrics", {})
        self.pipeline_stats.setText(
            self.pipeline_stats.text()
            + f"\nRUN        peak buffer {metrics.get('peak_used_bytes', 0) / (1024 * 1024):.2f} MiB"
            + f" / wait {metrics.get('wait_count', 0)}"
        )

    def _on_job_failure(self, error: dict[str, Any]) -> None:
        self._job_running = False
        self._set_running_ui(False)
        self.show_error(error)
        self.header_status.setText(error.get("code", "FAILED"))
        self.header_status.setStyleSheet(
            "color:#ffb3bd;background:#32161b;border:1px solid #793441;"
            "border-radius:11px;padding:3px 10px;font-family:'Cascadia Mono';font-size:9px;"
        )
        self._refresh_from_snapshot()

    def reprocess_all(self) -> None:
        if self.manager is None or self._job_running:
            return
        answer = QMessageBox.question(
            self,
            "Reprocess all",
            "This archives the current checkpoint snapshot and recomputes every segment. Continue?",
            QMessageBox.Yes | QMessageBox.No,
            QMessageBox.No,
        )
        if answer != QMessageBox.Yes:
            return
        self.manager.reset()
        self.output = None
        self._populate_table()
        self._refresh_from_snapshot(initial=True)
        self.start_processing()

    # ------------------------------------------------------------- inspect
    def _show_selected_segment(self, current_row: int, _column: int = 0, *_args: Any) -> None:
        if self.manager is None or current_row < 0:
            return
        item = self.table.item(current_row, 0)
        if item is None:
            return
        index = int(item.data(Qt.UserRole))
        record = self.manager.state.records[index]
        spec = self.manager.segments[index]
        if record.state != SegmentState.COMMITTED:
            self.selected_panel.set_message(f"Segment {index:03d} · {record.state.value}")
            self.segment_detail.fusion.set_message("No committed HDR buffer")
            self.segment_detail.chroma.set_message("No committed chroma buffer")
            self.segment_detail.composite.set_message("No committed display buffer")
            self.segment_detail.stats.setText(
                f"SEGMENT {index:03d}  ·  {record.state.value.upper()}\n"
                f"scope {spec.label}\n"
                f"error {json.dumps(record.error, ensure_ascii=False) if record.error else 'none'}"
            )
            return
        try:
            loaded = self.manager.load_segment(index)
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        arrays = loaded["arrays"]
        meta = loaded["meta"]
        self.selected_panel.set_array(
            arrays["display"],
            caption=f"segment {index:03d} · {spec.label}",
        )
        fusion = tone_to_uint8(arrays["radiance"])
        self.segment_detail.fusion.set_array(np.repeat(fusion[..., np.newaxis], 3, axis=2))
        chroma = arrays["rgb_chroma"].astype(np.float64)
        maximum = np.maximum(chroma.max(axis=2, keepdims=True), 1e-9)
        chroma_rgb = np.rint(np.power(np.clip(chroma / maximum, 0.0, 1.0), 1 / 2.2) * 255).astype(np.uint8)
        self.segment_detail.chroma.set_array(chroma_rgb)
        self.segment_detail.composite.set_array(arrays["display"])
        stats = record.stats
        self.segment_detail.stats.setText(
            f"SEG {index:03d}  {spec.label}\n"
            f"state      {record.state.value}\n"
            f"param      {_short_hash(meta['param_version'])}\n"
            f"content    {_short_hash(meta['content_hash'])}\n"
            f"peak       {stats.get('peak_radiance', 0.0):.6g}\n"
            f"mean       {stats.get('mean_radiance', 0.0):.6g}\n"
            f"gamut      {stats.get('gamut_pixel_count', 0)} px / "
            f"{stats.get('gamut_channel_count', 0)} ch\n"
            f"residual   {meta.get('alignment_residual', 0.0):.6g}\n"
            f"exposures  {meta['params']['exposure_indices']}"
        )

    # --------------------------------------------------------------- export
    def export_results(self) -> None:
        if self.manager is None or self.scene is None:
            return
        try:
            output = assemble_output(self.scene, self.manager, require_complete=True)
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        default = self.workspace / "exports" / self.scene.metadata.scene_id
        default.mkdir(parents=True, exist_ok=True)
        chosen = QFileDialog.getExistingDirectory(
            self,
            "Export complete composite",
            str(default),
            QFileDialog.ShowDirsOnly,
        )
        if not chosen:
            return
        try:
            interpolation = str(self.manager.segmentation_config["interpolation"])
            alignment = (
                self.prepared
                if self.prepared is not None
                and self.prepared.alignment_report.get("interpolation") == interpolation
                else prepare_scene(self.scene, interpolation)
            )
            manifest = export_output(
                output,
                Path(chosen),
                scene_id=self.scene.metadata.scene_id,
                title=self.scene.metadata.title,
                segment_order=[segment.index for segment in self.manager.segments],
                alignment=alignment,
            )
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        self.show_info(
            f"Export complete: {chosen}\n"
            "PNG / NPY / stats / export manifest written in stable segment order.\n"
            f"{manifest['files']}"
        )

    # -------------------------------------------------------------- compare
    def compare_full(self) -> None:
        if self._compare_running or self._job_running or self.manager is None:
            return
        try:
            current = assemble_output(self.scene, self.manager, require_complete=True)
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())
            return
        self._compare_running = True
        self.compare_button.setEnabled(False)
        self.compare_tab_button.setEnabled(False)
        self.compare_report.setText("Running independent full recompute and exact pixel comparison...")
        self.compare_thread = CompareThread(
            self.scene.metadata.root,
            self.workspace,
            self.current_params(),
            self._structure(),
            current,
            self,
        )
        self.compare_thread.result_ready.connect(self._on_compare_finished)
        self.compare_thread.failure.connect(self._on_compare_failed)
        self.compare_thread.start()

    def _on_compare_finished(self, payload: dict[str, Any]) -> None:
        self._compare_running = False
        self.compare_button.setEnabled(True)
        self.compare_tab_button.setEnabled(True)
        report = payload["report"]
        mismatch = report.get("first_mismatch")
        self.heatmap.set_difference(payload["diff"], mismatch)
        if report.get("equal"):
            self.compare_report.setText(
                "Pixel-exact: radiance / chroma / linear RGB / display / gamut mask\n"
                "Peak radiance and the out-of-gamut set are identical.\n"
                f"Independent snapshot {payload['workspace']}"
            )
            self.header_status.setText("EXACT MATCH")
        else:
            self.compare_report.setText(
                "E_DIVERGE\n"
                f"stage {mismatch['stage']} · segment {mismatch['segment_id']} · "
                f"({mismatch['x']},{mismatch['y']})\n"
                f"left {mismatch.get('left')} · right {mismatch.get('right')}"
            )
            self.show_error(
                {
                    "code": "E_DIVERGE",
                    "message": "Full recompute differs from committed snapshot",
                    "details": mismatch,
                }
            )
            self.header_status.setText("E_DIVERGE")

    def _on_compare_failed(self, error: dict[str, Any]) -> None:
        self._compare_running = False
        self.compare_button.setEnabled(True)
        self.compare_tab_button.setEnabled(True)
        self.show_error(error)
        self.compare_report.setText(json.dumps(error, ensure_ascii=False, indent=2))

    # --------------------------------------------------------------- misc
    def regenerate_samples(self) -> None:
        if self._job_running:
            return
        answer = QMessageBox.question(
            self,
            "Regenerate samples",
            "Regenerate samples/ from the fixed seed? Mismatched checkpoints will be invalidated. Continue?",
            QMessageBox.Yes | QMessageBox.No,
            QMessageBox.No,
        )
        if answer != QMessageBox.Yes:
            return
        try:
            if self.manager is not None:
                self.manager.close()
                self.manager = None
            generate_all(self.samples, seed=424242, scene_count=3, with_faults=True)
            self._load_catalog(self.scene.metadata.scene_id if self.scene else None)
        except WorkbenchError as exc:
            self.show_error(exc.to_dict())

    def show_error(self, error: dict[str, Any]) -> None:
        code = error.get("code", "E_INTERNAL")
        message = error.get("message", "Unknown error")
        details = error.get("details")
        text = f"{code}  ·  {message}"
        if details:
            text += "\n" + json.dumps(details, ensure_ascii=False, default=str)[:700]
        self.error_banner.setText(text)
        self.error_banner.show()

    def show_info(self, message: str) -> None:
        self.pipeline_stats.setText(self.pipeline_stats.text() + f"\n{message}")

    def closeEvent(self, event: Any) -> None:
        if self._job_running and self.job is not None:
            self.job.pause()
            if self.job_thread is not None:
                self.job_thread.wait(10000)
                if not self.job_thread.isRunning():
                    self._job_running = False
            if self._job_running:
                QMessageBox.warning(
                    self,
                    "Still processing",
                    "In-flight segments are still completing atomic commits. Please exit again shortly.",
                )
                event.ignore()
                return
        if self.compare_thread is not None and self._compare_running:
            self.compare_thread.wait(10000)
            if not self.compare_thread.isRunning():
                self._compare_running = False
        if self._compare_running:
            QMessageBox.warning(
                self,
                "Comparison still running",
                "The independent recompute comparison is still finishing. Please exit again shortly.",
            )
            event.ignore()
            return
        if self.manager is not None:
            self.manager.close()
            self.manager = None
        event.accept()


def _short_hash(value: str | None) -> str:
    if not value:
        return "—"
    return value.split(":", 1)[-1][:12].upper()


def run(
    samples: Path = Path("samples"),
    workspace: Path = Path("runtime"),
    scene: str | None = None,
) -> int:
    app = QApplication.instance() or QApplication([])
    app.setApplicationName("SpectraForge")
    app.setStyle("Fusion")
    app.setStyleSheet(APP_QSS)
    window = MainWindow(samples=samples, workspace=workspace, initial_scene=scene)
    window.show()
    return int(app.exec())
