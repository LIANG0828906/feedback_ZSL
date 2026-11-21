from __future__ import annotations

import json
import threading
from fractions import Fraction
from pathlib import Path
from typing import Any

from PySide6.QtCore import QObject, QPointF, QRectF, Qt, QThread, QTimer, Signal, Slot
from PySide6.QtGui import QColor, QFont, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFormLayout,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QPushButton,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from .conversion import parse_score
from .errors import EngineError
from .model import Score, SolverParameters
from .performance_validation import ValidationTolerance, validate_performance
from .publish import VersionedPublisher
from .solver import ExpressionCurve, PerformanceSolver, SolverCancelled, TempoMap


def _fraction_from_spin(value: float) -> Fraction:
    return Fraction(str(round(value, 6)))


def _heat_colour(ratio: float, has_violation: bool) -> QColor:
    if has_violation:
        return QColor(220, 70, 62)
    ratio = max(0.0, min(1.0, ratio))
    red = int(42 + ratio * 213)
    green = int(184 - ratio * 82)
    blue = int(204 - ratio * 130)
    return QColor(red, green, blue)


def _draw_empty(painter: QPainter, rect: QRectF, text: str) -> None:
    painter.setPen(QColor(130, 140, 155))
    painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, text)


class ScoreGridCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumSize(640, 280)
        self.score: Score | None = None
        self.sequence: dict[str, Any] | None = None
        self.validation: dict[str, Any] | None = None

    def set_data(
        self,
        score: Score | None,
        sequence: dict[str, Any] | None,
        validation: dict[str, Any] | None,
    ) -> None:
        self.score = score
        self.sequence = sequence
        self.validation = validation
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor(22, 27, 36))
        if self.score is None:
            _draw_empty(painter, QRectF(self.rect()), "Load a score to show the note grid and deviation heatmap")
            return
        left = 110.0
        right = 24.0
        top = 42.0
        bottom = 24.0
        plot = QRectF(left, top, max(10.0, self.width() - left - right), max(10.0, self.height() - top - bottom))
        end_beat = max(
            (note.end for _part, note in self.score.all_notes()),
            default=Fraction(8),
        )
        end_beat = max(end_beat, Fraction(4))
        start_beat = Fraction(0)

        def x_for(beat: Fraction) -> float:
            ratio = float((beat - start_beat) / end_beat)
            return plot.left() + ratio * plot.width()

        painter.setFont(QFont("Sans", 9))
        painter.setPen(QColor(92, 103, 120))
        position = Fraction(0)
        while position < end_beat:
            signature = self.score.signature_at(position)
            bar_index = int((position - signature.start) // signature.bar_length)
            next_bar = signature.start + (bar_index + 1) * signature.bar_length
            x = x_for(position)
            painter.drawLine(QPointF(x, plot.top()), QPointF(x, plot.bottom()))
            painter.drawText(QPointF(x + 4, plot.top() - 8), f"{signature.numerator}/{signature.denominator}")
            position = min(next_bar, end_beat)
        painter.setPen(QPen(QColor(55, 64, 79), 1))
        painter.drawRect(plot)

        note_records = {
            note["note_id"]: note
            for part in (self.sequence or {}).get("parts", [])
            for note in part.get("notes", [])
        }
        reports = {
            note["note_id"]: note
            for note in (self.validation or {}).get("notes", [])
        }
        lane_height = plot.height() / max(len(self.score.parts), 1)
        for part_index, part in enumerate(self.score.parts):
            lane = QRectF(
                plot.left(),
                plot.top() + part_index * lane_height,
                plot.width(),
                lane_height,
            )
            painter.setPen(QColor(190, 200, 218))
            painter.drawText(
                QRectF(8, lane.top(), left - 16, lane.height()),
                Qt.AlignmentFlag.AlignVCenter | Qt.AlignmentFlag.AlignLeft,
                f"{part.name}\n{part.instrument}",
            )
            painter.setPen(QPen(QColor(45, 53, 67), 0.8))
            painter.drawLine(lane.topLeft(), lane.topRight())
            pitch_span = max(part.max_pitch - part.min_pitch, 1)
            for note in part.notes:
                record = note_records.get(note.id, {})
                report = reports.get(note.id, {})
                start = Fraction(record.get("start_beat", note.start))
                end = Fraction(record.get("actual_end_beat", note.end))
                x1 = x_for(start)
                x2 = max(x1 + 3.0, x_for(end))
                pitch_ratio = (part.max_pitch - note.pitch) / pitch_span
                note_height = max(7.0, lane.height() * 0.22)
                y = lane.top() + 8 + pitch_ratio * max(2.0, lane.height() - note_height - 16)
                deviations = report.get("deviations", {})
                tolerance = (self.validation or {}).get("tolerance", {})
                ratios = [
                    float(deviations.get("start_time_ms", 0.0)) / max(float(tolerance.get("timing_ms", 2.0)), 0.001),
                    float(deviations.get("end_time_ms", 0.0)) / max(float(tolerance.get("timing_ms", 2.0)), 0.001),
                    float(deviations.get("velocity", 0.0)) / max(float(tolerance.get("velocity", 1.0)), 0.001),
                    float(deviations.get("pitch_range", 0.0)),
                ]
                has_violation = bool(report.get("flags"))
                colour = _heat_colour(max(ratios, default=0.0), has_violation) if report else QColor(61, 148, 211)
                painter.setBrush(colour)
                painter.setPen(QPen(QColor(12, 16, 23), 0.8))
                painter.drawRoundedRect(QRectF(x1, y, x2 - x1, note_height), 2.5, 2.5)
        painter.setPen(QColor(210, 220, 235))
        painter.drawText(
            QRectF(left, 8, plot.width(), 24),
            Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignVCenter,
            "Per-note deviation heatmap: blue = normal, red = outside tolerance",
        )


class CurveCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumSize(640, 260)
        self.score: Score | None = None

    def set_data(self, score: Score | None) -> None:
        self.score = score
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor(22, 27, 36))
        if self.score is None:
            _draw_empty(painter, QRectF(self.rect()), "Expression anchors and tempo changes")
            return
        end_beat = max(
            (note.end for _part, note in self.score.all_notes()),
            default=Fraction(8),
        )
        plot = QRectF(70, 32, self.width() - 100, self.height() - 78)

        def x_for(beat: Fraction) -> float:
            return plot.left() + float(beat / end_beat) * plot.width()

        painter.setPen(QPen(QColor(58, 67, 82), 1))
        painter.drawLine(plot.topLeft(), plot.bottomLeft())
        painter.drawLine(plot.bottomLeft(), plot.bottomRight())
        palette = [QColor(86, 204, 164), QColor(93, 174, 255), QColor(238, 184, 92)]
        for index, part in enumerate(self.score.parts):
            curve = ExpressionCurve(self.score)
            anchors = curve._anchors_for(part.id)
            painter.setPen(QPen(palette[index % len(palette)], 1.7))
            previous: QPointF | None = None
            for anchor in anchors:
                point = QPointF(
                    x_for(anchor.start),
                    plot.top() + (1 - float(anchor.value)) * plot.height() * 0.52,
                )
                if previous is not None:
                    painter.drawLine(previous, point)
                painter.setBrush(palette[index % len(palette)])
                painter.drawEllipse(point, 4, 4)
                previous = point
        painter.setPen(QColor(210, 220, 235))
        painter.drawText(QRectF(plot.left(), 6, plot.width(), 20), "Expression curve (0..1)")

        tempo_top = plot.top() + plot.height() * 0.62
        tempo_height = plot.height() * 0.34
        bpms = [float(segment.bpm) for segment in self.score.tempo_segments]
        min_bpm = min(bpms)
        max_bpm = max(bpms)
        span = max(max_bpm - min_bpm, 1.0)
        painter.setPen(QPen(QColor(245, 152, 90), 2.0))
        for segment in self.score.tempo_segments:
            x1 = x_for(segment.start)
            x2 = x_for(segment.end if segment.end is not None else end_beat)
            y = tempo_top + (1 - (float(segment.bpm) - min_bpm) / span) * tempo_height
            painter.drawLine(QPointF(x1, y), QPointF(x2, y))
            painter.setPen(QColor(230, 190, 155))
            painter.drawText(QPointF(x1 + 3, y - 4), f"{float(segment.bpm):.0f}")
            painter.setPen(QPen(QColor(245, 152, 90), 2.0))
        painter.setFont(QFont("Sans", 9))
        painter.setPen(QColor(210, 220, 235))
        painter.drawText(QRectF(plot.left(), tempo_top - 22, plot.width(), 18), "Tempo segments (BPM)")
        for part_index, part in enumerate(self.score.parts):
            painter.setPen(palette[part_index % len(palette)])
            painter.drawText(QPointF(8, 40 + part_index * 16), part.id)


class PedalCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumSize(640, 220)
        self.score: Score | None = None
        self.sequence: dict[str, Any] | None = None

    def set_data(self, score: Score | None, sequence: dict[str, Any] | None) -> None:
        self.score = score
        self.sequence = sequence
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor(22, 27, 36))
        if self.score is None:
            _draw_empty(painter, QRectF(self.rect()), "Sustain-pedal spans and cross-measure holds")
            return
        end_beat = max(
            (note.end for _part, note in self.score.all_notes()),
            default=Fraction(8),
        )
        plot = QRectF(110, 32, self.width() - 140, self.height() - 70)

        def x_for(beat: Fraction) -> float:
            return plot.left() + float(beat / end_beat) * plot.width()

        position = Fraction(0)
        painter.setPen(QPen(QColor(52, 61, 75), 1))
        while position < end_beat:
            signature = self.score.signature_at(position)
            bar_index = int((position - signature.start) // signature.bar_length)
            next_bar = signature.start + (bar_index + 1) * signature.bar_length
            painter.drawLine(QPointF(x_for(position), plot.top()), QPointF(x_for(position), plot.bottom()))
            position = min(next_bar, end_beat)
        lane_height = plot.height() / max(len(self.score.parts), 1)
        for index, part in enumerate(self.score.parts):
            y = plot.top() + index * lane_height + lane_height / 2
            painter.setPen(QColor(195, 205, 222))
            painter.drawText(QRectF(8, y - 10, 96, 20), part.id)
            painter.setPen(QPen(QColor(70, 80, 96), 1))
            painter.drawLine(QPointF(plot.left(), y), QPointF(plot.right(), y))
            for pedal in part.pedals:
                x1 = x_for(pedal.start)
                x2 = x_for(pedal.end)
                paint_y = y - min(12.0, lane_height * 0.18)
                height = max(6.0, min(24.0, lane_height * 0.36))
                painter.setBrush(QColor(170, 117, 230, 190))
                painter.setPen(QPen(QColor(210, 180, 245), 1))
                painter.drawRoundedRect(QRectF(x1, paint_y, x2 - x1, height), 3, 3)
                painter.setPen(QColor(240, 230, 255))
                painter.drawText(QPointF(x1 + 4, paint_y + height - 2), pedal.id)
        painter.setPen(QColor(210, 220, 235))
        painter.drawText(QRectF(plot.left(), 6, plot.width(), 20), "Pedal spans (cross-measure spans remain continuous)")


class TimelineCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumSize(640, 200)
        self.entries: list[dict[str, Any]] = []

    def set_entries(self, entries: list[dict[str, Any]]) -> None:
        self.entries = entries
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor(22, 27, 36))
        painter.setPen(QColor(210, 220, 235))
        painter.drawText(QRectF(18, 8, self.width() - 36, 22), "Version timeline")
        if not self.entries:
            _draw_empty(painter, QRectF(self.rect()).adjusted(0, 36, 0, 0), "Each atomic publish appends an immutable version")
            return
        left = 24.0
        y = 58.0
        box_height = 58.0
        box_width = max(150.0, min(230.0, (self.width() - 48) / max(len(self.entries), 1) - 8))
        painter.drawLine(QPointF(left, y + box_height / 2), QPointF(self.width() - 24, y + box_height / 2))
        for index, entry in enumerate(self.entries):
            x = left + index * (box_width + 8)
            rect = QRectF(x, y, box_width, box_height)
            painter.setBrush(QColor(42, 50, 64))
            painter.setPen(QPen(QColor(91, 172, 224), 1.3))
            painter.drawRoundedRect(rect, 5, 5)
            painter.setPen(QColor(225, 233, 245))
            painter.drawText(rect.adjusted(8, 5, -8, -5), Qt.AlignmentFlag.AlignLeft, f"v{entry['sequence_version']}")
            painter.setPen(QColor(155, 169, 190))
            painter.drawText(
                rect.adjusted(8, 24, -8, -5),
                Qt.AlignmentFlag.AlignLeft,
                f"{entry['content_hash'][:16]}…\n{entry['note_count']} notes",
            )


class CancellationToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    def is_cancelled(self) -> bool:
        return self._event.is_set()


class SolveWorker(QObject):
    finished = Signal(object, object)
    failed = Signal(str)

    def __init__(
        self,
        score: Score,
        parameters: SolverParameters,
        tolerance: ValidationTolerance,
        token: CancellationToken,
    ) -> None:
        super().__init__()
        self.score = score
        self.parameters = parameters
        self.tolerance = tolerance
        self.token = token

    @Slot()
    def run(self) -> None:
        try:
            solver = PerformanceSolver(self.parameters)
            sequence = solver.solve(self.score, cancel_check=self.token.is_cancelled)
            validation = validate_performance(
                self.score,
                sequence,
                tolerance=self.tolerance,
                parameters=self.parameters,
            )
            self.finished.emit(sequence, validation)
        except SolverCancelled:
            self.failed.emit(json.dumps({"code": "E_CANCELLED", "message": "solve cancelled"}, ensure_ascii=False))
        except EngineError as exc:
            self.failed.emit(json.dumps(exc.to_dict(), ensure_ascii=False))
        except Exception as exc:
            self.failed.emit(json.dumps({"code": "E_INTERNAL", "message": str(exc)}, ensure_ascii=False))


class MainWindow(QMainWindow):
    def __init__(self, score_path: str | None = None) -> None:
        super().__init__()
        self.setWindowTitle("Digital Score Performance Dashboard")
        self.resize(1100, 760)
        self.score: Score | None = None
        self.sequence: dict[str, Any] | None = None
        self.validation: dict[str, Any] | None = None
        self.thread: QThread | None = None
        self.worker: SolveWorker | None = None
        self.token: CancellationToken | None = None
        self.publisher = VersionedPublisher()
        self._build_ui()
        initial = Path(score_path) if score_path else self._default_score_path()
        if initial is not None and initial.exists():
            self._load_path(initial)

    def _default_score_path(self) -> Path | None:
        candidates = [
            Path("artifacts/generated/scores/virtual_v3.json"),
            Path("artifacts/acceptance/scores/virtual_v3.json"),
        ]
        return next((path for path in candidates if path.exists()), None)

    def _build_ui(self) -> None:
        central = QWidget()
        root_layout = QVBoxLayout(central)
        controls = QHBoxLayout()
        self.score_combo = QComboBox()
        self.score_combo.setMinimumWidth(270)
        for path in self._sample_paths():
            self.score_combo.addItem(path.name, str(path))
        load_button = QPushButton("Load")
        load_button.clicked.connect(self._load_selected)
        self.solve_button = QPushButton("Solve")
        self.solve_button.clicked.connect(self.start_solve)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.clicked.connect(self.cancel_solve)
        self.cancel_button.setEnabled(False)
        controls.addWidget(QLabel("Sample"))
        controls.addWidget(self.score_combo)
        controls.addWidget(load_button)
        controls.addStretch(1)
        controls.addWidget(self.solve_button)
        controls.addWidget(self.cancel_button)
        root_layout.addLayout(controls)

        body = QHBoxLayout()
        side = QWidget()
        side_layout = QVBoxLayout(side)
        form = QFormLayout()
        self.expression_spin = QDoubleSpinBox()
        self.expression_spin.setRange(0.0, 2.0)
        self.expression_spin.setSingleStep(0.1)
        self.expression_spin.setValue(1.0)
        self.velocity_spin = QDoubleSpinBox()
        self.velocity_spin.setRange(0.1, 2.0)
        self.velocity_spin.setSingleStep(0.05)
        self.velocity_spin.setValue(1.0)
        self.timing_spin = QDoubleSpinBox()
        self.timing_spin.setRange(0.0, 100.0)
        self.timing_spin.setDecimals(3)
        self.timing_spin.setValue(2.0)
        self.velocity_tolerance_spin = QDoubleSpinBox()
        self.velocity_tolerance_spin.setRange(0.0, 127.0)
        self.velocity_tolerance_spin.setValue(1.0)
        form.addRow("Expression strength", self.expression_spin)
        form.addRow("Velocity scale", self.velocity_spin)
        form.addRow("Timing tolerance (ms)", self.timing_spin)
        form.addRow("Velocity tolerance", self.velocity_tolerance_spin)
        side_layout.addLayout(form)
        self.status = QLabel("Ready")
        self.status.setWordWrap(True)
        self.status.setStyleSheet("color:#aebbd0; padding:8px;")
        side_layout.addWidget(self.status)
        side_layout.addStretch(1)
        side.setMaximumWidth(240)
        body.addWidget(side)

        self.tabs = QTabWidget()
        self.grid_canvas = ScoreGridCanvas()
        self.curve_canvas = CurveCanvas()
        self.pedal_canvas = PedalCanvas()
        self.timeline_canvas = TimelineCanvas()
        self.tabs.addTab(self.grid_canvas, "Score grid / heatmap")
        self.tabs.addTab(self.curve_canvas, "Expression / tempo")
        self.tabs.addTab(self.pedal_canvas, "Pedals")
        self.tabs.addTab(self.timeline_canvas, "Version timeline")
        body.addWidget(self.tabs, 1)
        root_layout.addLayout(body, 1)
        self.setCentralWidget(central)

    def _sample_paths(self) -> list[Path]:
        roots = [
            Path("artifacts/generated/scores"),
            Path("artifacts/acceptance/scores"),
        ]
        paths: set[Path] = set()
        for root in roots:
            if root.exists():
                paths.update(root.glob("*.json"))
        return sorted(paths, key=lambda path: path.name)

    def _load_selected(self) -> None:
        path_text = self.score_combo.currentData()
        if path_text:
            self._load_path(Path(path_text))

    def _load_path(self, path: Path) -> None:
        try:
            combo_index = self.score_combo.findData(str(path))
            if combo_index >= 0:
                self.score_combo.setCurrentIndex(combo_index)
            self.score = parse_score(path)
            self.sequence = None
            self.validation = None
            self.grid_canvas.set_data(self.score, None, None)
            self.curve_canvas.set_data(self.score)
            self.pedal_canvas.set_data(self.score, None)
            self.status.setText(
                f"Loaded {path.name}: {len(self.score.parts)} parts / {self.score.note_count()} notes"
            )
        except EngineError as exc:
            self.status.setText(f"{exc.code} at {exc.note_id or '-'} / {exc.field or '-'}")

    def _parameters(self) -> SolverParameters:
        return SolverParameters(
            expression_strength=_fraction_from_spin(self.expression_spin.value()),
            velocity_scale=_fraction_from_spin(self.velocity_spin.value()),
        )

    def _tolerance(self) -> ValidationTolerance:
        return ValidationTolerance(
            timing_ms=self.timing_spin.value(),
            velocity=self.velocity_tolerance_spin.value(),
        )

    def start_solve(self) -> None:
        if self.score is None or self.thread is not None:
            return
        self.token = CancellationToken()
        self.worker = SolveWorker(self.score, self._parameters(), self._tolerance(), self.token)
        self.thread = QThread(self)
        self.worker.moveToThread(self.thread)
        self.thread.started.connect(self.worker.run)
        self.worker.finished.connect(self._on_solved)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self.thread.quit)
        self.worker.failed.connect(self.thread.quit)
        self.thread.finished.connect(self._on_thread_finished)
        self.solve_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.status.setText("Solving...")
        self.thread.start()

    def cancel_solve(self) -> None:
        if self.token is not None:
            self.token.cancel()
            self.status.setText("Cooperative cancellation requested...")

    def solve_synchronously(self) -> None:
        if self.score is None:
            return
        try:
            sequence = PerformanceSolver(self._parameters()).solve(self.score)
            validation = validate_performance(
                self.score,
                sequence,
                tolerance=self._tolerance(),
                parameters=self._parameters(),
            )
            self._apply_result(sequence, validation)
        except EngineError as exc:
            self.status.setText(f"{exc.code} at {exc.note_id or '-'} / {exc.field or '-'}")

    @Slot(object, object)
    def _on_solved(self, sequence: object, validation: object) -> None:
        self._apply_result(sequence, validation)  # type: ignore[arg-type]

    def _apply_result(self, sequence: dict[str, Any], validation: dict[str, Any]) -> None:
        self.sequence = sequence
        self.validation = validation
        snapshot = self.publisher.publish(sequence)
        self.grid_canvas.set_data(self.score, sequence, validation)
        self.pedal_canvas.set_data(self.score, sequence)
        self.timeline_canvas.set_entries(list(self.publisher.timeline()))
        summary = validation["summary"]
        maximum = validation["maximum_deviation_note"]
        self.status.setText(
            f"Published v{snapshot.version}  {snapshot.content_hash[:16]}...\n"
            f"{summary['note_count']} notes, {summary['violation_count']} violations, "
            f"max deviation {maximum['note_id'] if maximum else '-'}"
        )
        if self.score is not None:
            self.curve_canvas.set_data(self.score)

    @Slot(str)
    def _on_failed(self, message: str) -> None:
        try:
            payload = json.loads(message)
            location = payload.get("location", {})
            self.status.setText(
                f"{payload.get('code')} at {location.get('note_id', '-')} / {location.get('field', '-')}"
            )
        except json.JSONDecodeError:
            self.status.setText(message)

    @Slot()
    def _on_thread_finished(self) -> None:
        if self.thread is not None:
            self.thread.deleteLater()
        if self.worker is not None:
            self.worker.deleteLater()
        self.thread = None
        self.worker = None
        self.solve_button.setEnabled(True)
        self.cancel_button.setEnabled(False)

    def closeEvent(self, event: Any) -> None:
        if self.token is not None:
            self.token.cancel()
        if self.thread is not None:
            self.thread.quit()
            self.thread.wait(1500)
        super().closeEvent(event)


def run_gui(*, score_path: str | None = None, smoke: bool = False) -> int:
    app = QApplication.instance() or QApplication([])
    window = MainWindow(score_path)
    if smoke:
        window.solve_synchronously()
        app.processEvents()
        window.close()
        app.processEvents()
        return 0
    window.show()
    QTimer.singleShot(100, window.solve_synchronously)
    return app.exec()

