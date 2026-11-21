"""要求 8：诊断界面在 offscreen 环境可实例化并响应核心动作。"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

pytest.importorskip("PySide6")

from PySide6 import QtWidgets  # noqa: E402

from mediaaudit.gui import AuditMainWindow, build_demo_engine  # noqa: E402
from mediaaudit.parameters import TierParams  # noqa: E402


@pytest.fixture(scope="module")
def qapp() -> QtWidgets.QApplication:
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    return app


def test_window_builds_and_refreshes(qapp, tmp_path: Path) -> None:
    engine = build_demo_engine(tmp_path / "work", "mono_16k")
    engine.submit_params("tier_low", TierParams(16000, 256, 1000, 500_000))
    engine.recompute("tier_low")
    window = AuditMainWindow(engine)
    window.refresh()
    assert window.timeline.count() >= 1
    assert window.windowTitle()


def test_submit_and_span_check_actions(qapp, tmp_path: Path) -> None:
    engine = build_demo_engine(tmp_path / "work", "mono_16k")
    window = AuditMainWindow(engine)
    engine.submit_params("tier_high", TierParams(48000, 256, 750, 375_000))
    engine.recompute("tier_high")
    window.on_submit()
    window.on_span_check()
    text = window.alerts.toPlainText()
    assert "E_SPAN_MISMATCH" in text or "一致" in text


def test_cancel_without_running_job(qapp, tmp_path: Path) -> None:
    engine = build_demo_engine(tmp_path / "work", "mono_16k")
    window = AuditMainWindow(engine)
    window.on_cancel()
    assert "没有运行中的任务" in window.alerts.toPlainText()
