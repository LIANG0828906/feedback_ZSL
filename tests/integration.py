#!/usr/bin/env python3
"""Process-level SIGINT/SIGKILL and byte-identity checks (POSIX)."""

from __future__ import annotations

import argparse
import json
import pathlib
import shutil
import signal
import subprocess
import sys
import time


def run(command: list[str], timeout: float = 60.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        command,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )


def snapshot_count(path: pathlib.Path) -> int:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
        return len(document["payload"]["completedBlocks"])
    except (FileNotFoundError, json.JSONDecodeError, KeyError, TypeError):
        return 0


def wait_for_blocks(snapshot: pathlib.Path, minimum: int, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if snapshot_count(snapshot) >= minimum:
            return
        time.sleep(0.02)
    raise AssertionError(f"snapshot did not reach {minimum} blocks: {snapshot}")


def render_resume(exe: pathlib.Path, scene: pathlib.Path, out_dir: pathlib.Path) -> None:
    run(
        [
            str(exe),
            "render",
            "--scene",
            str(scene),
            "--out-dir",
            str(out_dir),
            "--quiet",
        ]
    )


def compare(exe: pathlib.Path, expected: pathlib.Path, actual: pathlib.Path) -> None:
    result = run(
        [
            str(exe),
            "compare",
            "--expected",
            str(expected),
            "--actual",
            str(actual),
        ]
    )
    document = json.loads(result.stdout)
    if not document.get("ok"):
        raise AssertionError(f"comparison failed: {document}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--exe", required=True)
    parser.add_argument("--work-dir", required=True)
    args = parser.parse_args()

    exe = pathlib.Path(args.exe).resolve()
    work = pathlib.Path(args.work_dir).resolve()
    shutil.rmtree(work, ignore_errors=True)
    work.mkdir(parents=True, exist_ok=True)
    scene = work / "scene.json"

    run([str(exe), "sample", "--out", str(scene)])
    full = work / "full"
    run(
        [
            str(exe),
            "render",
            "--scene",
            str(scene),
            "--out-dir",
            str(full),
            "--fresh",
            "--quiet",
        ]
    )

    graceful = work / "sigint"
    graceful.mkdir()
    process = subprocess.Popen(
        [
            str(exe),
            "render",
            "--scene",
            str(scene),
            "--out-dir",
            str(graceful),
            "--fresh",
            "--quiet",
            "--tile-delay-ms",
            "100",
        ],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    wait_for_blocks(graceful / "snapshot.json", 2)
    process.send_signal(signal.SIGINT)
    stdout, stderr = process.communicate(timeout=30)
    if process.returncode != 0:
        raise AssertionError(
            f"SIGINT render exited {process.returncode}\nstdout={stdout}\nstderr={stderr}"
        )
    recovery = json.loads((graceful / "recovery.json").read_text(encoding="utf-8"))
    if recovery.get("exitCode") != 0 or recovery.get("graceful") is not True:
        raise AssertionError(f"invalid recovery report: {recovery}")
    render_resume(exe, scene, graceful)
    compare(exe, full, graceful)
    if not (graceful / "recovery.json").exists():
        raise AssertionError("recovery report was removed after successful resume")

    forced = work / "sigkill"
    forced.mkdir()
    process = subprocess.Popen(
        [
            str(exe),
            "render",
            "--scene",
            str(scene),
            "--out-dir",
            str(forced),
            "--fresh",
            "--quiet",
            "--tile-delay-ms",
            "100",
        ],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    wait_for_blocks(forced / "snapshot.json", 2)
    process.kill()
    process.communicate(timeout=10)
    if process.returncode == 0:
        raise AssertionError("SIGKILL process unexpectedly exited successfully")
    render_resume(exe, scene, forced)
    compare(exe, full, forced)

    summary = {
        "ok": True,
        "code": "E_PROCESS_INTERRUPT_TEST_OK",
        "sigintExitCode": 0,
        "sigintRecoveryReport": True,
        "recoveryRetainedAfterResume": True,
        "sigkillExitCode": process.returncode,
        "sigkillRecoveryReportRequired": False,
        "resumedByteIdentical": True,
    }
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())

