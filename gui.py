# PySide6 图形界面：工程浏览、并行运行、暂停/继续/停止、断点续跑。
from __future__ import annotations

import os
import sys
import threading

from PySide6.QtCore import QThread, Signal
from PySide6.QtWidgets import (QApplication, QComboBox, QHBoxLayout, QLabel,
                               QListWidget, QMainWindow, QPlainTextEdit,
                               QProgressBar, QPushButton, QTableWidget,
                               QTableWidgetItem, QVBoxLayout, QWidget)

from caption_pipeline.engine import ALGO_VERSION, Engine
from caption_pipeline.checkpoint import CheckpointStore
from caption_pipeline.errors import PipelineError
from caption_pipeline.loader import load_project
from caption_pipeline.scheduler import Budget


class EngineWorker(QThread):
    progressed = Signal(str, int, int)
    logged = Signal(str)
    finished_ok = Signal(int, int)
    failed = Signal(str)

    def __init__(self, manifest_path, persist_dir, workers=4, parent=None):
        super().__init__(parent)
        self._manifest = manifest_path
        self._persist = persist_dir
        self._workers = workers
        self._stop = threading.Event()
        self._pause = threading.Event()  # set 表示暂停中

    def request_stop(self):
        self._stop.set()

    def request_pause(self):
        self._pause.set()

    def request_resume(self):
        self._pause.clear()

    def run(self):
        try:
            project = load_project(self._manifest)
            store = CheckpointStore(self._persist)

            def on_commit(line_id, committed, total):
                self.progressed.emit(line_id, committed, total)
                while self._pause.is_set() and not self._stop.is_set():
                    QThread.msleep(50)

            hooks = {"on_commit": on_commit,
                     "should_stop": self._stop.is_set}
            engine = Engine(project, store, Budget(max_workers=self._workers),
                            hooks=hooks)
            summary = engine.run()
            store.close()
            self.finished_ok.emit(summary["committed"], summary["total"])
        except PipelineError as exc:
            self.failed.emit(str(exc))
        except Exception as exc:  # noqa: BLE001
            self.failed.emit(f"[E_INTERNAL] {exc}")


class MainWindow(QMainWindow):
    def __init__(self, samples_root="samples", persist_root="ckpt_gui"):
        super().__init__()
        self.setWindowTitle("语音字幕时序对齐管线")
        self.resize(860, 560)
        self._samples_root = samples_root
        self._persist_root = persist_root
        self._worker = None

        central = QWidget()
        self.setCentralWidget(central)
        root = QHBoxLayout(central)

        left = QVBoxLayout()
        left.addWidget(QLabel("媒体工程"))
        self.project_list = QListWidget()
        left.addWidget(self.project_list)
        self.summary_label = QLabel("未加载")
        self.summary_label.setWordWrap(True)
        left.addWidget(self.summary_label)
        root.addLayout(left, 1)

        right = QVBoxLayout()
        self.progress = QProgressBar()
        right.addWidget(self.progress)
        self.table = QTableWidget(0, 4)
        self.table.setHorizontalHeaderLabels(["台词", "状态", "窗口", "取舍"])
        right.addWidget(self.table, 1)
        self.log = QPlainTextEdit(readOnly=True)
        self.log.setMaximumBlockCount(500)
        right.addWidget(self.log)

        btns = QHBoxLayout()
        self.btn_start = QPushButton("开始/续跑")
        self.btn_pause = QPushButton("暂停")
        self.btn_resume = QPushButton("继续")
        self.btn_stop = QPushButton("停止")
        for b in (self.btn_start, self.btn_pause, self.btn_resume,
                  self.btn_stop):
            btns.addWidget(b)
        right.addLayout(btns)
        root.addLayout(right, 2)

        self.btn_start.clicked.connect(self.start_run)
        self.btn_pause.clicked.connect(self.pause_run)
        self.btn_resume.clicked.connect(self.resume_run)
        self.btn_stop.clicked.connect(self.stop_run)
        self.project_list.currentRowChanged.connect(self.load_project_summary)
        self.reload_projects()

    def reload_projects(self):
        self.project_list.clear()
        self._manifests = []
        if os.path.isdir(self._samples_root):
            for name in sorted(os.listdir(self._samples_root)):
                path = os.path.join(self._samples_root, name, "manifest.json")
                if os.path.exists(path):
                    self._manifests.append(path)
                    self.project_list.addItem(name)
        if self._manifests:
            self.project_list.setCurrentRow(0)

    def load_project_summary(self, row):
        if row < 0 or row >= len(self._manifests):
            return
        try:
            project = load_project(self._manifests[row])
        except PipelineError as exc:
            self.summary_label.setText(str(exc))
            return
        self._project = project
        s = project.summary()
        self.summary_label.setText(
            f"工程 {s['project_id']}  台词 {s['total_lines']} 句\n"
            f"采样率 {s['sample_rate']}  帧长 {s['frame_len']}  "
            f"参数版本 {s['param_version']}")
        self.table.setRowCount(0)
        for line in project.timeline_order():
            r = self.table.rowCount()
            self.table.insertRow(r)
            self.table.setItem(r, 0, QTableWidgetItem(line.id))
            self.table.setItem(r, 1, QTableWidgetItem("待排布"))
        self.progress.setMaximum(project.total_lines)
        self.progress.setValue(0)

    def start_run(self):
        row = self.project_list.currentRow()
        if row < 0 or self._worker is not None:
            return
        manifest = self._manifests[row]
        persist = os.path.join(
            self._persist_root, os.path.basename(os.path.dirname(manifest)))
        self._worker = EngineWorker(manifest, persist)
        self._worker.progressed.connect(self.on_progress)
        self._worker.finished_ok.connect(self.on_finished)
        self._worker.failed.connect(self.on_failed)
        self._worker.finished.connect(self._clear_worker)
        self.log.appendPlainText(f"[run] {manifest} (algo {ALGO_VERSION})")
        self._worker.start()

    def pause_run(self):
        if self._worker:
            self._worker.request_pause()
            self.log.appendPlainText("[pause] 已暂停（提交边界处挂起）")

    def resume_run(self):
        if self._worker:
            self._worker.request_resume()
            self.log.appendPlainText("[resume] 继续")

    def stop_run(self):
        if self._worker:
            self._worker.request_stop()
            self.log.appendPlainText("[stop] 请求停止，已提交台词保留")

    def on_progress(self, line_id, committed, total):
        self.progress.setValue(committed)
        for r in range(self.table.rowCount()):
            if self.table.item(r, 0).text() == line_id:
                self.table.item(r, 1).setText("已提交")
        self.log.appendPlainText(f"[commit] {line_id} {committed}/{total}")

    def on_finished(self, committed, total):
        self.log.appendPlainText(f"[done] 已提交 {committed}/{total}")

    def on_failed(self, message):
        self.log.appendPlainText(f"[error] {message}")

    def _clear_worker(self):
        if self._worker:
            self._worker.deleteLater()
        self._worker = None


def main():
    app = QApplication(sys.argv)
    win = MainWindow(samples_root=sys.argv[1] if len(sys.argv) > 1 else "samples")
    win.show()
    return app.exec()


if __name__ == "__main__":
    sys.exit(main())
