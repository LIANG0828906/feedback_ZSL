from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

from .boolean import OPERATIONS
from .errors import E_PREVIEW_STALE, PipelineError
from .io import load_path
from .metrics import ErrorThresholds
from .project import load_project, save_project
from .raster import RasterFrame
from .samplegen import ensure_samples
from .tasks import BooleanService, CommittedResult


def main() -> int:
    try:
        from PySide6.QtCore import Qt, QTimer
        from PySide6.QtGui import QImage, QPixmap
        from PySide6.QtWidgets import (
            QApplication,
            QComboBox,
            QDoubleSpinBox,
            QFileDialog,
            QFormLayout,
            QHBoxLayout,
            QLabel,
            QLineEdit,
            QMainWindow,
            QMessageBox,
            QPushButton,
            QTextEdit,
            QVBoxLayout,
            QWidget,
        )
    except ImportError:
        print(
            "PySide6 is required for the desktop UI. "
            "Install pinned dependencies from requirements.txt.",
            file=sys.stderr,
        )
        return 2

    class MainWindow(QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("Vector Boolean Studio")
            self.resize(1120, 760)
            self.service = BooleanService()
            self.path_a = None
            self.path_b = None
            self.task_id: str | None = None
            self.current_result: CommittedResult | None = None
            self.rendered_result_hash: str | None = None
            self.preview_timer = QTimer(self)
            self.preview_timer.setInterval(200)
            self.preview_timer.timeout.connect(self.refresh_preview)
            self.sample_root = Path("samples")
            self.artifact_root = Path("artifacts")
            self.artifact_root.mkdir(parents=True, exist_ok=True)
            ensure_samples(self.sample_root)
            self._build_ui()
            self.preview_timer.start()

        def _build_ui(self) -> None:
            central = QWidget(self)
            layout = QVBoxLayout(central)
            form = QFormLayout()

            self.path_a_edit = QLineEdit(str(self.sample_root / "v2" / "base_a.json"))
            self.path_b_edit = QLineEdit(str(self.sample_root / "v2" / "base_b.json"))
            layout_a = QHBoxLayout()
            layout_a.addWidget(self.path_a_edit)
            button_a = QPushButton("浏览 A")
            button_a.clicked.connect(lambda: self.choose_path(self.path_a_edit))
            layout_a.addWidget(button_a)
            layout_b = QHBoxLayout()
            layout_b.addWidget(self.path_b_edit)
            button_b = QPushButton("浏览 B")
            button_b.clicked.connect(lambda: self.choose_path(self.path_b_edit))
            layout_b.addWidget(button_b)
            form.addRow("路径 A", layout_a)
            form.addRow("路径 B", layout_b)

            self.operation_combo = QComboBox()
            self.operation_combo.addItems(sorted(OPERATIONS))
            form.addRow("布尔运算", self.operation_combo)

            self.tolerance_spin = QDoubleSpinBox()
            self.tolerance_spin.setDecimals(8)
            self.tolerance_spin.setRange(1e-9, 100.0)
            self.tolerance_spin.setSingleStep(0.001)
            self.tolerance_spin.setValue(0.01)
            form.addRow("几何容差", self.tolerance_spin)

            self.area_threshold_spin = QDoubleSpinBox()
            self.area_threshold_spin.setDecimals(6)
            self.area_threshold_spin.setRange(0.0, 1.0)
            self.area_threshold_spin.setValue(0.02)
            form.addRow("面积相对阈值", self.area_threshold_spin)

            self.boundary_threshold_spin = QDoubleSpinBox()
            self.boundary_threshold_spin.setDecimals(6)
            self.boundary_threshold_spin.setRange(0.0, 1e6)
            self.boundary_threshold_spin.setValue(1.0)
            form.addRow("边界偏差阈值", self.boundary_threshold_spin)

            self.export_version_combo = QComboBox()
            self.export_version_combo.addItem("v2", 2)
            self.export_version_combo.addItem("v1", 1)
            form.addRow("导出版本", self.export_version_combo)
            layout.addLayout(form)

            buttons = QHBoxLayout()
            self.run_button = QPushButton("运行合成")
            self.run_button.clicked.connect(self.run_boolean)
            self.cancel_button = QPushButton("取消任务")
            self.cancel_button.clicked.connect(self.cancel_task)
            self.cancel_button.setEnabled(False)
            save_project_button = QPushButton("保存工程")
            save_project_button.clicked.connect(self.save_project)
            load_project_button = QPushButton("载入工程")
            load_project_button.clicked.connect(self.load_project)
            export_button = QPushButton("导出结果")
            export_button.clicked.connect(self.export_result)
            for button in (
                self.run_button,
                self.cancel_button,
                save_project_button,
                load_project_button,
                export_button,
            ):
                buttons.addWidget(button)
            layout.addLayout(buttons)

            self.preview_label = QLabel("尚无已提交结果")
            self.preview_label.setAlignment(Qt.AlignmentFlag.AlignCenter)
            self.preview_label.setMinimumHeight(480)
            self.preview_label.setStyleSheet(
                "background:#f6f7f9;border:1px solid #c9ced6;"
            )
            layout.addWidget(self.preview_label, 1)

            self.status_label = QLabel("就绪")
            self.status_label.setTextInteractionFlags(
                Qt.TextInteractionFlag.TextSelectableByMouse
            )
            layout.addWidget(self.status_label)
            self.log_view = QTextEdit()
            self.log_view.setReadOnly(True)
            self.log_view.setMaximumHeight(130)
            layout.addWidget(self.log_view)
            self.setCentralWidget(central)

        def choose_path(self, edit: Any) -> None:
            filename, _ = QFileDialog.getOpenFileName(
                self,
                "选择路径 JSON",
                str(self.sample_root),
                "Vector path (*.json);;All files (*)",
            )
            if filename:
                edit.setText(filename)

        def log(self, message: str) -> None:
            self.log_view.append(message)
            self.status_label.setText(message)

        def thresholds(self) -> ErrorThresholds:
            return ErrorThresholds(
                max_area_relative_error=self.area_threshold_spin.value(),
                max_boundary_error=self.boundary_threshold_spin.value(),
            )

        def load_inputs(self) -> tuple[Any, Any] | None:
            if (
                not self.path_a_edit.text().strip()
                and not self.path_b_edit.text().strip()
                and self.path_a is not None
                and self.path_b is not None
            ):
                return self.path_a, self.path_b
            try:
                self.path_a = load_path(self.path_a_edit.text())
                self.path_b = load_path(self.path_b_edit.text())
            except PipelineError as exc:
                self.show_error(exc)
                return None
            self.log(
                f"已载入 A(v{self.path_a.version}) / B(v{self.path_b.version})；"
                f"容差 A={self.path_a.tolerance:g} B={self.path_b.tolerance:g}"
            )
            return self.path_a, self.path_b

        def run_boolean(self) -> None:
            inputs = self.load_inputs()
            if inputs is None:
                return
            try:
                self.task_id = self.service.submit(
                    inputs[0],
                    inputs[1],
                    self.operation_combo.currentText(),
                    self.tolerance_spin.value(),
                    self.thresholds(),
                )
            except PipelineError as exc:
                self.show_error(exc)
                return
            self.run_button.setEnabled(False)
            self.cancel_button.setEnabled(True)
            self.log(f"任务 {self.task_id} 已提交")

        def cancel_task(self) -> None:
            if self.task_id and self.service.cancel(self.task_id):
                self.log("已请求协作式取消")

        def poll_task(self) -> None:
            if not self.task_id:
                return
            info = self.service.task(self.task_id)
            if info is None or info.status == "running":
                return
            self.run_button.setEnabled(True)
            self.cancel_button.setEnabled(False)
            if info.status == "committed":
                self.current_result = self.service.store.get(info.result_hash or "")
                if self.current_result is not None:
                    metrics = self.current_result.metrics
                    self.log(
                        f"已提交 {self.current_result.result_id}；"
                        f"面积误差={metrics.area_relative_error:.6g} "
                        f"边界上界={metrics.max_boundary_error_upper:.6g} "
                        f"降级={metrics.degraded}"
                    )
            elif info.status == "cancelled":
                self.log("任务已取消；保留上一次已提交结果")
            else:
                error = info.error or {}
                self.log(
                    f"任务失败 {error.get('code', 'E_UNKNOWN')}；"
                    "保留上一次已提交结果"
                )
            self.task_id = None

        def refresh_preview(self) -> None:
            self.poll_task()
            latest = self.service.store.latest()
            if latest is None:
                return
            try:
                record, frame = self.service.store.snapshot()
            except PipelineError as exc:
                if exc.code == E_PREVIEW_STALE:
                    self.preview_timer.stop()
                    self.log("E_PREVIEW_STALE：预览哈希与提交清单不符，已停止刷新")
                return
            if record.result_hash == self.rendered_result_hash:
                return
            image = QImage(
                frame.rgba,
                frame.width,
                frame.height,
                frame.width * 4,
                QImage.Format.Format_RGBA8888,
            ).copy()
            pixmap = QPixmap.fromImage(image).scaled(
                self.preview_label.size(),
                Qt.AspectRatioMode.KeepAspectRatio,
                Qt.TransformationMode.SmoothTransformation,
            )
            self.preview_label.setPixmap(pixmap)
            self.rendered_result_hash = record.result_hash
            self.current_result = record

        def save_project(self) -> None:
            inputs = self.load_inputs()
            if inputs is None:
                return
            filename, _ = QFileDialog.getSaveFileName(
                self,
                "保存工程",
                str(self.artifact_root / "project.vproj"),
                "Vector pipeline project (*.vproj)",
            )
            if not filename:
                return
            try:
                document = save_project(
                    filename,
                    inputs[0],
                    inputs[1],
                    self.operation_combo.currentText(),
                    self.tolerance_spin.value(),
                    {
                        "path_a_version": inputs[0].version,
                        "path_b_version": inputs[1].version,
                    },
                    self.thresholds().to_dict(),
                    self.current_result,
                )
            except PipelineError as exc:
                self.show_error(exc)
                return
            self.log(f"工程已原子保存：{document['content_hash']}")

        def load_project(self) -> None:
            filename, _ = QFileDialog.getOpenFileName(
                self,
                "载入工程",
                str(self.artifact_root),
                "Vector pipeline project (*.vproj)",
            )
            if not filename:
                return
            try:
                project = load_project(filename)
                self.path_a = project.path_a
                self.path_b = project.path_b
                self.operation_combo.setCurrentText(project.operation)
                self.tolerance_spin.setValue(project.tolerance)
                self.area_threshold_spin.setValue(
                    float(project.thresholds.get("max_area_relative_error", 0.02))
                )
                self.boundary_threshold_spin.setValue(
                    float(project.thresholds.get("max_boundary_error", 1.0))
                )
                self.path_a_edit.setText("")
                self.path_b_edit.setText("")
                self.current_result = self.service.restore_project(project)
            except PipelineError as exc:
                self.show_error(exc)
                return
            self.log("工程已恢复；仅使用磁盘上已提交的结果")
            self.refresh_preview()

        def export_result(self) -> None:
            if self.current_result is None:
                self.show_error(
                    PipelineError("E_EXPORT_DEGRADED", "没有可导出的已提交结果")
                )
                return
            version = int(self.export_version_combo.currentData())
            suffix = f".v{version}.json"
            filename, _ = QFileDialog.getSaveFileName(
                self,
                "导出结果",
                str(self.artifact_root / f"result{suffix}"),
                "Vector path (*.json)",
            )
            if not filename:
                return
            try:
                _, report = self.service.export_result(
                    self.current_result,
                    version,
                    filename,
                )
            except PipelineError as exc:
                self.show_error(exc)
                return
            if report.code:
                self.log(
                    f"导出完成并记录 {report.code}："
                    f"{len(report.affected)} 个属性无法无损表达"
                )
            else:
                self.log("导出完成")

        def show_error(self, error: PipelineError) -> None:
            self.log(f"{error.code}: {error.message}")
            QMessageBox.warning(self, error.code, error.message)

    app = QApplication(sys.argv)
    window = MainWindow()
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())

