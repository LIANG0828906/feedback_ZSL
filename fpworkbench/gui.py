"""Interactive workbench (PySide6).

Shows library version distribution, fingerprint field mappings and
migration progress; runs retrieval and migration on background threads so
the UI never blocks. Only one migration per library may run at a time --
a duplicate submission surfaces E_BUSY.

Run:  python -m fpworkbench.gui [library_root]
"""

from __future__ import annotations

import os
import sys

from PySide6.QtCore import QObject, Qt, QThread, Signal
from PySide6.QtGui import QColor, QPainter, QPen
from PySide6.QtWidgets import (QApplication, QComboBox, QGroupBox,
                               QHBoxLayout, QHeaderView, QLabel,
                               QMainWindow, QMessageBox, QProgressBar,
                               QPushButton, QTableWidget, QTableWidgetItem,
                               QVBoxLayout, QWidget)

from .errors import FpError
from .extract import extract_fingerprint
from .formats import load_fingerprint
from .library import Library
from .retrieve import search


class Worker(QObject):
    """Runs a blocking callable on a background QThread."""

    finished = Signal(object)
    failed = Signal(object)

    def __init__(self, fn):
        super().__init__()
        self._fn = fn

    def run(self):
        try:
            self.finished.emit(self._fn())
        except FpError as exc:
            self.failed.emit(exc)
        except Exception as exc:  # surfaced to the user, never swallowed
            self.failed.emit(exc)


