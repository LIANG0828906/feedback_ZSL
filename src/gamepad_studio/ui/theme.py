"""Dark industrial desktop theme."""

from __future__ import annotations

from PySide6.QtGui import QColor, QPalette
from PySide6.QtWidgets import QApplication

COLORS = {
    "background": "#0d1319",
    "panel": "#141d25",
    "panel_alt": "#192630",
    "border": "#2a3b47",
    "text": "#e7f0f3",
    "muted": "#8ca2ad",
    "accent": "#35d2b2",
    "accent_dim": "#1f806f",
    "orange": "#ffb454",
    "red": "#ff6f79",
    "blue": "#65b7ff",
    "purple": "#b79cff",
    "green": "#71d78a",
    "grid": "#22323d",
}


def apply_theme(app: QApplication) -> None:
    palette = QPalette()
    palette.setColor(QPalette.ColorRole.Window, QColor(COLORS["background"]))
    palette.setColor(QPalette.ColorRole.WindowText, QColor(COLORS["text"]))
    palette.setColor(QPalette.ColorRole.Base, QColor(COLORS["panel"]))
    palette.setColor(QPalette.ColorRole.AlternateBase, QColor(COLORS["panel_alt"]))
    palette.setColor(QPalette.ColorRole.ToolTipBase, QColor(COLORS["panel_alt"]))
    palette.setColor(QPalette.ColorRole.ToolTipText, QColor(COLORS["text"]))
    palette.setColor(QPalette.ColorRole.Text, QColor(COLORS["text"]))
    palette.setColor(QPalette.ColorRole.Button, QColor(COLORS["panel_alt"]))
    palette.setColor(QPalette.ColorRole.ButtonText, QColor(COLORS["text"]))
    palette.setColor(QPalette.ColorRole.BrightText, QColor(COLORS["red"]))
    palette.setColor(QPalette.ColorRole.Highlight, QColor(COLORS["accent_dim"]))
    palette.setColor(QPalette.ColorRole.HighlightedText, QColor(COLORS["text"]))
    app.setPalette(palette)
    app.setStyleSheet(
        f"""
        * {{
            font-family: "Inter", "Segoe UI", "Noto Sans", sans-serif;
            font-size: 12px;
            color: {COLORS["text"]};
        }}
        QMainWindow, QDialog {{
            background: {COLORS["background"]};
        }}
        QFrame#Header {{
            background: {COLORS["panel"]};
            border: 1px solid {COLORS["border"]};
            border-radius: 10px;
        }}
        QLabel#AppTitle {{
            font-size: 20px;
            font-weight: 700;
            letter-spacing: 0.5px;
            color: {COLORS["text"]};
        }}
        QLabel#AppSubtitle {{
            color: {COLORS["muted"]};
            font-size: 11px;
        }}
        QLabel#SectionTitle {{
            color: {COLORS["accent"]};
            font-weight: 600;
            text-transform: uppercase;
        }}
        QLabel#Badge {{
            background: {COLORS["panel_alt"]};
            border: 1px solid {COLORS["border"]};
            border-radius: 7px;
            padding: 3px 8px;
            color: {COLORS["muted"]};
        }}
        QLabel#BadgeOk {{
            background: #143328;
            border: 1px solid #2b7d62;
            border-radius: 7px;
            padding: 3px 8px;
            color: {COLORS["green"]};
            font-weight: 600;
        }}
        QLabel#BadgeWarn {{
            background: #3c2d18;
            border: 1px solid #8d632d;
            border-radius: 7px;
            padding: 3px 8px;
            color: {COLORS["orange"]};
            font-weight: 600;
        }}
        QGroupBox {{
            background: {COLORS["panel"]};
            border: 1px solid {COLORS["border"]};
            border-radius: 10px;
            margin-top: 14px;
            padding: 10px 8px 8px 8px;
            font-weight: 600;
        }}
        QGroupBox::title {{
            subcontrol-origin: margin;
            subcontrol-position: top left;
            left: 12px;
            padding: 2px 7px;
            color: {COLORS["accent"]};
            background: {COLORS["panel"]};
        }}
        QPushButton {{
            background: {COLORS["panel_alt"]};
            border: 1px solid {COLORS["border"]};
            border-radius: 7px;
            padding: 6px 12px;
            font-weight: 600;
        }}
        QPushButton:hover {{
            border-color: {COLORS["accent_dim"]};
            background: #21313c;
        }}
        QPushButton:pressed {{
            background: #0f171d;
        }}
        QPushButton#Primary {{
            background: {COLORS["accent_dim"]};
            border: 1px solid {COLORS["accent"]};
            color: white;
        }}
        QPushButton#Primary:hover {{
            background: #28977f;
        }}
        QPushButton#Danger {{
            background: #3c1d22;
            border-color: #82404a;
            color: #ffbcc0;
        }}
        QPushButton:disabled {{
            color: #596a73;
            background: #131a20;
            border-color: #233039;
        }}
        QComboBox, QSpinBox, QDoubleSpinBox, QLineEdit {{
            background: #0f171d;
            border: 1px solid {COLORS["border"]};
            border-radius: 6px;
            padding: 5px 7px;
            selection-background-color: {COLORS["accent_dim"]};
        }}
        QComboBox::drop-down {{
            border: none;
            width: 18px;
        }}
        QComboBox QAbstractItemView {{
            background: {COLORS["panel_alt"]};
            border: 1px solid {COLORS["border"]};
            selection-background-color: {COLORS["accent_dim"]};
        }}
        QTableWidget {{
            background: #101920;
            alternate-background-color: #132129;
            border: 1px solid {COLORS["border"]};
            border-radius: 7px;
            gridline-color: {COLORS["grid"]};
            selection-background-color: #1c4d42;
        }}
        QHeaderView::section {{
            background: #17242d;
            color: {COLORS["muted"]};
            border: none;
            border-bottom: 1px solid {COLORS["border"]};
            padding: 6px;
            font-weight: 600;
        }}
        QTabWidget::pane {{
            background: {COLORS["panel"]};
            border: 1px solid {COLORS["border"]};
            border-radius: 8px;
            top: -1px;
        }}
        QTabBar::tab {{
            background: #101920;
            color: {COLORS["muted"]};
            padding: 7px 16px;
            border: 1px solid {COLORS["border"]};
            border-bottom: none;
            border-top-left-radius: 7px;
            border-top-right-radius: 7px;
            margin-right: 2px;
        }}
        QTabBar::tab:selected {{
            background: {COLORS["panel"]};
            color: {COLORS["accent"]};
        }}
        QSplitter::handle {{
            background: {COLORS["background"]};
            width: 5px;
            height: 5px;
        }}
        QScrollBar:vertical {{
            background: #0f171d;
            width: 9px;
            margin: 0;
        }}
        QScrollBar::handle:vertical {{
            background: #334752;
            border-radius: 4px;
            min-height: 24px;
        }}
        QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical {{
            height: 0;
        }}
        QStatusBar {{
            background: {COLORS["panel"]};
            border-top: 1px solid {COLORS["border"]};
            color: {COLORS["muted"]};
        }}
        QToolTip {{
            background: {COLORS["panel_alt"]};
            border: 1px solid {COLORS["accent_dim"]};
            color: {COLORS["text"]};
        }}
        """
    )


__all__ = ["COLORS", "apply_theme"]

