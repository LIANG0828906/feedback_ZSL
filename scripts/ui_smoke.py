#!/usr/bin/env python3
"""Headless end-to-end UI smoke test with screenshot evidence."""

from __future__ import annotations

import json
import os
import shutil
import sys
import time
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication  # noqa: E402

from audio_repair.app import MainWindow  # noqa: E402
from audio_repair.engine import RunStatus  # noqa: E402
from audio_repair.segments import SegmentStatus  # noqa: E402


def main() -> int:
    root = Path.cwd()
    sample = root / "artifacts" / "samples" / "music_noisy_stereo.wav"
    checkpoint_root = root / "artifacts" / "ui_checkpoints"
    if checkpoint_root.exists():
        shutil.rmtree(checkpoint_root)
    app = QApplication([])
    window = MainWindow()
    window.show()
    app.processEvents()
    loaded = window.load_audio_path(sample)
    if not loaded:
        print(json.dumps({"ok": False, "reason": "fixture load failed"}))
        return 1
    app.processEvents()
    rows = window.table.rowCount()
    window._start_processing()
    deadline = time.monotonic() + 60
    while window.worker is not None and time.monotonic() < deadline:
        app.processEvents()
        time.sleep(0.01)
    app.processEvents()
    if window.worker is not None:
        print(json.dumps({"ok": False, "reason": "worker timeout"}))
        window.session.request_stop()
        window.worker.wait(5000)
        return 1
    committed, total = window.session.state_machine.progress()
    records = window.session.state_snapshot()
    result = window.session.result
    window._select_row(min(1, total - 1))
    app.processEvents()
    window._preview_current()
    app.processEvents()
    screenshot = root / "artifacts" / "ui_smoke.png"
    window.grab().save(str(screenshot))
    payload = {
        "ok": bool(
            rows == total
            and committed == total
            and all(
                record.status == SegmentStatus.COMMITTED
                for record in records.values()
            )
            and result is not None
            and result.status == RunStatus.COMPLETED
            and screenshot.exists()
        ),
        "rows": rows,
        "committed": committed,
        "total": total,
        "result": result.to_dict() if result is not None else None,
        "screenshot": str(screenshot),
        "screenshot_bytes": screenshot.stat().st_size if screenshot.exists() else 0,
        "preview_segment": window.preview_index,
        "preview_samples": (
            int(window.preview_output.shape[0])
            if window.preview_output is not None
            else 0
        ),
        "audit_tail": window.log.toPlainText().splitlines()[-6:],
    }
    report = root / "artifacts" / "ui_smoke.json"
    report.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps(payload, indent=2, ensure_ascii=False))
    window.close()
    app.processEvents()
    return 0 if payload["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

