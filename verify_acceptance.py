#!/usr/bin/env python3
"""Run the complete local acceptance suite and write a JSON summary."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", default="samples")
    parser.add_argument("--workspace", default="artifacts/acceptance")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    workspace = (root / args.workspace).resolve()
    workspace.mkdir(parents=True, exist_ok=True)
    commands = [
        [sys.executable, "-m", "unittest", "discover", "-s", "tests", "-v"],
        [
            sys.executable,
            "-m",
            "spectraforge",
            "--samples",
            args.samples,
            "verify",
            "--workspace",
            str(workspace / "paths"),
            "--report",
            str(workspace / "three_path_report.json"),
        ],
        [
            sys.executable,
            "-m",
            "spectraforge",
            "--samples",
            args.samples,
            "inspect",
            "--scene",
            "neon_forge_424242",
            "--with-truth",
            "--with-alignment",
        ],
        [
            sys.executable,
            "-m",
            "spectraforge",
            "--samples",
            args.samples,
            "check-errors",
            "--workspace",
            str(workspace / "errors"),
        ],
    ]
    results = []
    for command in commands:
        completed = subprocess.run(command, cwd=root, text=True, capture_output=True)
        results.append(
            {
                "command": command,
                "returncode": completed.returncode,
                "stdout_tail": completed.stdout[-3500:],
                "stderr_tail": completed.stderr[-3500:],
            }
        )
        if "check-errors" in command and completed.returncode == 0:
            (workspace / "error_codes.json").write_text(
                completed.stdout,
                encoding="utf-8",
            )
        if "inspect" in command and completed.returncode == 0:
            (workspace / "scene_inspection.json").write_text(
                completed.stdout,
                encoding="utf-8",
            )
        if completed.returncode != 0:
            break
    summary = {
        "ok": all(item["returncode"] == 0 for item in results),
        "commands": results,
    }
    (workspace / "acceptance_summary.json").write_text(
        json.dumps(summary, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(json.dumps({"ok": summary["ok"], "summary": str(workspace / "acceptance_summary.json")}, indent=2))
    return 0 if summary["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
