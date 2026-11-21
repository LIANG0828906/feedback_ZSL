from __future__ import annotations

import os
from pathlib import Path

from score_engine.sample_generator import generate_samples


def _relative_files(root: Path) -> dict[str, bytes]:
    return {
        path.relative_to(root).as_posix(): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def test_generator_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    first_result = generate_samples(first, seed=20261122)
    second_result = generate_samples(second, seed=20261122)
    assert first_result["trace_records"] == second_result["trace_records"]
    assert first_result["manifest"] == second_result["manifest"]
    assert _relative_files(first) == _relative_files(second)
    trace_text = (first / "trace.jsonl").read_text(encoding="utf-8")
    assert '"event":"incremental_equivalence"' in trace_text
    assert '"equivalent":true' in trace_text
    assert '"event":"recovery_completed"' in trace_text
    assert '"equivalent_to_fresh_solve":true' in trace_text


def test_gui_headless_smoke(generated: Path) -> None:
    os.environ["QT_QPA_PLATFORM"] = "offscreen"
    from score_engine.gui import run_gui

    result = run_gui(
        score_path=str(generated / "scores" / "virtual_v3.json"),
        smoke=True,
    )
    assert result == 0

