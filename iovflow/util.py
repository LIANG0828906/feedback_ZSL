from __future__ import annotations

import hashlib
import json
import math
import os
import re
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable, Iterator


UTC = timezone.utc
EPOCH = datetime(1970, 1, 1, tzinfo=UTC)
INF_TS = 2**62


def utc_now() -> datetime:
    return datetime.now(tz=UTC)


def iso(dt: datetime | None = None) -> str:
    dt = dt or utc_now()
    return dt.astimezone(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def parse_time(value: Any) -> int:
    """Convert common timestamp representations to integer UTC epoch seconds."""
    if value is None:
        raise ValueError("timestamp is null")
    if isinstance(value, (int, float)):
        # DuckDB epoch values may be float seconds.  Very large numbers are
        # assumed to be microseconds/nanoseconds.
        n = float(value)
        if abs(n) > 1e17:
            n /= 1e9
        elif abs(n) > 1e14:
            n /= 1e6
        elif abs(n) > 1e11:
            n /= 1e3
        return int(math.floor(n))
    if isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=UTC)
        return int(math.floor(value.timestamp()))
    text = str(value).strip()
    if not text:
        raise ValueError("empty timestamp")
    # Numeric strings are accepted too.
    try:
        if re.fullmatch(r"-?\d+(\.\d+)?", text):
            return parse_time(float(text))
    except ValueError:
        pass
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    # SQL timestamps frequently have a space instead of 'T'.
    if " " in text and "T" not in text:
        text = text.replace(" ", "T", 1)
    try:
        dt = datetime.fromisoformat(text)
    except ValueError:
        # A final fallback for e.g. "2026-01-01 00:00:00 UTC".
        for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d", "%Y/%m/%d %H:%M:%S"):
            try:
                dt = datetime.strptime(text.replace(" UTC", ""), fmt)
                break
            except ValueError:
                continue
        else:
            raise
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=UTC)
    return int(math.floor(dt.timestamp()))


def ts_iso(epoch_seconds: int | float) -> str:
    n = int(epoch_seconds)
    return datetime.fromtimestamp(n, tz=UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def floor_ts(epoch_seconds: int, width: int) -> int:
    return (int(epoch_seconds) // int(width)) * int(width)


def stable_hash64(text: str) -> int:
    return int.from_bytes(hashlib.blake2b(text.encode("utf-8"), digest_size=8).digest(), "big")


def stable_hash_hex(text: str, size: int = 16) -> str:
    return hashlib.blake2b(text.encode("utf-8"), digest_size=size).hexdigest()


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str)


def fingerprint(row: dict[str, Any], keys: Iterable[str] | None = None) -> str:
    if keys is None:
        payload: Any = row
    else:
        payload = {k: row.get(k) for k in keys}
    return hashlib.blake2b(canonical_json(payload).encode("utf-8"), digest_size=16).hexdigest()


def read_json(path: str | Path) -> Any:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def write_json(path: str | Path, value: Any) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + f".tmp.{os.getpid()}")
    with open(tmp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(value, fh, ensure_ascii=False, sort_keys=True, indent=2, default=str)
        fh.write("\n")
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def append_jsonl(path: Path, rows: Iterable[dict[str, Any]]) -> int:
    path.parent.mkdir(parents=True, exist_ok=True)
    count = 0
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True, default=str))
            fh.write("\n")
            count += 1
    return count


def iter_jsonl(path: str | Path) -> Iterator[dict[str, Any]]:
    with open(path, "r", encoding="utf-8-sig") as fh:
        for line_no, line in enumerate(fh, 1):
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError as exc:
                from .errors import EngineError

                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"invalid JSON at {path}:{line_no}: {exc}",
                    fragment=line[:300],
                ) from exc
            if not isinstance(obj, dict):
                from .errors import EngineError

                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"JSON line {path}:{line_no} is not an object",
                    fragment=line[:300],
                )
            yield obj


