#!/usr/bin/env python3
"""Capture reproducible UI evidence for the incremental dashboard."""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

os.environ["QT_QPA_PLATFORM"] = "offscreen"

from PySide6.QtWidgets import QApplication

from gamepad_studio.ui.main_window import MainWindow
from gamepad_studio.ui.theme import apply_theme


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    artifacts = root / "artifacts"
    artifacts.mkdir(parents=True, exist_ok=True)
    app = QApplication([])
    apply_theme(app)
    window = MainWindow(
        samples_root=root / "samples",
        initial_sample=root / "samples" / "dual_stick",
        snapshot_root=artifacts / "snapshots" / "ui-demo",
        async_initial_load=False,
    )
    window._suppress_dialogs = True
    window.resize(1600, 1010)
    window.show()
    app.processEvents()
    update = window.session.update_axis(
        "left_x",
        deadzone=0.20,
        curve_exponent=1.80,
        verify=True,
    )
    window._on_update_complete(update)
    app.processEvents()
    window.tabs.setCurrentIndex(0)
    app.processEvents()
    captured = {
        "incremental": window.capture(artifacts / "ui_incremental.png"),
        "dependency": False,
        "heatmap": False,
    }
    window.tabs.setCurrentIndex(1)
    app.processEvents()
    captured["dependency"] = window.capture(artifacts / "ui_dependency.png")
    window.tabs.setCurrentIndex(2)
    app.processEvents()
    captured["heatmap"] = window.capture(artifacts / "ui_diff_heatmap.png")
    payload = {
        "captured": captured,
        "cache_badge": window.cache_badge.text(),
        "equality_heatmap_badge": window.equality_heatmap.badge,
        "recomputed": update.run.recomputed_count,
        "reused": update.run.reused_count,
        "verification_equal": update.verification.equal if update.verification else None,
        "screenshots": {
            key: str((artifacts / f"ui_{key}.png").resolve())
            for key in ("incremental", "dependency", "diff_heatmap")
        },
    }
    print(json.dumps(payload, ensure_ascii=False, indent=2))
    window.close()
    app.processEvents()
    return 0 if all(captured.values()) else 1


if __name__ == "__main__":
    raise SystemExit(main())
