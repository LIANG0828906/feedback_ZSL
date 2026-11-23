"""PySide6 桌面工作台主窗口。

界面只负责编排，核心逻辑全部来自 :mod:`audiorest.core`，
因此无显示环境（CI/headless）下可用 QT_QPA_PLATFORM=offscreen 启动测试。
"""

from __future__ import annotations

from pathlib import Path

import numpy as np
from PySide6.QtCore import QThread, Signal
from PySide6.QtWidgets import (
    QComboBox,
    QFileDialog,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QSpinBox,
    QDoubleSpinBox,
    QVBoxLayout,
    QWidget,
)

from ..core.engine import RestorationEngine
from ..core.pcm import read_pcm
from ..errors import WorkbenchError
from ..formats.loader import (
    assert_equivalent,
    load_project,
)
from ..model import StageKind


class WorkerThread(QThread):
    log = Signal(str)
    progress = Signal(int, int)
    finished_ok = Signal(object)
    failed = Signal(str, str)

    def __init__(self, fn):
        super().__init__()
        self._fn = fn

    def run(self) -> None:
        try:
            result = self._fn(self.progress)
        except WorkbenchError as exc:
            self.failed.emit(exc.code, str(exc))
        except Exception as exc:  # pragma: no cover - 界面兜底
            self.failed.emit("E_UNKNOWN", str(exc))
        else:
            self.finished_ok.emit(result)


