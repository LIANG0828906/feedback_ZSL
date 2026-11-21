from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .errors import BusinessError, E_BUSY


def _pid_is_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    if os.name == "nt":
        import ctypes
        from ctypes import wintypes

        process_query_limited_information = 0x1000
        still_active = 259
        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
        kernel32.OpenProcess.restype = wintypes.HANDLE
        kernel32.GetExitCodeProcess.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.DWORD)]
        kernel32.GetExitCodeProcess.restype = wintypes.BOOL
        kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
        kernel32.CloseHandle.restype = wintypes.BOOL
        handle = kernel32.OpenProcess(process_query_limited_information, False, pid)
        if not handle:
            return False
        try:
            exit_code = wintypes.DWORD()
            if not kernel32.GetExitCodeProcess(handle, ctypes.byref(exit_code)):
                return False
            return exit_code.value == still_active
        finally:
            kernel32.CloseHandle(handle)
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False
    return True


class AssetSessionLock:
    """Cross-platform exclusive session marker.

    The marker is an O_EXCL file rather than an OS-level share lock so a hard
    kill leaves enough evidence to reclaim the session after verifying that the
    recorded process is gone.
    """

    def __init__(self, path: Path | str, *, session_token: str | None = None) -> None:
        self.path = Path(path)
        self.token = session_token or os.urandom(16).hex()
        self._acquired = False

    def acquire(self) -> "AssetSessionLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        for _ in range(3):
            try:
                fd = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            except FileExistsError:
                owner = self._read_owner()
                pid = int(owner.get("pid", -1)) if owner else -1
                if pid > 0 and _pid_is_alive(pid):
                    raise BusinessError(
                        E_BUSY,
                        "another layout session owns this asset",
                        {
                            "asset_lock": str(self.path),
                            "owner_pid": pid,
                            "owner_token": owner.get("token"),
                        },
                    ) from None
                try:
                    self.path.unlink()
                except FileNotFoundError:
                    pass
                except OSError as exc:
                    raise BusinessError(
                        E_BUSY,
                        "stale session lock could not be reclaimed",
                        {"asset_lock": str(self.path), "detail": str(exc)},
                    ) from exc
                continue
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump(
                    {"pid": os.getpid(), "token": self.token},
                    handle,
                    sort_keys=True,
                    separators=(",", ":"),
                )
                handle.flush()
                os.fsync(handle.fileno())
            self._acquired = True
            return self
        raise BusinessError(E_BUSY, "could not acquire the asset session lock", {"asset_lock": str(self.path)})

    def _read_owner(self) -> dict[str, Any] | None:
        try:
            with self.path.open("r", encoding="utf-8") as handle:
                value = json.load(handle)
        except (OSError, UnicodeError, json.JSONDecodeError):
            return None
        return value if isinstance(value, dict) else None

    def release(self) -> None:
        if not self._acquired:
            return
        owner = self._read_owner()
        if owner and owner.get("token") != self.token:
            self._acquired = False
            return
        try:
            self.path.unlink()
        except FileNotFoundError:
            pass
        finally:
            self._acquired = False

    def __enter__(self) -> "AssetSessionLock":
        return self.acquire()

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.release()

