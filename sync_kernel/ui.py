from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Any

from .client import PredictiveClient
from .engine import MatchEngine
from .models import AuthoritativeState
from .protocol import decode_frame
from .samples import load_samples


try:
    from PySide6.QtCore import Qt
    from PySide6.QtGui import QColor, QPainter, QPen
    from PySide6.QtWidgets import (
        QApplication,
        QComboBox,
        QHBoxLayout,
        QLabel,
        QMainWindow,
        QPushButton,
        QSlider,
        QTableWidget,
        QTableWidgetItem,
        QVBoxLayout,
        QWidget,
    )

    QT_AVAILABLE = True
    QT_IMPORT_ERROR = ""
except Exception as exc:  # pragma: no cover - depends on optional GUI package
    QT_AVAILABLE = False
    QT_IMPORT_ERROR = str(exc)


def _decoded_timelines(
    sample_root: str | Path,
) -> tuple[Any, dict[int, list[AuthoritativeState]], list[dict[str, Any]]]:
    bundle = load_samples(sample_root)
    timelines: dict[int, list[AuthoritativeState]] = {}
    for version, frames in bundle.frames_by_version.items():
        base: AuthoritativeState | None = None
        timeline: list[AuthoritativeState] = []
        for sequence, frame in enumerate(frames):
            decoded = decode_frame(
                frame,
                base=None if sequence == 0 else base,
                context=bundle.context,
            )
            timeline.append(decoded.state.clone())
            base = decoded.state
        timelines[version] = timeline

    local_inputs = {
        tick: [
            intent
            for intent in intents
            if intent.get("player_id") == "p1"
        ]
        for tick, intents in bundle.inputs_by_tick.items()
    }
    predictor = PredictiveClient(
        version=3,
        context=bundle.context,
        initial_state=bundle.initial_state,
        inputs_by_tick=local_inputs,
    )
    last_tick = len(bundle.frames_by_version[3]) - 1
    predictor.predict_to(last_tick)
    for sequence, frame in enumerate(bundle.frames_by_version[3]):
        predictor.feed(frame)
    correction_curve = [
        {
            "tick": tick,
            "correction": sum(
                abs(correction.delta)
                for correction in predictor.corrections
                if correction.tick == tick
            ),
        }
        for tick in range(last_tick + 1)
    ]
    return bundle, timelines, correction_curve


