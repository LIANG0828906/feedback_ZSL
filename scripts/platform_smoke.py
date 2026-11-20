#!/usr/bin/env python3
"""Cross-platform PyQt5/QImage and renderer smoke test."""

from __future__ import annotations

import json
import os
import platform
import sys
from pathlib import Path

import numpy as np
from PyQt5.QtCore import QT_VERSION_STR
from PyQt5.QtGui import QImage
from PyQt5.QtWidgets import QApplication, QWidget

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender import TransferFunction, VolumeData, VolumeRenderer  # noqa: E402


def main() -> int:
    if platform.system() == "Windows":
        os.environ.pop("QT_QPA_PLATFORM", None)
    app = QApplication.instance() or QApplication([])
    widget = QWidget()
    widget.resize(320, 240)
    widget.show()
    app.processEvents()
    image = QImage(48, 48, QImage.Format_RGBA8888_Premultiplied)
    image.fill(0)
    image_ok = not image.isNull() and image.width() == 48 and image.height() == 48
    widget_ok = widget.isVisible()

    cases = json.loads((ROOT / "cases" / "cases.json").read_text(encoding="utf-8"))
    default_points = next(
        case["points"]
        for case in cases["transfer_functions"]
        if case["name"] == "medical_default"
    )
    view = np.asarray(cases["views"][0]["matrix"], dtype=np.float64)
    volume = VolumeData.from_file(ROOT / "samples" / "density_64.vol")
    transfer = TransferFunction(default_points)
    renders = {}
    for mode in ("float", "fixed"):
        renderer = VolumeRenderer(
            volume=volume,
            transfer=transfer,
            mode=mode,
            view_matrix=view,
            step=1.0,
            image_size=(48, 48),
            error_threshold=1.0e-3,
        )
        result = renderer.render()
        renders[mode] = {
            "content_hash": result.content_hash,
            "state_hash": result.state_hash,
            "max_error": result.error_summary.max_error,
            "untrusted_pixels": result.error_summary.untrusted_pixels,
            "mean_rgb": float(np.mean(result.linear_rgb)),
        }
    report = {
        "python": platform.python_version(),
        "python_executable": sys.executable,
        "numpy": np.__version__,
        "PyQt5_Qt": QT_VERSION_STR,
        "platform": platform.platform(),
        "widget_created": bool(widget_ok),
        "qimage_constructed": bool(image_ok),
        "renders": renders,
    }
    widget.close()
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if widget_ok and image_ok else 4


if __name__ == "__main__":
    raise SystemExit(main())
