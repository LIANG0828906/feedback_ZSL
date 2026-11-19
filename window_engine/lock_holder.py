"""Hold the state LOCK from an independent process (real contention test)."""

from __future__ import annotations

import sys
import time

from window_engine.util import FileLock


def main() -> int:
    lock_path = sys.argv[1]
    seconds = float(sys.argv[2])
    lock = FileLock(lock_path)
    lock.acquire()
    end = time.time() + seconds
    while time.time() < end:
        time.sleep(0.05)
    lock.release()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