class MainWindow(QMainWindow):
    def __init__(self, workdir: Path):
        super().__init__()
        self.setWindowTitle("语音与音乐修复工作台")
        self.resize(980, 720)
        self.workdir = Path(workdir)
        self.workdir.mkdir(parents=True, exist_ok=True)
        self.project = None
        self.noisy = None
        self.engine: RestorationEngine | None = None
        self._worker: WorkerThread | None = None
        self._paused = False
        self._build_ui()

    def _build_ui(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)

        path_row = QHBoxLayout()
        self.path_edit = QLineEdit()
        self.path_edit.setPlaceholderText("选择工程描述文件（v1/v2/v3 JSON）")
        browse = QPushButton("浏览…")
        browse.clicked.connect(self._browse_project)
        load_btn = QPushButton("加载工程")
        load_btn.clicked.connect(self._load_project)
        path_row.addWidget(self.path_edit)
        path_row.addWidget(browse)
        path_row.addWidget(load_btn)
        layout.addLayout(path_row)

        self.source_label = QLabel("源版本：未加载")
        layout.addWidget(self.source_label)

        param_row = QHBoxLayout()
        param_row.addWidget(QLabel("谱减强度"))
        self.strength_spin = QDoubleSpinBox()
        self.strength_spin.setRange(0.01, 5.0)
        self.strength_spin.setSingleStep(0.1)
        self.strength_spin.setValue(1.4)
        param_row.addWidget(self.strength_spin)
        param_row.addWidget(QLabel("噪声门限(dB)"))
        self.gate_spin = QDoubleSpinBox()
        self.gate_spin.setRange(-90.0, 0.0)
        self.gate_spin.setValue(-45.0)
        param_row.addWidget(self.gate_spin)
        param_row.addWidget(QLabel("并行度"))
        self.worker_spin = QSpinBox()
        self.worker_spin.setRange(1, 4)
        self.worker_spin.setValue(2)
        param_row.addWidget(self.worker_spin)
        apply_btn = QPushButton("应用到未提交窗口")
        apply_btn.clicked.connect(self._apply_params)
        param_row.addWidget(apply_btn)
        layout.addLayout(param_row)

        run_row = QHBoxLayout()
        self.mode_combo = QComboBox()
        self.mode_combo.addItems(["实时逐窗口", "断点续跑", "一次性全量重算"])
        run_row.addWidget(self.mode_combo)
        start_btn = QPushButton("开始")
        start_btn.clicked.connect(self._start)
        pause_btn = QPushButton("暂停/继续")
        pause_btn.clicked.connect(self._toggle_pause)
        stop_btn = QPushButton("停止并退出运行")
        stop_btn.clicked.connect(self._stop)
        export_btn = QPushButton("导出修复 PCM")
        export_btn.clicked.connect(self._export)
        run_row.addWidget(start_btn)
        run_row.addWidget(pause_btn)
        run_row.addWidget(stop_btn)
        run_row.addWidget(export_btn)
        layout.addLayout(run_row)

        self.progress = QProgressBar()
        layout.addWidget(self.progress)
        self.state_label = QLabel("窗口状态：-")
        layout.addWidget(self.state_label)

        self.log_view = QPlainTextEdit()
        self.log_view.setReadOnly(True)
        layout.addWidget(self.log_view)
        self.setCentralWidget(central)

    def log(self, text: str) -> None:
        self.log_view.appendPlainText(text)

    def _browse_project(self) -> None:
        path, _ = QFileDialog.getOpenFileName(self, "选择工程文件", str(self.workdir), "JSON (*.json)")
        if path:
            self.path_edit.setText(path)

    def _load_project(self) -> None:
        try:
            project = load_project(self.path_edit.text())
            audio_path = Path(self.path_edit.text()).parent / project.audio.path
            noisy = read_pcm(audio_path, project.audio.channels)
        except WorkbenchError as exc:
            QMessageBox.critical(self, f"加载失败 {exc.code}", f"{exc}\n字段路径: {exc.path}")
            return
        self.project = project
        self.noisy = noisy
        self.engine = None
        self.source_label.setText(
            f"源版本：v{project.source_version}  工程：{project.project_id}  "
            f"补齐痕迹：{len(project.conversion_trace)} 条"
        )
        sub = project.stage(StageKind.SPECTRAL_SUBTRACTION)
        gate = project.stage(StageKind.NOISE_GATE)
        self.strength_spin.setValue(float(sub.config["strength"]))
        self.gate_spin.setValue(float(gate.config["threshold_db"]))
        self.log(f"已加载 {self.path_edit.text()} -> 内部模型 v{project.source_version}")

    def _ensure_engine(self) -> RestorationEngine:
        if self.project is None:
            raise WorkbenchError("E_VER", "尚未加载工程", "<root>")
        if self.engine is None:
            project_path = Path(self.path_edit.text())
            ckpt_dir = self.workdir / "checkpoints" / self.project.project_id
            self.engine = RestorationEngine(
                self.project,
                self.noisy,
                ckpt_dir,
                project_path.parent / self.project.audio.path,
                max_workers=self.worker_spin.value(),
            )
        return self.engine

    def _apply_params(self) -> None:
        try:
            engine = self._ensure_engine()
            engine.update_runtime_params(
                strength=self.strength_spin.value(),
                gate_threshold_db=self.gate_spin.value(),
            )
        except WorkbenchError as exc:
            QMessageBox.critical(self, f"参数被拒绝 {exc.code}", f"{exc}\n字段: {exc.path}")
            return
        self.log("参数已更新，仅作用于未提交窗口（已提交窗口参数快照冻结）")

    def _start(self) -> None:
        try:
            engine = self._ensure_engine()
        except WorkbenchError as exc:
            QMessageBox.critical(self, exc.code, str(exc))
            return
        mode = self.mode_combo.currentText()

        def task(progress_signal):
            def on_window(index, progress):
                progress_signal.emit(progress[0], progress[1])

            if mode == "一次性全量重算":
                return engine.run_full_recompute(on_window=on_window)
            if mode == "断点续跑":
                return engine.resume(on_window=on_window)
            return engine.run_incremental(on_window=on_window)

        self._run_worker(task, f"模式：{mode}")

    def _run_worker(self, task, title: str) -> None:
        self.log(title)
        self._worker = WorkerThread(task)
        self._worker.log.connect(self.log)
        self._worker.progress.connect(self._on_progress)
        self._worker.failed.connect(self._on_failed)
        self._worker.finished_ok.connect(self._on_finished)
        self._worker.start()

    def _on_progress(self, committed: int, total: int) -> None:
        self.progress.setMaximum(total)
        self.progress.setValue(committed)

    def _on_failed(self, code: str, message: str) -> None:
        self.log(f"[失败 {code}] {message}")
        QMessageBox.critical(self, f"运行失败 {code}", message)

    def _on_finished(self, result) -> None:
        if self.engine:
            committed, total = self.engine.progress
            self.progress.setMaximum(total)
            self.progress.setValue(committed)
            states = ", ".join(s.value for s in self.engine.state_machine.states)
            self.state_label.setText(f"窗口状态：{states}")
            self.log(f"完成，进度 {committed}/{total}，输出采样 shape={np.asarray(result).shape}")

    def _toggle_pause(self) -> None:
        if not self.engine:
            return
        if self._paused:
            self.engine.resume_processing()
            self._paused = False
            self.log("已继续")
        else:
            self.engine.pause()
            self._paused = True
            self.log("已暂停（已提交窗口检查点保留，可退出后重启续跑）")

    def _stop(self) -> None:
        if self.engine:
            self.engine.stop()
            self.log("已请求停止，已提交窗口检查点保留")

    def _export(self) -> None:
        if not self.engine:
            QMessageBox.warning(self, "无法导出", "请先运行并提交全部窗口")
            return
        try:
            out = self.workdir / "exports" / f"{self.project.project_id}.restored.pcm"
            self.engine.export_restored(out)
        except (WorkbenchError, RuntimeError) as exc:
            QMessageBox.warning(self, "导出失败", str(exc))
            return
        self.log(f"已按时间轴顺序导出: {out}")

    def check_cross_version(self, document_paths: list[str]) -> None:
        """供测试/菜单调用的跨版本等价性检查入口。"""
        projects = [load_project(path) for path in document_paths]
        roots = [Path(path).parent for path in document_paths]
        for index in range(1, len(projects)):
            assert_equivalent(
                projects[0],
                projects[index],
                left_audio_root=roots[0],
                right_audio_root=roots[index],
            )
        self.log(f"跨版本等价性校验通过：{len(projects)} 个版本内部模型逐项等价")
