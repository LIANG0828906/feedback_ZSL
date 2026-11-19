"""Small cross-platform helpers: stable hashing, memory probe, file IO and locks."""

from __future__ import annotations

import ctypes
import ctypes.util
import io
import json
import os
import sys

IS_WINDOWS = os.name == "nt"


def stable_hash(text: str) -> int:
    """Deterministic 64 bit hash identical on every OS / Python process."""

    # FNV-1a 64 bit. Never relies on Python's randomized hash().
    h = 0xCBF29CE484222325
    for byte in text.encode("utf-8"):
        h ^= byte
        h = (h * 0x100000001B3) & 0xFFFFFFFFFFFFFFFF
    return h


def rss_bytes() -> int:
    """Resident set size of the current process in bytes."""

    if IS_WINDOWS:
        class _ProcessMemoryCounters(ctypes.Structure):
            _fields_ = [
                ("cb", ctypes.c_ulong),
                ("PageFaultCount", ctypes.c_ulong),
                ("PeakWorkingSetSize", ctypes.c_size_t),
                ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t),
                ("PeakPagefileUsage", ctypes.c_size_t),
            ]

        psapi = ctypes.WinDLL(ctypes.util.find_library("psapi"))
        counters = _ProcessMemoryCounters()
        counters.cb = ctypes.sizeof(counters)
        handle = ctypes.windll.kernel32.GetCurrentProcess()
        if psapi.GetProcessMemoryInfo(
            handle, ctypes.byref(counters), counters.cb
        ):
            return int(counters.WorkingSetSize)
        return 0

    # Linux / WSL: /proc/self/statm field 2 is resident pages.
    try:
        with open("/proc/self/statm", "r", encoding="ascii") as fh:
            pages = int(fh.read().split()[1])
        return pages * os.sysconf("SC_PAGE_SIZE")
    except (OSError, ValueError, IndexError):
        return 0


def ensure_dir(path: str) -> str:
    os.makedirs(path, exist_ok=True)
    return path


def atomic_write(path: str, payload: bytes) -> None:
    """Write ``payload`` to ``path`` atomically (same directory temp + replace)."""

    directory = os.path.dirname(os.path.abspath(path))
    ensure_dir(directory)
    tmp = os.path.join(directory, f".{os.path.basename(path)}.tmp-{os.getpid()}")
    with open(tmp, "wb") as fh:
        fh.write(payload)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def write_json(path: str, obj: dict) -> None:
    atomic_write(path, json.dumps(obj, sort_keys=True, ensure_ascii=False).encode("utf-8"))


def read_json(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


class FileLock:
    """Cross-platform exclusive lock. Raises OSError immediately when held."""

    def __init__(self, path: str):
        self.path = path
        ensure_dir(os.path.dirname(os.path.abspath(path)))
        self._fh: io.BufferedRandom | None = None

    def acquire(self) -> None:
        # Open for update without truncating; create if missing.
        self._fh = open(self.path, "a+b")
        try:
            if IS_WINDOWS:
                import msvcrt

                # LK_NBLCK: non-blocking exclusive lock on one byte.
                msvcrt.locking(self._fh.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self._fh.close()
            self._fh = None
            raise

    def release(self) -> None:
        if self._fh is None:
            return
        try:
            if IS_WINDOWS:
                import msvcrt

                self._fh.seek(0)
                msvcrt.locking(self._fh.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._fh.fileno(), fcntl.LOCK_UN)
        finally:
            self._fh.close()
            self._fh = None

    def __enter__(self) -> "FileLock":
        self.acquire()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.release()


def directory_size(path: str) -> int:
    total = 0
    if not os.path.isdir(path):
        return 0
    for root, _dirs, files in os.walk(path):
        for name in files:
            try:
                total += os.path.getsize(os.path.join(root, name))
            except OSError:
                pass
    return total


def python_label() -> str:
    return f"{sys.version_info.major}.{sys.version_info.minor}.{sys.version_info.micro}"
