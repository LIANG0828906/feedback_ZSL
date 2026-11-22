from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path


def main() -> int:
    root = Path(__file__).resolve().parent
    local_deps = root / ".deps"
    if local_deps.exists():
        sys.path.insert(0, str(local_deps))
    sys.path.insert(0, str(root))
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    suite = unittest.defaultTestLoader.discover(str(root / "tests"))
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    raise SystemExit(main())
