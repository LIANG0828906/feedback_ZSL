"""PySide6 preview: per-material spheres per target colour space, side by
side, with code values, degradation stage annotation and the trace log."""
from __future__ import annotations

import json
from pathlib import Path

from PySide6.QtCore import Qt
from PySide6.QtGui import QColor, QPixmap
from PySide6.QtWidgets import (
    QApplication,
    QHeaderView,
    QLabel,
    QMainWindow,
    QPlainTextEdit,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)


def _swatch(codes: list[int], size: int = 48) -> QLabel:
    pixmap = QPixmap(size, size)
    pixmap.fill(QColor(*codes))
    label = QLabel()
    label.setPixmap(pixmap)
    return label


class PreviewWindow(QMainWindow):
    def __init__(self, manifest_path, trace_path=None):
        super().__init__()
        self.setWindowTitle("Spectral Material Preview")
        manifest = json.loads(Path(manifest_path).read_text(encoding="utf-8"))
        targets = sorted(manifest["entries"][0]["codes"]) if manifest["entries"] else []
        degradation = manifest.get("degradation") or []
        stages = ",".join(d["stage"] for d in degradation) or "none"

        central = QWidget()
        layout = QVBoxLayout(central)
        header = QLabel(
            f"batch={manifest['batch_id']}  "
            f"affected={manifest.get('affected_count')}  "
            f"reused={manifest.get('reused_count')}  "
            f"degradation={stages}  "
            f"chain_tip={manifest['hash_chain'][-1][:16]}"
        )
        layout.addWidget(header)

        table = QTableWidget(len(manifest["entries"]), 2 + 2 * len(targets))
        headers = ["material", "index"]
        for t in targets:
            headers += [f"{t} sphere", f"{t} codes"]
        table.setHorizontalHeaderLabels(headers)
        table.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeToContents)
        for row, entry in enumerate(manifest["entries"]):
            table.setItem(row, 0, QTableWidgetItem(entry["material_id"]))
            table.setItem(row, 1, QTableWidgetItem(str(entry["material_index"])))
            for col, target in enumerate(targets):
                codes = entry["codes"][target]
                table.setCellWidget(row, 2 + 2 * col, _swatch(codes))
                item = QTableWidgetItem(str(codes))
                item.setTextAlignment(Qt.AlignCenter)
                table.setItem(row, 3 + 2 * col, item)
        layout.addWidget(table)

        self.trace_view = QPlainTextEdit()
        self.trace_view.setReadOnly(True)
        self.trace_view.setMaximumBlockCount(5000)
        if trace_path and Path(trace_path).exists():
            self.trace_view.setPlainText(Path(trace_path).read_text(encoding="utf-8"))
        layout.addWidget(self.trace_view)
        self.setCentralWidget(central)
        self.resize(900, 600)


def create_app(manifest_path, trace_path=None):
    import sys

    app = QApplication.instance() or QApplication(sys.argv)
    window = PreviewWindow(manifest_path, trace_path)
    return app, window
