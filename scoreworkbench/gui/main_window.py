"""Main PySide6 workbench window."""

from __future__ import annotations

import json
import tempfile

from PySide6.QtCore import Qt, QTimer
from PySide6.QtWidgets import (
    QComboBox,
    QFileDialog,
    QDoubleSpinBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from ..errors import WorkbenchError
from ..persistence import SnapshotStore
from ..playback import PlaybackEngine
from ..session import EditSession
from ..versioning import export_score, load_score
from .timeline import TimelineWidget


class MainWindow(QMainWindow):
    def __init__(self, snapshot_dir: str | None = None):
        super().__init__()
        self.setWindowTitle("Score & Performance Expression Workbench")
        self.resize(1280, 860)
        self.project = None
        self.session: EditSession | None = None
        self.engine: PlaybackEngine | None = None
        self.snapshot_dir = snapshot_dir or tempfile.mkdtemp(prefix="scorebench-")
        self.store = SnapshotStore(self.snapshot_dir)
        self._build_ui()
        self._timer = QTimer(self)
        self._timer.setInterval(50)
        self._timer.timeout.connect(self._tick)

    # ------------------------------------------------------------------
    def _build_ui(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)

        toolbar = QHBoxLayout()
        self.open_button = QPushButton("Open score…")
        self.open_button.clicked.connect(self.action_open)
        self.snapshot_button = QPushButton("Save snapshot")
        self.snapshot_button.clicked.connect(self.action_save_snapshot)
        self.restore_button = QPushButton("Restore snapshot")
        self.restore_button.clicked.connect(self.action_restore_snapshot)
        self.version_combo = QComboBox()
        self.version_combo.addItems(["1.0", "2.0", "3.0"])
        self.export_button = QPushButton("Export…")
        self.export_button.clicked.connect(self.action_export)
        self.commit_button = QPushButton("Commit batch")
        self.commit_button.clicked.connect(self.action_commit)
        self.rollback_button = QPushButton("Rollback")
        self.rollback_button.clicked.connect(self.action_rollback)
        for widget in (
            self.open_button, self.snapshot_button, self.restore_button,
            QLabel("Target:"), self.version_combo, self.export_button,
            self.commit_button, self.rollback_button,
        ):
            toolbar.addWidget(widget)
        toolbar.addStretch(1)
        layout.addLayout(toolbar)

        splitter = QSplitter(Qt.Vertical)
        self.note_table = QTableWidget(0, 7)
        self.note_table.setHorizontalHeaderLabels(
            ["note_id", "measure", "start", "duration", "pitch",
             "velocity", "articulation"]
        )
        self.event_table = QTableWidget(0, 6)
        self.event_table.setHorizontalHeaderLabels(
            ["event_id", "kind", "note_id", "measure", "anchor", "value"]
        )
        self.timeline = TimelineWidget()
        self.timeline.seek_requested.connect(self.action_seek)
        splitter.addWidget(self.note_table)
        splitter.addWidget(self.event_table)
        splitter.addWidget(self.timeline)
        layout.addWidget(splitter, stretch=3)

        controls = QHBoxLayout()
        self.play_button = QPushButton("Play")
        self.play_button.clicked.connect(self.action_play)
        self.pause_button = QPushButton("Pause")
        self.pause_button.clicked.connect(self.action_pause)
        self.stop_button = QPushButton("Stop")
        self.stop_button.clicked.connect(self.action_stop)
        controls.addWidget(self.play_button)
        controls.addWidget(self.pause_button)
        controls.addWidget(self.stop_button)
        controls.addWidget(QLabel("Start beat:"))
        self.start_beat = QDoubleSpinBox()
        self.start_beat.setDecimals(3)
        self.start_beat.setMaximum(100000.0)
        controls.addWidget(self.start_beat)
        self.frame_label = QLabel("frame: -")
        controls.addWidget(self.frame_label)
        controls.addStretch(1)
        layout.addLayout(controls)

        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(1000)
        layout.addWidget(self.log, stretch=1)
        self.setCentralWidget(central)

    # ------------------------------------------------------------------
    def log_line(self, text: str) -> None:
        self.log.appendPlainText(text)

    def log_error(self, error: WorkbenchError) -> None:
        self.log_line(f"[{error.code}] {error.message} :: {error.details}")

    def action_open(self, path: str | None = None) -> None:
        if path is None:
            path, _ = QFileDialog.getOpenFileName(
                self, "Open score", "", "JSON scores (*.json)"
            )
        if not path:
            return
        try:
            self.project = load_score(path)
        except WorkbenchError as error:
            self.log_error(error)
            return
        self.session = EditSession(self.project.score)
        self.engine = PlaybackEngine(self.project.score)
        self.timeline.set_engine(self.engine)
        self.version_combo.setCurrentText(self.project.source_version)
        self.refresh_tables()
        self.log_line(
            f"Loaded {path} (schema {self.project.source_version}, "
            f"{len(self.project.score.traces)} conversion traces)"
        )

    def refresh_tables(self) -> None:
        score = self.session.working
        self.note_table.setRowCount(len(score.notes))
        for row, note in enumerate(score.notes):
            values = [
                note.note_id, str(note.measure), str(note.start_beat),
                str(note.duration_beats), str(note.pitch),
                f"{note.velocity:.3f}", note.articulation,
            ]
            for column, value in enumerate(values):
                self.note_table.setItem(row, column, QTableWidgetItem(value))
        self.event_table.setRowCount(len(score.events))
        for row, event in enumerate(score.events):
            values = [
                event.event_id, event.kind, event.note_id,
                str(event.measure), f"{event.anchor_beat:.3f}",
                f"{event.value:.3f}",
            ]
            for column, value in enumerate(values):
                self.event_table.setItem(row, column, QTableWidgetItem(value))

    # ------------------------------------------------------------------
    def action_commit(self) -> None:
        if self.session is None:
            return
        try:
            record = self.session.commit()
        except WorkbenchError as error:
            self.log_error(error)
            return
        self.refresh_tables()
        self.log_line(
            f"Committed {record.batch_id}; affected={record.affected_objects}"
        )

    def action_rollback(self) -> None:
        if self.session is None:
            return
        self.session.rollback()
        self.refresh_tables()
        self.log_line("Working copy rolled back to the committed model")

    def action_export(self) -> None:
        if self.session is None:
            return
        target = self.version_combo.currentText()
        score = self.session.committed
        try:
            document = export_score(score, target, self.project.provenance)
        except WorkbenchError as error:
            self.log_error(error)
            confirmed = QMessageBox.question(
                self, "Confirm lossy downgrade",
                f"{len(error.details.get('warnings', []))} field(s) cannot be "
                "represented in the target version. Export anyway?",
            )
            if confirmed != QMessageBox.Yes:
                return
            document = export_score(
                score, target, self.project.provenance, confirm_lossy=True
            )
        path, _ = QFileDialog.getSaveFileName(
            self, "Export score", f"export_v{target}.json", "JSON (*.json)"
        )
        if path:
            with open(path, "w", encoding="utf-8") as handle:
                json.dump(document, handle, indent=2, sort_keys=True)
            self.log_line(
                f"Exported to {path} with "
                f"{len(document['_lossy_warnings'])} lossy warning(s)"
            )

    # ------------------------------------------------------------------
    def action_save_snapshot(self) -> None:
        if self.session is None:
            return
        self.store.save(self.session.committed)
        self.log_line(f"Snapshot atomically written under {self.snapshot_dir}")

    def action_restore_snapshot(self) -> None:
        try:
            bundle = self.store.load()
        except WorkbenchError as error:
            self.log_error(error)
            return
        self.project = type(
            "Project", (),
            {"score": bundle.score, "provenance": {},
             "source_version": bundle.source_version,
             "source_path": bundle.source_path},
        )
        self.session = EditSession(bundle.score)
        self.engine = PlaybackEngine(bundle.score)
        self.timeline.set_engine(self.engine)
        self.refresh_tables()
        self.log_line("Restored model from the persisted snapshot")

    # ------------------------------------------------------------------
    def action_play(self) -> None:
        if self.engine is None:
            return
        try:
            self.engine.validate_event_ranges()
            beat = self.engine.play(self.start_beat.value())
        except WorkbenchError as error:
            self.log_error(error)
            return
        self._timer.start()
        self.log_line(f"Playback started at beat {beat:.3f}")

    def action_pause(self) -> None:
        if self.engine is None:
            return
        beat = self.engine.pause()
        self._timer.stop()
        self.timeline.set_playhead(beat)
        self.log_line(f"Playback paused at beat {beat:.3f}")

    def action_stop(self) -> None:
        if self.engine is None:
            return
        self.engine.stop()
        self._timer.stop()
        self.timeline.set_playhead(0.0)

    def action_seek(self, beat: float) -> None:
        if self.engine is None:
            return
        try:
            self.engine.seek(beat)
        except WorkbenchError as error:
            self.log_error(error)
            return
        self.timeline.set_playhead(beat)

    def _tick(self) -> None:
        frame = self.engine.frame()
        self.timeline.set_playhead(frame.current_beat)
        self.frame_label.setText(
            f"beat={frame.current_beat:.3f}s={frame.elapsed_seconds:.3f} "
            f"vel={frame.velocity:.2f} leg={frame.legato:.2f} "
            f"ped={frame.pedal:.2f}"
        )
        if frame.current_beat >= self.engine.total_beats:
            self._timer.stop()
