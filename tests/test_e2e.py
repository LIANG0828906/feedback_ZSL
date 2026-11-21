from __future__ import annotations

import hashlib
from pathlib import Path

from subtitle_localizer.demo import run_demo


def _tree_hashes(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for path in sorted(root.rglob("*")):
        if path.is_file():
            result[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


def test_demo_is_reproducible_and_covers_acceptance_checks(tmp_path: Path) -> None:
    first_dir = tmp_path / "first"
    second_dir = tmp_path / "second"
    first = run_demo(first_dir, seed=7)
    second = run_demo(second_dir, seed=7)
    assert first == second
    assert _tree_hashes(first_dir) == _tree_hashes(second_dir)
    assert first["error_checks"] == {
        "E_BUSY": "E_BUSY",
        "E_DESC_CONFLICT": "E_DESC_CONFLICT",
        "E_LAYOUT_MISMATCH": "E_LAYOUT_MISMATCH",
        "E_RECOVERY_INVALID": "E_RECOVERY_INVALID",
        "E_TIMEBASE_INVALID": "E_TIMEBASE_INVALID",
    }
    assert all(case["offline_equal"] for case in first["cases"])
    recovery = [case for case in first["cases"] if case.get("exit_code") == 70]
    assert len(recovery) == 1
    assert recovery[0]["exit_report_generated"] is False
    assert recovery[0]["pending_unfinished_sentence_ids"]
    assert recovery[0]["resumed_affected_ids"]
    assert recovery[0]["resumed_reused_ids"]
    trace = (first_dir / "trace.jsonl").read_text(encoding="utf-8")
    assert '"event":"refresh"' in trace
    assert '"refresh_seq":' in trace
    assert '"sentence_hashes":' in trace

