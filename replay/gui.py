"""PyQt5 可交互回放界面（PyQt5 懒导入，无头环境可跳过）。"""
from __future__ import annotations

from .errors import ReplayError
from .pipeline import ReplayPipeline


def create_app():
    from PyQt5 import QtCore, QtWidgets

    class ReplayWorker(QtCore.QThread):
        progress = QtCore.pyqtSignal(int, int, str)
        finished_ok = QtCore.pyqtSignal(dict)
        failed = QtCore.pyqtSignal(dict)

        def __init__(self, replay_path: str, snapshot_dir: str, baseline: str | None, parent=None):
            super().__init__(parent)
            self.pipeline = ReplayPipeline(snapshot_dir=snapshot_dir, baseline_path=baseline)
            self.replay_path = replay_path

        def pause(self) -> None:
            self.pipeline.pause()

        def resume(self) -> None:
            self.pipeline.resume()

        def cancel(self) -> None:
            self.pipeline.request_cancel()

        def run(self) -> None:
            try:
                result = self.pipeline.run(self.replay_path)
            except ReplayError as exc:
                self.failed.emit(exc.to_dict())
                return
            for tick, report in sorted(result.reports.items()):
                self.progress.emit(tick, result.total_ticks, report.state_hash)
            self.finished_ok.emit(result.to_summary())

    class ReplayWindow(QtWidgets.QWidget):
        def __init__(self):
            super().__init__()
            self.setWindowTitle("本地赛事复盘回放")
            self.worker: ReplayWorker | None = None
            layout = QtWidgets.QVBoxLayout(self)

            self.queue_view = QtWidgets.QListWidget()
            self.queue_view.addItem("回放队列：等待启动")
            layout.addWidget(self.queue_view)

            self.version_label = QtWidgets.QLabel("格式版本分布：-")
            layout.addWidget(self.version_label)

            self.progress_bar = QtWidgets.QProgressBar()
            layout.addWidget(self.progress_bar)

            self.hash_table = QtWidgets.QTableWidget(0, 2)
            self.hash_table.setHorizontalHeaderLabels(["步号", "状态哈希"])
            layout.addWidget(self.hash_table)

            self.diff_list = QtWidgets.QListWidget()
            layout.addWidget(self.diff_list)

            buttons = QtWidgets.QHBoxLayout()
            self.start_btn = QtWidgets.QPushButton("启动")
            self.pause_btn = QtWidgets.QPushButton("暂停")
            self.cancel_btn = QtWidgets.QPushButton("取消")
            self.recover_btn = QtWidgets.QPushButton("从快照恢复")
            for button in (self.start_btn, self.pause_btn, self.cancel_btn, self.recover_btn):
                buttons.addWidget(button)
            layout.addLayout(buttons)

            self.status = QtWidgets.QLabel("就绪")
            layout.addWidget(self.status)

            self.start_btn.clicked.connect(self.start)
            self.pause_btn.clicked.connect(self.toggle_pause)
            self.cancel_btn.clicked.connect(self.cancel)
            self.recover_btn.clicked.connect(self.recover)

        def configure(self, replay_path: str, snapshot_dir: str, baseline: str | None = None) -> None:
            self.replay_path = replay_path
            self.snapshot_dir = snapshot_dir
            self.baseline = baseline

        def start(self) -> None:
            if self.worker and self.worker.isRunning():
                self.diff_list.addItem("E_BUSY: 同一房间已有推进任务")
                return
            self.worker = ReplayWorker(self.replay_path, self.snapshot_dir, self.baseline)
            self.worker.progress.connect(self.on_progress)
            self.worker.finished_ok.connect(self.on_finished)
            self.worker.failed.connect(self.on_failed)
            self.worker.start()
            self.status.setText("推进中")

        def toggle_pause(self) -> None:
            if not self.worker:
                return
            if self.pause_btn.text() == "暂停":
                self.worker.pause()
                self.pause_btn.setText("继续")
                self.status.setText("已暂停")
            else:
                self.worker.resume()
                self.pause_btn.setText("暂停")
                self.status.setText("推进中")

        def cancel(self) -> None:
            if self.worker:
                self.worker.cancel()
                self.status.setText("协作式取消中")

        def recover(self) -> None:
            if self.worker and self.worker.isRunning():
                self.diff_list.addItem("E_BUSY: 推进任务运行中，无法恢复")
                return
            self.worker = ReplayWorker(self.replay_path, self.snapshot_dir, self.baseline)
            self.worker.finished_ok.connect(self.on_finished)
            self.worker.failed.connect(self.on_failed)
            self.worker.progress.connect(self.on_progress)
            self.worker.start()
            self.status.setText("从磁盘已提交快照恢复中")

        def on_progress(self, tick: int, total: int, state_hash: str) -> None:
            self.progress_bar.setMaximum(total)
            self.progress_bar.setValue(tick)
            row = self.hash_table.rowCount()
            self.hash_table.insertRow(row)
            self.hash_table.setItem(row, 0, QtWidgets.QTableWidgetItem(str(tick)))
            self.hash_table.setItem(row, 1, QtWidgets.QTableWidgetItem(state_hash))

        def on_finished(self, summary: dict) -> None:
            self.status.setText("完成")
            self.version_label.setText(f"格式版本分布：v{summary['version']} x{summary['total_ticks']}")
            if summary.get("fork"):
                fork = summary["fork"]
                self.diff_list.addItem(
                    f"分叉 tick={fork['tick']} path={fork['field_path']} "
                    f"expected={fork['expected']} actual={fork['actual']}"
                )

        def on_failed(self, payload: dict) -> None:
            self.status.setText(f"失败：{payload['code']}")
            self.diff_list.addItem(json_dumps(payload))

    return QtWidgets, ReplayWindow


def json_dumps(payload: dict) -> str:
    import json

    return json.dumps(payload, ensure_ascii=False, sort_keys=True)


def main() -> int:
    import sys

    QtWidgets, ReplayWindow = create_app()
    app = QtWidgets.QApplication(sys.argv)
    window = ReplayWindow()
    if len(sys.argv) >= 3:
        window.configure(sys.argv[1], sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else None)
    window.resize(900, 700)
    window.show()
    return app.exec_()
