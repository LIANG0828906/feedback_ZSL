#!/usr/bin/env python3
"""Terminate a live iovflow stage worker by PID from the state database."""

from __future__ import annotations

import argparse
import json
import os
import signal
import sqlite3
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


def terminate(pid: int) -> None:
    if os.name == "nt":
        import ctypes
        from ctypes import wintypes

        PROCESS_TERMINATE = 0x0001
        handle = ctypes.windll.kernel32.OpenProcess(PROCESS_TERMINATE, False, int(pid))
        if not handle:
            raise OSError(f"OpenProcess failed for pid {pid}")
        try:
            if not ctypes.windll.kernel32.TerminateProcess(handle, 1):
                raise OSError(f"TerminateProcess failed for pid {pid}")
        finally:
            ctypes.windll.kernel32.CloseHandle(handle)
    else:
        os.kill(int(pid), signal.SIGKILL)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--state-db", default="state/iovflow_state.db")
    parser.add_argument("--job-id", required=True)
    parser.add_argument("--worker-id", default=None)
    parser.add_argument("--task", default=None, help="match current_task, e.g. window_aggregate:40")
    parser.add_argument("--pid", type=int, default=None)
    parser.add_argument("--list", action="store_true")
    args = parser.parse_args()
    conn = sqlite3.connect(args.state_db)
    conn.row_factory = sqlite3.Row
    rows = [
        dict(r)
        for r in conn.execute(
            "SELECT * FROM job_workers WHERE job_id=? ORDER BY worker_id",
            (args.job_id,),
        ).fetchall()
    ]
    conn.close()
    if args.list:
        print(json.dumps(rows, indent=2, sort_keys=True, default=str))
        return 0
    target = None
    for row in rows:
        if args.pid is not None and int(row.get("pid") or -1) == args.pid:
            target = row
            break
        if args.worker_id and row["worker_id"] == args.worker_id:
            target = row
            break
        if args.task and args.task in str(row.get("current_task") or ""):
            target = row
            break
    if target is None:
        raise SystemExit("no matching live worker; use --list to inspect job_workers")
    pid = int(target.get("pid") or 0)
    if pid <= 0:
        raise SystemExit("matched worker has no PID")
    terminate(pid)
    print(json.dumps({"terminated": pid, "worker": target}, sort_keys=True, default=str))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

