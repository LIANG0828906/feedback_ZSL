#!/usr/bin/env python3
"""桌面示例: 分段状态 / 台词时间轴 / 字符高亮推进 / 误差热力图。

支持暂停、恢复与误差报告导出; 无显卡环境可用
QT_QPA_PLATFORM=offscreen 运行(附 --smoke 自检模式)。
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from PySide6.QtCore import QTimer
from PySide6.QtGui import QColor, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QListWidget,
    QListWidgetItem,
    QMainWindow,
    QPushButton,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from a11y_media import statemachine as sm
from a11y_media.api import MediaAccessibilityService

STATE_COLORS = {
    sm.PENDING: QColor("#9e9e9e"),
    sm.PROCESSING: QColor("#ffb300"),
    sm.COMMITTED: QColor("#43a047"),
    sm.FAILED: QColor("#e53935"),
}


class TimelineWidget(QWidget):
    """台词时间轴 + 字符高亮推进 + 播放时钟。"""

    def __init__(self, service):
        super().__init__()
        self.service = service
        self.playhead = 0
        self.setMinimumHeight(220)

    def _x(self, sample, width):
        total = max(1, self.service.project.total_samples)
        return int(sample / total * (width - 20)) + 10

    def paintEvent(self, event):
        p = QPainter(self)
        p.fillRect(self.rect(), QColor("#1e1e1e"))
        w = self.width()
        proj = self.service.project
        for seg in proj.segments:
            x0, x1 = self._x(seg.start, w), self._x(seg.end, w)
            p.fillRect(x0, 20, max(1, x1 - x0), 30, QColor("#37474f"))
        result = self.service._result
        if result is not None:
            for i, (w0, w1) in enumerate(result.windows):
                x0, x1 = self._x(w0, w), self._x(w1, w)
                p.fillRect(x0, 60, max(1, x1 - x0), 24, QColor("#1565c0"))
                p.setPen(QPen(QColor("#ffffff")))
                p.drawText(x0 + 2, 76, f"句{i}")
            for i, (w0, w1) in enumerate(result.windows):
                if not (w0 <= self.playhead < w1):
                    continue
                starts = result.char_starts[i]
                for j, st in enumerate(starts):
                    en = starts[j + 1] if j + 1 < len(starts) else w1
                    if st <= self.playhead:
                        x0, x1 = self._x(st, w), self._x(en, w)
                        p.fillRect(x0, 95, max(1, x1 - x0), 22, QColor("#fb8c00"))
                text = proj.subtitles[i].text
                p.setPen(QPen(QColor("#ffb300")))
                p.drawText(10, 150, f"当前句: {text}")
                break
        p.setPen(QPen(QColor("#ef5350"), 2))
        x = self._x(self.playhead, w)
        p.drawLine(x, 10, x, 130)
        p.end()


class MainWindow(QMainWindow):
    def __init__(self, project_dir: str, checkpoint_root: str):
        super().__init__()
        self.setWindowTitle("无障碍媒体呈现组件示例")
        self.svc = MediaAccessibilityService(
            project_dir, checkpoint_root=checkpoint_root
        )
        self.engine = self.svc.new_engine()
        self.playing = False
        self.seg_list = QListWidget()
        self.progress_label = QLabel()
        self.timeline = TimelineWidget(self.svc)
        self.heatmap = QTableWidget()
        self.heatmap.verticalHeader().setVisible(False)
        self.btn_run = QPushButton("开始/继续")
        self.btn_pause = QPushButton("暂停")
        self.btn_export = QPushButton("导出误差报告")
        self.btn_run.clicked.connect(self.start_processing)
        self.btn_pause.clicked.connect(self.pause)
        self.btn_export.clicked.connect(self.export_report)
        btns = QHBoxLayout()
        for b in (self.btn_run, self.btn_pause, self.btn_export):
            btns.addWidget(b)
        right = QVBoxLayout()
        right.addWidget(QLabel("分段状态"))
        right.addWidget(self.seg_list)
        right.addWidget(self.progress_label)
        right.addLayout(btns)
        right.addWidget(QLabel("误差热力图(实测/上界)"))
        right.addWidget(self.heatmap)
        root = QHBoxLayout()
        root.addWidget(self.timeline, stretch=3)
        root.addLayout(right, stretch=2)
        host = QWidget()
        host.setLayout(root)
        self.setCentralWidget(host)
        self.resize(1100, 640)
        self.proc_timer = QTimer(self)
        self.proc_timer.setInterval(120)
        self.proc_timer.timeout.connect(self._process_one)
        self.play_timer = QTimer(self)
        self.play_timer.setInterval(33)
        self.play_timer.timeout.connect(self._tick_playback)
        self._refresh_states()

    def start_processing(self):
        self.proc_timer.start()

    def pause(self):
        self.proc_timer.stop()
        self.play_timer.stop()
        self.playing = False

    def _process_one(self):
        result = self.engine.run(stop_after=1)
        self.svc.set_result(result)
        self._refresh_states()
        done, total = self.engine.tracker.progress()
        if done == total:
            self.proc_timer.stop()
            self._fill_heatmap(result.report)
            self.playing = True
            self.play_timer.start()

    def _refresh_states(self):
        self.seg_list.clear()
        states = self.engine.tracker.states()
        for i, st in enumerate(states):
            item = QListWidgetItem(f"分段 {i:02d}: {st}")
            item.setBackground(STATE_COLORS[st])
            self.seg_list.addItem(item)
        done, total = self.engine.tracker.progress()
        self.progress_label.setText(f"进度: 已提交 {done} / {total} 段")

    def _tick_playback(self):
        proj = self.svc.project
        step = int(proj.sample_rate * 0.033 * proj.assist.speed)
        self.timeline.playhead += step
        if self.timeline.playhead >= proj.total_samples:
            self.timeline.playhead = 0
        self.timeline.update()

    def _fill_heatmap(self, report):
        cols = ["envelope_err_start", "envelope_err_end", "window_dev", "char_res_max"]
        bounds = report["bounds"]
        bound_of = {
            "envelope_err_start": bounds["envelope"],
            "envelope_err_end": bounds["envelope"],
            "window_dev": bounds["window"],
            "char_res_max": bounds["schedule"],
        }
        rows = report["sentences"]
        self.heatmap.setRowCount(len(rows))
        self.heatmap.setColumnCount(len(cols))
        self.heatmap.setHorizontalHeaderLabels(cols)
        self.heatmap.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeToContents
        )
        for r, entry in enumerate(rows):
            for c, key in enumerate(cols):
                v = entry[key]
                val = v[0] / v[1] if isinstance(v, list) else v
                ratio = min(1.0, val / max(1, bound_of[key]))
                item = QTableWidgetItem(f"{val:.3g}")
                item.setBackground(
                    QColor(int(255 * ratio), int(200 * (1 - ratio)), 60)
                )
                self.heatmap.setItem(r, c, item)

    def export_report(self):
        if self.svc._result is None:
            self.progress_label.setText("尚无结果可导出")
            return
        path = os.path.abspath("data/error_report_gui.json")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            json.dump(self.svc.get_error_report(), f, ensure_ascii=False, indent=1)
        self.progress_label.setText(f"已导出: {path}")


def main() -> int:
    ap = argparse.ArgumentParser(description="无障碍媒体呈现桌面示例")
    ap.add_argument("project", help="工程目录(含 manifest.json)")
    ap.add_argument("--checkpoints", default="data/checkpoints_gui")
    ap.add_argument("--smoke", action="store_true", help="自检模式: 自动跑完后退出")
    args = ap.parse_args()
    app = QApplication(sys.argv)
    win = MainWindow(args.project, args.checkpoints)
    win.show()
    if args.smoke:
        win.start_processing()
        QTimer.singleShot(8000, app.quit)
    return app.exec()


if __name__ == "__main__":
    sys.exit(main())
