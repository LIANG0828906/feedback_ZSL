from __future__ import annotations

import json
import threading
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from PySide6.QtCore import QThread, Qt, Signal
from PySide6.QtGui import QAction, QColor
from PySide6.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QComboBox,
    QDialog,
    QDialogButtonBox,
    QDoubleSpinBox,
    QFileDialog,
    QFrame,
    QGridLayout,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QSplitter,
    QStatusBar,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QTextBrowser,
    QVBoxLayout,
    QWidget,
)

from ..budget import SimulationRequest, build_budget_plan
from ..compare import compare_results
from ..errors import BalanceError, ErrorCode
from ..generator import apply_project_injection, generate_all
from ..models import (
    Candidate,
    CandidateModification,
    CandidateSet,
    Project,
    RunResult,
)
from ..report import BalanceReport, build_report
from ..scheduler import RunProgress, run_validation
from ..util import human_bp, human_hp
from ..validation import load_candidate_set, load_project, parse_project
from .charts import DamageCurveChart, WinRateChart
from .graph import DependencyGraphView
from .theme import (
    AMBER,
    CYAN,
    GREEN,
    RED,
    app_stylesheet,
    color_for_state,
)


ROOT = Path(__file__).resolve().parents[2]
DATA_DIR = ROOT / "data" / "samples"
CHECKPOINT_DIR = ROOT / "artifacts" / "ui_checkpoints"


@dataclass(frozen=True)
class ProjectBundle:
    slug: str
    manifest: dict[str, Any]
    project: Project
    candidate_set: CandidateSet
    project_path: Path
    candidate_path: Path

    @property
    def request(self) -> SimulationRequest:
        row = self.manifest.get("demo_request", {})
        return SimulationRequest(
            target_samples=int(row.get("target_samples", 64)),
            min_samples=int(row.get("min_samples", 2)),
            total_sample_budget=row.get("total_sample_budget"),
        )


class SampleRepository:
    def __init__(self, data_dir: Path) -> None:
        self.data_dir = data_dir
        self.manifest: dict[str, Any] = {}
        self.bundles: list[ProjectBundle] = []

    def load(self) -> list[ProjectBundle]:
        manifest_path = self.data_dir / "manifest.json"
        if not manifest_path.exists():
            self.manifest = generate_all(self.data_dir)
        else:
            self.manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        bundles: list[ProjectBundle] = []
        for row in self.manifest["projects"]:
            project_path = self.data_dir / row["project_file"]
            candidate_path = self.data_dir / row["candidate_file"]
            project = load_project(project_path)
            candidate_set = load_candidate_set(candidate_path, project)
            bundles.append(
                ProjectBundle(
                    slug=row["slug"],
                    manifest=row,
                    project=project,
                    candidate_set=candidate_set,
                    project_path=project_path,
                    candidate_path=candidate_path,
                )
            )
        self.bundles = bundles
        return bundles


class ValidationThread(QThread):
    progress = Signal(object)
    result_ready = Signal(object)
    error_ready = Signal(object)

    def __init__(
        self,
        *,
        project: Project,
        candidate_set: CandidateSet,
        request: SimulationRequest,
        mode: str,
        checkpoint_path: Path | None,
        parallel_slots: int | None = None,
        parent=None,
    ) -> None:
        super().__init__(parent)
        self.project = project
        self.candidate_set = candidate_set
        self.request = request
        self.mode = mode
        self.checkpoint_path = checkpoint_path
        self.parallel_slots = parallel_slots
        self.pause_event = threading.Event()

    def run(self) -> None:
        try:
            result = run_validation(
                self.project,
                self.candidate_set,
                self.request,
                checkpoint_path=self.checkpoint_path,
                mode=self.mode,
                progress=lambda item: self.progress.emit(item),
                pause_event=self.pause_event,
                parallel_slots_override=self.parallel_slots,
            )
            self.result_ready.emit(result)
        except BalanceError as exc:
            self.error_ready.emit(exc)
        except Exception as exc:  # Defensive UI boundary.
            self.error_ready.emit(
                BalanceError(
                    ErrorCode.STATE,
                    f"unexpected worker failure: {exc}",
                    "ui",
                    "worker",
                )
            )


