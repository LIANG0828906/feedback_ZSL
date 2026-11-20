"""PyQt5 desktop workbench for local deterministic voice restoration."""

from __future__ import annotations

import json
import sys
import threading
from pathlib import Path
from typing import Any

import numpy as np
from PyQt5.QtCore import Qt, QThread, pyqtSignal
from PyQt5.QtGui import QColor, QFont, QPainter, QPen
from PyQt5.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .compare import compare_versions, write_compare_outputs
from .errors import BusinessError
from .processing import segment_bounds
from .recipes import Recipe, load_recipe, recipe_from_mapping
from .restore import RestorationEngine
from .samples import build_recipes
from .wav_io import AudioData, read_wav


class WaveformWidget(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.audio: AudioData | None = None
        self.segment_seconds = 1.0
        self.setMinimumHeight(190)

    def set_audio(self, audio: AudioData | None) -> None:
        self.audio = audio
        self.update()

    def paintEvent(self, _event: Any) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor(18, 22, 29))
        rect = self.rect().adjusted(8, 8, -8, -8)
        painter.setPen(QPen(QColor(54, 64, 78), 1))
        for fraction in (0.0, 0.25, 0.5, 0.75, 1.0):
            y = int(rect.top() + fraction * rect.height())
            painter.drawLine(rect.left(), y, rect.right(), y)
        if self.audio is None or self.audio.frames == 0:
            painter.setPen(QColor(145, 158, 176))
            painter.drawText(rect, Qt.AlignCenter, "载入 16-bit PCM WAV 后显示波形")
            return

        painter.setPen(QPen(QColor(91, 116, 148), 1, Qt.DotLine))
        seg = max(1, int(round(self.audio.sample_rate * self.segment_seconds)))
        for start in range(seg, self.audio.frames, seg):
            x = rect.left() + int(start / self.audio.frames * rect.width())
            painter.drawLine(x, rect.top(), x, rect.bottom())

        samples = self.audio.samples
        width = max(1, rect.width())
        colors = (QColor(75, 200, 180), QColor(240, 176, 88))
        for channel in range(samples.shape[1]):
            points = []
            for x in range(width):
                begin = int(x * samples.shape[0] / width)
                end = max(begin + 1, int((x + 1) * samples.shape[0] / width))
                block = samples[begin:end, channel]
                low = float(np.min(block))
                high = float(np.max(block))
                center = rect.center().y()
                half = rect.height() / 2.0 - 2
                points.append(
                    (
                        rect.left() + x,
                        int(center - high * half),
                        int(center - low * half),
                    )
                )
            painter.setPen(QPen(colors[channel % len(colors)], 1))
            for x, top, bottom in points:
                painter.drawLine(x, top, x, max(top + 1, bottom))


class _RestoreWorker(QThread):
    segment_done = pyqtSignal(dict)
    completed = pyqtSignal(object)
    failed = pyqtSignal(dict)

    def __init__(self, engine: RestorationEngine) -> None:
        super().__init__()
        self.engine = engine
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            result = self.engine.run(
                cancel_event=self.cancel_event,
                on_segment=self.segment_done.emit,
            )
            self.completed.emit(result)
        except BusinessError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:
            self.failed.emit(
                {
                    "code": "E_PROCESSING",
                    "message": str(exc),
                    "details": {"error_type": type(exc).__name__},
                }
            )


class _CompareWorker(QThread):
    completed = pyqtSignal(object)
    failed = pyqtSignal(dict)

    def __init__(self, source: str, recipes: list[Recipe], work_root: Path) -> None:
        super().__init__()
        self.source = source
        self.recipes = recipes
        self.work_root = work_root

    def run(self) -> None:
        try:
            report = compare_versions(self.source, self.recipes, self.work_root)
            write_compare_outputs(
                report,
                self.work_root / "comparison.json",
                self.work_root / "comparison_trace.jsonl",
            )
            self.completed.emit(report)
        except BusinessError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:
            self.failed.emit(
                {
                    "code": "E_PROCESSING",
                    "message": str(exc),
                    "details": {"error_type": type(exc).__name__},
                }
            )


