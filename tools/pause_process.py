from __future__ import annotations

import argparse
import ctypes
import json
import os
import signal
import sys
from pathlib import Path

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from train.common import resolve_path


def _windows_suspend_resume(pid: int, resume: bool) -> None:
    process_suspend_resume = 0x0800
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    ntdll = ctypes.WinDLL("ntdll")
    handle = kernel32.OpenProcess(process_suspend_resume, False, pid)
    if not handle:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        function = ntdll.NtResumeProcess if resume else ntdll.NtSuspendProcess
        status = function(handle)
        if status != 0:
            raise OSError(f"NtSuspendProcess/NtResumeProcess returned {status:#x}")
    finally:
        kernel32.CloseHandle(handle)


def change_pause(identity: str, resume: bool) -> dict:
    path = resolve_path("run/pids") / f"{identity}.pid"
    if not path.exists():
        raise RuntimeError(f"pid file does not exist for {identity}")
    pid = int(path.read_text(encoding="ascii").strip())
    if os.name == "nt":
        _windows_suspend_resume(pid, resume)
        mode = "resume" if resume else "suspend"
    else:
        os.kill(pid, signal.SIGCONT if resume else signal.SIGSTOP)
        mode = "SIGCONT" if resume else "SIGSTOP"
    return {"identity": identity, "mode": mode, "pid": pid}


def main() -> int:
    parser = argparse.ArgumentParser(description="Temporarily pause or resume a local worker")
    parser.add_argument("--identity", required=True)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--pause", action="store_true")
    group.add_argument("--resume", action="store_true")
    args = parser.parse_args()
    result = change_pause(args.identity, resume=args.resume)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
