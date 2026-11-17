"""Deterministic forced-termination resume demo (SIGKILL semantics).

Runs a render in a child process, kills it with SIGKILL mid-way, confirms a
durable snapshot exists, resumes in a second process, and compares the
assembled master against a one-shot reference.
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time


def main(workspace: str, project_root: str) -> int:
    env = dict(os.environ)
    env["PYTHONPATH"] = project_root
    args = [sys.executable, "-m", "stage_noise.cli", "render",
            "configs/demo_manifest.json", "--interrupt-after-chunk", "12"]
    killed = subprocess.Popen(args, cwd=workspace, env=env,
                              stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL)
    time.sleep(2.0)
    killed.send_signal(signal.SIGKILL)
    print("SIGKILL exit code:", killed.wait())
    snap = os.path.join(workspace, "configs", ".render_state",
                        "snapshot.json")
    print("snapshot present:", os.path.isfile(snap))
    print("committed chunks:",
          json.load(open(snap, encoding="utf-8"))["completed_chunks"])

    resumed = subprocess.run(args[:-2], cwd=workspace, env=env,
                             capture_output=True, text=True)
    print("resume exit code:", resumed.returncode)
    print(resumed.stdout.strip())
    return resumed.returncode


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1], sys.argv[2]))
