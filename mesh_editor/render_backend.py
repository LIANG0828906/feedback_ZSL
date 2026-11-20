from __future__ import annotations

import os
import subprocess
import sys
from dataclasses import dataclass


@dataclass(frozen=True)
class BackendInfo:
    name: str
    detail: str

    def label(self) -> str:
        return f"{self.name} ({self.detail})"


_PROBE = r"""
from PyQt5.QtGui import QOffscreenSurface, QOpenGLContext
from PyQt5.QtWidgets import QApplication
from OpenGL.GL import GL_RENDERER, GL_VENDOR, glGetString
app = QApplication([])
surface = QOffscreenSurface()
surface.create()
context = QOpenGLContext()
if not context.create():
    raise SystemExit(2)
if not context.makeCurrent(surface):
    raise SystemExit(3)
renderer = glGetString(GL_RENDERER)
vendor = glGetString(GL_VENDOR)
if renderer is None:
    renderer = b"unknown"
if vendor is None:
    vendor = b"unknown"
print("RENDERER=" + renderer.decode("utf-8", "replace"))
print("VENDOR=" + vendor.decode("utf-8", "replace"))
print("OPENGL_PROBE_OK")
"""


def _probe_opengl(env: dict[str, str]) -> tuple[bool, str, str]:
    try:
        result = subprocess.run(
            [sys.executable, "-c", _PROBE],
            env=env,
            capture_output=True,
            text=True,
            timeout=12,
            check=False,
        )
    except Exception as exc:
        return False, f"{type(exc).__name__}: {exc}", ""
    output = (result.stdout + result.stderr).strip().splitlines()
    detail = output[-1] if output else f"exit {result.returncode}"
    renderer = ""
    for line in output:
        if line.startswith("RENDERER="):
            renderer = line.split("=", 1)[1]
    return result.returncode == 0, detail, renderer


def configure_render_backend(preference: str = "auto") -> BackendInfo:
    explicit = os.environ.get("MESH_EDITOR_RENDER_BACKEND", "").strip().lower()
    if explicit in {"software", "opengl"}:
        preference = explicit
    if preference == "software":
        os.environ["QT_OPENGL"] = "software"
        os.environ["LIBGL_ALWAYS_SOFTWARE"] = "1"
        return BackendInfo("software", "Mesa llvmpipe / QT_OPENGL=software")
    if preference == "opengl":
        os.environ.pop("QT_OPENGL", None)
        return BackendInfo("opengl", "desktop driver selected")
    if os.environ.get("MESH_EDITOR_SKIP_GL_PROBE") == "1":
        return BackendInfo("auto", "OpenGL probe skipped")

    hardware_env = dict(os.environ)
    hardware_env.pop("LIBGL_ALWAYS_SOFTWARE", None)
    hardware_env.pop("QT_OPENGL", None)
    ok, detail, renderer = _probe_opengl(hardware_env)
    if ok:
        software_renderers = ("llvmpipe", "softpipe", "swrast", "software rasterizer")
        if any(token in renderer.lower() for token in software_renderers):
            os.environ["QT_OPENGL"] = "software"
            os.environ["LIBGL_ALWAYS_SOFTWARE"] = "1"
            return BackendInfo("software", f"driver reports {renderer}")
        os.environ.pop("QT_OPENGL", None)
        return BackendInfo("opengl", f"driver reports {renderer or 'OpenGL'}")

    software_env = dict(os.environ)
    software_env["QT_OPENGL"] = "software"
    software_env["LIBGL_ALWAYS_SOFTWARE"] = "1"
    software_ok, software_detail, software_renderer = _probe_opengl(software_env)
    os.environ["QT_OPENGL"] = "software"
    os.environ["LIBGL_ALWAYS_SOFTWARE"] = "1"
    if software_ok:
        return BackendInfo(
            "software",
            f"fallback after hardware probe failed: {detail}; driver reports {software_renderer or 'software'}",
        )
    return BackendInfo(
        "software",
        f"forced fallback; both probes failed: hardware={detail}; software={software_detail}",
    )
