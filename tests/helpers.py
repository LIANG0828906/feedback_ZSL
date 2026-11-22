from __future__ import annotations

import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


def sample_path(version: int) -> Path:
    return SAMPLES / f"scene_vpfc{version}.json"


def load_sample_document(version: int) -> dict:
    return json.loads(sample_path(version).read_text(encoding="utf-8"))


def write_json(path: Path, value: dict) -> None:
    path.write_text(
        json.dumps(value, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