class TimelineWidget(QWidget):
    """Paints matched segments on a time axis plus the vote histogram."""

    def __init__(self):
        super().__init__()
        self._candidates = []
        self._query_duration = 1.0
        self.setMinimumHeight(180)

    def show_results(self, candidates, query_duration_ms):
        self._candidates = candidates
        self._query_duration = max(query_duration_ms, 1.0)
        self.update()

    def paintEvent(self, event):
        p = QPainter(self)
        p.fillRect(self.rect(), QColor("#1b1b1b"))
        if not self._candidates:
            p.setPen(QColor("#888888"))
            p.drawText(self.rect(), Qt.AlignCenter, "no match results")
            return
        w, h = self.width(), self.height()
        t_max = max(c["end_ms"] for c in self._candidates) * 1.1
        t_max = max(t_max, 1.0)
        lane_h = max(18, (h - 60) // len(self._candidates))
        colors = ["#4fc3f7", "#aed581", "#ffb74d", "#f06292", "#ba68c8"]
        p.setPen(QColor("#aaaaaa"))
        p.drawLine(30, h - 30, w - 10, h - 30)
        for i, c in enumerate(self._candidates):
            y = 10 + i * lane_h
            x0 = 30 + (w - 50) * c["start_ms"] / t_max
            x1 = 30 + (w - 50) * c["end_ms"] / t_max
            p.fillRect(int(x0), y, max(2, int(x1 - x0)), lane_h - 6,
                       QColor(colors[i % len(colors)]))
            p.setPen(QColor("#dddddd"))
            p.drawText(30, y + lane_h - 8,
                       f"{c['sample_id']}  off={c['offset_ms']:.0f}ms "
                       f"votes={c['votes']} conf={c['confidence']:.3f}")
            # vote histogram ticks under the axis
            hist = c.get("vote_histogram", {})
            vmax = max(hist.values()) if hist else 1
            p.setPen(QPen(QColor(colors[i % len(colors)]), 2))
            for off_s, n in hist.items():
                x = 30 + (w - 50) * max(0.0, float(off_s)) / t_max
                tick = 4 + 18 * n / vmax
                p.drawLine(int(x), h - 30, int(x), int(h - 30 - tick))
        p.setPen(QColor("#aaaaaa"))
        p.drawText(30, h - 12, "0 ms")
        p.drawText(w - 90, h - 12, f"{t_max:.0f} ms")


class MainWindow(QMainWindow):
    def __init__(self, lib_root):
        super().__init__()
        self.setWindowTitle("Fingerprint Workbench")
        self.resize(1000, 700)
        self.lib = Library(lib_root)
        if os.path.exists(self.lib.manifest_path):
            try:
                self.lib.load()
            except FpError as exc:
                QMessageBox.critical(self, exc.code, str(exc))
        self._threads = []
        self._candidates = []

        root = QWidget()
        self.setCentralWidget(root)
        layout = QVBoxLayout(root)

        # version distribution + migration controls
        top = QHBoxLayout()
        self.dist_label = QLabel()
        top.addWidget(self.dist_label)
        self.migrate_btn = QPushButton("Migrate all to v3")
        self.migrate_btn.clicked.connect(self.start_migration)
        top.addWidget(self.migrate_btn)
        self.progress = QProgressBar()
        top.addWidget(self.progress)
        layout.addLayout(top)

        # sample table
        self.table = QTableWidget(0, 4)
        self.table.setHorizontalHeaderLabels(
            ["sample_id", "version", "migrated", "path"])
        self.table.horizontalHeader().setSectionResizeMode(
            QHeaderView.Stretch)
        layout.addWidget(self.table)

        # query row
        qrow = QHBoxLayout()
        qrow.addWidget(QLabel("Query WAV:"))
        self.query_box = QComboBox()
        self.query_box.setEditable(True)
        qrow.addWidget(self.query_box)
        self.search_btn = QPushButton("Search")
        self.search_btn.clicked.connect(self.start_search)
        qrow.addWidget(self.search_btn)
        layout.addLayout(qrow)

        # results + field mapping
        mid = QHBoxLayout()
        self.result_table = QTableWidget(0, 5)
        self.result_table.setHorizontalHeaderLabels(
            ["sample", "offset_ms", "votes", "confidence", "lib_version"])
        self.result_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.Stretch)
        mid.addWidget(self.result_table)
        map_box = QGroupBox("Field mappings (last migration)")
        map_layout = QVBoxLayout(map_box)
        self.map_table = QTableWidget(0, 3)
        self.map_table.setHorizontalHeaderLabels(
            ["source", "target", "action"])
        self.map_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.Stretch)
        map_layout.addWidget(self.map_table)
        mid.addWidget(map_box)
        layout.addLayout(mid)

        self.timeline = TimelineWidget()
        layout.addWidget(self.timeline)

        self.status = QLabel("ready")
        layout.addWidget(self.status)
        self.refresh_library_view()

    # ---------------- helpers ----------------

    def _run_bg(self, fn, on_done, on_fail=None):
        thread = QThread(self)
        worker = Worker(fn)
        worker.moveToThread(thread)
        thread.started.connect(worker.run)
        worker.finished.connect(on_done)
        worker.failed.connect(on_fail or self._on_error)
        worker.finished.connect(thread.quit)
        worker.failed.connect(thread.quit)
        thread.finished.connect(worker.deleteLater)
        thread.finished.connect(thread.deleteLater)
        # keep Python references alive until the thread finishes, otherwise
        # the worker wrapper is garbage-collected and run() never fires
        handle = (thread, worker)
        self._threads.append(handle)
        thread.finished.connect(lambda: self._threads.remove(handle))
        thread.start()

    def _on_error(self, exc):
        if isinstance(exc, FpError):
            self.status.setText(f"{exc.code}: {exc.message}")
            QMessageBox.warning(self, exc.code, str(exc))
        else:
            self.status.setText(f"error: {exc}")
            QMessageBox.warning(self, "error", str(exc))

    def refresh_library_view(self):
        dist = self.lib.version_distribution()
        self.dist_label.setText(
            "Version distribution: " +
            (", ".join(f"v{k}={v}" for k, v in dist.items()) or "empty"))
        self.table.setRowCount(len(self.lib.samples))
        for row, s in enumerate(sorted(self.lib.samples,
                                       key=lambda s: s["sample_id"])):
            self.table.setItem(row, 0, QTableWidgetItem(s["sample_id"]))
            self.table.setItem(row, 1, QTableWidgetItem(f"v{s['version']}"))
            self.table.setItem(row, 2, QTableWidgetItem(
                "yes" if s["migrated"] else "no"))
            self.table.setItem(row, 3, QTableWidgetItem(s["path"]))

    # ---------------- actions ----------------

    def start_migration(self):
        self.migrate_btn.setEnabled(False)
        self.status.setText("migration running...")

        def work():
            return self.lib.migrate_all(
                progress_cb=lambda d, t, sid: None)

        def done(reports):
            self.migrate_btn.setEnabled(True)
            self.progress.setValue(100)
            self.status.setText(
                f"migration done: {len(reports)} sample(s) migrated")
            rows = []
            for r in reports:
                rows.extend(r.field_mappings)
                for loss in r.lost_fields:
                    rows.append({"source": loss["field"],
                                 "target": "(lost)",
                                 "action": f"{loss['code']}: "
                                           f"{loss['reason']}"})
            self.map_table.setRowCount(len(rows))
            for i, m in enumerate(rows):
                self.map_table.setItem(i, 0, QTableWidgetItem(str(m["source"])))
                self.map_table.setItem(i, 1, QTableWidgetItem(str(m["target"])))
                self.map_table.setItem(i, 2, QTableWidgetItem(str(m["action"])))
            self.refresh_library_view()

        def failed(exc):
            self.migrate_btn.setEnabled(True)
            self._on_error(exc)

        self._run_bg(work, done, failed)

    def start_search(self):
        wav_path = self.query_box.currentText().strip()
        if not wav_path:
            self.status.setText("choose a query WAV first")
            return
        self.search_btn.setEnabled(False)
        self.status.setText("searching...")

        def work():
            q = extract_fingerprint(wav_path,
                                    os.path.basename(wav_path))
            entries = self.lib.load_fingerprints()
            return q, search(q, entries)

        def done(result):
            self.search_btn.setEnabled(True)
            q, candidates = result
            self._candidates = candidates
            self.result_table.setRowCount(len(candidates))
            for i, c in enumerate(candidates):
                self.result_table.setItem(
                    i, 0, QTableWidgetItem(c["sample_id"]))
                self.result_table.setItem(
                    i, 1, QTableWidgetItem(f"{c['offset_ms']:.0f}"))
                self.result_table.setItem(
                    i, 2, QTableWidgetItem(str(c["votes"])))
                self.result_table.setItem(
                    i, 3, QTableWidgetItem(f"{c['confidence']:.3f}"))
                self.result_table.setItem(
                    i, 4, QTableWidgetItem(f"v{c['lib_version']}"))
            self.timeline.show_results(candidates, q.duration_ms)
            self.status.setText(f"{len(candidates)} candidate(s)")

        def failed(exc):
            self.search_btn.setEnabled(True)
            self._on_error(exc)

        self._run_bg(work, done, failed)


def main():
    app = QApplication(sys.argv)
    lib_root = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
        os.path.dirname(__file__), "..", "artifacts", "demo")
    win = MainWindow(os.path.abspath(lib_root))
    win.show()
    sys.exit(app.exec())


if __name__ == "__main__":
    main()
