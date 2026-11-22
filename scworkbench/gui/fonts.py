from __future__ import annotations

from pathlib import Path

from PySide6.QtGui import QFont, QFontDatabase
from PySide6.QtWidgets import QApplication


def register_bundled_fonts() -> list[str]:
    root = Path(__file__).resolve().parents[1]
    font_path = root / "assets" / "fonts" / "NotoSansCJKsc-Regular.otf"
    if not font_path.exists():
        return []
    font_id = QFontDatabase.addApplicationFont(str(font_path))
    if font_id < 0:
        return []
    return QFontDatabase.applicationFontFamilies(font_id)


def apply_application_font(app: QApplication) -> None:
    families = register_bundled_fonts()
    if families:
        family = families[0]
        font = QFont(family, 9)
        app.setFont(font)
        app.setProperty("uiFontFamily", family)