class ThreePathThread(QThread):
    status = Signal(str)
    result_ready = Signal(object)
    error_ready = Signal(object)

    def __init__(
        self,
        project: Project,
        candidate_set: CandidateSet,
        request: SimulationRequest,
        checkpoint_dir: Path,
        parent=None,
    ) -> None:
        super().__init__(parent)
        self.project = project
        self.candidate_set = candidate_set
        self.request = request
        self.checkpoint_dir = checkpoint_dir

    def run(self) -> None:
        try:
            self.checkpoint_dir.mkdir(parents=True, exist_ok=True)
            for path in self.checkpoint_dir.glob(f"{self.project.project_id}*.sqlite3*"):
                path.unlink()
            self.status.emit("实时路径运行中…")
            realtime_cp = self.checkpoint_dir / f"{self.project.project_id}.realtime.sqlite3"
            resumed_cp = self.checkpoint_dir / f"{self.project.project_id}.resumed.sqlite3"
            realtime = run_validation(
                self.project,
                self.candidate_set,
                self.request,
                checkpoint_path=realtime_cp,
                mode="realtime",
            )
            self.status.emit("创建中断快照…")
            partial_request = replace(
                self.request,
                interrupt_after_commits=max(1, len(realtime.combos) // 3),
            )
            partial = run_validation(
                self.project,
                self.candidate_set,
                partial_request,
                checkpoint_path=resumed_cp,
                mode="resume",
            )
            self.status.emit("断点恢复运行中…")
            resumed = run_validation(
                self.project,
                self.candidate_set,
                self.request,
                checkpoint_path=resumed_cp,
                mode="resume",
            )
            self.status.emit("全量一次性运行中…")
            full = run_validation(self.project, self.candidate_set, self.request, mode="full")
            first = compare_results(realtime, full, left_label="realtime", right_label="full")
            second = compare_results(resumed, full, left_label="resumed", right_label="full")
            payload = {
                "ok": first.equal and second.equal,
                "realtime_vs_full": first.to_dict(),
                "resumed_vs_full": second.to_dict(),
                "partial_interrupted": partial.interrupted,
                "partial_committed_combos": partial.schedule.committed_combos,
                "restored_combos": resumed.schedule.restored_combos,
                "simulated_after_resume": resumed.schedule.simulated_combos,
            }
            if payload["ok"]:
                self.result_ready.emit(payload)
            else:
                self.error_ready.emit(
                    BalanceError(
                        ErrorCode.DIFF,
                        "三路径一致性校验失败",
                        self.project.project_id,
                        "result",
                        detail=payload,
                    )
                )
        except BalanceError as exc:
            self.error_ready.emit(exc)
        except Exception as exc:
            self.error_ready.emit(
                BalanceError(ErrorCode.STATE, f"three-path worker failure: {exc}", "ui", "worker")
            )


class KpiCard(QFrame):
    def __init__(self, label: str, value: str = "—", hint: str = "", parent=None) -> None:
        super().__init__(parent)
        self.setObjectName("KpiCard")
        layout = QVBoxLayout(self)
        layout.setContentsMargins(15, 10, 15, 10)
        layout.setSpacing(2)
        label_widget = QLabel(label)
        label_widget.setObjectName("KpiLabel")
        self.value = QLabel(value)
        self.value.setObjectName("KpiValue")
        self.hint = QLabel(hint)
        self.hint.setObjectName("Muted")
        self.hint.setStyleSheet("font-size: 10px;")
        layout.addWidget(label_widget)
        layout.addWidget(self.value)
        layout.addWidget(self.hint)

    def set_value(self, value: str, hint: str | None = None) -> None:
        self.value.setText(value)
        if hint is not None:
            self.hint.setText(hint)


class MainWindow(QMainWindow):
    def __init__(self, data_dir: Path = DATA_DIR) -> None:
        super().__init__()
        self.setWindowTitle("Battle Balance Lab · 战斗数值平衡验证工作台")
        self.resize(1500, 940)
        self.setMinimumSize(1180, 760)
        self.repository = SampleRepository(data_dir)
        self.bundles: list[ProjectBundle] = []
        self.bundle: ProjectBundle | None = None
        self.plan = None
        self.result: RunResult | None = None
        self.report: BalanceReport | None = None
        self.worker: ValidationThread | None = None
        self.probe_worker: ValidationThread | None = None
        self.three_worker: ThreePathThread | None = None
        self.probe_counter = 0
        self.probe_candidates: dict[str, Candidate] = {}
        self.probe_results: dict[str, RunResult] = {}
        self.selected_candidate_id: str | None = None
        self._closing = False
        self._build_ui()
        self._install_menu()
        self._load_repository()

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root_layout = QVBoxLayout(central)
        root_layout.setContentsMargins(0, 0, 0, 0)
        root_layout.setSpacing(0)
        root_layout.addWidget(self._build_header())

        body = QHBoxLayout()
        body.setContentsMargins(0, 0, 0, 0)
        body.setSpacing(0)
        body.addWidget(self._build_sidebar())
        body.addWidget(self._build_main_area(), 1)
        root_layout.addLayout(body, 1)

        status = QStatusBar()
        self.status_label = QLabel("就绪 · 本地确定性样本已加载")
        self.status_label.setObjectName("Muted")
        self.progress = QProgressBar()
        self.progress.setFixedWidth(260)
        self.progress.setRange(0, 100)
        self.progress.setValue(0)
        self.progress.setTextVisible(True)
        status.addWidget(self.status_label, 1)
        status.addPermanentWidget(self.progress)
        self.setStatusBar(status)

    def _build_header(self) -> QWidget:
        header = QFrame()
        header.setObjectName("Header")
        header.setFixedHeight(78)
        layout = QHBoxLayout(header)
        layout.setContentsMargins(20, 10, 18, 10)
        layout.setSpacing(14)
        brand_box = QVBoxLayout()
        brand_box.setSpacing(0)
        eyebrow = QLabel("LOCAL MULTIPLAYER · BALANCE ENGINE")
        eyebrow.setObjectName("Eyebrow")
        brand = QLabel("BATTLE BALANCE LAB")
        brand.setObjectName("Brand")
        brand_box.addWidget(eyebrow)
        brand_box.addWidget(brand)
        layout.addLayout(brand_box)
        layout.addSpacing(18)
        selector_box = QVBoxLayout()
        selector_box.setSpacing(3)
        selector_label = QLabel("数值工程")
        selector_label.setObjectName("Muted")
        selector_label.setStyleSheet("font-size: 10px;")
        self.project_combo = QComboBox()
        self.project_combo.setMinimumWidth(240)
        self.project_combo.currentIndexChanged.connect(self._on_project_changed)
        selector_box.addWidget(selector_label)
        selector_box.addWidget(self.project_combo)
        layout.addLayout(selector_box)
        meta_box = QVBoxLayout()
        meta_box.setSpacing(3)
        hash_label = QLabel("CONTENT HASH")
        hash_label.setObjectName("Muted")
        hash_label.setStyleSheet("font-size: 10px;")
        self.hash_badge = QLabel("—")
        self.hash_badge.setObjectName("Badge")
        self.hash_badge.setStyleSheet("font-family: 'Cascadia Mono'; font-size: 10px;")
        meta_box.addWidget(hash_label)
        meta_box.addWidget(self.hash_badge, 0, Qt.AlignmentFlag.AlignLeft)
        layout.addLayout(meta_box)
        layout.addStretch(1)
        self.three_path_button = QPushButton("三路径校验")
        self.three_path_button.clicked.connect(self._run_three_path)
        layout.addWidget(self.three_path_button)
        self.injection_button = QPushButton("异常链路")
        self.injection_button.clicked.connect(self._show_injection_results)
        layout.addWidget(self.injection_button)
        self.export_button = QPushButton("导出报告")
        self.export_button.clicked.connect(self._export_report)
        layout.addWidget(self.export_button)
        self.pause_button = QPushButton("暂停")
        self.pause_button.setEnabled(False)
        self.pause_button.clicked.connect(self._pause_validation)
        layout.addWidget(self.pause_button)
        self.run_button = QPushButton("▶  运行验证")
        self.run_button.setObjectName("Primary")
        self.run_button.setMinimumWidth(136)
        self.run_button.clicked.connect(self._start_validation)
        layout.addWidget(self.run_button)
        return header

    def _build_sidebar(self) -> QWidget:
        sidebar = QFrame()
        sidebar.setObjectName("Sidebar")
        sidebar.setFixedWidth(314)
        layout = QVBoxLayout(sidebar)
        layout.setContentsMargins(14, 14, 14, 14)
        layout.setSpacing(10)
        title = QLabel("候选调整方案")
        title.setObjectName("SectionTitle")
        layout.addWidget(title)
        self.candidate_meta = QLabel("—")
        self.candidate_meta.setObjectName("Muted")
        self.candidate_meta.setWordWrap(True)
        layout.addWidget(self.candidate_meta)
        self.candidate_table = QTableWidget(0, 3)
        self.candidate_table.setHorizontalHeaderLabels(["方案", "状态", "胜率"])
        self._configure_table(self.candidate_table)
        self.candidate_table.horizontalHeader().setSectionResizeMode(0, QHeaderView.ResizeMode.Stretch)
        self.candidate_table.horizontalHeader().setSectionResizeMode(1, QHeaderView.ResizeMode.ResizeToContents)
        self.candidate_table.horizontalHeader().setSectionResizeMode(2, QHeaderView.ResizeMode.ResizeToContents)
        self.candidate_table.itemSelectionChanged.connect(self._on_candidate_selected)
        layout.addWidget(self.candidate_table, 1)

        probe_frame = QFrame()
        probe_frame.setObjectName("Panel")
        probe_layout = QVBoxLayout(probe_frame)
        probe_layout.setContentsMargins(11, 10, 11, 10)
        probe_layout.setSpacing(7)
        probe_title = QLabel("系数探针 · 增量评估")
        probe_title.setStyleSheet(f"color: {CYAN}; font-weight: 700;")
        probe_layout.addWidget(probe_title)
        note = QLabel("主验证结果保持冻结；探针仅评估新调整，不覆盖已提交组合。")
        note.setObjectName("Muted")
        note.setWordWrap(True)
        note.setStyleSheet("font-size: 10px;")
        probe_layout.addWidget(note)
        self.probe_skill = QComboBox()
        self.probe_field = QComboBox()
        self.probe_field.addItems(["base_damage", "growth_coeff", "cooldown", "resource_cost"])
        self.probe_percent = QDoubleSpinBox()
        self.probe_percent.setRange(-90.0, 200.0)
        self.probe_percent.setDecimals(1)
        self.probe_percent.setSingleStep(1.0)
        self.probe_percent.setValue(5.0)
        self.probe_percent.setSuffix(" %")
        probe_layout.addWidget(self.probe_skill)
        row = QHBoxLayout()
        row.addWidget(self.probe_field, 1)
        row.addWidget(self.probe_percent)
        probe_layout.addLayout(row)
        self.probe_button = QPushButton("冻结基线并评估探针")
        self.probe_button.clicked.connect(self._run_probe)
        probe_layout.addWidget(self.probe_button)
        self.probe_status = QLabel("尚未运行探针")
        self.probe_status.setObjectName("Muted")
        self.probe_status.setWordWrap(True)
        self.probe_status.setStyleSheet("font-size: 10px;")
        probe_layout.addWidget(self.probe_status)
        layout.addWidget(probe_frame)
        return sidebar

    def _build_main_area(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(16, 14, 16, 12)
        layout.setSpacing(12)
        kpi_row = QHBoxLayout()
        kpi_row.setSpacing(10)
        self.kpi_candidates = KpiCard("已提交方案", "0 / 0", "方案级状态机")
        self.kpi_combos = KpiCard("已提交组合", "0 / 0", "场景级状态机")
        self.kpi_slots = KpiCard("实际峰值槽位", "—", "受最大并行度约束")
        self.kpi_queue = KpiCard("排队峰值", "—", "预算不足只阻塞，不丢弃")
        for card in (self.kpi_candidates, self.kpi_combos, self.kpi_slots, self.kpi_queue):
            card.setMinimumHeight(84)
            kpi_row.addWidget(card, 1)
        layout.addLayout(kpi_row)

        self.tabs = QTabWidget()
        self.tabs.addTab(self._build_overview_tab(), "诊断总览")
        self.tabs.addTab(self._build_budget_tab(), "预算与取舍")
        self.tabs.addTab(self._build_candidate_tab(), "逐方案诊断")
        self.tabs.addTab(self._build_report_tab(), "验证报告")
        layout.addWidget(self.tabs, 1)
        return panel

    def _build_overview_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(0, 4, 0, 0)
        splitter = QSplitter(Qt.Orientation.Vertical)
        graph_panel = QFrame()
        graph_panel.setObjectName("Panel")
        graph_layout = QVBoxLayout(graph_panel)
        graph_layout.setContentsMargins(8, 8, 8, 8)
        graph_header = QHBoxLayout()
        graph_title = QLabel("伤害公式依赖图")
        graph_title.setObjectName("SectionTitle")
        graph_header.addWidget(graph_title)
        self.graph_filter = QComboBox()
        self.graph_filter.setMinimumWidth(160)
        self.graph_filter.currentIndexChanged.connect(self._on_graph_filter_changed)
        graph_header.addWidget(self.graph_filter)
        graph_header.addStretch(1)
        self.graph_meta = QLabel("无环 DAG · 技能引用已校验")
        self.graph_meta.setObjectName("Muted")
        graph_header.addWidget(self.graph_meta)
        graph_layout.addLayout(graph_header)
        self.graph_container = QVBoxLayout()
        graph_layout.addLayout(self.graph_container, 1)
        splitter.addWidget(graph_panel)
        charts = QWidget()
        chart_layout = QHBoxLayout(charts)
        chart_layout.setContentsMargins(0, 0, 0, 0)
        chart_layout.setSpacing(10)
        self.win_chart = WinRateChart()
        self.damage_chart = DamageCurveChart()
        chart_layout.addWidget(self.win_chart, 1)
        chart_layout.addWidget(self.damage_chart, 1)
        splitter.addWidget(charts)
        splitter.setSizes([470, 290])
        layout.addWidget(splitter, 1)
        return widget

    def _build_budget_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(0, 4, 0, 0)
        layout.setSpacing(10)
        bars_frame = QFrame()
        bars_frame.setObjectName("Panel")
        grid = QGridLayout(bars_frame)
        grid.setContentsMargins(14, 11, 14, 11)
        grid.setHorizontalSpacing(18)
        grid.setVerticalSpacing(7)
        self.slot_bar = QProgressBar()
        self.sample_bar = QProgressBar()
        self.queue_bar = QProgressBar()
        self.buffer_label = QLabel("—")
        self.buffer_label.setStyleSheet("font-family: 'Cascadia Mono';")
        rows = [
            ("并行槽位占用", self.slot_bar),
            ("采样预算占用", self.sample_bar),
            ("排队水位", self.queue_bar),
            ("单线程缓冲上限", self.buffer_label),
        ]
        for row_index, (label, value_widget) in enumerate(rows):
            label_widget = QLabel(label)
            label_widget.setObjectName("Muted")
            grid.addWidget(label_widget, row_index, 0)
            grid.addWidget(value_widget, row_index, 1)
        layout.addWidget(bars_frame)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        decisions_panel = QFrame()
        decisions_panel.setObjectName("Panel")
        decisions_layout = QVBoxLayout(decisions_panel)
        decisions_layout.setContentsMargins(10, 9, 10, 10)
        decisions_title = QLabel("取舍决策序列")
        decisions_title.setObjectName("SectionTitle")
        decisions_layout.addWidget(decisions_title)
        self.decisions_table = QTableWidget(0, 4)
        self.decisions_table.setHorizontalHeaderLabels(["#", "动作", "原因", "剩余采样"])
        self._configure_table(self.decisions_table)
        self.decisions_table.horizontalHeader().setSectionResizeMode(1, QHeaderView.ResizeMode.ResizeToContents)
        self.decisions_table.horizontalHeader().setSectionResizeMode(2, QHeaderView.ResizeMode.Stretch)
        decisions_layout.addWidget(self.decisions_table, 1)
        splitter.addWidget(decisions_panel)
        combos_panel = QFrame()
        combos_panel.setObjectName("Panel")
        combos_layout = QVBoxLayout(combos_panel)
        combos_layout.setContentsMargins(10, 9, 10, 10)
        combos_title = QLabel("组合采样明细")
        combos_title.setObjectName("SectionTitle")
        combos_layout.addWidget(combos_title)
        self.budget_combos_table = QTableWidget(0, 5)
        self.budget_combos_table.setHorizontalHeaderLabels(
            ["方案 / 场景", "实际/目标", "内存上限", "时间上限", "削减原因"]
        )
        self._configure_table(self.budget_combos_table)
        self.budget_combos_table.horizontalHeader().setSectionResizeMode(0, QHeaderView.ResizeMode.Stretch)
        for column in range(1, 5):
            self.budget_combos_table.horizontalHeader().setSectionResizeMode(
                column, QHeaderView.ResizeMode.ResizeToContents
            )
        combos_layout.addWidget(self.budget_combos_table, 1)
        splitter.addWidget(combos_panel)
        splitter.setSizes([490, 700])
        layout.addWidget(splitter, 1)
        return widget

    def _build_candidate_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(0, 4, 0, 0)
        header = QHBoxLayout()
        self.candidate_detail_title = QLabel("选择一个候选方案")
        self.candidate_detail_title.setObjectName("SectionTitle")
        header.addWidget(self.candidate_detail_title)
        header.addStretch(1)
        self.candidate_detail_badge = QLabel("—")
        self.candidate_detail_badge.setObjectName("Badge")
        header.addWidget(self.candidate_detail_badge)
        layout.addLayout(header)
        splitter = QSplitter(Qt.Orientation.Vertical)
        scenario_frame = QFrame()
        scenario_frame.setObjectName("Panel")
        scenario_layout = QVBoxLayout(scenario_frame)
        scenario_layout.setContentsMargins(10, 8, 10, 10)
        self.scenario_table = QTableWidget(0, 8)
        self.scenario_table.setHorizontalHeaderLabels(
            ["场景", "模式", "采样", "胜率", "95% CI", "真值区间", "削减原因", "平均伤害"]
        )
        self._configure_table(self.scenario_table)
        self.scenario_table.horizontalHeader().setSectionResizeMode(0, QHeaderView.ResizeMode.Stretch)
        for column in range(1, 8):
            self.scenario_table.horizontalHeader().setSectionResizeMode(
                column, QHeaderView.ResizeMode.ResizeToContents
            )
        self.scenario_table.itemSelectionChanged.connect(self._on_scenario_selected)
        scenario_layout.addWidget(self.scenario_table)
        splitter.addWidget(scenario_frame)
        threshold_frame = QFrame()
        threshold_frame.setObjectName("Panel")
        threshold_layout = QVBoxLayout(threshold_frame)
        threshold_layout.setContentsMargins(10, 8, 10, 10)
        threshold_title = QLabel("关键属性阈值命中")
        threshold_title.setObjectName("SectionTitle")
        threshold_layout.addWidget(threshold_title)
        self.threshold_table = QTableWidget(0, 6)
        self.threshold_table.setHorizontalHeaderLabels(
            ["场景", "属性", "实际", "阈值", "结果", "说明"]
        )
        self._configure_table(self.threshold_table)
        self.threshold_table.horizontalHeader().setSectionResizeMode(5, QHeaderView.ResizeMode.Stretch)
        for column in range(5):
            self.threshold_table.horizontalHeader().setSectionResizeMode(
                column, QHeaderView.ResizeMode.ResizeToContents
            )
        threshold_layout.addWidget(self.threshold_table)
        splitter.addWidget(threshold_frame)
        splitter.setSizes([390, 210])
        layout.addWidget(splitter, 1)
        return widget

    def _build_report_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(0, 4, 0, 0)
        toolbar = QHBoxLayout()
        self.report_status = QLabel("报告将在验证完成后生成")
        self.report_status.setObjectName("Muted")
        toolbar.addWidget(self.report_status)
        toolbar.addStretch(1)
        export_md = QPushButton("导出 Markdown + JSON")
        export_md.clicked.connect(self._export_report)
        toolbar.addWidget(export_md)
        layout.addLayout(toolbar)
        self.report_browser = QTextBrowser()
        self.report_browser.setOpenExternalLinks(False)
        layout.addWidget(self.report_browser, 1)
        return widget

    def _install_menu(self) -> None:
        data_menu = self.menuBar().addMenu("数据")
        reload_action = QAction("重新加载本地样本", self)
        reload_action.triggered.connect(self._reload_repository)
        data_menu.addAction(reload_action)
        generate_action = QAction("重新生成固定种子样本", self)
        generate_action.triggered.connect(self._regenerate_samples)
        data_menu.addAction(generate_action)
        tools_menu = self.menuBar().addMenu("工具")
        compare_action = QAction("三路径一致性校验", self)
        compare_action.triggered.connect(self._run_three_path)
        tools_menu.addAction(compare_action)
        injection_action = QAction("错误码注入链路", self)
        injection_action.triggered.connect(self._show_injection_results)
        tools_menu.addAction(injection_action)

    def _configure_table(self, table: QTableWidget) -> None:
        table.verticalHeader().setVisible(False)
        table.setShowGrid(False)
        table.setAlternatingRowColors(True)
        table.setEditTriggers(QAbstractItemView.EditTrigger.NoEditTriggers)
        table.setSelectionBehavior(QAbstractItemView.SelectionBehavior.SelectRows)
        table.setSelectionMode(QAbstractItemView.SelectionMode.SingleSelection)
        table.setFocusPolicy(Qt.FocusPolicy.NoFocus)
        table.verticalHeader().setDefaultSectionSize(30)
        table.horizontalHeader().setHighlightSections(False)

    def _load_repository(self) -> None:
        try:
            self.bundles = self.repository.load()
        except BalanceError as exc:
            self._show_error(exc)
            return
        self.project_combo.blockSignals(True)
        self.project_combo.clear()
        for bundle in self.bundles:
            self.project_combo.addItem(bundle.project.name, bundle.slug)
        self.project_combo.blockSignals(False)
        if self.bundles:
            self.project_combo.setCurrentIndex(0)
            self._activate_bundle(self.bundles[0])

    def _reload_repository(self) -> None:
        if self._busy():
            QMessageBox.warning(self, "正在运行", "请先暂停并等待当前任务结束。")
            return
        current_slug = self.project_combo.currentData()
        self._load_repository()
        if current_slug:
            for index in range(self.project_combo.count()):
                if self.project_combo.itemData(index) == current_slug:
                    self.project_combo.setCurrentIndex(index)
                    break

    def _regenerate_samples(self) -> None:
        if self._busy():
            QMessageBox.warning(self, "正在运行", "请先暂停并等待当前任务结束。")
            return
        try:
            generate_all(self.repository.data_dir)
            self._load_repository()
            self.status_label.setText("固定种子样本已重新生成，哈希稳定")
        except Exception as exc:
            self._show_error(BalanceError(ErrorCode.STATE, str(exc), "generator", "run"))

    def _on_project_changed(self, index: int) -> None:
        if index < 0 or index >= len(self.bundles):
            return
        if self._busy():
            QMessageBox.warning(self, "正在运行", "切换工程前请暂停当前验证。")
            self.project_combo.blockSignals(True)
            self.project_combo.setCurrentIndex(self.bundles.index(self.bundle) if self.bundle else 0)
            self.project_combo.blockSignals(False)
            return
        self._activate_bundle(self.bundles[index])

    def _activate_bundle(self, bundle: ProjectBundle) -> None:
        self.bundle = bundle
        self.result = None
        self.report = None
        self.probe_results.clear()
        self.probe_candidates.clear()
        self.probe_counter = 0
        self.selected_candidate_id = None
        self.hash_badge.setText(bundle.project.content_hash()[:18] + "…")
        self.hash_badge.setToolTip(bundle.project.content_hash())
        self.candidate_meta.setText(
            f"{len(bundle.project.classes)} 职业 · {len(bundle.project.skills)} 技能 · "
            f"{len(bundle.project.formulas)} 公式节点 · {len(bundle.project.scenarios)} 场景"
        )
        self.probe_skill.blockSignals(True)
        self.probe_skill.clear()
        for skill in sorted(bundle.project.skills.values(), key=lambda item: item.id):
            self.probe_skill.addItem(f"{skill.name} · {skill.id}", skill.id)
        self.probe_skill.blockSignals(False)
        self.graph_filter.blockSignals(True)
        self.graph_filter.clear()
        self.graph_filter.addItem("全部职业", None)
        for class_id in sorted(bundle.project.classes):
            self.graph_filter.addItem(bundle.project.classes[class_id].name, class_id)
        self.graph_filter.setCurrentIndex(1)
        self.graph_filter.blockSignals(False)
        self._replace_graph(bundle.project)
        self._populate_plan(bundle)
        self._populate_candidates()
        self.win_chart.set_data([])
        self.damage_chart.set_series("", (), ())
        self.report_browser.clear()
        self.report_status.setText("报告将在验证完成后生成")
        self.scenario_table.setRowCount(0)
        self.threshold_table.setRowCount(0)
        self.candidate_detail_title.setText("选择一个候选方案")
        self.candidate_detail_badge.setText("—")
        self.status_label.setText(
            f"{bundle.project.name} 已加载 · 固定种子 {bundle.project.generator_seed}"
        )
        self._update_kpis()
        self.run_button.setText("▶  运行验证")
        self.run_button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.progress.setValue(0)
        self.progress.setFormat("%p%")

    def _replace_graph(self, project: Project) -> None:
        while self.graph_container.count():
            item = self.graph_container.takeAt(0)
            if item.widget():
                item.widget().deleteLater()
        class_filter = self.graph_filter.currentData() if hasattr(self, "graph_filter") else None
        self.graph_view = DependencyGraphView(project, class_filter=class_filter)
        self.graph_container.addWidget(self.graph_view)
        self.graph_meta.setText(
            f"{len(project.formulas)} 节点 · "
            f"{sum(len(item.formula_dependencies()) for item in project.formulas.values())} 公式依赖 · DAG 已校验"
        )

    def _on_graph_filter_changed(self, index: int) -> None:
        if self.bundle is not None:
            self._replace_graph(self.bundle.project)

    def _populate_plan(self, bundle: ProjectBundle) -> None:
        try:
            self.plan = build_budget_plan(bundle.project, bundle.candidate_set.candidates, bundle.request)
        except BalanceError as exc:
            self.plan = None
            self._show_error(exc)
            return
        self.decisions_table.setRowCount(0)
        for decision in self.plan.decisions:
            row = self.decisions_table.rowCount()
            self.decisions_table.insertRow(row)
            values = [
                f"{decision.sequence:03d}",
                decision.action,
                decision.reason,
                str(decision.budget_remaining),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column == 1:
                    item.setForeground(QColor(CYAN if decision.action == "EXPAND_SAMPLING" else AMBER))
                self.decisions_table.setItem(row, column, item)
        self.budget_combos_table.setRowCount(0)
        for combo in self.plan.combos:
            row = self.budget_combos_table.rowCount()
            self.budget_combos_table.insertRow(row)
            values = [
                f"{combo.candidate_id} / {combo.scenario_id}",
                f"{combo.samples}/{combo.requested_samples}",
                str(combo.memory_cap),
                str(combo.time_cap),
                combo.reduction_reason or "TARGET_MET",
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column == 4 and combo.reduction_reason:
                    item.setForeground(QColor(AMBER))
                self.budget_combos_table.setItem(row, column, item)
        used = sum(combo.samples for combo in self.plan.combos)
        target = sum(min(combo.requested_samples, combo.memory_cap, combo.time_cap) for combo in self.plan.combos)
        self.sample_bar.setRange(0, max(1, target))
        self.sample_bar.setValue(used)
        self.sample_bar.setFormat(f"{used} / {target} 采样")
        self.slot_bar.setRange(0, bundle.project.validation_budget.parallel_slots)
        self.slot_bar.setValue(0)
        self.slot_bar.setFormat(f"0 / {bundle.project.validation_budget.parallel_slots}")
        self.queue_bar.setRange(0, max(1, len(self.plan.combos)))
        self.queue_bar.setValue(len(self.plan.combos))
        self.queue_bar.setFormat(f"queue {len(self.plan.combos)}")
        self.buffer_label.setText(f"{self.plan.per_thread_buffer_kb} KiB / thread")
        self.buffer_label.setToolTip(
            f"工程总缓冲 {bundle.project.validation_budget.memory_buffer_kb} KiB ÷ "
            f"{bundle.project.validation_budget.parallel_slots} 槽位"
        )
        self.kpi_slots.set_value(
            f"— / {bundle.project.validation_budget.parallel_slots}",
            f"每轮最多 {bundle.project.validation_budget.max_matches_per_round} 组合",
        )
        self.kpi_queue.set_value(
            str(len(self.plan.combos)),
            "初始队列 · 预算不足只阻塞",
        )

    def _populate_candidates(self) -> None:
        assert self.bundle is not None
        self.candidate_table.setRowCount(0)
        summaries = {item.candidate_id: item for item in self.result.candidates} if self.result else {}
        for candidate in self.bundle.candidate_set.candidates:
            self._append_candidate_row(
                candidate.id,
                candidate.name,
                summaries.get(candidate.id).state if candidate.id in summaries else "PENDING",
                summaries.get(candidate.id).win_rate_bp if candidate.id in summaries else None,
                f"{candidate.id} · {candidate.note}",
            )
        if self.candidate_table.rowCount():
            self.candidate_table.selectRow(0)
            self.selected_candidate_id = self.candidate_table.item(0, 0).data(Qt.ItemDataRole.UserRole)

    def _append_candidate_row(
        self,
        candidate_id: str,
        name: str,
        state: str,
        win_rate_bp: int | None,
        tooltip: str,
    ) -> None:
        row = self.candidate_table.rowCount()
        self.candidate_table.insertRow(row)
        name_item = QTableWidgetItem(name)
        name_item.setData(Qt.ItemDataRole.UserRole, candidate_id)
        name_item.setToolTip(tooltip)
        state_item = QTableWidgetItem(state)
        state_item.setForeground(color_for_state(state))
        state_item.setToolTip(candidate_id)
        win_item = QTableWidgetItem("—" if win_rate_bp is None else human_bp(win_rate_bp))
        win_item.setTextAlignment(Qt.AlignmentFlag.AlignRight | Qt.AlignmentFlag.AlignVCenter)
        self.candidate_table.setItem(row, 0, name_item)
        self.candidate_table.setItem(row, 1, state_item)
        self.candidate_table.setItem(row, 2, win_item)

    def _all_candidates(self) -> dict[str, Candidate]:
        if not self.bundle:
            return {}
        result = self.bundle.candidate_set.by_id()
        result.update(self.probe_candidates)
        return result

    def _candidate_run(self, candidate_id: str) -> RunResult | None:
        if candidate_id in self.probe_results:
            return self.probe_results[candidate_id]
        return self.result

    def _on_candidate_selected(self) -> None:
        selected = self.candidate_table.selectedItems()
        if not selected:
            return
        self.selected_candidate_id = self.candidate_table.item(selected[0].row(), 0).data(
            Qt.ItemDataRole.UserRole
        )
        self._refresh_candidate_detail()

    def _refresh_candidate_detail(self) -> None:
        candidate_id = self.selected_candidate_id
        if not candidate_id or not self.bundle:
            return
        candidate = self._all_candidates().get(candidate_id)
        run = self._candidate_run(candidate_id)
        if candidate is None:
            return
        self.candidate_detail_title.setText(candidate.name)
        summary = (
            next((item for item in run.candidates if item.candidate_id == candidate_id), None)
            if run
            else None
        )
        self.candidate_detail_badge.setText(summary.state if summary else "PENDING")
        self.candidate_detail_badge.setStyleSheet(
            f"background: {color_for_state(summary.state if summary else 'PENDING').name()}22;"
            f"color: {color_for_state(summary.state if summary else 'PENDING').name()};"
            "border-radius: 9px; padding: 3px 8px;"
        )
        self.scenario_table.setRowCount(0)
        self.threshold_table.setRowCount(0)
        if run is None:
            return
        combo_results = {
            (item.candidate_id, item.scenario_id): item for item in run.combos
        }
        truth_win = {
            item["scenario_id"]: item for item in self.bundle.project.truth.get("win_rates", [])
        }
        truth_thresholds: dict[str, list[dict[str, Any]]] = {}
        for item in self.bundle.project.truth.get("thresholds", []):
            truth_thresholds.setdefault(item["scenario_id"], []).append(item)
        from ..validation import build_effective_values

        effective = build_effective_values(self.bundle.project, candidate)
        rows: list[dict[str, Any]] = []
        for scenario in self.bundle.project.scenarios:
            result = combo_results.get((candidate_id, scenario.id))
            if result is None:
                continue
            truth = truth_win.get(scenario.id, {})
            truth_low = truth.get("expected_low_bp", 0)
            truth_high = truth.get("expected_high_bp", 10000)
            rows.append(
                {
                    "scenario_id": scenario.id,
                    "scenario_name": scenario.name,
                    "truth_low_bp": truth_low,
                    "truth_high_bp": truth_high,
                    "win_rate_bp": result.win_rate_bp,
                    "ci_low_bp": result.ci_low_bp,
                    "ci_high_bp": result.ci_high_bp,
                }
            )
            row = self.scenario_table.rowCount()
            self.scenario_table.insertRow(row)
            overlap = not (result.ci_high_bp < truth_low or result.ci_low_bp > truth_high)
            values = [
                scenario.name,
                scenario.mode.upper(),
                str(result.samples),
                human_bp(result.win_rate_bp),
                f"{result.ci_low_bp / 100:.1f}–{result.ci_high_bp / 100:.1f}%",
                f"{truth_low / 100:.1f}–{truth_high / 100:.1f}%",
                result.reduction_reason or "TARGET_MET",
                human_hp(result.damage_mean_total),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setData(Qt.ItemDataRole.UserRole, scenario.id)
                if column == 5:
                    item.setForeground(QColor(GREEN if overlap else AMBER))
                self.scenario_table.setItem(row, column, item)
            attrs = effective["loadouts"][scenario.primary_loadout_id]
            for threshold in truth_thresholds.get(scenario.id, []):
                actual = attrs[threshold["attribute"]]
                passed = actual >= int(threshold["threshold"])
                threshold_row = self.threshold_table.rowCount()
                self.threshold_table.insertRow(threshold_row)
                threshold_values = [
                    scenario.id,
                    threshold["attribute"],
                    str(actual),
                    str(threshold["threshold"]),
                    "PASS" if passed else "MISS",
                    threshold.get("description", ""),
                ]
                for column, value in enumerate(threshold_values):
                    item = QTableWidgetItem(value)
                    if column == 4:
                        item.setForeground(QColor(GREEN if passed else RED))
                    self.threshold_table.setItem(threshold_row, column, item)
        self.win_chart.set_data(rows)
        if rows:
            first_scenario = rows[0]["scenario_id"]
            self._set_damage_curve(candidate_id, first_scenario)
            if self.scenario_table.rowCount():
                self.scenario_table.selectRow(0)

    def _set_damage_curve(self, candidate_id: str, scenario_id: str) -> None:
        run = self._candidate_run(candidate_id)
        if not run:
            self.damage_chart.set_series("", (), ())
            return
        result = next(
            (
                item
                for item in run.combos
                if item.candidate_id == candidate_id and item.scenario_id == scenario_id
            ),
            None,
        )
        if result:
            self.damage_chart.set_series(
                scenario_id,
                result.round_curve_a,
                result.round_curve_b,
            )

    def _on_scenario_selected(self) -> None:
        if not self.selected_candidate_id or not self.scenario_table.selectedItems():
            return
        scenario_id = self.scenario_table.item(
            self.scenario_table.selectedItems()[0].row(), 0
        ).data(Qt.ItemDataRole.UserRole)
        if scenario_id:
            self._set_damage_curve(self.selected_candidate_id, scenario_id)

    def _start_validation(self) -> None:
        if not self.bundle or self._busy():
            return
        checkpoint = CHECKPOINT_DIR / f"{self.bundle.project.project_id}.sqlite3"
        checkpoint.parent.mkdir(parents=True, exist_ok=True)
        mode = "resume" if checkpoint.exists() else "realtime"
        self.worker = ValidationThread(
            project=self.bundle.project,
            candidate_set=self.bundle.candidate_set,
            request=self.bundle.request,
            mode=mode,
            checkpoint_path=checkpoint,
        )
        self.worker.progress.connect(self._on_progress)
        self.worker.result_ready.connect(self._on_result)
        self.worker.error_ready.connect(self._on_worker_error)
        self.worker.finished.connect(self._on_worker_finished)
        self._set_running(True, f"验证中 · {mode}")
        self.progress.setRange(0, max(1, len(self.plan.combos) if self.plan else 1))
        self.progress.setValue(0)
        self.progress.setFormat("0 / %m")
        self.worker.start()

    def _pause_validation(self) -> None:
        if self.worker and self.worker.isRunning():
            self.worker.pause_event.set()
            self.pause_button.setEnabled(False)
            self.status_label.setText("暂停请求已发送：将在当前调度轮提交后停止")
        elif self.result and self.result.interrupted:
            self._start_validation()

    def _on_progress(self, progress: RunProgress) -> None:
        self.progress.setRange(0, max(1, progress.total_combos))
        self.progress.setValue(progress.submitted_combos)
        self.progress.setFormat(f"{progress.submitted_combos} / {progress.total_combos}")
        self.status_label.setText(
            f"{progress.phase} · {progress.candidate_id} / {progress.scenario_id} · "
            f"{progress.submitted_candidates}/{progress.total_candidates} 方案已提交"
        )
        self.kpi_candidates.set_value(
            f"{progress.submitted_candidates} / {progress.total_candidates}"
        )
        self.kpi_combos.set_value(f"{progress.submitted_combos} / {progress.total_combos}")

    def _on_result(self, result: RunResult) -> None:
        assert self.bundle is not None
        self.result = result
        self.report = build_report(self.bundle.project, self.bundle.candidate_set, result)
        self._populate_candidates()
        self._update_kpis()
        self.report_browser.setMarkdown(self.report.to_markdown())
        summary = self.report.payload["summary"]
        self.report_status.setText(
            f"报告状态 {summary['status']} · 真值重叠 {summary['truth_overlap_combos']}/"
            f"{summary['total_combos']} · 阈值 {summary['thresholds_passed']}/"
            f"{summary['thresholds_total']}"
        )
        self.tabs.setCurrentIndex(0)
        self._refresh_candidate_detail()
        if result.interrupted:
            self.run_button.setText("▶  继续验证")
            self.status_label.setText("验证已暂停，检查点已持久化；可继续或安全退出")
        else:
            self.run_button.setText("▶  重新载入检查点")
            self.status_label.setText(
                f"验证完成 · 峰值槽位 {result.schedule.actual_peak_slots} · "
                f"调度轮 {result.schedule.dispatch_rounds}"
            )
            self.progress.setValue(result.schedule.total_combos)
        self._set_running(False)

    def _update_kpis(self) -> None:
        if not self.bundle:
            return
        if self.result:
            submitted_candidates = sum(
                1 for item in self.result.candidates if item.state == "SUBMITTED"
            )
            self.kpi_candidates.set_value(
                f"{submitted_candidates} / {len(self.result.candidates)}",
                "方案级状态机",
            )
            self.kpi_combos.set_value(
                f"{self.result.schedule.committed_combos} / {self.result.schedule.total_combos}",
                "场景级状态机",
            )
            self.kpi_slots.set_value(
                f"{self.result.schedule.actual_peak_slots} / "
                f"{self.result.schedule.configured_parallel_slots}",
                "实际并发上界已执行",
            )
            self.kpi_queue.set_value(
                str(self.result.schedule.queued_peak),
                f"阻塞轮次 {self.result.schedule.blocked_waves}",
            )
            self.slot_bar.setRange(0, self.result.schedule.configured_parallel_slots)
            self.slot_bar.setValue(self.result.schedule.actual_peak_slots)
            self.slot_bar.setFormat(
                f"{self.result.schedule.actual_peak_slots} / "
                f"{self.result.schedule.configured_parallel_slots}"
            )
            self.queue_bar.setRange(0, max(1, self.result.schedule.total_combos))
            self.queue_bar.setValue(self.result.schedule.queued_peak)
            self.queue_bar.setFormat(f"peak {self.result.schedule.queued_peak}")
        else:
            self.kpi_candidates.set_value(
                f"0 / {len(self.bundle.candidate_set.candidates)}", "等待验证"
            )
            self.kpi_combos.set_value(
                f"0 / {len(self.plan.combos) if self.plan else 0}", "等待验证"
            )

    def _on_worker_error(self, error: BalanceError) -> None:
        self._show_error(error)
        self.status_label.setText(f"验证失败 · {error.code.value}")
        self.run_button.setText("▶  重试验证")
        self._set_running(False)

    def _on_worker_finished(self) -> None:
        self.pause_button.setEnabled(False)
        self.worker = None

    def _on_worker_finished_idle(self) -> None:
        self.pause_button.setEnabled(False)

    def _set_running(self, running: bool, message: str = "") -> None:
        self.run_button.setEnabled(not running)
        self.three_path_button.setEnabled(not running)
        self.probe_button.setEnabled(not running)
        self.injection_button.setEnabled(not running)
        self.export_button.setEnabled(not running)
        self.project_combo.setEnabled(not running)
        self.pause_button.setEnabled(running)
        if message:
            self.status_label.setText(message)

    def _busy(self) -> bool:
        return any(
            thread is not None
            for thread in (self.worker, self.probe_worker, self.three_worker)
        )

    def _run_probe(self) -> None:
        if not self.bundle or self._busy():
            return
        skill_id = self.probe_skill.currentData()
        if not skill_id:
            return
        field_name = self.probe_field.currentText()
        percent = self.probe_percent.value()
        self.probe_counter += 1
        candidate_id = f"candidate.probe.{self.probe_counter}"
        if field_name == "cooldown":
            delta = int(round(percent / 100.0))
            if delta == 0:
                delta = 1 if percent >= 0 else -1
            modification = CandidateModification("skill", skill_id, field_name, "add", delta)
            label = f"{delta:+d} 回合"
        else:
            factor = int(round(10000 * (1.0 + percent / 100.0)))
            modification = CandidateModification("skill", skill_id, field_name, "scale_bp", factor)
            label = f"{percent:+.1f}%"
        candidate = Candidate(
            id=candidate_id,
            name=f"探针 {skill_id.split('.')[-1]}.{field_name} {label}",
            priority=1,
            modifications=(modification,),
            note="UI 增量探针；主验证已提交结果保持不变。",
        )
        candidate_set = CandidateSet(
            project_id=self.bundle.project.project_id,
            candidates=(candidate,),
        )
        request = SimulationRequest(
            target_samples=self.bundle.request.target_samples,
            min_samples=self.bundle.request.min_samples,
            total_sample_budget=None,
        )
        self.probe_worker = ValidationThread(
            project=self.bundle.project,
            candidate_set=candidate_set,
            request=request,
            mode="full",
            checkpoint_path=None,
        )
        self.probe_worker.result_ready.connect(
            lambda result, candidate=candidate: self._on_probe_result(candidate, result)
        )
        self.probe_worker.error_ready.connect(self._on_probe_error)
        self.probe_worker.finished.connect(self._on_probe_finished)
        self.probe_status.setText("探针评估中…")
        self.probe_button.setEnabled(False)
        self.run_button.setEnabled(False)
        self.status_label.setText(f"增量探针运行中 · {candidate_id}")
        self.probe_worker.start()

    def _on_probe_result(self, candidate: Candidate, result: RunResult) -> None:
        self.probe_candidates[candidate.id] = candidate
        self.probe_results[candidate.id] = result
        summary = result.candidates[0] if result.candidates else None
        self._append_candidate_row(
            candidate.id,
            candidate.name,
            summary.state if summary else "SUBMITTED",
            summary.win_rate_bp if summary else None,
            candidate.note,
        )
        self.probe_status.setText(
            f"探针完成 · {len(result.combos)} 组合 · "
            f"{summary.samples if summary else 0} 采样 · 主结果未变更"
        )
        self.status_label.setText("增量探针完成；已提交主验证结果保持冻结")
        self.candidate_table.selectRow(self.candidate_table.rowCount() - 1)
        self._refresh_candidate_detail()

    def _on_probe_error(self, error: BalanceError) -> None:
        self.probe_status.setText(f"探针失败 · {error.code.value}")
        self._show_error(error)

    def _on_probe_finished(self) -> None:
        self.probe_worker = None
        self.probe_button.setEnabled(True)
        self.run_button.setEnabled(True)

    def _run_three_path(self) -> None:
        if not self.bundle or self._busy():
            return
        self.three_worker = ThreePathThread(
            self.bundle.project,
            self.bundle.candidate_set,
            self.bundle.request,
            CHECKPOINT_DIR / "three_path",
        )
        self.three_worker.status.connect(lambda text: self.status_label.setText(text))
        self.three_worker.result_ready.connect(self._on_three_path_result)
        self.three_worker.error_ready.connect(self._on_three_path_error)
        self.three_worker.finished.connect(self._on_three_path_finished)
        self._set_running(True, "三路径一致性校验启动…")
        self.three_worker.start()

    def _on_three_path_result(self, payload: dict[str, Any]) -> None:
        text = (
            "三路径逐场一致。\n\n"
            f"中断快照: {payload['partial_committed_combos']} 组合提交，"
            f"interrupted={payload['partial_interrupted']}\n"
            f"恢复复用: {payload['restored_combos']} 组合\n"
            f"恢复补算: {payload['simulated_after_resume']} 组合\n\n"
            "实时 vs 全量: OK\n断点恢复 vs 全量: OK"
        )
        QMessageBox.information(self, "三路径一致性 · 通过", text)
        self.status_label.setText("三路径一致性校验通过")

    def _on_three_path_error(self, error: BalanceError) -> None:
        self._show_error(error)
        self.status_label.setText(f"三路径校验失败 · {error.code.value}")

    def _on_three_path_finished(self) -> None:
        self.three_worker = None
        self._set_running(False)

    def _show_injection_results(self) -> None:
        specs_path = self.repository.data_dir / "injection_cases.json"
        try:
            specs = json.loads(specs_path.read_text(encoding="utf-8"))
            rows: list[str] = []
            passed = 0
            for case in specs["project_cases"]:
                raw = json.loads(
                    (self.repository.data_dir / case["project"]).read_text(encoding="utf-8")
                )
                mutated = apply_project_injection(raw, case["operation"])
                try:
                    parse_project(mutated)
                except BalanceError as exc:
                    ok = (
                        exc.code.value == case["expected_code"]
                        and exc.object_id == case["expected_object_id"]
                        and exc.field == case["expected_field"]
                    )
                    passed += int(ok)
                    rows.append(
                        f"{'PASS' if ok else 'FAIL'}  {case['id']:<31} "
                        f"{exc.code.value:<8} {exc.object_id}.{exc.field}"
                    )
                else:
                    rows.append(f"FAIL  {case['id']:<31} no error raised")
            runtime = specs.get("runtime_cases", [])
            runtime_text = "\n".join(
                f"TEST  {item['id']:<31} {item['expected_code']:<8} {item['operation']}"
                for item in runtime
            )
            text = (
                f"工程注入链路: {passed}/{len(specs['project_cases'])} 通过\n\n"
                + "\n".join(rows)
                + "\n\n运行时异常链路（由 pytest 覆盖）：\n"
                + runtime_text
            )
            self._show_text_dialog("异常链路自检", text)
        except Exception as exc:
            QMessageBox.critical(self, "异常自检失败", str(exc))

    def _show_error(self, error: BalanceError) -> None:
        detail = ""
        if error.detail:
            detail = "\n\n" + json.dumps(error.detail, ensure_ascii=False, indent=2)
        QMessageBox.critical(
            self,
            f"{error.code.value} · 诊断错误",
            f"{error}\n\n对象: {error.object_id or '—'}\n字段: {error.field or '—'}{detail}",
        )

    def _show_text_dialog(self, title: str, text: str) -> None:
        dialog = QDialog(self)
        dialog.setWindowTitle(title)
        dialog.resize(820, 620)
        layout = QVBoxLayout(dialog)
        browser = QTextBrowser()
        browser.setPlainText(text)
        browser.setStyleSheet("font-family: 'Cascadia Mono'; font-size: 11px;")
        layout.addWidget(browser)
        buttons = QDialogButtonBox(QDialogButtonBox.StandardButton.Close)
        buttons.rejected.connect(dialog.reject)
        layout.addWidget(buttons)
        dialog.exec()

    def _export_report(self) -> None:
        if not self.report:
            QMessageBox.information(self, "暂无报告", "请先完成一次验证。")
            return
        path, _ = QFileDialog.getSaveFileName(
            self,
            "导出平衡验证报告",
            str(ROOT / "artifacts" / f"{self.bundle.project.project_id}_report.json"),
            "JSON report (*.json);;All files (*)",
        )
        if not path:
            return
        json_path = Path(path)
        if json_path.suffix.lower() != ".json":
            json_path = json_path.with_suffix(".json")
        md_path = json_path.with_suffix(".md")
        json_path.parent.mkdir(parents=True, exist_ok=True)
        json_path.write_text(
            json.dumps(self.report.to_dict(), ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        md_path.write_text(self.report.to_markdown(), encoding="utf-8")
        self.status_label.setText(f"报告已导出 · {json_path.name} / {md_path.name}")

    def closeEvent(self, event) -> None:  # type: ignore[override]
        if self._busy():
            answer = QMessageBox.question(
                self,
                "验证仍在运行",
                "退出会请求暂停，并仅保留已经原子写入检查点的组合。\n"
                "强制终止时不会承诺生成完整报告。是否暂停并退出？",
                QMessageBox.StandardButton.Yes | QMessageBox.StandardButton.No,
                QMessageBox.StandardButton.No,
            )
            if answer != QMessageBox.StandardButton.Yes:
                event.ignore()
                return
            if self.worker:
                self.worker.pause_event.set()
                self.worker.wait(5000)
            if self.probe_worker:
                self.probe_worker.wait(3000)
            if self.three_worker:
                self.three_worker.wait(8000)
        event.accept()

    def showEvent(self, event) -> None:  # type: ignore[override]
        super().showEvent(event)


def create_application(argv: list[str] | None = None) -> QApplication:
    app = QApplication.instance() or QApplication(argv or [])
    app.setApplicationName("Battle Balance Lab")
    app.setOrganizationName("Local Balance Lab")
    app.setStyleSheet(app_stylesheet())
    return app


def create_main_window(data_dir: Path = DATA_DIR) -> MainWindow:
    return MainWindow(data_dir=data_dir)
