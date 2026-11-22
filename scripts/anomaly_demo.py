from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from mediaforge.errors import WorkbenchError
from mediaforge.injection import InjectionConfig
from mediaforge.pipeline import load_sample


def main() -> int:
    sample = ROOT / "samples" / "g01_neon_city"
    results = []
    for name in ("conflict", "order", "missing_track", "corrupt_segment"):
        config = InjectionConfig.from_file(ROOT / "samples" / "injections" / f"{name}.json")
        try:
            load_sample(sample, "v3", injection=config)
            results.append({"name": name, "ok": False, "error": "unexpected success"})
        except WorkbenchError as exc:
            results.append({"name": name, "ok": True, "error": exc.to_dict()})
    print(json.dumps(results, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if all(item["ok"] for item in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
