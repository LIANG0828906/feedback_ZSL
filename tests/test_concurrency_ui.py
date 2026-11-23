from __future__ import annotations

import os
import subprocess
import sys
import time
from pathlib import Path

import pytest

from acoustic_workbench.codes import E_PARA
from acoustic_workbench.concurrency import BoundedParallelRunner
from acoustic_workbench.errors import WorkbenchError

ROOT = Path(__file__).resolve().parents[1]


def test_parallel_runner_preserves_order() -> None:
    runner = BoundedParallelRunner(3, 1024 * 1024)

    def worker(value: int) -> int:
        time.sleep((4 - value) * 0.005)
        return value

    values = list(
        runner.map_ordered(
            range(4),
            worker,
            buffer_estimator=lambda _: 100,
        )
    )
    assert values == [0, 1, 2, 3]


def test_parallel_runner_enforces_per_thread_buffer() -> None:
    runner = BoundedParallelRunner(2, 512)
    with pytest.raises(WorkbenchError) as raised:
        list(
            runner.map_ordered(
                [0],
                lambda value: value,
                buffer_estimator=lambda _: 1024,
            )
        )
    assert raised.value.code == E_PARA
    assert raised.value.field == "thread_buffer_mb"


def test_pyqt_window_starts_offscreen() -> None:
    env = dict(os.environ)
    env["QT_QPA_PLATFORM"] = "offscreen"
    completed = subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "check_ui.py")],
        cwd=ROOT,
        env=env,
        text=True,
        capture_output=True,
        timeout=60,
        check=False,
    )
    assert completed.returncode == 0, completed.stderr
    assert "UI_OK" in completed.stdout
