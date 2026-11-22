"""Application theme for the SpectraForge workbench."""

APP_QSS = """
* {
    color: #dce7f3;
    font-family: "Segoe UI", "Noto Sans", "DejaVu Sans";
    font-size: 12px;
}
QMainWindow, QWidget#appRoot {
    background: #0b0f14;
}
QFrame#header {
    background: #101720;
    border-bottom: 1px solid #213040;
}
QLabel#brand {
    color: #f5fbff;
    font-size: 24px;
    font-weight: 700;
    letter-spacing: 3px;
}
QLabel#brandAccent {
    color: #26d0ce;
    font-size: 12px;
    font-weight: 600;
    letter-spacing: 2px;
}
QLabel#muted, QLabel.muted {
    color: #7f91a6;
}
QLabel#sectionTitle {
    color: #9fb0c5;
    font-size: 11px;
    font-weight: 700;
    letter-spacing: 1px;
}
QFrame#panel {
    background: #111821;
    border: 1px solid #203040;
    border-radius: 7px;
}
QFrame#card {
    background: #0e151d;
    border: 1px solid #1e2c3a;
    border-radius: 6px;
}
QScrollArea {
    border: none;
    background: transparent;
}
QScrollBar:vertical {
    background: #0c1219;
    width: 9px;
    margin: 0;
}
QScrollBar::handle:vertical {
    background: #31465a;
    min-height: 26px;
    border-radius: 4px;
}
QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical,
QScrollBar::add-page:horizontal, QScrollBar::sub-page:horizontal {
    height: 0;
    width: 0;
}
QComboBox, QSpinBox, QDoubleSpinBox, QLineEdit {
    background: #0b1219;
    border: 1px solid #27384a;
    border-radius: 5px;
    padding: 5px 8px;
    min-height: 22px;
    selection-background-color: #1b5e62;
}
QComboBox:hover, QSpinBox:hover, QDoubleSpinBox:hover, QLineEdit:hover {
    border-color: #3b596f;
}
QComboBox::drop-down {
    border: none;
    width: 20px;
}
QComboBox QAbstractItemView {
    background: #101923;
    border: 1px solid #2b3f52;
    selection-background-color: #17484d;
    outline: none;
}
QPushButton {
    background: #172431;
    border: 1px solid #2b4053;
    border-radius: 5px;
    padding: 7px 12px;
    font-weight: 600;
}
QPushButton:hover {
    background: #1d3040;
    border-color: #3d5d73;
}
QPushButton:pressed {
    background: #101a24;
}
QPushButton:disabled {
    color: #566675;
    background: #10161d;
    border-color: #1a2733;
}
QPushButton#primary {
    background: #1b8b88;
    border-color: #2bd6d1;
    color: #021113;
}
QPushButton#primary:hover {
    background: #27b6b1;
}
QPushButton#danger {
    background: #4c2228;
    border-color: #8f3944;
    color: #ffd9de;
}
QPushButton#accent {
    background: #4b3820;
    border-color: #a97c37;
    color: #ffd795;
}
QCheckBox {
    spacing: 7px;
}
QCheckBox::indicator {
    width: 14px;
    height: 14px;
    border-radius: 3px;
    border: 1px solid #365066;
    background: #0b1219;
}
QCheckBox::indicator:checked {
    background: #26d0ce;
    border-color: #6ef3ef;
}
QSlider::groove:horizontal {
    background: #1a2733;
    height: 4px;
    border-radius: 2px;
}
QSlider::sub-page:horizontal {
    background: #26d0ce;
    border-radius: 2px;
}
QSlider::handle:horizontal {
    background: #d9ffff;
    border: 1px solid #26d0ce;
    width: 13px;
    margin: -5px 0;
    border-radius: 7px;
}
QTableWidget {
    background: #0c1219;
    alternate-background-color: #0f1720;
    border: 1px solid #1f2e3c;
    gridline-color: #182532;
    selection-background-color: #153d42;
    selection-color: #e7ffff;
}
QHeaderView::section {
    background: #131d27;
    color: #8fa2b6;
    border: none;
    border-right: 1px solid #1d2c3a;
    border-bottom: 1px solid #26394a;
    padding: 6px;
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 1px;
}
QTabWidget::pane {
    border: 1px solid #1f2f3e;
    background: #0d141c;
    border-radius: 5px;
}
QTabBar::tab {
    background: #0e151d;
    color: #7e91a6;
    padding: 8px 14px;
    border: none;
    border-bottom: 2px solid transparent;
    font-weight: 600;
}
QTabBar::tab:selected {
    color: #dffdfc;
    background: #112028;
    border-bottom-color: #26d0ce;
}
QTabBar::tab:hover {
    color: #c4d3e3;
}
QProgressBar {
    background: #0b1118;
    border: 1px solid #243545;
    border-radius: 4px;
    text-align: center;
    color: #dffdfc;
    font-size: 10px;
    min-height: 16px;
}
QProgressBar::chunk {
    background: #26d0ce;
    border-radius: 3px;
}
QToolTip {
    background: #17232f;
    color: #e0edf9;
    border: 1px solid #37546a;
    padding: 5px;
}
QStatusBar {
    background: #0d141c;
    border-top: 1px solid #1c2a37;
}
QMessageBox {
    background: #111a23;
}
"""

STATE_COLORS = {
    "pending": "#76889b",
    "processing": "#ffb454",
    "committed": "#36d6a0",
    "failed": "#ff667a",
}
