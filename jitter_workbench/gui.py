"""PyQt5 desktop workbench for jitter-buffer debugging.

All heavy logic lives in the deterministic core (engine/controller/
paths); the GUI only triggers runs and visualises results, so the
displayed numbers are exactly what the tests verify.
"""
from __future__ import annotations

import os
import sys
import traceback

import numpy as np
from PyQt5.QtCore import QObject, QThread, pyqtSignal, Qt
from PyQt5.QtGui import QPainter, QPen, QColor
from PyQt5.QtWidgets import (QApplication, QComboBox, QFormLayout,
                             QGroupBox, QHBoxLayout, QLabel, QMainWindow,
                             QMessageBox, QPushButton, QSpinBox,
                             QTabWidget, QTableWidget, QTableWidgetItem,
                             QPlainTextEdit, QVBoxLayout, QWidget)

from .controller import ControllerParams
from .errors import WorkbenchError
from .loader import load_sample
from .sample_gen import generate_sample
from .paths import run_offline, run_realtime, run_resume
from .compare import compare_three


class CurveWidget(QWidget):
    """Minimal deterministic line-plot widget (no external deps)."""

    def __init__(self, parent=None):
        super().__init__(parent)
        self._series = []
        self._markers = []
        self.setMinimumHeight(220)

    def set_series(self, series, markers=None):
        self._series = series
        self._markers = markers or []
        self.update()

    def paintEvent(self, event):
        p = QPainter(self)
        p.fillRect(self.rect(), QColor(24, 24, 28))
        if not self._series:
            p.setPen(QColor(200, 200, 200))
            p.drawText(self.rect(), Qt.AlignCenter, "no data")
            p.end()
            return
        w, h = self.width(), self.height()
        pad = 30
        ymin = min(float(np.min(s[0])) for s in self._series if len(s[0]))
        ymax = max(float(np.max(s[0])) for s in self._series if len(s[0]))
        if ymax <= ymin:
            ymax = ymin + 1.0
        n = max(len(s[0]) for s in self._series)

        def xy(i, v, m):
            x = pad + (w - 2 * pad) * (i / max(1, m - 1))
            y = h - pad - (h - 2 * pad) * ((v - ymin) / (ymax - ymin))
            return x, y

        p.setPen(QPen(QColor(90, 90, 90), 1))
        p.drawRect(pad, pad, w - 2 * pad, h - 2 * pad)
        for mx, color in self._markers:
            x = pad + (w - 2 * pad) * (mx / max(1, n - 1))
            p.setPen(QPen(color, 1, Qt.DashLine))
            p.drawLine(int(x), pad, int(x), h - pad)
        for idx, (arr, color, label) in enumerate(self._series):
            if not len(arr):
                continue
            p.setPen(QPen(color, 1.4))
            step = max(1, len(arr) // (w * 2))
            pts = [xy(i, float(arr[i]), len(arr))
                   for i in range(0, len(arr), step)]
            for (x0, y0), (x1, y1) in zip(pts, pts[1:]):
                p.drawLine(int(x0), int(y0), int(x1), int(y1))
            p.setPen(QPen(color, 1))
            p.drawText(pad + 6 + 120 * idx, 14, label)
        p.setPen(QColor(180, 180, 180))
        p.drawText(4, h - 6, "min %.3g  max %.3g" % (ymin, ymax))
        p.end()


class RunWorker(QObject):
    finished = pyqtSignal(dict)
    failed = pyqtSignal(str)

    def __init__(self, fn):
        super().__init__()
        self._fn = fn

    def run(self):
        try:
            self.finished.emit(self._fn())
        except WorkbenchError as exc:
            self.failed.emit("%s: %s" % (exc.code, exc))
        except Exception:
            self.failed.emit(traceback.format_exc())


class MainWindow(QMainWindow):
    def __init__(self, root_dir):
        super().__init__()
        self.root_dir = root_dir
        self.sample = None
        self.results = {}
        self.setWindowTitle("Jitter Buffer Workbench")
        self.resize(1100, 760)

        central = QWidget()
        self.setCentralWidget(central)
        lay = QVBoxLayout(central)

        top = QHBoxLayout()
        self.sample_combo = QComboBox()
        self._refresh_samples()
        top.addWidget(QLabel("样本:"))
        top.addWidget(self.sample_combo)
        for text, fn in (("生成样本", self.on_generate),
                         ("加载校验", self.on_load),
                         ("运行三路径验证", self.on_run_all),
                         ("清除检查点", self.on_clear_ckpt)):
            b = QPushButton(text)
            b.clicked.connect(fn)
            top.addWidget(b)
        lay.addLayout(top)

        grp = QGroupBox("自适应策略参数")
        form = QFormLayout(grp)
        self.sp_init = QSpinBox(maximum=2000, value=120, suffix=" ms")
        self.sp_min = QSpinBox(maximum=2000, value=40, suffix=" ms")
        self.sp_max = QSpinBox(maximum=2000, value=400, suffix=" ms")
        self.sp_step = QSpinBox(minimum=1, maximum=500, value=20,
                                suffix=" ms")
        form.addRow("初始目标延迟", self.sp_init)
        form.addRow("最小目标延迟", self.sp_min)
        form.addRow("最大目标延迟", self.sp_max)
        form.addRow("调整步长", self.sp_step)
        lay.addWidget(grp)

        self.tabs = QTabWidget()
        self.wave_view = CurveWidget()
        self.delay_view = CurveWidget()
        self.energy_view = CurveWidget()
        self.decision_table = QTableWidget(0, 6)
        self.decision_table.setHorizontalHeaderLabels(
            ["tick(ms)", "原因", "旧延迟", "新延迟", "断音数", "依据"])
        self.log = QPlainTextEdit(readOnly=True)
        self.tabs.addTab(self.wave_view, "播放波形")
        self.tabs.addTab(self.delay_view, "目标延迟/决策")
        self.tabs.addTab(self.energy_view, "逐帧能量")
        self.tabs.addTab(self.decision_table, "决策日志")
        self.tabs.addTab(self.log, "运行日志")
        lay.addWidget(self.tabs)
        self.statusBar().showMessage("就绪")

    def _refresh_samples(self):
        base = os.path.join(self.root_dir, "samples")
        self.sample_combo.clear()
        if os.path.isdir(base):
            for name in sorted(os.listdir(base)):
                if os.path.exists(os.path.join(base, name,
                                               "manifest.json")):
                    self.sample_combo.addItem(name)

    def _sample_dir(self):
        return os.path.join(self.root_dir, "samples",
                            self.sample_combo.currentText())

    def _params(self):
        return ControllerParams(
            init_delay_ms=self.sp_init.value(),
            min_delay_ms=self.sp_min.value(),
            max_delay_ms=self.sp_max.value(),
            step_ms=self.sp_step.value())

    def _log(self, msg):
        self.log.appendPlainText(msg)
        self.statusBar().showMessage(msg[:120])

    def _start(self, fn, done):
        self._thread = QThread()
        self._worker = RunWorker(fn)
        self._worker.moveToThread(self._thread)
        self._thread.started.connect(self._worker.run)
        self._worker.finished.connect(done)
        self._worker.finished.connect(self._thread.quit)
        self._worker.failed.connect(self._on_error)
        self._worker.failed.connect(self._thread.quit)
        self._thread.start()

    def _on_error(self, msg):
        self._log("错误: " + msg.splitlines()[-1])
        QMessageBox.warning(self, "运行失败", msg.splitlines()[-1])

    def on_generate(self):
        out = os.path.join(self.root_dir, "samples", "voice_seed42")
        m = generate_sample(out)
        self._refresh_samples()
        self._log("已生成样本 %s: %d 帧, %d 条到达记录"
                  % (m["sample_id"], m["num_frames"], len(m["arrivals"])))

    def on_load(self):
        try:
            self.sample = load_sample(self._sample_dir())
        except WorkbenchError as exc:
            self._on_error("%s (字段: %s)" % (exc, exc.field))
            return
        s = self.sample
        self._log("加载成功: %d 帧, 帧长 %d, 帧移 %d, 采样率 %d, hash %s"
                  % (s.num_frames, s.frame_len, s.hop, s.sample_rate,
                     s.content_hash[:12]))

    def on_clear_ckpt(self):
        d = os.path.join(self.root_dir, "checkpoints")
        for name in os.listdir(d) if os.path.isdir(d) else []:
            p = os.path.join(d, name)
            if os.path.isfile(p):
                os.unlink(p)
        self._log("检查点已清除")

    def on_run_all(self):
        if self.sample is None:
            self.on_load()
            if self.sample is None:
                return
        sample, params = self.sample, self._params()
        ck = os.path.join(self.root_dir, "checkpoints")

        def job():
            r_off = run_offline(sample, params)
            r_rt = run_realtime(sample, params, ck)
            r_re = run_resume(sample, params, ck, stop_after_windows=137)
            report = compare_three(r_rt, r_re, r_off)
            return {"rt": r_rt, "off": r_off, "re": r_re,
                    "report": report, "params": params}

        self._log("三路径运行中 ...")
        self._start(job, self._show_results)

    def _show_results(self, out):
        r = out["off"]
        self.results = out
        rep = out["report"]
        st = r.stats
        self._log("三路径一致: %s | 提交窗口 %d | 断音 %d | 平均排队 %.1fms "
                  "| 最终目标延迟 %dms | 重复丢弃 %d | 迟到丢弃 %d"
                  % (rep["match"], st["committed_windows"],
                     st["gap_count"], st["avg_queue_delay_ms"],
                     st["final_target_delay_ms"], st["dup_dropped"],
                     st["late_dropped"]))
        gap_marks = [(i * self.sample.hop, QColor(200, 60, 60))
                     for i, g in enumerate(r.gap_flags) if g]
        self.wave_view.set_series(
            [(self.sample.truth_wave, QColor(90, 160, 220), "truth"),
             (r.output.astype(np.float32), QColor(240, 200, 80),
              "output")],
            markers=gap_marks)
        dec = r.decisions
        if dec:
            ys = np.array([d.new_delay_ms for d in dec])
            self.delay_view.set_series(
                [(ys, QColor(120, 220, 120), "target_delay_ms")])
            self.decision_table.setRowCount(len(dec))
            for i, d in enumerate(dec):
                row = [str(d.tick), d.reason, str(d.old_delay_ms),
                       str(d.new_delay_ms), str(d.gap_count), d.rationale]
                for j, v in enumerate(row):
                    self.decision_table.setItem(i, j, QTableWidgetItem(v))
        self.energy_view.set_series(
            [(self.sample.truth_energy, QColor(90, 160, 220), "truth"),
             (r.energy, QColor(240, 200, 80), "measured")])


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    app = QApplication(sys.argv)
    win = MainWindow(root)
    win.show()
    sys.exit(app.exec_())


if __name__ == "__main__":
    main()
