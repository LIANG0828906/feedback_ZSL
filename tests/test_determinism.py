from __future__ import annotations

import hashlib
from pathlib import Path

from hdr_studio.demo import run_demo


def _tree_hashes(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for path in sorted(item for item in root.rglob("*") if item.is_file()):
        result[path.relative_to(root).as_posix()] = hashlib.sha256(
            path.read_bytes()
        ).hexdigest()
    return result


def test_demo_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    baseline_a = run_demo(first, seed=77, force=True)
    baseline_b = run_demo(second, seed=77, force=True)
    assert baseline_a == baseline_b
    hashes_a = _tree_hashes(first)
    hashes_b = _tree_hashes(second)
    assert hashes_a == hashes_b
    assert hashes_a["trace/trace.jsonl"]
    assert any(name.endswith(".hdrseq") for name in hashes_a)
    assert any("/frame_" in name for name in hashes_a)

