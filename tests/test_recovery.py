from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

from mesh_repair.binary import file_sha256


def _repair_command(source: Path, output: Path, work: Path) -> list[str]:
    return [
        sys.executable,
        "-m",
        "mesh_repair.cli",
        "repair",
        str(source),
        "-o",
        str(output),
        "--work-dir",
        str(work),
        "--blocks",
        "2,2,1",
        "--parallelism",
        "2",
        "--global-budget",
        "8M",
        "--task-budget",
        "4M",
    ]


def test_forced_termination_resume_matches_uninterrupted(
    sample_bundle: dict, tmp_path: Path
) -> None:
    source = Path(sample_bundle["path"])
    output = tmp_path / "resumed.msh"
    work = tmp_path / "work"
    env = dict(os.environ)
    env["MESH_REPAIR_CRASH_AFTER_COMMITS"] = "1"
    crashed = subprocess.run(
        _repair_command(source, output, work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert crashed.returncode == 137
    assert not output.exists()
    assert len(list((work / "sessions").glob("*/blocks/block-*.json"))) == 1

    env.pop("MESH_REPAIR_CRASH_AFTER_COMMITS")
    resumed = subprocess.run(
        _repair_command(source, output, work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert resumed.returncode == 0, resumed.stderr
    report = json.loads(Path(str(output) + ".report.json").read_text())
    assert sum(1 for item in report["commits"] if item["reused"]) == 1
    assert report["boundary_consistency"]["watertight"] is True

    normal_output = tmp_path / "normal.msh"
    normal = subprocess.run(
        _repair_command(source, normal_output, tmp_path / "normal-work"),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert normal.returncode == 0, normal.stderr
    assert file_sha256(output) == file_sha256(normal_output)


def test_corrupt_committed_block_blocks_recovery(
    sample_bundle: dict, tmp_path: Path
) -> None:
    source = Path(sample_bundle["path"])
    output = tmp_path / "corrupt.msh"
    work = tmp_path / "work"
    env = dict(os.environ)
    env["MESH_REPAIR_CRASH_AFTER_COMMITS"] = "1"
    crashed = subprocess.run(
        _repair_command(source, output, work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert crashed.returncode == 137
    block = next((work / "sessions").glob("*/blocks/block-*.json"))
    block.write_bytes(block.read_bytes() + b"\nBROKEN")
    env.pop("MESH_REPAIR_CRASH_AFTER_COMMITS")
    resumed = subprocess.run(
        _repair_command(source, output, work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert resumed.returncode == 6
    assert '"code": "E_RECOVERY_INVALID"' in resumed.stderr
    assert not output.exists()
