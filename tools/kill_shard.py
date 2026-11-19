from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
from pathlib import Path

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from train.common import load_config, resolve_path


def pid_path(shard_id: str) -> Path:
    return resolve_path("run/pids") / f"{shard_id}.pid"


def kill_shard(config_path: str, shard_id: str) -> dict:
    config = load_config(config_path)
    declared = {item["shard_id"] for item in config["shards"]}
    if shard_id not in declared:
        raise ValueError(f"unknown shard {shard_id!r}")
    path = pid_path(shard_id)
    if not path.exists():
        raise RuntimeError(f"pid file does not exist for {shard_id}")
    pid = int(path.read_text(encoding="ascii").strip())
    if os.name == "nt":
        completed = subprocess.run(
            ["taskkill", "/PID", str(pid), "/T", "/F"],
            capture_output=True,
            text=True,
            check=False,
        )
        return {
            "command": "taskkill",
            "pid": pid,
            "returncode": completed.returncode,
            "shard_id": shard_id,
            "stderr": completed.stderr.strip(),
            "stdout": completed.stdout.strip(),
        }
    os.kill(pid, signal.SIGKILL)
    return {
        "pid": pid,
        "returncode": 0,
        "shard_id": shard_id,
        "signal": "SIGKILL",
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Forcefully terminate one parameter shard")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--shard-id", required=True)
    args = parser.parse_args()
    result = kill_shard(args.config, args.shard_id)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0 if result.get("returncode") == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