if QT_AVAILABLE:

    class CorrectionChart(QWidget):  # pragma: no cover - GUI rendering
        def __init__(self, curve: list[dict[str, Any]]) -> None:
            super().__init__()
            self.curve = curve
            self.setMinimumHeight(180)

        def paintEvent(self, _event: Any) -> None:
            painter = QPainter(self)
            painter.fillRect(self.rect(), QColor("#111820"))
            area = self.rect().adjusted(38, 16, -16, -28)
            painter.setPen(QPen(QColor("#33414f"), 1))
            for step in range(5):
                y = area.top() + int(area.height() * step / 4)
                painter.drawLine(area.left(), y, area.right(), y)
            if not self.curve:
                return
            maximum = max(item["correction"] for item in self.curve) or 1
            count = max(1, len(self.curve) - 1)
            points = []
            for index, item in enumerate(self.curve):
                x = area.left() + int(area.width() * index / count)
                y = area.bottom() - int(
                    area.height() * item["correction"] / maximum
                )
                points.append((x, y))
            painter.setPen(QPen(QColor("#40c4ff"), 2))
            for left, right in zip(points, points[1:]):
                painter.drawLine(left[0], left[1], right[0], right[1])
            painter.setPen(QPen(QColor("#9fb3c8"), 1))
            painter.drawText(
                8,
                area.top() + 12,
                f"max correction {maximum}",
            )
            painter.drawText(area.left(), self.height() - 8, "prediction correction by tick")

    class MainWindow(QMainWindow):  # pragma: no cover - GUI rendering
        def __init__(
            self,
            bundle: Any,
            timelines: dict[int, list[AuthoritativeState]],
            correction_curve: list[dict[str, Any]],
        ) -> None:
            super().__init__()
            self.bundle = bundle
            self.timelines = timelines
            self.setWindowTitle("Deterministic Sync Kernel - Local Demo")
            self.resize(1180, 720)
            root = QWidget()
            layout = QVBoxLayout(root)
            controls = QHBoxLayout()
            controls.addWidget(QLabel("Protocol version"))
            self.version_box = QComboBox()
            for version in sorted(timelines):
                self.version_box.addItem(f"v{version}", version)
            self.version_box.setCurrentIndex(self.version_box.count() - 1)
            controls.addWidget(self.version_box)
            controls.addWidget(QLabel("Tick"))
            self.tick_slider = QSlider(Qt.Horizontal)
            self.tick_slider.setMinimum(0)
            self.tick_slider.setMaximum(len(timelines[3]) - 1)
            self.tick_slider.setValue(len(timelines[3]) - 1)
            controls.addWidget(self.tick_slider, 1)
            self.hash_label = QLabel()
            controls.addWidget(self.hash_label)
            replay = QPushButton("Reset to final")
            replay.clicked.connect(
                lambda: self.tick_slider.setValue(self.tick_slider.maximum())
            )
            controls.addWidget(replay)
            layout.addLayout(controls)

            self.table = QTableWidget(0, 8)
            self.table.setHorizontalHeaderLabels(
                [
                    "Unit",
                    "Owner",
                    "Position",
                    "Heading",
                    "HP",
                    "Cooldown",
                    "Resource",
                    "Status",
                ]
            )
            self.table.horizontalHeader().setStretchLastSection(True)
            layout.addWidget(self.table, 2)
            layout.addWidget(QLabel("Prediction correction (local-only p1 inputs vs authority)"))
            self.chart = CorrectionChart(correction_curve)
            layout.addWidget(self.chart, 1)
            self.setCentralWidget(root)
            self.version_box.currentIndexChanged.connect(self.refresh)
            self.tick_slider.valueChanged.connect(self.refresh)
            self.refresh()

        def refresh(self) -> None:
            version = int(self.version_box.currentData())
            timeline = self.timelines[version]
            tick = min(self.tick_slider.value(), len(timeline) - 1)
            state = timeline[tick]
            units = [state.units[key] for key in sorted(state.units)]
            self.table.setRowCount(len(units))
            for row, unit in enumerate(units):
                values = [
                    unit.unit_id,
                    unit.owner_id,
                    f"{unit.x},{unit.y}",
                    f"{unit.heading / 1000:.3f}",
                    f"{unit.hp}/{unit.max_hp}",
                    str(unit.cooldown),
                    str(unit.resource),
                    str(unit.status_mask),
                ]
                for column, value in enumerate(values):
                    self.table.setItem(row, column, QTableWidgetItem(value))
            self.hash_label.setText(
                f"tick={state.tick} source=v{state.source_version} "
                f"hash={state.snapshot_hash[:12]}"
            )


def ui_available() -> bool:
    return QT_AVAILABLE


def launch_ui(sample_root: str | Path) -> int:
    if not QT_AVAILABLE:
        print(f"GUI unavailable, running headless only: {QT_IMPORT_ERROR}")
        return 0
    has_display = (
        sys.platform.startswith("win")
        or bool(os.environ.get("DISPLAY"))
        or bool(os.environ.get("WAYLAND_DISPLAY"))
    )
    force_headless = os.environ.get("SYNC_FORCE_HEADLESS") == "1"
    if not has_display or force_headless:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
        print("No display detected; using Qt offscreen smoke mode.")
        app = QApplication.instance() or QApplication([])
        bundle, timelines, corrections = _decoded_timelines(sample_root)
        window = MainWindow(bundle, timelines, corrections)
        window.refresh()
        app.processEvents()
        window.close()
        return 0
    app = QApplication.instance() or QApplication([])
    bundle, timelines, corrections = _decoded_timelines(sample_root)
    window = MainWindow(bundle, timelines, corrections)
    window.show()
    return int(app.exec())