def iter_jsonl_range(path: str | Path, start: int, end: int | None) -> Iterator[dict[str, Any]]:
    """Iterate a byte range, trimming a partial first line.

    ``start`` is the first byte owned by this task.  If it points into a line,
    the rest of that line is skipped.  If ``end`` is not None the task stops at
    the next newline at/after ``end``; this means every line is owned by
    exactly one task when the parent uses non-overlapping offsets.
    """
    size = os.path.getsize(path)
    start = max(0, min(int(start), size))
    end = size if end is None else max(start, min(int(end), size))
    with open(path, "rb") as fh:
        if start:
            fh.seek(start - 1)
            if fh.read(1) != b"\n":
                fh.readline()
        else:
            fh.seek(0)
        while True:
            pos = fh.tell()
            line = fh.readline()
            if not line:
                break
            if pos >= end:
                break
            decoded = line.decode("utf-8-sig").strip()
            if not decoded:
                continue
            try:
                obj = json.loads(decoded)
            except json.JSONDecodeError as exc:
                from .errors import EngineError

                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"invalid JSON at {path}: {exc}",
                    fragment=decoded[:300],
                ) from exc
            if not isinstance(obj, dict):
                from .errors import EngineError

                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"JSON line in {path} is not an object",
                    fragment=decoded[:300],
                )
            yield obj


def tmp_size(path: str | Path) -> int:
    p = Path(path)
    if not p.exists():
        return 0
    if p.is_file():
        return p.stat().st_size
    total = 0
    for child in p.rglob("*"):
        try:
            if child.is_file():
                total += child.stat().st_size
        except OSError:
            pass
    return total


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    vals = sorted(values)
    if len(vals) == 1:
        return float(vals[0])
    k = (len(vals) - 1) * (pct / 100.0)
    lo = int(math.floor(k))
    hi = int(math.ceil(k))
    if lo == hi:
        return float(vals[lo])
    return float(vals[lo] + (vals[hi] - vals[lo]) * (k - lo))


def process_rss_bytes(pid: int) -> int:
    """Best-effort RSS for a process.

    On Linux /proc is used; on Windows the Win32 API is used through ctypes.
    The function returns 0 when the platform does not expose the information.
    """
    if sys.platform.startswith("linux"):
        try:
            with open(f"/proc/{pid}/status", "r", encoding="ascii", errors="ignore") as fh:
                for line in fh:
                    if line.startswith("VmRSS:"):
                        return int(line.split()[1]) * 1024
        except OSError:
            return 0
    if os.name == "nt":
        try:
            import ctypes
            from ctypes import wintypes

            class PROCESS_MEMORY_COUNTERS(ctypes.Structure):
                _fields_ = [
                    ("cb", wintypes.DWORD),
                    ("PageFaultCount", wintypes.DWORD),
                    ("PeakWorkingSetSize", ctypes.c_size_t),
                    ("WorkingSetSize", ctypes.c_size_t),
                    ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                    ("QuotaPagedPoolUsage", ctypes.c_size_t),
                    ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                    ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                    ("PagefileUsage", ctypes.c_size_t),
                    ("PeakPagefileUsage", ctypes.c_size_t),
                ]

            access = 0x1000 | 0x0400  # PROCESS_QUERY_INFORMATION | PROCESS_QUERY_LIMITED_INFORMATION
            handle = ctypes.windll.kernel32.OpenProcess(access, False, int(pid))
            if not handle:
                return 0
            try:
                counters = PROCESS_MEMORY_COUNTERS()
                counters.cb = ctypes.sizeof(counters)
                if ctypes.windll.psapi.GetProcessMemoryInfo(
                    handle, ctypes.byref(counters), counters.cb
                ):
                    return int(counters.WorkingSetSize)
            finally:
                ctypes.windll.kernel32.CloseHandle(handle)
        except Exception:
            return 0
    return 0


def process_alive(pid: int) -> bool:
    """Return whether a PID is still running without sending a signal."""
    if pid <= 0:
        return False
    if os.name == "nt":
        try:
            import ctypes
            from ctypes import wintypes

            PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
            handle = ctypes.windll.kernel32.OpenProcess(
                PROCESS_QUERY_LIMITED_INFORMATION, False, int(pid)
            )
            if not handle:
                return False
            try:
                code = wintypes.DWORD()
                if not ctypes.windll.kernel32.GetExitCodeProcess(handle, ctypes.byref(code)):
                    return False
                return int(code.value) == 259  # STILL_ACTIVE
            finally:
                ctypes.windll.kernel32.CloseHandle(handle)
        except Exception:
            return True
    try:
        os.kill(int(pid), 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False


def safe_identifier(name: str, fallback: str = "col") -> str:
    out = re.sub(r"[^A-Za-z0-9_]+", "_", str(name)).strip("_")
    if not out:
        out = fallback
    if out[0].isdigit():
        out = "c_" + out
    return out


def merge_sorted_unique(a: Iterable[Any], b: Iterable[Any]) -> list[Any]:
    return sorted(set(a) | set(b), key=lambda v: (v is None, str(v)))


def sleep_ms(ms: float) -> None:
    if ms > 0:
        time.sleep(ms / 1000.0)

