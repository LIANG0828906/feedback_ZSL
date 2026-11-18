"""Deterministic offline rose-window validation renderer."""

from __future__ import annotations

from typing import Any

__all__ = ["RenderResult", "Scene", "load_scene", "render_scene"]
__version__ = "1.0.0"


def __getattr__(name: str) -> Any:
    if name in {"RenderResult", "render_scene"}:
        from .renderer import RenderResult, render_scene

        return {"RenderResult": RenderResult, "render_scene": render_scene}[name]
    if name in {"Scene", "load_scene"}:
        from .scene import Scene, load_scene

        return {"Scene": Scene, "load_scene": load_scene}[name]
    raise AttributeError(name)
