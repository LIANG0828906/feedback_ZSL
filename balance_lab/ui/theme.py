from __future__ import annotations

import platform

from PySide6.QtGui import QColor, QFont


BG = "#09111f"
SURFACE = "#0f1b2d"
SURFACE_2 = "#14243a"
SURFACE_3 = "#1a2d46"
BORDER = "#263b57"
TEXT = "#e6eef9"
MUTED = "#8ea4c1"
CYAN = "#47d7ff"
TEAL = "#3ee0b0"
AMBER = "#ffbf69"
RED = "#ff6b7a"
GREEN = "#72e39a"
PURPLE = "#9b8cff"


UI_FONT = "Microsoft YaHei UI" if platform.system() == "Windows" else "Noto Sans CJK SC"
MONO_FONT = "Cascadia Mono" if platform.system() == "Windows" else "Ubuntu Mono"


def app_stylesheet() -> str:
    return f"""
    QWidget {{
        background: {BG};
        color: {TEXT};
        font-family: "Microsoft YaHei UI", "Noto Sans CJK SC", "Segoe UI", sans-serif;
        font-size: 13px;
    }}
    QMainWindow {{
        background: {BG};
    }}
    QFrame#Header {{
        background: {SURFACE};
        border-bottom: 1px solid {BORDER};
    }}
    QFrame#Sidebar {{
        background: {SURFACE};
        border-right: 1px solid {BORDER};
    }}
    QFrame#Panel {{
        background: {SURFACE};
        border: 1px solid {BORDER};
        border-radius: 8px;
    }}
    QFrame#KpiCard {{
        background: {SURFACE};
        border: 1px solid {BORDER};
        border-radius: 8px;
    }}
    QLabel#Brand {{
        color: {TEXT};
        font-size: 21px;
        font-weight: 700;
        letter-spacing: 1px;
    }}
    QLabel#Eyebrow {{
        color: {CYAN};
        font-size: 10px;
        font-weight: 700;
        letter-spacing: 2px;
    }}
    QLabel#Muted {{
        color: {MUTED};
    }}
    QLabel#SectionTitle {{
        color: {TEXT};
        font-size: 15px;
        font-weight: 650;
    }}
    QLabel#KpiValue {{
        color: {TEXT};
        font-size: 25px;
        font-weight: 700;
    }}
    QLabel#KpiLabel {{
        color: {MUTED};
        font-size: 11px;
    }}
    QLabel#Badge {{
        background: {SURFACE_3};
        border: 1px solid {BORDER};
        border-radius: 10px;
        padding: 3px 9px;
        color: {CYAN};
        font-size: 11px;
    }}
    QPushButton {{
        background: {SURFACE_2};
        border: 1px solid {BORDER};
        border-radius: 6px;
        padding: 7px 13px;
        color: {TEXT};
        font-weight: 550;
    }}
    QPushButton:hover {{
        background: {SURFACE_3};
        border-color: {CYAN};
    }}
    QPushButton:pressed {{
        background: #0b1627;
    }}
    QPushButton#Primary {{
        background: {CYAN};
        color: #06101c;
        border: none;
        font-weight: 700;
    }}
    QPushButton#Primary:hover {{
        background: #76e5ff;
    }}
    QPushButton#Danger {{
        color: {RED};
        border-color: #6d3341;
    }}
    QPushButton:disabled {{
        color: #5f7188;
        background: #111d2d;
        border-color: #1c2d43;
    }}
    QComboBox, QSpinBox, QDoubleSpinBox, QLineEdit {{
        background: {SURFACE_2};
        border: 1px solid {BORDER};
        border-radius: 6px;
        padding: 6px 8px;
        selection-background-color: {CYAN};
        selection-color: #06101c;
    }}
    QComboBox:hover, QSpinBox:hover {{
        border-color: #3a587d;
    }}
    QComboBox QAbstractItemView {{
        background: {SURFACE_2};
        border: 1px solid {BORDER};
        selection-background-color: {SURFACE_3};
        color: {TEXT};
        outline: none;
    }}
    QTabWidget::pane {{
        border: none;
        background: transparent;
    }}
    QTabBar::tab {{
        background: transparent;
        color: {MUTED};
        padding: 9px 16px;
        border-bottom: 2px solid transparent;
        font-weight: 600;
    }}
    QTabBar::tab:selected {{
        color: {TEXT};
        border-bottom-color: {CYAN};
    }}
    QTabBar::tab:hover {{
        color: {TEXT};
    }}
    QTableWidget, QTableView {{
        background: {SURFACE};
        alternate-background-color: #101e31;
        border: none;
        gridline-color: #1d3049;
        selection-background-color: #173653;
        selection-color: {TEXT};
        outline: none;
    }}
    QHeaderView::section {{
        background: {SURFACE_2};
        color: {MUTED};
        border: none;
        border-bottom: 1px solid {BORDER};
        padding: 7px;
        font-size: 11px;
        font-weight: 650;
    }}
    QTableCornerButton::section {{
        background: {SURFACE_2};
        border: none;
    }}
    QScrollBar:vertical {{
        background: transparent;
        width: 9px;
        margin: 2px;
    }}
    QScrollBar::handle:vertical {{
        background: #2d4969;
        min-height: 30px;
        border-radius: 4px;
    }}
    QScrollBar::handle:vertical:hover {{
        background: #3d628d;
    }}
    QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical {{
        height: 0;
    }}
    QScrollBar:horizontal {{
        background: transparent;
        height: 9px;
        margin: 2px;
    }}
    QScrollBar::handle:horizontal {{
        background: #2d4969;
        min-width: 30px;
        border-radius: 4px;
    }}
    QProgressBar {{
        background: #0b1727;
        border: 1px solid {BORDER};
        border-radius: 4px;
        text-align: center;
        color: {TEXT};
        font-size: 10px;
    }}
    QProgressBar::chunk {{
        background: {CYAN};
        border-radius: 3px;
    }}
    QTextBrowser, QTextEdit {{
        background: #0b1627;
        border: 1px solid {BORDER};
        border-radius: 6px;
        padding: 10px;
        color: {TEXT};
        selection-background-color: {CYAN};
    }}
    QSplitter::handle {{
        background: transparent;
        width: 6px;
        height: 6px;
    }}
    QToolTip {{
        background: {SURFACE_3};
        color: {TEXT};
        border: 1px solid {BORDER};
        padding: 5px;
    }}
    QStatusBar {{
        background: {SURFACE};
        border-top: 1px solid {BORDER};
        color: {MUTED};
    }}
    QMenu {{
        background: {SURFACE_2};
        border: 1px solid {BORDER};
        padding: 5px;
    }}
    QMenu::item {{
        padding: 7px 24px 7px 12px;
        border-radius: 4px;
    }}
    QMenu::item:selected {{
        background: {SURFACE_3};
    }}
    """


def pill_style(color: str) -> str:
    return (
        f"background: {color}22; color: {color}; border: 1px solid {color}66; "
        "border-radius: 9px; padding: 2px 7px; font-size: 10px; font-weight: 700;"
    )


def color_for_state(state: str) -> QColor:
    if state == "SUBMITTED":
        return QColor(GREEN)
    if state == "EVALUATING":
        return QColor(CYAN)
    if state == "FAILED":
        return QColor(RED)
    return QColor(MUTED)


def mono_font(size: int = 10, weight: QFont.Weight = QFont.Weight.Normal) -> QFont:
    font = QFont(MONO_FONT, size)
    font.setStyleHint(QFont.StyleHint.Monospace)
    font.setWeight(weight)
    return font
