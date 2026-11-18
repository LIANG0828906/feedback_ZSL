from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path
from typing import Callable

import pytest

from choir_master.generate import init_workspace
from choir_master.render import render_project


REPO_ROOT = Path(__file__).resolve().parents[1]


@pytest.fixture(scope="session")
def workspace(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("choir-base")
    init_workspace(
        root,
        seed=235,
        duration_seconds=4.0,
        block_frames=12000,
        target_loudness_lufs=-20.0,
        true_peak_limit_dbtp=-1.0,
    )
    return root


@pytest.fixture(scope="session")
def manifest_path(workspace: Path) -> Path:
    return workspace / "project.json"


@pytest.fixture(scope="session")
def reference_render(
    manifest_path: Path,
    tmp_path_factory: pytest.TempPathFactory,
) -> dict[str, Path]:
    root = tmp_path_factory.mktemp("reference-render")
    output = root / "reference.wav"
    report = root / "reference.json"
    snapshot = root / "reference.snapshot"
    render_project(
        manifest_path,
        output_path=output,
        report_path=report,
        snapshot_dir=snapshot,
        block_frames=12000,
        fresh=True,
    )
    return {"root": root, "output": output, "report": report, "snapshot": snapshot}


def write_manifest_variant(
    manifest_path: Path,
    destination: Path,
    mutate: Callable[[dict], None],
) -> Path:
    with open(manifest_path, "r", encoding="utf-8") as handle:
        document = json.load(handle)
    mutate(document)
    with open(destination, "w", encoding="utf-8") as handle:
        json.dump(document, handle, ensure_ascii=False, indent=2)
    return destination


def run_cli(
    args: list[str],
    *,
    cwd: Path = REPO_ROOT,
    timeout: float = 90.0,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "choir_master", *args],
        cwd=cwd,
        text=True,
        capture_output=True,
        timeout=timeout,
    )


def start_cli(args: list[str]) -> subprocess.Popen[str]:
    return subprocess.Popen(
        [sys.executable, "-m", "choir_master", *args],
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def wait_for_blocks(snapshot: Path, minimum: int, timeout: float = 45.0) -> dict:
    deadline = time.monotonic() + timeout
    state_path = snapshot / "state.json"
    last_state = {}
    while time.monotonic() < deadline:
        if state_path.is_file():
            try:
                with open(state_path, "r", encoding="utf-8") as handle:
                    last_state = json.load(handle)
                if int(last_state.get("completed_blocks", 0)) >= minimum:
                    return last_state
            except (OSError, ValueError, json.JSONDecodeError):
                pass
        time.sleep(0.02)
    raise AssertionError(f"snapshot did not reach {minimum} blocks: {last_state}")


def load_json(path: Path) -> dict:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)

