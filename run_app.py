from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


def main() -> int:
    parser = argparse.ArgumentParser(description="Battle Balance Lab desktop application")
    parser.add_argument("--data-dir", default=str(ROOT / "data" / "samples"))
    parser.add_argument("--screenshot", help="render one deterministic demo frame to this PNG")
    parser.add_argument(
        "--offscreen",
        action="store_true",
        help="force Qt offscreen mode (also available through QT_QPA_PLATFORM)",
    )
    args = parser.parse_args()
    if args.offscreen:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

    from PySide6.QtCore import QTimer

    from balance_lab.scheduler import run_validation
    from balance_lab.ui.app import create_application, create_main_window

    app = create_application(sys.argv[:1])
    window = create_main_window(Path(args.data_dir))
    window.show()
    app.processEvents()

    if args.screenshot:
        if window.bundle is not None:
            result = run_validation(
                window.bundle.project,
                window.bundle.candidate_set,
                window.bundle.request,
                mode="full",
            )
            window._on_result(result)
            app.processEvents()
        path = Path(args.screenshot)
        path.parent.mkdir(parents=True, exist_ok=True)
        window.grab().save(str(path))
        QTimer.singleShot(100, app.quit)
        app.exec()
        print(str(path))
        return 0
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())

