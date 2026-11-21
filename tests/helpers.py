from __future__ import annotations

import json
from pathlib import Path

from motion_workbench.bvh import parse_bvh_text
from motion_workbench.generator import (
    SampleSpec,
    generate_bvh_text,
    generate_constraints_dict,
)
from motion_workbench.hashing import sha256_text


def write_sample(directory: str | Path, spec: SampleSpec) -> tuple[Path, Path]:
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    bvh_text = generate_bvh_text(spec)
    bvh = parse_bvh_text(bvh_text, source_hash=sha256_text(bvh_text))
    constraints = generate_constraints_dict(bvh, spec)
    bvh_path = directory / f"{spec.stem}.bvh"
    constraints_path = directory / f"{spec.stem}.constraints.json"
    bvh_path.write_text(bvh_text, encoding="utf-8")
    constraints_path.write_text(
        json.dumps(constraints, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return bvh_path, constraints_path


def read_constraint_document(path: str | Path) -> dict:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def write_constraint_document(path: str | Path, document: dict) -> None:
    Path(path).write_text(
        json.dumps(document, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
