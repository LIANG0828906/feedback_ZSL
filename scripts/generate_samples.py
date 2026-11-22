from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from scworkbench.samplegen import write_sample_set  # noqa: E402


if __name__ == "__main__":
    output = write_sample_set(ROOT / "samples")
    print(json.dumps(output, ensure_ascii=False, indent=2))
