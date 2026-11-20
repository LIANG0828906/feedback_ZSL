"""Runtime readiness checks for the local workbench."""

from __future__ import annotations

import importlib
import json
import os
import platform
import sys
import tempfile
from pathlib import Path
from typing import Any


def environment_report(temp_dir: str | Path | None = None) -> dict[str, Any]:
    checks: dict[str, Any] = {
        "platform": platform.platform(),
        "python_executable": sys.executable,
        "python_version": platform.python_version(),
    }
    python_ok = sys.version_info >= (3, 11)
    checks["python_3_11_or_newer"] = {
        "ok": python_ok,
        "detail": platform.python_version(),
    }
    try:
        numpy = importlib.import_module("numpy")
        numpy_ok = True
        numpy_detail = str(numpy.__version__)
    except Exception as exc:
        numpy_ok = False
        numpy_detail = f"{type(exc).__name__}: {exc}"
    checks["numpy"] = {"ok": numpy_ok, "detail": numpy_detail}
    try:
        tkinter = importlib.import_module("tkinter")
        tkinter_ok = True
        tkinter_detail = f"Tk {tkinter.TkVersion}"
    except Exception as exc:
        tkinter_ok = False
        tkinter_detail = f"{type(exc).__name__}: {exc}"
    checks["tkinter"] = {"ok": tkinter_ok, "detail": tkinter_detail}

    directory = Path(temp_dir) if temp_dir is not None else Path(tempfile.gettempdir())
    try:
        directory.mkdir(parents=True, exist_ok=True)
        descriptor, name = tempfile.mkstemp(prefix=".glyph-atlas-check.", dir=str(directory))
        os.close(descriptor)
        Path(name).unlink()
        temp_ok = True
        temp_detail = str(directory)
    except Exception as exc:
        temp_ok = False
        temp_detail = f"{type(exc).__name__}: {exc}"
    checks["temp_write"] = {"ok": temp_ok, "detail": temp_detail}
    checks["windows"] = {"ok": platform.system() == "Windows", "detail": platform.system()}
    checks["ok"] = bool(python_ok and numpy_ok and tkinter_ok and temp_ok)
    return checks


def format_environment_report(report: dict[str, Any]) -> str:
    return json.dumps(report, sort_keys=True, ensure_ascii=False)