class RestorationWindow(QMainWindow):
    def __init__(self) -> None:
        super().__init__()
        self.setWindowTitle("多语种游戏配音修复工作台")
        self.resize(1300, 860)
        self.audio: AudioData | None = None
        self.recipes: dict[int, Recipe] = build_recipes()
        self._editing_version = 1
        self._worker: _RestoreWorker | None = None
        self._compare_worker: _CompareWorker | None = None
        self._segment_rows: list[tuple[int, int]] = []
        self._build_ui()
        self._load_recipe_controls(self.recipes[1])

    def _build_ui(self) -> None:
        central = QWidget()
        outer = QVBoxLayout(central)
        top = QHBoxLayout()
        self.load_button = QPushButton("载入 WAV")
        self.recipe_button = QPushButton("载入配方 JSON")
        self.run_button = QPushButton("启动修复")
        self.cancel_button = QPushButton("取消任务")
        self.compare_button = QPushButton("跨版本一致性比对")
        self.cancel_button.setEnabled(False)
        for button in (
            self.load_button,
            self.recipe_button,
            self.run_button,
            self.cancel_button,
            self.compare_button,
        ):
            top.addWidget(button)
        top.addStretch(1)
        outer.addLayout(top)

        splitter = QSplitter(Qt.Horizontal)
        left = QWidget()
        left_layout = QVBoxLayout(left)
        self.file_label = QLabel("尚未载入 WAV")
        self.file_label.setWordWrap(True)
        left_layout.addWidget(self.file_label)
        self.waveform = WaveformWidget()
        left_layout.addWidget(self.waveform)
        self.table = QTableWidget(0, 9)
        self.table.setHorizontalHeaderLabels(
            [
                "片段",
                "帧区间",
                "状态",
                "配方",
                "残留噪声 dBFS",
                "峰值 dBFS",
                "响度 LUFS",
                "增益跳变 dB",
                "不一致",
            ]
        )
        self.table.horizontalHeader().setStretchLastSection(True)
        left_layout.addWidget(self.table, 1)
        splitter.addWidget(left)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.addWidget(self._recipe_group())
        right_layout.addWidget(self._paths_group())
        right_layout.addWidget(self._mapping_group())
        right_layout.addWidget(QLabel("运行日志"))
        self.log = QTextEdit()
        self.log.setReadOnly(True)
        self.log.setFont(QFont("Consolas", 9))
        right_layout.addWidget(self.log, 1)
        splitter.addWidget(right)
        splitter.setSizes([820, 440])
        outer.addWidget(splitter, 1)
        self.setCentralWidget(central)
        self.statusBar().showMessage("就绪")

        self.load_button.clicked.connect(self.load_wav)
        self.recipe_button.clicked.connect(self.load_recipe_file)
        self.run_button.clicked.connect(self.start_restore)
        self.cancel_button.clicked.connect(self.cancel_restore)
        self.compare_button.clicked.connect(self.start_compare)
        self.version_combo.currentIndexChanged.connect(self._version_changed)

    def _recipe_group(self) -> QGroupBox:
        group = QGroupBox("配方版本与参数")
        form = QFormLayout(group)
        self.version_combo = QComboBox()
        for version in (1, 2, 3):
            self.version_combo.addItem(f"v{version}", version)
        form.addRow("配方版本", self.version_combo)

        self.denoise_spin = QDoubleSpinBox()
        self.denoise_spin.setRange(0.0, 1.0)
        self.denoise_spin.setSingleStep(0.05)
        form.addRow("降噪强度", self.denoise_spin)
        self.gain_spin = QDoubleSpinBox()
        self.gain_spin.setRange(-24.0, 24.0)
        self.gain_spin.setSuffix(" dB")
        form.addRow("固定增益", self.gain_spin)
        self.dc_check = QCheckBox("去直流偏移（按声道独立）")
        form.addRow("v2+", self.dc_check)
        self.gate_spin = QDoubleSpinBox()
        self.gate_spin.setRange(-80.0, -10.0)
        self.gate_spin.setSuffix(" dBFS")
        form.addRow("区间静音门限（v2+）", self.gate_spin)
        self.channel_combo = QComboBox()
        for mode in ("preserve", "mono_mix", "left", "right"):
            self.channel_combo.addItem(mode, mode)
        form.addRow("声道合并（v3）", self.channel_combo)
        self.spectral_ms = QSpinBox()
        self.spectral_ms.setRange(10, 100)
        self.spectral_ms.setSuffix(" ms")
        form.addRow("谱减分段（v3）", self.spectral_ms)
        self.oversubtract_spin = QDoubleSpinBox()
        self.oversubtract_spin.setRange(0.5, 8.0)
        self.oversubtract_spin.setSingleStep(0.1)
        form.addRow("谱减过减系数（v3）", self.oversubtract_spin)
        self.floor_spin = QDoubleSpinBox()
        self.floor_spin.setRange(0.0, 0.5)
        self.floor_spin.setSingleStep(0.005)
        form.addRow("谱底（v3）", self.floor_spin)
        self.loudness_spin = QDoubleSpinBox()
        self.loudness_spin.setRange(-40.0, -8.0)
        self.loudness_spin.setSuffix(" LUFS")
        form.addRow("响度目标（v3）", self.loudness_spin)
        return group

    def _paths_group(self) -> QGroupBox:
        group = QGroupBox("任务与导出")
        layout = QVBoxLayout(group)
        row1 = QHBoxLayout()
        row1.addWidget(QLabel("恢复目录"))
        self.work_edit = QLineEdit()
        row1.addWidget(self.work_edit, 1)
        choose = QPushButton("选择")
        choose.clicked.connect(self._choose_work)
        row1.addWidget(choose)
        layout.addLayout(row1)
        self.export_label = QLabel("最终 WAV、report.json、trace.jsonl 将原子写入恢复目录")
        self.export_label.setWordWrap(True)
        layout.addWidget(self.export_label)
        return group

    def _mapping_group(self) -> QGroupBox:
        group = QGroupBox("字段映射与版本升级来源")
        layout = QVBoxLayout(group)
        self.mapping_view = QTextEdit()
        self.mapping_view.setReadOnly(True)
        self.mapping_view.setMaximumHeight(145)
        layout.addWidget(self.mapping_view)
        return group

    def _append_log(self, message: str) -> None:
        self.log.append(message)
        self.statusBar().showMessage(message)

    def _set_busy(self, busy: bool, comparing: bool = False) -> None:
        self.run_button.setEnabled(not busy)
        self.compare_button.setEnabled(not busy)
        self.load_button.setEnabled(not busy)
        self.recipe_button.setEnabled(not busy)
        self.cancel_button.setEnabled(busy and not comparing)
        self.version_combo.setEnabled(not busy)

    def _choose_work(self) -> None:
        selected = QFileDialog.getExistingDirectory(self, "选择恢复目录", self.work_edit.text())
        if selected:
            self.work_edit.setText(selected)

    def load_wav(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(self, "载入 WAV", "", "WAV (*.wav)")
        if not filename:
            return
        try:
            audio = read_wav(filename)
        except BusinessError as exc:
            self._show_error(exc.to_dict())
            return
        self.audio = audio
        self.waveform.set_audio(audio)
        self.file_label.setText(
            f"{filename}\n{audio.sample_rate} Hz · {audio.channels} 声道 · "
            f"{audio.frames} 帧 · {audio.duration_s:.3f} s · SHA-256 {audio.sha256[:16]}…"
        )
        stem = Path(filename).stem
        default_work = str(Path(filename).parent / f".{stem}.restore.v{self.version_combo.currentData()}")
        self.work_edit.setText(default_work)
        self._populate_segment_rows(audio)
        self._append_log(f"已载入 {filename}")

    def load_recipe_file(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(self, "载入配方", "", "JSON (*.json)")
        if not filename:
            return
        try:
            recipe = load_recipe(filename)
        except BusinessError as exc:
            self._show_error(exc.to_dict())
            return
        self.recipes[recipe.version] = recipe
        self._editing_version = recipe.version
        self.version_combo.blockSignals(True)
        self.version_combo.setCurrentIndex(recipe.version - 1)
        self.version_combo.blockSignals(False)
        self._load_recipe_controls(recipe)
        self._append_log(f"已载入 v{recipe.version} 配方：{filename}")

    def _populate_segment_rows(self, audio: AudioData) -> None:
        bounds = segment_bounds(audio.frames, audio.sample_rate, 1.0)
        self._segment_rows = bounds
        self.table.setRowCount(len(bounds))
        for row, (start, end) in enumerate(bounds):
            values = [str(row), f"{start}–{end}", "待处理", "", "", "", "", "", ""]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setFlags(item.flags() & ~Qt.ItemIsEditable)
                self.table.setItem(row, column, item)
        self.table.resizeColumnsToContents()

    def _recipe_payload(self, version_override: int | None = None) -> dict[str, Any]:
        version = (
            int(version_override)
            if version_override is not None
            else int(self.version_combo.currentData())
        )
        payload: dict[str, Any] = {
            "version": version,
            "denoise_strength": self.denoise_spin.value(),
            "gain_db": self.gain_spin.value(),
        }
        if version >= 2:
            payload["remove_dc"] = self.dc_check.isChecked()
            payload["silence_threshold_dbfs"] = self.gate_spin.value()
        if version >= 3:
            payload["channel_merge_mode"] = self.channel_combo.currentData()
            payload["spectral_subtraction"] = {
                "segment_ms": self.spectral_ms.value(),
                "oversubtraction": self.oversubtract_spin.value(),
                "spectral_floor": self.floor_spin.value(),
            }
            payload["loudness_target_lufs"] = self.loudness_spin.value()
        return payload

    def _current_recipe(self) -> Recipe:
        return recipe_from_mapping(self._recipe_payload(), source="<gui>")

    def _store_current_recipe(self, version_override: int | None = None) -> None:
        try:
            recipe = recipe_from_mapping(
                self._recipe_payload(version_override), source="<gui>"
            )
        except BusinessError:
            return
        self.recipes[recipe.version] = recipe

    def _load_recipe_controls(self, recipe: Recipe) -> None:
        self.denoise_spin.setValue(recipe.denoise_strength)
        self.gain_spin.setValue(recipe.gain_db)
        self.dc_check.setChecked(recipe.remove_dc)
        self.gate_spin.setValue(
            recipe.silence_threshold_dbfs
            if recipe.silence_threshold_dbfs is not None
            else -54.0
        )
        index = self.channel_combo.findData(recipe.channel_merge_mode)
        self.channel_combo.setCurrentIndex(max(0, index))
        self.spectral_ms.setValue(recipe.spectral_subtraction.segment_ms)
        self.oversubtract_spin.setValue(recipe.spectral_subtraction.oversubtraction)
        self.floor_spin.setValue(recipe.spectral_subtraction.spectral_floor)
        self.loudness_spin.setValue(
            recipe.loudness_target_lufs
            if recipe.loudness_target_lufs is not None
            else -20.0
        )
        self._sync_version_controls(recipe.version)
        mapping_lines = [
            f"{row['field']} = {row['value']}  [{row['source']} / v{row['source_version']}]"
            for row in recipe.field_mapping
        ]
        if recipe.lost_fields:
            mapping_lines.append("")
            mapping_lines.append("源版本无法表达（影响范围）：")
            mapping_lines.extend(
                f"- {row['field']}: {row['impact']}" for row in recipe.lost_fields
            )
        self.mapping_view.setPlainText("\n".join(mapping_lines))

    def _sync_version_controls(self, version: int) -> None:
        self.dc_check.setEnabled(version >= 2)
        self.gate_spin.setEnabled(version >= 2)
        self.channel_combo.setEnabled(version >= 3)
        self.spectral_ms.setEnabled(version >= 3)
        self.oversubtract_spin.setEnabled(version >= 3)
        self.floor_spin.setEnabled(version >= 3)
        self.loudness_spin.setEnabled(version >= 3)

    def _version_changed(self) -> None:
        self._store_current_recipe(self._editing_version)
        version = int(self.version_combo.currentData())
        self._editing_version = version
        self._load_recipe_controls(self.recipes[version])
        if self.audio is not None:
            stem = Path(self.audio.source_path).stem
            self.work_edit.setText(
                str(Path(self.audio.source_path).parent / f".{stem}.restore.v{version}")
            )

    def start_restore(self) -> None:
        if self.audio is None:
            QMessageBox.warning(self, "缺少 WAV", "请先载入 WAV。")
            return
        work_text = self.work_edit.text().strip()
        if not work_text:
            QMessageBox.warning(self, "缺少恢复目录", "请选择恢复目录。")
            return
        try:
            recipe = self._current_recipe()
        except BusinessError as exc:
            self._show_error(exc.to_dict())
            return
        self.recipes[recipe.version] = recipe
        work = Path(work_text)
        engine = RestorationEngine(
            self.audio.source_path,
            recipe,
            work,
            output_wav=work / "restored.wav",
            report_path=work / "report.json",
            trace_path=work / "trace.jsonl",
        )
        self._worker = _RestoreWorker(engine)
        self._worker.segment_done.connect(self._segment_done)
        self._worker.completed.connect(self._restore_completed)
        self._worker.failed.connect(self._restore_failed)
        self._set_busy(True)
        self._append_log(
            f"启动 v{recipe.version} 修复；片段边界检查点可取消，已完成片段会持久化。"
        )
        self._worker.start()

    def cancel_restore(self) -> None:
        if self._worker is not None:
            self._worker.cancel_event.set()
            self._append_log("已请求取消；将在下一个片段边界停止。")

    def _segment_done(self, entry: dict[str, Any]) -> None:
        index = int(entry["index"])
        if index < self.table.rowCount():
            metrics = entry["metrics"]
            values = {
                2: "已提交",
                3: f"v{entry['recipe_version']}",
                4: f"{metrics.get('residual_noise_dbfs', -120):.2f}",
                5: f"{metrics.get('peak_dbfs', -120):.2f}",
                6: f"{metrics.get('loudness_lufs', -120):.2f}",
                7: f"{metrics.get('gain_jump_db', 0.0):.2f}",
            }
            for column, value in values.items():
                self.table.setItem(index, column, QTableWidgetItem(str(value)))
            self.table.scrollToItem(self.table.item(index, 0))
        self._append_log(
            f"片段 {index:04d} 已原子提交，链头 {entry['chain_hash'][:12]}…"
        )

    def _restore_completed(self, result: Any) -> None:
        self._set_busy(False)
        if result.status == "completed":
            self._append_log(
                f"修复完成：{result.committed_segments} 片段；输出 {result.output_wav}"
            )
            QMessageBox.information(
                self,
                "修复完成",
                f"输出 WAV：\n{result.output_wav}\n\n报告：\n{result.report_path}",
            )
        else:
            self._append_log(
                f"任务已取消，保留 {result.committed_segments}/{result.total_segments} 个已提交片段。"
            )
        self._worker = None

    def _restore_failed(self, payload: dict[str, Any]) -> None:
        self._set_busy(False)
        self._show_error(payload)
        self._worker = None

    def start_compare(self) -> None:
        if self.audio is None:
            QMessageBox.warning(self, "缺少 WAV", "请先载入 WAV。")
            return
        self._store_current_recipe()
        work_root = Path(self.work_edit.text().strip() or ".") / "comparison"
        self._compare_worker = _CompareWorker(
            self.audio.source_path,
            [self.recipes[1], self.recipes[2], self.recipes[3]],
            work_root,
        )
        self._compare_worker.completed.connect(self._compare_completed)
        self._compare_worker.failed.connect(self._compare_failed)
        self._set_busy(True, comparing=True)
        self._append_log(f"启动 v1/v2/v3 跨版本比对：{work_root}")
        self._compare_worker.start()

    def _compare_completed(self, report: dict[str, Any]) -> None:
        self._set_busy(False)
        for row, segment in enumerate(report["segments"]):
            if row >= self.table.rowCount():
                continue
            incompatible = [
                comparison["pair"]
                for comparison in segment["comparisons"]
                if comparison["incompatible"]
            ]
            item = QTableWidgetItem("; ".join(incompatible) if incompatible else "一致")
            if incompatible:
                item.setBackground(QColor(122, 42, 48))
                item.setForeground(QColor(255, 224, 224))
                notes = []
                for comparison in segment["comparisons"]:
                    if comparison["incompatible"]:
                        for note in comparison["attribution"]:
                            notes.append(
                                f"{comparison['pair']} {note['field']}: {note['message']}"
                            )
                item.setToolTip("\n".join(notes))
            else:
                item.setBackground(QColor(34, 87, 66))
            self.table.setItem(row, 8, item)
        summary = json.dumps(report["summary"], ensure_ascii=False, indent=2)
        self._append_log(
            f"跨版本比对完成，hash {report['hash'][:16]}…\n{summary}"
        )
        self._compare_worker = None

    def _compare_failed(self, payload: dict[str, Any]) -> None:
        self._set_busy(False)
        self._show_error(payload)
        self._compare_worker = None

    def _show_error(self, payload: dict[str, Any]) -> None:
        self._append_log(
            f"{payload.get('code')}: {payload.get('message')} "
            f"{json.dumps(payload.get('details', {}), ensure_ascii=False)}"
        )
        QMessageBox.critical(
            self,
            payload.get("code", "错误"),
            f"{payload.get('message')}\n\n"
            f"{json.dumps(payload.get('details', {}), ensure_ascii=False, indent=2)}",
        )

    def closeEvent(self, event: Any) -> None:
        if self._worker is not None:
            self._worker.cancel_event.set()
            self._worker.wait(5000)
        if self._compare_worker is not None:
            self._compare_worker.wait(5000)
        event.accept()


def main(argv: list[str] | None = None) -> int:
    app = QApplication(argv if argv is not None else sys.argv)
    window = RestorationWindow()
    window.show()
    return app.exec_()


if __name__ == "__main__":
    raise SystemExit(main())
