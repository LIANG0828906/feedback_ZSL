from __future__ import annotations

APP_STYLE = """
* {
    font-family: "Noto Sans CJK SC", "Segoe UI", "Microsoft YaHei UI", sans-serif;
    color: #e7f0fb;
}
QMainWindow, QWidget#AppRoot {
    background: #07111f;
}
QWidget#TopBar {
    background: #091728;
    border-bottom: 1px solid #193149;
}
QLabel#BrandMark {
    background: #49d5ff;
    color: #03101b;
    border-radius: 10px;
    font-size: 17px;
    font-weight: 800;
}
QLabel#Title {
    font-size: 19px;
    font-weight: 700;
    letter-spacing: 1px;
}
QLabel#Subtitle, QLabel#Muted, QLabel.muted {
    color: #7f97af;
    font-size: 11px;
}
QLabel#SectionTitle {
    font-size: 11px;
    font-weight: 700;
    color: #9bb3ca;
    letter-spacing: 1px;
}
QLabel#BigValue {
    font-size: 24px;
    font-weight: 700;
    color: #f2f8ff;
}
QLabel#MetricValue {
    font-size: 18px;
    font-weight: 700;
    color: #f2f8ff;
}
QLabel#MetricCaption {
    font-size: 10px;
    color: #7289a1;
    letter-spacing: 0.5px;
}
QLabel#PhasePill {
    background: #12354a;
    border: 1px solid #2b7691;
    border-radius: 11px;
    color: #7de5ff;
    font-size: 11px;
    font-weight: 700;
    padding: 4px 12px;
}
QLabel#Chip {
    background: #0d2236;
    border: 1px solid #1c3b55;
    border-radius: 9px;
    color: #9cc3df;
    font-size: 10px;
    padding: 3px 8px;
}
QFrame#Card, QFrame#MetricCard {
    background: #0b192a;
    border: 1px solid #19324b;
    border-radius: 12px;
}
QFrame#Card QLabel {
    background: transparent;
}
QPushButton {
    background: #10243a;
    border: 1px solid #244562;
    border-radius: 8px;
    padding: 7px 13px;
    font-size: 11px;
    font-weight: 600;
}
QPushButton:hover {
    background: #173551;
    border-color: #3b7194;
}
QPushButton:pressed {
    background: #0d1d2f;
}
QPushButton:disabled {
    color: #4c6278;
    background: #0b1725;
    border-color: #172a3d;
}
QPushButton#Primary {
    background: #28b9e6;
    border: 1px solid #63dfff;
    color: #04131e;
}
QPushButton#Primary:hover {
    background: #49d5ff;
}
QPushButton#Danger {
    background: #2a1422;
    border-color: #6f2947;
    color: #ff8db0;
}
QComboBox, QSpinBox {
    background: #0b1b2c;
    border: 1px solid #23435e;
    border-radius: 7px;
    padding: 5px 9px;
    min-height: 18px;
    font-size: 11px;
}
QComboBox:hover, QSpinBox:hover {
    border-color: #357398;
}
QComboBox QAbstractItemView {
    background: #0c1c2d;
    border: 1px solid #2a526e;
    selection-background-color: #154360;
    outline: 0;
}
QProgressBar {
    background: #081321;
    border: 1px solid #183148;
    border-radius: 4px;
    height: 7px;
    text-align: center;
    color: transparent;
}
QProgressBar::chunk {
    background: #39c9f5;
    border-radius: 3px;
}
QProgressBar#InputProgress::chunk {
    background: #f2b34c;
}
QTabWidget::pane {
    border: 1px solid #19324b;
    border-radius: 9px;
    background: #091624;
    top: -1px;
}
QTabBar::tab {
    background: transparent;
    color: #7189a0;
    padding: 8px 14px;
    border: 0;
    font-size: 11px;
    font-weight: 600;
}
QTabBar::tab:selected {
    color: #69dcff;
    border-bottom: 2px solid #49d5ff;
}
QTableWidget {
    background: #081421;
    alternate-background-color: #0a1828;
    border: 0;
    gridline-color: #13273b;
    selection-background-color: #12324a;
    font-size: 10px;
}
QHeaderView::section {
    background: #0d1d2e;
    color: #7690a8;
    border: 0;
    border-bottom: 1px solid #1a334a;
    padding: 5px 7px;
    font-size: 9px;
    font-weight: 700;
}
QPlainTextEdit {
    background: #06111d;
    border: 0;
    color: #9bb7cc;
    font-family: "Cascadia Code", "Consolas", monospace;
    font-size: 9px;
    selection-background-color: #1b4967;
}
QScrollBar:vertical {
    background: transparent;
    width: 8px;
    margin: 0;
}
QScrollBar::handle:vertical {
    background: #24435d;
    border-radius: 4px;
    min-height: 30px;
}
QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical {
    height: 0;
}
QSplitter::handle {
    background: #07111f;
    width: 7px;
}
QStatusBar {
    background: #07111f;
    border-top: 1px solid #12253a;
    color: #6d879e;
    font-size: 10px;
}
QToolTip {
    background: #0e2539;
    color: #d9efff;
    border: 1px solid #2b5b7a;
    padding: 5px;
}
"""

PHASE_COLORS = {
    "negotiating": "#f2b34c",
    "mirroring": "#49d5ff",
    "paused": "#a281ff",
    "reconnecting": "#ff9e5c",
    "disconnected": "#ff567f",
}

STATUS_COLORS = {
    "committed": "#49d5ff",
    "missing": "#ff567f",
    "failed": "#ff9e5c",
    "rejected": "#8a6fb8",
    "replayed": "#49d5ff",
    "duplicate": "#f2b34c",
    "ok": "#284861",
    "retransmitted": "#f2b34c",
    "lost": "#4b2637",
    "corrupt": "#ff567f",
    "session-rejected": "#a281ff",
    "pending": "#16283a",
}
