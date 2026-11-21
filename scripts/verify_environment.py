#!/usr/bin/env python3
"""Check the local Python/NumPy/PySide6 runtime used by this workspace."""

from __future__ import annotations

import json
import platform
import sys


def main() -> int:
    payload = {
        "python": platform.python_version(),
        "python_ok": sys.version_info >= (3, 11),
    }
    try:
        import numpy

        payload["numpy"] = numpy.__version__
    except ImportError:
        payload["numpy"] = None
    try:
        import PySide6

        payload["pyside6"] = PySide6.__version__
    except ImportError:
        payload["pyside6"] = None
    payload["ok"] = bool(
        payload["python_ok"] and payload["numpy"] and payload["pyside6"]
    )
    print(json.dumps(payload, indent=2))
    return 0 if payload["ok"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
