"""桌面主窗口：乐谱加载、编辑提交、精度校验、回放与断点恢复."""

from __future__ import annotations

import json
from pathlib import Path

from PySide6.QtCore import Qt, QTimer
from PySide6.QtWidgets import (
    QComboBox,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from ..checkpoint import CheckpointStore, restore_session
from ..concurrency import ParallelCommitter
from ..errors import ScoreError
from ..generator import write_samples
from ..model import Score, load_project
from ..playback import PlaybackEngine
from ..precision import verify_truth
from ..session import EditSession
from .timeline_widget import TimelineWidget


class MainWindow(QMainWindow):
    def __init__(self, sample_dir="samples", checkpoint_dir="checkpoints", parent=None):
        super().__init__(parent)
        self.setWindowTitle("ScoreForge - 数字乐谱与演奏表达编辑器")
        self.sample_dir = Path(sample_dir)
        self.checkpoint_dir = Path(checkpoint_dir)
        self.project_data = None
        self.session: EditSession | None = None
        self.truth = None
        self.playback: PlaybackEngine | None = None

        central = QWidget()
        self.setCentralWidget(central)
        root = QHBoxLayout(central)

        left = QVBoxLayout()
        root.addLayout(left, 0)

        load_box = QGroupBox("工程")
        load_form = QFormLayout(load_box)
        self.project_combo = QComboBox()
        load_form.addRow("样本工程", self.project_combo)
        btn_load = QPushButton("加载 / 校验")
        btn_load.clicked.connect(self.on_load)
        load_form.addRow(btn_load)
        btn_gen = QPushButton("重新生成样本")
        btn_gen.clicked.connect(self.on_generate)
        load_form.addRow(btn_gen)
        left.addWidget(load_box)

        self.measure_table = QTableWidget(0, 5)
        self.measure_table.setHorizontalHeaderLabels(
            ["声部", "小节", "音符数", "事件数", "已提交"])
        self.measure_table.setSelectionBehavior(QTableWidget.SelectRows)
        left.addWidget(self.measure_table, 1)

        edit_box = QGroupBox("编辑（仅工作副本）")
        edit_form = QFormLayout(edit_box)
        self.note_combo = QComboBox()
        edit_form.addRow("音符", self.note_combo)
        self.duration_spin = QSpinBox()
        self.duration_spin.setRange(1, 100000)
        self.duration_spin.setValue(480)
        edit_form.addRow("新时值(tick)", self.duration_spin)
        btn_dur = QPushButton("修改时值")
        btn_dur.clicked.connect(self.on_edit_duration)
        edit_form.addRow(btn_dur)
        self.dyn_combo = QComboBox()
        edit_form.addRow("力度事件", self.dyn_combo)
        self.dyn_spin = QSpinBox()
        self.dyn_spin.setRange(0, 127)
        edit_form.addRow("新力度", self.dyn_spin)
        btn_dyn = QPushButton("修改力度")
        btn_dyn.clicked.connect(self.on_edit_dynamics)
        edit_form.addRow(btn_dyn)
        left.addWidget(edit_box)

        commit_box = QGroupBox("提交 / 校验 / 恢复")
        commit_form = QFormLayout(commit_box)
        btn_commit = QPushButton("提交选中小节")
        btn_commit.clicked.connect(self.on_commit_selected)
        commit_form.addRow(btn_commit)
        btn_commit_all = QPushButton("并行提交全部待提交小节")
        btn_commit_all.clicked.connect(self.on_commit_all)
        commit_form.addRow(btn_commit_all)
        btn_prec = QPushButton("真值精度校验")
        btn_prec.clicked.connect(self.on_verify_precision)
        commit_form.addRow(btn_prec)
        btn_restore = QPushButton("从检查点恢复")
        btn_restore.clicked.connect(self.on_restore)
        commit_form.addRow(btn_restore)
        left.addWidget(commit_box)

        right = QVBoxLayout()
        root.addLayout(right, 1)
        transport = QHBoxLayout()
        transport.addWidget(QLabel("起播拍位(tick):"))
        self.start_spin = QSpinBox()
        self.start_spin.setRange(0, 10_000_000)
        transport.addWidget(self.start_spin)
        self.btn_play = QPushButton("播放")
        self.btn_play.clicked.connect(self.on_play_pause)
        transport.addWidget(self.btn_play)
        self.pos_label = QLabel("0 ms / tick 0")
        transport.addWidget(self.pos_label)
        transport.addStretch(1)
        right.addLayout(transport)

        self.timeline = TimelineWidget()
        scroll = QScrollArea()
        scroll.setWidget(self.timeline)
        scroll.setWidgetResizable(False)
        right.addWidget(scroll, 1)

        self.timer = QTimer(self)
        self.timer.setInterval(30)
        self.timer.timeout.connect(self.on_tick)
        self.timer.start()

        self.refresh_project_list()
        self.resize(1100, 700)

    def refresh_project_list(self):
        self.project_combo.clear()
        if self.sample_dir.exists():
            for path in sorted(self.sample_dir.glob("*.json")):
                self.project_combo.addItem(path.name, str(path))

    def show_error(self, exc: ScoreError):
        QMessageBox.warning(self, exc.code, str(exc))
        self.statusBar().showMessage(str(exc), 8000)

    def current_key(self):
        row = self.measure_table.currentRow()
        if row < 0:
            return None
        return (self.measure_table.item(row, 0).text(),
                int(self.measure_table.item(row, 1).text()))

    def refresh_measure_table(self):
        score = self.session.score if self.session else None
        if score is None:
            return
        keys = score.measure_keys()
        self.measure_table.setRowCount(len(keys))
        for row, (pid, mi) in enumerate(keys):
            m = score.part(pid).measures[mi]
            n_events = len(score.events_of_measure(pid, mi))
            committed = "是" if (pid, mi) in self.session.committed else "否"
            for col, text in enumerate(
                    (pid, str(mi), str(len(m.notes)), str(n_events), committed)):
                self.measure_table.setItem(row, col, QTableWidgetItem(text))
        self.refresh_edit_combos()
        self.timeline.set_score(self.session.score)
        self.timeline.repaint()

    def refresh_edit_combos(self):
        key = self.current_key()
        self.note_combo.clear()
        self.dyn_combo.clear()
        if key is None or self.session is None:
            return
        pid, mi = key
        for n in self.session.working.part(pid).measures[mi].notes:
            self.note_combo.addItem(f"{n.note_id} ({n.duration_ticks}t)", n.note_id)
        for ev in self.session.working.events_of_measure(pid, mi):
            if ev.type == "dynamics":
                self.dyn_combo.addItem(f"{ev.event_id}={ev.value}", ev.event_id)

    def on_generate(self):
        paths = write_samples(self.sample_dir)
        self.refresh_project_list()
        self.statusBar().showMessage(f"已生成 {len(paths)} 个样本工程", 5000)

    def on_load(self):
        path = self.project_combo.currentData()
        if not path:
            self.statusBar().showMessage("样本目录为空，请先生成样本", 5000)
            return
        try:
            data = json.loads(Path(path).read_text(encoding="utf-8"))
            score, truth = load_project(data)
        except ScoreError as exc:
            self.show_error(exc)
            return
        self.project_data = data
        self.truth = truth
        store = CheckpointStore(self.checkpoint_dir, data["project_id"])
        self.session = EditSession(score, store=store)
        self.playback = PlaybackEngine(score)
        self.start_spin.setMaximum(score.total_ticks)
        self.refresh_measure_table()
        self.statusBar().showMessage(f"已加载 {data['project_id']}（校验通过）", 5000)

    def on_edit_duration(self):
        if self.session is None or self.note_combo.currentData() is None:
            return
        key = self.current_key()
        try:
            self.session.set_note_duration(
                key[0], key[1], self.note_combo.currentData(),
                self.duration_spin.value())
        except ScoreError as exc:
            self.show_error(exc)
        self.refresh_edit_combos()

    def on_edit_dynamics(self):
        if self.session is None or self.dyn_combo.currentData() is None:
            return
        try:
            self.session.set_event_value(
                self.dyn_combo.currentData(), float(self.dyn_spin.value()))
        except ScoreError as exc:
            self.show_error(exc)

    def on_commit_selected(self):
        key = self.current_key()
        if self.session is None or key is None:
            return
        try:
            record = self.session.commit_measure(*key)
            self.statusBar().showMessage(
                f"已提交 {key} seq={record.seq} hash={record.content_hash[:12]}…", 5000)
        except ScoreError as exc:
            self.show_error(exc)
        self.refresh_measure_table()

    def on_commit_all(self):
        if self.session is None:
            return
        committer = ParallelCommitter(self.session, max_workers=4, buffer_size=8)
        results = committer.commit_all_pending()
        failed = [r for r in results if r.status == "failed"]
        msg = f"提交 {len(results) - len(failed)} 个小节"
        if failed:
            msg += f"，失败 {len(failed)}：{failed[0].error}"
        self.statusBar().showMessage(msg, 8000)
        self.refresh_measure_table()

    def on_verify_precision(self):
        if self.session is None:
            return
        report = verify_truth(self.session.score, self.truth)
        try:
            report.raise_if_failed()
            QMessageBox.information(self, "E_PREC 校验通过", report.summary())
        except ScoreError as exc:
            self.show_error(exc)
        self.statusBar().showMessage(
            f"不可校验小节（真值缺失）: {len(report.unverifiable)}", 8000)

    def on_restore(self):
        if self.project_data is None:
            return
        store = CheckpointStore(self.checkpoint_dir, self.project_data["project_id"])
        self.session = restore_session(self.project_data, store)
        self.playback = PlaybackEngine(self.session.score)
        self.refresh_measure_table()
        self.statusBar().showMessage(
            f"已从检查点恢复 {len(self.session.committed)} 个已提交小节", 5000)

    def on_play_pause(self):
        if self.playback is None:
            return
        if self.playback.playing:
            self.playback.pause()
            self.btn_play.setText("播放")
        else:
            self.playback.play(self.start_spin.value())
            self.btn_play.setText("暂停")

    def on_tick(self):
        if self.playback is None:
            return
        self.playback.advance(30)
        ms = self.playback.position_ms
        self.pos_label.setText(
            f"{ms:.0f} ms / tick {self.playback.position_tick}")
        self.timeline.set_position_ms(ms)


def run(sample_dir="samples", checkpoint_dir="checkpoints"):  # pragma: no cover
    import sys

    from PySide6.QtWidgets import QApplication

    app = QApplication(sys.argv)
    window = MainWindow(sample_dir, checkpoint_dir)
    window.show()
    return app.exec()
