"""PySide6 desktop application entry point."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

if "--offscreen" in sys.argv or "--smoke-test" in sys.argv:
    os.environ["QT_QPA_PLATFORM"] = "offscreen"

from PySide6.QtCore import QTimer
from PySide6.QtWidgets import QApplication

from .ui.main_window import MainWindow, prepare_samples_if_needed
from .ui.theme import apply_theme


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="PadWeave Studio desktop application")
    parser.add_argument("--samples", default="samples", help="deterministic sample root")
    parser.add_argument("--sample", default=None, help="sample directory or device id")
    parser.add_argument(
        "--snapshot-root",
        default="artifacts/snapshots",
        help="per-device snapshot directory",
    )
    parser.add_argument("--offscreen", action="store_true", help="use Qt offscreen platform")
    parser.add_argument("--smoke-test", action="store_true", help="load, render, and exit")
    parser.add_argument(
        "--screenshot",
        default="artifacts/ui_dashboard.png",
        help="smoke-test screenshot path",
    )
    parser.add_argument(
        "--auto-close",
        type=int,
        default=0,
        help="close after N milliseconds (0 keeps the window open)",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    prepare_samples_if_needed(args.samples)
    app = QApplication(sys.argv[:1])
    app.setApplicationName("PadWeave Studio")
    apply_theme(app)
    window = MainWindow(
        samples_root=args.samples,
        initial_sample=args.sample,
        snapshot_root=args.snapshot_root,
        async_initial_load=not args.smoke_test,
    )
    window.resize(1600, 1010)
    window.show()
    if args.smoke_test:
        window._suppress_dialogs = True
        if window.session is None:
            window.load_sample_sync(args.sample)
        app.processEvents()

        def finish_smoke() -> None:
            captured = window.capture(args.screenshot)
            payload = {
                "captured": captured,
                "screenshot": str(Path(args.screenshot).resolve()),
                "sample": window.session.bundle.device.id if window.session else None,
                "frames": len(window.last_run.result.frames) if window.last_run else 0,
                "truth_badge": window.truth_badge.text(),
                "cache_badge": window.cache_badge.text(),
                "snapshot_badge": window.snapshot_badge.text(),
                "event_buffer_limit": window.event_buffer_limit,
                "worker_threads": window.thread_pool.maxThreadCount(),
            }
            print(json.dumps(payload, ensure_ascii=False))
            window.close()
            app.quit()

        QTimer.singleShot(350, finish_smoke)
    elif args.auto_close > 0:
        QTimer.singleShot(args.auto_close, window.close)
        QTimer.singleShot(args.auto_close + 100, app.quit)
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())

