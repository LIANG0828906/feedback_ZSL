from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from mediaforge.selfcheck import run_selfcheck


if __name__ == "__main__":
    report = run_selfcheck("samples", checkpoint_root="artifacts/checkpoints")
    print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
    raise SystemExit(0 if report["ok"] else 1)
