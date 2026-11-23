from __future__ import annotations

import os
import sys
from pathlib import Path


def _prepend_offline_dependencies() -> None:
    root = Path(__file__).resolve().parent.parent
    configured = os.environ.get("PANOWORKBENCH_DEPS")
    candidates = [Path(configured).expanduser()] if configured else []
    candidates.append(root / ".deps")
    for candidate in candidates:
        if candidate.is_dir() and str(candidate) not in sys.path:
            sys.path.insert(0, str(candidate))
            return


_prepend_offline_dependencies()

from .cli import main


if __name__ == "__main__":
    raise SystemExit(main())
