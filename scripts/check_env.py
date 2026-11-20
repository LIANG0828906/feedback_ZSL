"""Print the interpreter and pinned dependency versions as JSON."""

from __future__ import annotations

import json
import platform
import sys


def main() -> int:
    payload = {
        "implementation": platform.python_implementation(),
        "platform": platform.system(),
        "python": platform.python_version(),
    }
    try:
        import numpy
        import PySide6
    except ImportError as error:
        payload["ok"] = False
        payload["error"] = str(error)
        print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
        return 1
    payload.update(
        {
            "numpy": numpy.__version__,
            "ok": True,
            "pyside6": PySide6.__version__,
        }
    )
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
