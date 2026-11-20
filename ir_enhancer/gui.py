"""PySide6 图形界面：文件选择、增强执行、报告查看。无界面环境请使用 CLI。"""

from __future__ import annotations

import json
import traceback


def run_gui() -> int:
    from PySide6.QtCore import QThread, Signal
    from PySide6.QtWidgets import (
        QApplication,
        QFileDialog,
        QHBoxLayout,
        QLabel,
        QLineEdit,
        QMainWindow,
        QMessageBox,
        QPushButton,
        QPlainTextEdit,
        QVBoxLayout,
        QWidget,
    )

    from .config import EnhanceConfig
    from .engine import Engine
    from .errors import EnhancerError

    class Worker(QThread):
        done = Signal(dict)
        failed = Signal(str)

        def __init__(self, wav_path, workdir):
            super().__init__()
            self.wav_path = wav_path
            self.workdir = workdir

        def run(self):
            try:
                engine = Engine(self.workdir)
                report = engine.run(self.wav_path, EnhanceConfig())
                engine.export(self.workdir + "/export")
                self.done.emit(report)
            except EnhancerError as exc:
                self.failed.emit(json.dumps(exc.to_dict(), ensure_ascii=False))
            except Exception:
                self.failed.emit(traceback.format_exc())

    class MainWindow(QMainWindow):
        def __init__(self):
            super().__init__()
            self.setWindowTitle("脉冲响应噪声抑制与增强处理器")
            self.worker = None
            root = QWidget()
            lay = QVBoxLayout(root)
            row = QHBoxLayout()
            self.path_edit = QLineEdit()
            browse = QPushButton("选择 WAV…")
            browse.clicked.connect(self._browse)
            row.addWidget(QLabel("输入:"))
            row.addWidget(self.path_edit)
            row.addWidget(browse)
            lay.addLayout(row)
            self.run_btn = QPushButton("执行增强")
            self.run_btn.clicked.connect(self._run)
            lay.addWidget(self.run_btn)
            self.output = QPlainTextEdit()
            self.output.setReadOnly(True)
            lay.addWidget(self.output)
            self.setCentralWidget(root)
            self.resize(760, 560)

        def _browse(self):
            path, _ = QFileDialog.getOpenFileName(
                self, "选择脉冲响应 WAV", "", "WAV (*.wav)"
            )
            if path:
                self.path_edit.setText(path)

        def _run(self):
            path = self.path_edit.text().strip()
            if not path:
                QMessageBox.warning(self, "提示", "请先选择输入 WAV 文件")
                return
            self.run_btn.setEnabled(False)
            self.worker = Worker(path, "work/gui_run")
            self.worker.done.connect(self._done)
            self.worker.failed.connect(self._failed)
            self.worker.start()

        def _done(self, report):
            self.run_btn.setEnabled(True)
            self.output.setPlainText(json.dumps(report, ensure_ascii=False, indent=1))

        def _failed(self, message):
            self.run_btn.setEnabled(True)
            self.output.setPlainText("错误：\n" + message)

    app = QApplication.instance() or QApplication([])
    win = MainWindow()
    win.show()
    return app.exec()
