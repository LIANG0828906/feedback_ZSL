from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path


def _run(*arguments: str) -> tuple[int, dict]:
    result = subprocess.run(
        [sys.executable, "-m", "repair_app", *arguments],
        cwd=Path(__file__).resolve().parent.parent,
        capture_output=True,
        text=True,
        timeout=30,
    )
    payload = json.loads(result.stdout) if result.stdout.strip() else {}
    return result.returncode, payload


def test_cli_new_process_status_and_update(tmp_path: Path) -> None:
    input_path = tmp_path / "input.wav"
    session_path = tmp_path / "session.json"
    output_path = tmp_path / "output.wav"
    code, payload = _run(
        "generate",
        "--out",
        str(input_path),
        "--rate",
        "8000",
        "--channels",
        "1",
        "--seconds",
        "0.5",
        "--seed",
        "99",
    )
    assert code == 0 and payload["ok"]
    code, payload = _run(
        "new",
        "--input",
        str(input_path),
        "--session",
        str(session_path),
        "--output",
        str(output_path),
        "--block-size",
        "500",
        "--interval",
        "0.1:0.3:3",
    )
    assert code == 0 and payload["status"] == "created"
    code, payload = _run("process", "--session", str(session_path), "--max-new-blocks", "2")
    assert code == 0 and payload["committed_blocks"] == 2
    code, payload = _run("status", "--session", str(session_path))
    assert code == 0 and payload["complete"] is False
    code, payload = _run(
        "update",
        "--session",
        str(session_path),
        "--set",
        "noise_gate.threshold_db=-55",
    )
    assert code == 0 and payload["dirty_blocks"]
    code, payload = _run("process", "--session", str(session_path))
    assert code == 0 and payload["status"] == "completed"


def test_cli_demo_runs_full_chain(tmp_path: Path) -> None:
    code, payload = _run("demo", "--workdir", str(tmp_path / "demo"), "--seed", "77")
    assert code == 0, payload
    assert payload["ok"] is True
    assert payload["determinism"]["output_byte_equal"] is True
    assert payload["determinism"]["block_summaries_equal"] is True
    assert payload["forced_crash"]["only_complete_block_regions_written"] is True
    assert payload["incremental_recompute"]["reused_blocks"] > 0
    assert (tmp_path / "demo" / "demo_report.json").is_file()

