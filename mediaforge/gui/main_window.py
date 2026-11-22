from __future__ import annotations

import json
import os
from dataclasses import replace
from pathlib import Path
from typing import Any

from ..errors import E_PARAM, WorkbenchError, fail
from ..injection import InjectionConfig
from ..metadata import export_metadata, warnings_as_dicts
from ..model import Chapter, ContainerModel, MediaTrack
from ..package import build_package, write_package
from ..pipeline import discover_sample_groups, load_sample, metadata_path, truth_path
from ..segmented import pack_segmented
from ..timebase import Timebase
from ..validator import validate_artifact
from .qt_compat import (
    QT_API,
    QtCore,
    QtGui,
    QtWidgets,
    exec_application,
    extended_selection,
    no_edit_triggers,
    resize_to_contents,
    select_rows,
    stretch_section,
)


PROJECT_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_SAMPLES = PROJECT_ROOT / "samples"
DEFAULT_ARTIFACTS = PROJECT_ROOT / "artifacts" / "gui"
DEFAULT_CHECKPOINTS = PROJECT_ROOT / "artifacts" / "checkpoints"


class MainWindow(QtWidgets.QMainWindow):
    def __init__(self) -> None:
        super().__init__()
        self.setWindowTitle(f"MediaForge Asset Container Workbench · {QT_API}")
        self.resize(1280, 860)
        self.model: ContainerModel | None = None
        self.truth_file: Path | None = None
        self.current_sample: Path | None = None
        self.loaded_version = ""
        self.last_artifact_path: Path | None = None
        self._build_ui()
        self._populate_samples()

    def _build_ui(self) -> None:
        central = QtWidgets.QWidget(self)
        layout = QtWidgets.QVBoxLayout(central)
        layout.setContentsMargins(12, 10, 12, 10)
        layout.setSpacing(8)

        title = QtWidgets.QLabel("多媒体容器封装与元数据工作台")
        title.setStyleSheet("font-size: 20px; font-weight: 600;")
        subtitle = QtWidgets.QLabel(
            "确定性样本 · v1/v2/v3 映射 · 跨版本损失报告 · 分段检查点 · 独立校验"
        )
        subtitle.setStyleSheet("color: #556070;")
        layout.addWidget(title)
        layout.addWidget(subtitle)

        source_box = QtWidgets.QGroupBox("样本与版本")
        source_layout = QtWidgets.QGridLayout(source_box)
        self.samples_root_edit = QtWidgets.QLineEdit(str(DEFAULT_SAMPLES))
        self.sample_combo = QtWidgets.QComboBox()
        self.source_version_combo = QtWidgets.QComboBox()
        self.source_version_combo.addItems(["v1", "v2", "v3"])
        self.source_version_combo.setCurrentText("v3")
        self.target_version_combo = QtWidgets.QComboBox()
        self.target_version_combo.addItems(["v1", "v2", "v3"])
        self.target_version_combo.setCurrentText("v1")
        browse_samples = QtWidgets.QPushButton("选择目录")
        load_button = QtWidgets.QPushButton("加载并导入裸流")
        load_button.setDefault(True)
        source_layout.addWidget(QtWidgets.QLabel("samples"), 0, 0)
        source_layout.addWidget(self.samples_root_edit, 0, 1, 1, 2)
        source_layout.addWidget(browse_samples, 0, 3)
        source_layout.addWidget(load_button, 0, 4)
        source_layout.addWidget(QtWidgets.QLabel("样本组"), 1, 0)
        source_layout.addWidget(self.sample_combo, 1, 1)
        source_layout.addWidget(QtWidgets.QLabel("源版本"), 1, 2)
        source_layout.addWidget(self.source_version_combo, 1, 3)
        source_layout.addWidget(QtWidgets.QLabel("目标版本"), 1, 4)
        source_layout.addWidget(self.target_version_combo, 1, 5)
        layout.addWidget(source_box)

        action_row = QtWidgets.QHBoxLayout()
        self.export_button = QtWidgets.QPushButton("直接封装并校验")
        self.segment_button = QtWidgets.QPushButton("分段封装（并发+检查点）")
        self.preview_button = QtWidgets.QPushButton("预览目标元数据")
        self.validate_button = QtWidgets.QPushButton("独立校验产物")
        self.clear_checkpoint_button = QtWidgets.QPushButton("清空检查点")
        self.inject_button = QtWidgets.QPushButton("执行异常注入")
        for button in (
            self.export_button,
            self.segment_button,
            self.preview_button,
            self.validate_button,
            self.clear_checkpoint_button,
            self.inject_button,
        ):
            action_row.addWidget(button)
        action_row.addStretch(1)
        layout.addLayout(action_row)

        self.tabs = QtWidgets.QTabWidget()
        self.tracks_tab = QtWidgets.QWidget()
        self.chapters_tab = QtWidgets.QWidget()
        self.metadata_tab = QtWidgets.QWidget()
        self.validation_tab = QtWidgets.QWidget()
        self.checkpoint_tab = QtWidgets.QWidget()
        self.injection_tab = QtWidgets.QWidget()
        self.tabs.addTab(self.tracks_tab, "轨道")
        self.tabs.addTab(self.chapters_tab, "章节")
        self.tabs.addTab(self.metadata_tab, "元数据 / 损失")
        self.tabs.addTab(self.validation_tab, "独立校验")
        self.tabs.addTab(self.checkpoint_tab, "检查点")
        self.tabs.addTab(self.injection_tab, "异常注入")
        layout.addWidget(self.tabs, 1)

        self._build_tracks_tab()
        self._build_chapters_tab()
        self._build_metadata_tab()
        self._build_validation_tab()
        self._build_checkpoint_tab()
        self._build_injection_tab()

        self.status = self.statusBar()
        self.status.showMessage("就绪")

        load_button.clicked.connect(self.load_current_sample)
        browse_samples.clicked.connect(self.browse_samples)
        self.export_button.clicked.connect(self.export_direct)
        self.segment_button.clicked.connect(self.export_segmented)
        self.preview_button.clicked.connect(self.preview_metadata)
        self.validate_button.clicked.connect(self.validate_artifact)
        self.clear_checkpoint_button.clicked.connect(self.clear_checkpoints)
        self.inject_button.clicked.connect(self.run_injection)
        self.target_version_combo.currentTextChanged.connect(self.preview_metadata)

    def _build_tracks_tab(self) -> None:
        layout = QtWidgets.QVBoxLayout(self.tracks_tab)
        hint = QtWidgets.QLabel(
            "可编辑语言、角色（逗号分隔）与 HDR；采样数/帧数、时基为受保护字段。"
        )
        hint.setStyleSheet("color: #556070;")
        layout.addWidget(hint)
        self.track_table = QtWidgets.QTableWidget(0, 9)
        self.track_table.setHorizontalHeaderLabels(
            ["Track ID", "类型", "语言", "时基", "采样/帧数", "时长 ticks", "Codec", "角色", "HDR"]
        )
        self.track_table.setSelectionBehavior(select_rows())
        self.track_table.setSelectionMode(extended_selection())
        self.track_table.horizontalHeader().setSectionResizeMode(resize_to_contents())
        self.track_table.horizontalHeader().setStretchLastSection(True)
        layout.addWidget(self.track_table, 1)
        apply_button = QtWidgets.QPushButton("应用轨道编辑")
        apply_button.clicked.connect(self.apply_track_edits)
        layout.addWidget(apply_button, 0, QtCore.Qt.AlignmentFlag.AlignRight if hasattr(QtCore.Qt, "AlignmentFlag") else QtCore.Qt.AlignRight)

    def _build_chapters_tab(self) -> None:
        layout = QtWidgets.QVBoxLayout(self.chapters_tab)
        self.chapter_table = QtWidgets.QTableWidget(0, 6)
        self.chapter_table.setHorizontalHeaderLabels(
            ["编号", "Chapter ID", "开始 ticks", "结束 ticks", "标题", "标签 JSON"]
        )
        self.chapter_table.setSelectionBehavior(select_rows())
        self.chapter_table.setSelectionMode(extended_selection())
        self.chapter_table.horizontalHeader().setSectionResizeMode(resize_to_contents())
        self.chapter_table.horizontalHeader().setStretchLastSection(True)
        layout.addWidget(self.chapter_table, 1)
        row = QtWidgets.QHBoxLayout()
        add_button = QtWidgets.QPushButton("新增章节")
        remove_button = QtWidgets.QPushButton("删除选中")
        apply_button = QtWidgets.QPushButton("应用章节编辑")
        row.addWidget(add_button)
        row.addWidget(remove_button)
        row.addStretch(1)
        row.addWidget(apply_button)
        layout.addLayout(row)
        add_button.clicked.connect(self.add_chapter_row)
        remove_button.clicked.connect(self.remove_chapter_rows)
        apply_button.clicked.connect(self.apply_chapter_edits)

    def _build_metadata_tab(self) -> None:
        splitter = QtWidgets.QSplitter(QtCore.Qt.Orientation.Vertical if hasattr(QtCore.Qt, "Orientation") else QtCore.Qt.Vertical)
        top = QtWidgets.QWidget()
        top_layout = QtWidgets.QVBoxLayout(top)
        top_layout.setContentsMargins(0, 0, 0, 0)
        top_layout.addWidget(QtWidgets.QLabel("E_LOSSY 降级警告"))
        self.loss_table = QtWidgets.QTableWidget(0, 4)
        self.loss_table.setHorizontalHeaderLabels(["CODE", "字段路径", "目标版本", "说明"])
        self.loss_table.setEditTriggers(no_edit_triggers())
        self.loss_table.horizontalHeader().setSectionResizeMode(resize_to_contents())
        self.loss_table.horizontalHeader().setStretchLastSection(True)
        top_layout.addWidget(self.loss_table)
        bottom = QtWidgets.QWidget()
        bottom_layout = QtWidgets.QVBoxLayout(bottom)
        bottom_layout.setContentsMargins(0, 0, 0, 0)
        bottom_layout.addWidget(QtWidgets.QLabel("目标版本元数据预览"))
        self.metadata_text = QtWidgets.QPlainTextEdit()
        self.metadata_text.setReadOnly(True)
        self.metadata_text.setLineWrapMode(QtWidgets.QPlainTextEdit.LineWrapMode.NoWrap if hasattr(QtWidgets.QPlainTextEdit, "LineWrapMode") else QtWidgets.QPlainTextEdit.NoWrap)
        self.metadata_text.setFont(QtGui.QFontDatabase.systemFont(QtGui.QFontDatabase.SystemFont.FixedFont) if hasattr(QtGui.QFontDatabase, "SystemFont") else QtGui.QFont("Consolas"))
        bottom_layout.addWidget(self.metadata_text)
        splitter.addWidget(top)
        splitter.addWidget(bottom)
        splitter.setSizes([280, 440])
        layout = QtWidgets.QVBoxLayout(self.metadata_tab)
        layout.addWidget(splitter)

    def _build_validation_tab(self) -> None:
        layout = QtWidgets.QVBoxLayout(self.validation_tab)
        row = QtWidgets.QHBoxLayout()
        self.artifact_path_edit = QtWidgets.QLineEdit()
        browse = QtWidgets.QPushButton("选择产物")
        row.addWidget(QtWidgets.QLabel("封装产物"))
        row.addWidget(self.artifact_path_edit, 1)
        row.addWidget(browse)
        layout.addLayout(row)
        self.validation_text = QtWidgets.QPlainTextEdit()
        self.validation_text.setReadOnly(True)
        layout.addWidget(self.validation_text, 1)
        browse.clicked.connect(self.browse_artifact)

    def _build_checkpoint_tab(self) -> None:
        layout = QtWidgets.QVBoxLayout(self.checkpoint_tab)
        row = QtWidgets.QHBoxLayout()
        self.checkpoint_root_edit = QtWidgets.QLineEdit(str(DEFAULT_CHECKPOINTS))
        refresh = QtWidgets.QPushButton("刷新")
        row.addWidget(QtWidgets.QLabel("检查点目录"))
        row.addWidget(self.checkpoint_root_edit, 1)
        row.addWidget(refresh)
        layout.addLayout(row)
        self.checkpoint_text = QtWidgets.QPlainTextEdit()
        self.checkpoint_text.setReadOnly(True)
        layout.addWidget(self.checkpoint_text, 1)
        self.checkpoint_workers = QtWidgets.QSpinBox()
        self.checkpoint_workers.setRange(1, 4)
        self.checkpoint_workers.setValue(2)
        self.checkpoint_buffer = QtWidgets.QSpinBox()
        self.checkpoint_buffer.setRange(64 * 1024, 64 * 1024 * 1024)
        self.checkpoint_buffer.setSingleStep(64 * 1024)
        self.checkpoint_buffer.setValue(4 * 1024 * 1024)
        controls = QtWidgets.QHBoxLayout()
        controls.addWidget(QtWidgets.QLabel("最大并行度"))
        controls.addWidget(self.checkpoint_workers)
        controls.addWidget(QtWidgets.QLabel("每线程缓冲上限 (bytes)"))
        controls.addWidget(self.checkpoint_buffer)
        controls.addStretch(1)
        layout.addLayout(controls)
        refresh.clicked.connect(self.refresh_checkpoints)

    def _build_injection_tab(self) -> None:
        layout = QtWidgets.QFormLayout(self.injection_tab)
        self.injection_mode = QtWidgets.QComboBox()
        self.injection_mode.addItems(
            ["metadata_conflict", "timestamp_inversion", "missing_track", "corrupt_segment"]
        )
        self.injection_track = QtWidgets.QComboBox()
        self.injection_offset = QtWidgets.QSpinBox()
        self.injection_offset.setRange(0, 10 * 1024 * 1024)
        self.injection_offset.setValue(73)
        self.injection_output = QtWidgets.QPlainTextEdit()
        self.injection_output.setReadOnly(True)
        layout.addRow("异常类型", self.injection_mode)
        layout.addRow("目标轨道", self.injection_track)
        layout.addRow("损坏字节偏移", self.injection_offset)
        layout.addRow(self.injection_output)

    def _populate_samples(self) -> None:
        self.sample_combo.clear()
        try:
            groups = discover_sample_groups(self.samples_root_edit.text())
        except WorkbenchError as exc:
            self.status.showMessage(exc.message)
            return
        for group in groups:
            self.sample_combo.addItem(group.name, str(group))

    def browse_samples(self) -> None:
        directory = QtWidgets.QFileDialog.getExistingDirectory(self, "选择 samples 目录", str(DEFAULT_SAMPLES))
        if directory:
            self.samples_root_edit.setText(directory)
            self._populate_samples()

    def browse_artifact(self) -> None:
        filename, _ = QtWidgets.QFileDialog.getOpenFileName(
            self,
            "选择封装产物",
            str(DEFAULT_ARTIFACTS),
            "MediaForge Container (*.mfz);;All Files (*)",
        )
        if filename:
            self.artifact_path_edit.setText(filename)
            self.last_artifact_path = Path(filename)

    def load_current_sample(self) -> None:
        data = self.sample_combo.currentData()
        if not data:
            self._error("没有可用样本", "请先选择 samples 目录。")
            return
        try:
            loaded = load_sample(Path(data), self.source_version_combo.currentText())
        except WorkbenchError as exc:
            self._error(exc.code, json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))
            return
        self.model = loaded.model
        self.truth_file = truth_path(loaded.sample_dir)
        self.current_sample = loaded.sample_dir
        self.loaded_version = loaded.metadata_version
        self._refresh_model_views()
        self.status.showMessage(
            f"已加载 {loaded.sample_dir.name} / {loaded.metadata_version}，"
            f"{len(self.model.tracks)} 轨，{len(self.model.chapters)} 章"
        )
        self.refresh_checkpoints()

    def _refresh_model_views(self) -> None:
        self._refresh_track_table()
        self._refresh_chapter_table()
        self.preview_metadata()
        self._refresh_injection_tracks()

    def _refresh_track_table(self) -> None:
        if self.model is None:
            self.track_table.setRowCount(0)
            return
        self.track_table.setRowCount(len(self.model.tracks))
        for row, track in enumerate(self.model.tracks):
            values = [
                track.track_id,
                track.kind,
                track.language,
                f"{track.timebase.num}/{track.timebase.den}",
                str(track.sample_count),
                str(track.duration_ticks),
                track.codec,
                ", ".join(track.roles),
                track.hdr or "",
            ]
            for column, value in enumerate(values):
                item = QtWidgets.QTableWidgetItem(value)
                if column in {0, 1, 3, 4, 5, 6}:
                    item.setFlags(item.flags() & ~QtCore.Qt.ItemFlag.ItemIsEditable if hasattr(QtCore.Qt, "ItemFlag") else item.flags() & ~QtCore.Qt.ItemIsEditable)
                self.track_table.setItem(row, column, item)

    def _refresh_chapter_table(self) -> None:
        if self.model is None:
            self.chapter_table.setRowCount(0)
            return
        self.chapter_table.setRowCount(len(self.model.chapters))
        for row, chapter in enumerate(self.model.chapters):
            values = [
                str(chapter.index),
                chapter.chapter_id,
                str(chapter.start_ticks),
                str(chapter.end_ticks),
                chapter.title,
                json.dumps(chapter.tags, ensure_ascii=False, sort_keys=True),
            ]
            for column, value in enumerate(values):
                self.chapter_table.setItem(row, column, QtWidgets.QTableWidgetItem(value))

    def _refresh_injection_tracks(self) -> None:
        self.injection_track.clear()
        if self.model is None:
            return
        for track in self.model.tracks:
            self.injection_track.addItem(f"{track.track_id} ({track.kind})", track.track_id)

    def _apply_track_edits_to_model(self) -> ContainerModel:
        if self.model is None:
            fail(E_PARAM, "$.model", "load a sample first")
        tracks: list[MediaTrack] = []
        for row, track in enumerate(self.model.tracks):
            language_item = self.track_table.item(row, 2)
            roles_item = self.track_table.item(row, 7)
            hdr_item = self.track_table.item(row, 8)
            language = language_item.text().strip() if language_item else track.language
            roles = tuple(
                part.strip()
                for part in (roles_item.text() if roles_item else ",".join(track.roles)).split(",")
                if part.strip()
            )
            hdr = (hdr_item.text().strip() if hdr_item else "") or None
            if not language:
                fail(E_PARAM, f"$.tracks[{row}].language", "track language cannot be empty")
            tracks.append(replace(track, language=language, roles=roles, hdr=hdr))
        return self.model.copy(tracks=tuple(tracks))

    def _apply_chapter_edits_to_model(self) -> ContainerModel:
        if self.model is None:
            fail(E_PARAM, "$.model", "load a sample first")
        chapters: list[Chapter] = []
        for row in range(self.chapter_table.rowCount()):
            try:
                number = int(self._cell(self.chapter_table, row, 0))
                chapter_id = self._cell(self.chapter_table, row, 1).strip()
                start = int(self._cell(self.chapter_table, row, 2))
                end = int(self._cell(self.chapter_table, row, 3))
                title = self._cell(self.chapter_table, row, 4)
                tags_value = self._cell(self.chapter_table, row, 5).strip() or "{}"
                tags = json.loads(tags_value)
            except (ValueError, json.JSONDecodeError) as exc:
                fail(E_PARAM, f"$.chapters[{row}]", f"invalid chapter row: {exc}")
            if not isinstance(tags, dict):
                fail(E_PARAM, f"$.chapters[{row}].tags", "chapter tags must be a JSON object")
            if not chapter_id:
                fail(E_PARAM, f"$.chapters[{row}].chapter_id", "chapter id cannot be empty")
            chapters.append(
                Chapter(
                    chapter_id=chapter_id,
                    index=number,
                    start_ticks=start,
                    end_ticks=end,
                    title=title,
                    tags=tags,
                )
            )
        return self.model.with_chapters(chapters)

    def apply_track_edits(self) -> None:
        try:
            self.model = self._apply_track_edits_to_model()
        except WorkbenchError as exc:
            self._error(exc.code, json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))
            return
        self._refresh_model_views()
        self.status.showMessage("轨道属性已应用到内部模型")

    def apply_chapter_edits(self) -> None:
        try:
            self.model = self._apply_chapter_edits_to_model()
        except WorkbenchError as exc:
            self._error(exc.code, json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))
            return
        self._refresh_model_views()
        self.status.showMessage("章节表已应用到内部模型")

    def add_chapter_row(self) -> None:
        row = self.chapter_table.rowCount()
        self.chapter_table.insertRow(row)
        duration = self.model.duration_ticks if self.model else 0
        start = self.model.chapters[-1].end_ticks if self.model and self.model.chapters else 0
        defaults = [str(row + 1), f"chapter_{row + 1}", str(start), str(duration), "新章节", "{}"]
        for column, value in enumerate(defaults):
            self.chapter_table.setItem(row, column, QtWidgets.QTableWidgetItem(value))

    def remove_chapter_rows(self) -> None:
        rows = sorted({index.row() for index in self.chapter_table.selectedIndexes()}, reverse=True)
        for row in rows:
            self.chapter_table.removeRow(row)

    def preview_metadata(self) -> None:
        if self.model is None:
            return
        try:
            exported = export_metadata(self.model, self.target_version_combo.currentText())
        except WorkbenchError as exc:
            self.metadata_text.setPlainText(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))
            return
        self.loss_table.setRowCount(len(exported.warnings))
        for row, warning in enumerate(exported.warnings):
            values = [warning.code, warning.field_path, warning.target_version, warning.message]
            for column, value in enumerate(values):
                self.loss_table.setItem(row, column, QtWidgets.QTableWidgetItem(value))
        self.metadata_text.setPlainText(
            json.dumps(exported.metadata, ensure_ascii=False, indent=2, sort_keys=True)
        )

    def _prepare_export_path(self) -> Path:
        if self.current_sample is None:
            fail(E_PARAM, "$.sample", "load a sample first")
        target = self.target_version_combo.currentText()
        DEFAULT_ARTIFACTS.mkdir(parents=True, exist_ok=True)
        return DEFAULT_ARTIFACTS / f"{self.current_sample.name}_{target}.mfz"

    def export_direct(self) -> None:
        if self.model is None:
            self._error(E_PARAM, "请先加载样本。")
            return
        try:
            self.model = self._apply_track_edits_to_model()
            model = self._apply_chapter_edits_to_model()
            target = self.target_version_combo.currentText()
            result = build_package(model, target)
            path = self._prepare_export_path()
            write_package(result, path)
            self.model = model
            self.last_artifact_path = path
            self.artifact_path_edit.setText(str(path))
            self._show_validation(validate_artifact(path, self.truth_file) if self.truth_file else None)
            self.status.showMessage(
                f"直接封装完成：{path.name} · {result.artifact_sha256[:16]}… · "
                f"E_LOSSY {len(result.warnings)} 条"
            )
        except WorkbenchError as exc:
            self._error(exc.code, json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))

    def export_segmented(self) -> None:
        if self.model is None:
            self._error(E_PARAM, "请先加载样本。")
            return
        try:
            self.model = self._apply_track_edits_to_model()
            model = self._apply_chapter_edits_to_model()
            target = self.target_version_combo.currentText()
            path = self._prepare_export_path()
            result = pack_segmented(
                model,
                target,
                self.checkpoint_root_edit.text(),
                workers=self.checkpoint_workers.value(),
                per_thread_buffer_limit=self.checkpoint_buffer.value(),
                parameter_version=f"gui-{target}-p1",
                artifact_path=path,
                truth_path=self.truth_file,
            )
            self.model = model
            self.last_artifact_path = path
            self.artifact_path_edit.setText(str(path))
            self._show_validation(result.validation)
            self.refresh_checkpoints()
            if not result.ok:
                self._error(
                    "分段封装失败",
                    json.dumps([failure.to_dict() for failure in result.failures], ensure_ascii=False, indent=2),
                )
                return
            self.status.showMessage(
                f"分段封装完成：{path.name} · reused={len(result.reused_tracks)} "
                f"built={len(result.built_tracks)} · {result.artifact_sha256[:16]}…"
            )
        except WorkbenchError as exc:
            self._error(exc.code, json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))

    def validate_artifact(self) -> None:
        path = Path(self.artifact_path_edit.text().strip())
        if not path.exists():
            if self.last_artifact_path and self.last_artifact_path.exists():
                path = self.last_artifact_path
            else:
                self._error(E_PARAM, "请选择已持久化的封装产物。")
                return
        truth = self.truth_file
        if truth is None or not truth.exists():
            self._error(E_PARAM, "缺少真值清单。")
            return
        result = validate_artifact(path, truth)
        self._show_validation(result)
        if result.ok:
            self.status.showMessage(f"独立校验通过：{result.artifact_sha256[:16]}…")
        else:
            self.status.showMessage("独立校验失败")

    def _show_validation(self, result: Any) -> None:
        if result is None:
            self.validation_text.setPlainText("未提供真值清单，跳过独立校验。")
            return
        self.validation_text.setPlainText(
            json.dumps(result.to_dict(), ensure_ascii=False, indent=2, sort_keys=True)
        )

    def refresh_checkpoints(self) -> None:
        root = Path(self.checkpoint_root_edit.text())
        if not root.exists():
            self.checkpoint_text.setPlainText("检查点目录尚不存在。")
            return
        rows = []
        for manifest_path in sorted(root.glob("*/manifest.json")):
            try:
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                rows.append({"snapshot": manifest_path.parent.name, "error": str(exc)})
                continue
            rows.append(
                {
                    "snapshot": manifest.get("snapshot_id"),
                    "state": manifest.get("state"),
                    "completed": manifest.get("completed_count"),
                    "target": manifest.get("metadata_version"),
                    "parameter_version": manifest.get("parameter_version"),
                    "algorithm_version": manifest.get("algorithm_version"),
                }
            )
        self.checkpoint_text.setPlainText(
            json.dumps(rows, ensure_ascii=False, indent=2, sort_keys=True)
        )

    def clear_checkpoints(self) -> None:
        root = Path(self.checkpoint_root_edit.text())
        if not root.exists():
            return
        answer = QtWidgets.QMessageBox.question(
            self,
            "清空检查点",
            f"确认删除 {root} 下的所有检查点快照？",
        )
        yes = getattr(QtWidgets.QMessageBox, "Yes", QtWidgets.QMessageBox.StandardButton.Yes)
        if answer != yes:
            return
        import shutil

        for child in root.iterdir():
            if child.is_dir():
                shutil.rmtree(child, ignore_errors=True)
        self.refresh_checkpoints()
        self.status.showMessage("检查点目录已清空")

    def run_injection(self) -> None:
        if self.model is None:
            self._error(E_PARAM, "请先加载样本。")
            return
        mode = self.injection_mode.currentText()
        track_id = self.injection_track.currentData()
        config = InjectionConfig(
            conflict_track=track_id if mode == "metadata_conflict" else None,
            order_track=track_id if mode == "timestamp_inversion" else None,
            missing_track=track_id if mode == "missing_track" else None,
            corrupt_track=track_id if mode == "corrupt_segment" else None,
            corrupt_offset=self.injection_offset.value(),
            parameter_version=f"gui-inject-{mode}",
        )
        output: dict[str, Any] = {"mode": mode, "track_id": track_id}
        if mode in {"metadata_conflict", "timestamp_inversion", "missing_track"}:
            data = self.sample_combo.currentData()
            if not data:
                self._error(E_PARAM, "没有可用样本。")
                return
            try:
                load_sample(Path(data), self.source_version_combo.currentText(), injection=config)
                output["result"] = "unexpected success"
            except WorkbenchError as exc:
                output["error"] = exc.to_dict()
                output["result"] = "error chain observed"
        else:
            result = pack_segmented(
                self.model,
                self.target_version_combo.currentText(),
                self.checkpoint_root_edit.text(),
                workers=self.checkpoint_workers.value(),
                per_thread_buffer_limit=self.checkpoint_buffer.value(),
                parameter_version=f"gui-{self.target_version_combo.currentText()}-p1",
                injection=config,
            )
            output["segmented"] = result.to_dict()
            self.refresh_checkpoints()
        self.injection_output.setPlainText(json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True))

    @staticmethod
    def _cell(table: QtWidgets.QTableWidget, row: int, column: int) -> str:
        item = table.item(row, column)
        return item.text() if item is not None else ""

    def _error(self, title: str, message: str) -> None:
        QtWidgets.QMessageBox.critical(self, title, message)
        self.status.showMessage(f"{title}: {message.splitlines()[0] if message else ''}")


def run() -> int:
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    app.setApplicationName("MediaForge Workbench")
    window = MainWindow()
    window.show()
    return exec_application(app)
