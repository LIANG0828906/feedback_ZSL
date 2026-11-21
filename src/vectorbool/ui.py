from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

from PySide6.QtCore import QObject, Qt, Signal
from PySide6.QtGui import QPixmap
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSplitter,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .errors import BusinessError, ErrorCode
from .generator import load_samples
from .model import Operation
from .parser import parse_path
from .preview import render_preview
from .project import load_project, save_project
from .service import BooleanTaskService, TaskOutcome


class _SignalBridge(QObject):
    success = Signal(object)
    failure = Signal(object)
    cancelled = Signal()


class MainWindow(QMainWindow):
    def __init__(self, samples_file: str) -> None:
        super().__init__()
        self.setWindowTitle("VectorBool — 容差可观测布尔编辑器")
        self.resize(1180, 760)
        self.samples_file = samples_file
        self.samples: dict[str, Any] | None = None
        self.current_token: str | None = None
        self.path_a = None
        self.path_b = None
        self.raw_a: Any = None
        self.raw_b: Any = None
        self.last_result = None
        self.last_report = None
        self.service = BooleanTaskService()
        self.bridge = _SignalBridge()
        self.bridge.success.connect(self._on_success)
        self.bridge.failure.connect(self._on_failure)
        self.bridge.cancelled.connect(self._on_cancelled)
        self._build_ui()

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)

        form = QFormLayout()
        self.sample_combo = QComboBox()
        self.sample_combo.setMinimumWidth(260)
        self.a_edit = QLineEdit()
        self.b_edit = QLineEdit()
        self.operation_combo = QComboBox()
        for operation in Operation:
            self.operation_combo.addItem(operation.value, operation.value)
        self.tolerance_edit = QLineEdit("1e-6")
        self.injection_combo = QComboBox()
        self.injection_combo.addItem("none", "")
        for kind in (
            "coincident_edge",
            "degenerate_segment",
            "zero_area_contour",
            "hole_reversed",
            "non_finite_coordinate",
        ):
            self.injection_combo.addItem(kind, kind)
        form.addRow("本地样本", self.sample_combo)
        form.addRow("路径 A", self._file_row(self.a_edit))
        form.addRow("路径 B", self._file_row(self.b_edit))
        form.addRow("运算", self.operation_combo)
        form.addRow("几何容差", self.tolerance_edit)
        form.addRow("异常注入", self.injection_combo)
        root.addLayout(form)

        buttons = QHBoxLayout()
        self.load_samples_button = QPushButton("载入样本目录")
        self.run_button = QPushButton("运行")
        self.cancel_button = QPushButton("取消")
        self.cancel_button.setEnabled(False)
        self.save_button = QPushButton("保存工程")
        self.load_button = QPushButton("载入工程")
        buttons.addWidget(self.load_samples_button)
        buttons.addWidget(self.run_button)
        buttons.addWidget(self.cancel_button)
        buttons.addStretch(1)
        buttons.addWidget(self.save_button)
        buttons.addWidget(self.load_button)
        root.addLayout(buttons)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        self.preview_label = QLabel("尚未生成成功预览")
        self.preview_label.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.preview_label.setMinimumSize(480, 480)
        self.preview_label.setStyleSheet(
            "background:#12161e;color:#8a94a6;border:1px solid #2a3444;"
        )
        self.log = QTextEdit()
        self.log.setReadOnly(True)
        splitter.addWidget(self.preview_label)
        splitter.addWidget(self.log)
        splitter.setStretchFactor(0, 3)
        splitter.setStretchFactor(1, 2)
        root.addWidget(splitter, 1)

        self.load_samples_button.clicked.connect(self._choose_samples)
        self.sample_combo.currentIndexChanged.connect(self._load_selected_sample)
        self.a_edit.textEdited.connect(self._invalidate_result)
        self.b_edit.textEdited.connect(self._invalidate_result)
        self.operation_combo.currentIndexChanged.connect(self._invalidate_result)
        self.tolerance_edit.textEdited.connect(self._invalidate_result)
        self.run_button.clicked.connect(self._run)
        self.cancel_button.clicked.connect(self._cancel)
        self.save_button.clicked.connect(self._save_project)
        self.load_button.clicked.connect(self._load_project)

        self._load_samples(Path(self.samples_file))

    def _file_row(self, edit: QLineEdit) -> QWidget:
        widget = QWidget()
        layout = QHBoxLayout(widget)
        layout.setContentsMargins(0, 0, 0, 0)
        button = QPushButton("浏览")
        button.clicked.connect(lambda: self._choose_path(edit))
        layout.addWidget(edit, 1)
        layout.addWidget(button)
        return widget

    def _choose_path(self, edit: QLineEdit) -> None:
        filename, _ = QFileDialog.getOpenFileName(self, "选择路径 JSON", "", "JSON (*.json)")
        if filename:
            edit.setText(filename)

    def _choose_samples(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(self, "选择样本 JSON", "", "JSON (*.json)")
        if filename:
            self._load_samples(Path(filename))

    def _load_samples(self, filename: Path) -> None:
        try:
            self.samples = load_samples(filename)
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as error:
            self._log(f"样本载入失败: {error}")
            return
        self.sample_combo.blockSignals(True)
        self.sample_combo.clear()
        self.sample_combo.addItem("自定义路径", None)
        for case in self.samples.get("catalog", []):
            self.sample_combo.addItem(case["name"], case)
        self.sample_combo.blockSignals(False)
        self._log(f"已载入 {filename}，共 {len(self.samples.get('catalog', []))} 个样本")

    def _load_selected_sample(self) -> None:
        case = self.sample_combo.currentData()
        if not isinstance(case, dict):
            return
        self.raw_a = case["path_a"]
        self.raw_b = case["path_b"]
        self.path_a = None
        self.path_b = None
        self.last_result = None
        self.last_report = None
        self.a_edit.setText(f"<sample:{case['name']}:a>")
        self.b_edit.setText(f"<sample:{case['name']}:b>")

    def _selected_paths(self) -> tuple[Any, Any]:
        case = self.sample_combo.currentData()
        if isinstance(case, dict):
            return case["path_a"], case["path_b"]
        if (
            self.raw_a is not None
            and self.raw_b is not None
            and self.a_edit.text().startswith("<project:")
        ):
            return self.raw_a, self.raw_b
        with open(self.a_edit.text(), "r", encoding="utf-8") as stream:
            raw_a = json.load(stream)
        with open(self.b_edit.text(), "r", encoding="utf-8") as stream:
            raw_b = json.load(stream)
        return raw_a, raw_b

    def _run(self) -> None:
        try:
            self._invalidate_result()
            tolerance = float(self.tolerance_edit.text())
            raw_a, raw_b = self._selected_paths()
            self.path_a = parse_path(raw_a, tolerance=tolerance, path_name="a")
            self.path_b = parse_path(raw_b, tolerance=tolerance, path_name="b")
            self.run_button.setEnabled(False)
            self.cancel_button.setEnabled(True)
            self.current_token = self.service.submit(
                self.path_a,
                self.path_b,
                self.operation_combo.currentData(),
                tolerance=tolerance,
                on_success=lambda outcome: self.bridge.success.emit(outcome),
                on_error=lambda error: self.bridge.failure.emit(error),
                on_cancel=lambda: self.bridge.cancelled.emit(),
            )
            self._log(f"任务 {self.current_token[:8]} 已提交")
        except BusinessError as error:
            self._on_failure(error)
        except (OSError, ValueError, json.JSONDecodeError) as error:
            self._on_failure(
                BusinessError(ErrorCode.PATH_INVALID, f"input error: {error}")
            )

    def _cancel(self) -> None:
        if self.current_token is not None:
            self.service.cancel(self.current_token)
            self._log("已请求取消；保留上一次成功预览")

    def _on_success(self, outcome: TaskOutcome) -> None:
        self.current_token = None
        self.run_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        self.last_result = outcome.result
        self.last_report = outcome.result.report
        self._log(self._result_summary(outcome.result))
        if outcome.preview is not None:
            pixmap = QPixmap()
            if pixmap.loadFromData(outcome.preview.png, "PNG"):
                self.preview_label.setPixmap(pixmap)
        elif outcome.result.report is not None:
            self._log("结果为降级状态，未替换预览。")

    def _on_failure(self, error: BusinessError) -> None:
        self.current_token = None
        self.run_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        self._log(json.dumps(error.to_dict(), ensure_ascii=False, indent=2))

    def _on_cancelled(self) -> None:
        self.current_token = None
        self.run_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        self._log("任务已取消，离屏缓冲已释放，上一次预览保持不变。")

    def _save_project(self) -> None:
        if self.path_a is None or self.path_b is None:
            QMessageBox.warning(self, "无法保存", "请先解析输入路径")
            return
        filename, _ = QFileDialog.getSaveFileName(self, "保存工程", "", "VectorBool (*.vbproj.json)")
        if not filename:
            return
        try:
            digest = save_project(
                filename,
                path_a=self.path_a,
                path_b=self.path_b,
                operation=self.operation_combo.currentData(),
                tolerance=float(self.tolerance_edit.text()),
                result=self.last_result,
            )
            self._log(f"工程已原子保存，内容哈希 {digest}")
        except (OSError, BusinessError) as error:
            self._on_failure(error if isinstance(error, BusinessError) else BusinessError(ErrorCode.IO, str(error)))

    def _load_project(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(self, "载入工程", "", "VectorBool (*.json)")
        if not filename:
            return
        try:
            project = load_project(filename)
            tolerance = project.tolerance
            self.raw_a = project.path_a
            self.raw_b = project.path_b
            self.path_a = parse_path(project.path_a, tolerance=tolerance, path_name="a")
            self.path_b = parse_path(project.path_b, tolerance=tolerance, path_name="b")
            self.tolerance_edit.setText(repr(tolerance))
            index = self.operation_combo.findData(project.operation)
            if index >= 0:
                self.operation_combo.setCurrentIndex(index)
            self.sample_combo.setCurrentIndex(0)
            self.a_edit.setText("<project:a>")
            self.b_edit.setText("<project:b>")
            if project.committed and project.result is not None:
                self.last_result = project.result_object
                self.last_report = (
                    project.result_object.report if project.result_object else None
                )
                if project.result_object is not None:
                    preview = render_preview(
                        self.path_a,
                        self.path_b,
                        project.result_object,
                        tolerance=tolerance,
                    )
                    pixmap = QPixmap()
                    if pixmap.loadFromData(preview.png, "PNG"):
                        self.preview_label.setPixmap(pixmap)
                self._log(
                    f"已恢复提交结果 {project.content_hash[:16]}；"
                    f"轮廓数 {len(project.result.get('contours', []))}"
                )
            else:
                self.last_result = None
                self._log("工程仅恢复输入与报告；未提交结果不会作为成功结果恢复")
        except BusinessError as error:
            self._on_failure(error)

    def _result_summary(self, result) -> str:
        report = result.report
        return (
            f"{result.operation.value}: {len(result.contours)} 轮廓, "
            f"{result.intersection_count} 交点, 状态 {report.status}, "
            f"面积相对偏差 {report.area_relative_deviation:.6g}, "
            f"边界上界 {report.boundary_deviation_upper_bound:.6g}, "
            f"容差放大 {report.tolerance_amplification:.6g}"
        )

    def _invalidate_result(self, *_args) -> None:
        self.last_result = None
        self.last_report = None

    def _log(self, message: str) -> None:
        self.log.append(message)


def launch_ui(samples_file: str = "artifacts/samples/samples.json") -> int:
    application = QApplication.instance() or QApplication(sys.argv)
    window = MainWindow(samples_file)
    window.show()
    return application.exec()

