from __future__ import annotations

import json
import subprocess
import sys
import time
from pathlib import Path

from repair_app.engine import SessionEngine
from repair_app.generator import generate_sample_file
from repair_app.models import Interval


def _read_events(path: Path) -> list[dict]:
    if not path.exists():
        return []
    return [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def test_forced_kill_then_recover_does_not_rewrite_completed_blocks(tmp_path: Path) -> None:
    input_path = tmp_path / "input.wav"
    session_path = tmp_path / "session.json"
    output_path = tmp_path / "output.wav"
    progress_path = tmp_path / "progress.jsonl"
    trace_path = tmp_path / "trace.jsonl"
    generate_sample_file(
        input_path, sample_rate=48_000, channels=2, seconds=1.5, seed=555
    )
    engine = SessionEngine(session_path)
    engine.create(
        input_path=input_path,
        output_path=output_path,
        block_size=1024,
        intervals=[Interval(1, 8_000, 40_000, gain_db=2.0)],
    )
    command = [
        sys.executable,
        "-m",
        "repair_app",
        "process",
        "--session",
        str(session_path),
        "--post-commit-delay-ms",
        "40",
        "--progress-file",
        str(progress_path),
        "--trace",
        str(trace_path),
        "--run-label",
        "crash",
        "--quiet",
    ]
    process = subprocess.Popen(
        command,
        cwd=Path(__file__).resolve().parent.parent,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if process.poll() is not None:
            break
        processed = [
            event
            for event in _read_events(progress_path)
            if event.get("phase") == "processing"
        ]
        if len(processed) >= 3:
            process.kill()
            process.wait(timeout=10)
            break
        time.sleep(0.01)
    else:
        process.kill()
        process.wait(timeout=10)
        raise AssertionError("child process did not reach the kill point")
    assert process.returncode != 0
    killed = engine.status()
    assert 0 < killed["committed_blocks"] < killed["total_blocks"]
    assert output_path.stat().st_size == 44 + killed["frames"] * killed["channels"] * 2

    recovered = engine.recover()
    assert recovered["recovered_blocks"] == killed["committed_blocks"]
    before = {key: dict(value) for key, value in engine.store.load().completed.items()}
    events: list[dict] = []
    resumed = engine.process(trace=events.append)
    after = {key: dict(value) for key, value in engine.store.load().completed.items()}
    assert resumed["status"] == "completed"
    assert resumed["reused_blocks"] == killed["committed_blocks"]
    for key, summary in before.items():
        assert after[key] == summary
    reused_indexes = {
        event["index"] for event in events if event.get("status") == "reused"
    }
    assert reused_indexes == {int(key) for key in before}

