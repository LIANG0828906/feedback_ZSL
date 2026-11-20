"""Tkinter workbench for glyph, tile, and resource inspection."""

from __future__ import annotations

import dataclasses
import queue
import threading
from pathlib import Path
from typing import Any

import numpy as np

from .config import TargetConfig, WorkConfig, load_config
from .errors import E_CONFIG_INVALID, E_CANCELLED, GlyphAtlasError, fail
from .font import FontSpec, load_font
from .pipeline import run_loaded


def launch_gui(config_path: str | Path = "samples/generated/mixed_nonzero/config.json") -> None:
    try:
        import tkinter as tk
        from tkinter import ttk
    except ImportError as exc:
        fail(
            E_CONFIG_INVALID,
            "Tkinter is not importable in this Python installation",
            python="python3",
            remedy="Install the platform Tk package (Windows Python 3.11 normally includes it)",
            error=str(exc),
        )

    config = load_config(config_path)
    font = load_font(config.font_path)
    try:
        root = tk.Tk()
    except tk.TclError as exc:
        fail(
            E_CONFIG_INVALID,
            "Tkinter is installed but no graphical display is available",
            error=str(exc),
        )
    root.title("Glyph Atlas Workbench")
    root.geometry("900x660")
    app = _Workbench(root, tk, ttk, config, font)
    root.mainloop()


class _Workbench:
    def __init__(self, root: Any, tk: Any, ttk: Any, config: WorkConfig, font: FontSpec) -> None:
        self.root = root
        self.tk = tk
        self.ttk = ttk
        self.config = config
        self.font = font
        self.events: queue.Queue[dict[str, Any]] = queue.Queue()
        self.cancel_event = threading.Event()
        self.worker: threading.Thread | None = None
        self.preview = np.zeros((1, 1), dtype=np.uint8)
        self.completed_tiles: set[tuple[int, int]] = set()
        self.tile_width = config.tile_width
        self.tile_height = config.tile_height
        self.current_glyph = ""

        self.frame = ttk.Frame(root, padding=8)
        self.frame.pack(fill="both", expand=True)
        self._build_controls()
        self._build_canvas()

    def _build_controls(self) -> None:
        ttk = self.ttk
        controls = ttk.Frame(self.frame)
        controls.pack(fill="x", pady=(0, 8))
        ttk.Label(controls, text="Text").pack(side="left")
        self.text_var = self.tk.StringVar(value="AO")
        ttk.Entry(controls, textvariable=self.text_var, width=18).pack(side="left", padx=(4, 12))

        ttk.Label(controls, text="Size").pack(side="left")
        self.size_var = self.tk.StringVar(value=str(self.config.targets[0].pixel_size))
        ttk.Spinbox(controls, from_=6, to=256, increment=1, width=6, textvariable=self.size_var).pack(
            side="left", padx=(4, 12)
        )

        ttk.Label(controls, text="Subpixel X").pack(side="left")
        self.subpixel_x_var = self.tk.StringVar(value=str(self.config.targets[0].subpixel_x))
        ttk.Spinbox(
            controls, from_=0.0, to=0.999, increment=0.125, width=7, textvariable=self.subpixel_x_var
        ).pack(side="left", padx=(4, 12))

        ttk.Label(controls, text="Y").pack(side="left")
        self.subpixel_y_var = self.tk.StringVar(value=str(self.config.targets[0].subpixel_y))
        ttk.Spinbox(
            controls, from_=0.0, to=0.999, increment=0.125, width=7, textvariable=self.subpixel_y_var
        ).pack(side="left", padx=(4, 12))

        self.start_button = ttk.Button(controls, text="Rasterize", command=self._start)
        self.start_button.pack(side="left", padx=(6, 4))
        self.cancel_button = ttk.Button(controls, text="Cancel", command=self._cancel, state="disabled")
        self.cancel_button.pack(side="left")

        self.status_var = self.tk.StringVar(value="Ready")
        ttk.Label(self.frame, textvariable=self.status_var).pack(fill="x")

        info = ttk.Frame(self.frame)
        info.pack(fill="x", pady=(2, 6))
        self.resource_var = self.tk.StringVar(value="Resources: 0 samples / 0 bytes")
        self.tile_var = self.tk.StringVar(value="Tile: -")
        ttk.Label(info, textvariable=self.resource_var).pack(side="left")
        ttk.Label(info, textvariable=self.tile_var).pack(side="right")

    def _build_canvas(self) -> None:
        self.canvas = self.tk.Canvas(self.frame, width=860, height=500, background="#151923", highlightthickness=0)
        self.canvas.pack(fill="both", expand=True)
        self.canvas.bind("<Configure>", lambda _event: self._draw())

    def _start(self) -> None:
        if self.worker is not None and self.worker.is_alive():
            return
        try:
            names = self._selected_glyphs()
            size = float(self.size_var.get())
            subpixel_x = float(self.subpixel_x_var.get())
            subpixel_y = float(self.subpixel_y_var.get())
            if size <= 0:
                raise ValueError("size must be positive")
        except (ValueError, GlyphAtlasError) as exc:
            self.status_var.set(str(exc))
            return
        target = TargetConfig(
            name=f"gui-{size:g}",
            pixel_size=size,
            subpixel_x=subpixel_x,
            subpixel_y=subpixel_y,
        )
        run_config = dataclasses.replace(
            self.config,
            glyphs=tuple(names),
            targets=(target,),
            output_dir=self.config.output_dir / "gui-preview",
            snapshot_dir=self.config.snapshot_dir / "gui-preview",
            temp_dir=self.config.temp_dir / "gui-preview",
        )
        self.cancel_event = threading.Event()
        self.completed_tiles.clear()
        self.preview = np.zeros((1, 1), dtype=np.uint8)
        self.start_button.configure(state="disabled")
        self.cancel_button.configure(state="normal")
        self.status_var.set("Rasterizing...")
        self.resource_var.set("Resources: 0 samples / 0 bytes")
        self.worker = threading.Thread(
            target=self._worker,
            args=(run_config, self.cancel_event),
            name="gui-raster",
            daemon=True,
        )
        self.worker.start()
        self.root.after(40, self._poll)

    def _selected_glyphs(self) -> list[str]:
        by_codepoint: dict[int, str] = {}
        for glyph in self.font.glyphs:
            for codepoint in glyph.unicode:
                by_codepoint[codepoint] = glyph.name
        names: list[str] = []
        for character in self.text_var.get():
            name = by_codepoint.get(ord(character))
            if name is None:
                fail(
                    E_CONFIG_INVALID,
                    "Text contains a code point absent from the local font",
                    character=character,
                    codepoint=ord(character),
                )
            if name not in names:
                names.append(name)
        if not names:
            fail(E_CONFIG_INVALID, "Enter at least one mapped character")
        return names

    def _worker(self, config: WorkConfig, cancel_event: threading.Event) -> None:
        try:
            report = run_loaded(
                config,
                self.font,
                resume=False,
                progress=self.events.put,
                cancel_event=cancel_event,
            )
            self.events.put({"event": "done", "report": report})
        except GlyphAtlasError as exc:
            self.events.put({"event": "error", "error": exc.as_dict()})
        except Exception as exc:  # pragma: no cover - GUI safety net
            self.events.put(
                {
                    "event": "error",
                    "error": {"code": "E_GUI", "message": str(exc), "details": {}},
                }
            )

    def _cancel(self) -> None:
        self.cancel_event.set()
        self.status_var.set("Cancellation requested; committed tiles remain on disk")

    def _poll(self) -> None:
        try:
            while True:
                event = self.events.get_nowait()
                if event.get("event") == "tile_commit":
                    self._on_tile(event)
                elif event.get("event") == "done":
                    self._on_done(event["report"])
                elif event.get("event") == "error":
                    self._on_error(event["error"])
        except queue.Empty:
            pass
        if self.worker is not None and self.worker.is_alive():
            self.root.after(40, self._poll)

    def _on_tile(self, event: dict[str, Any]) -> None:
        width = int(event["width"])
        height = int(event["height"])
        if self.preview.shape != (height, width):
            self.preview = np.zeros((height, width), dtype=np.uint8)
        coord = tuple(int(value) for value in event["coord"])
        tile = np.asarray(event["coverage"], dtype=np.uint8)
        x = coord[0] * int(event["tile_width"])
        y = coord[1] * int(event["tile_height"])
        self.preview[y : y + tile.shape[0], x : x + tile.shape[1]] = tile
        self.completed_tiles.add(coord)
        self.current_glyph = str(event["glyph"])
        self.tile_width = int(event["tile_width"])
        self.tile_height = int(event["tile_height"])
        self.resource_var.set(
            "Resources: {samples} samples / {memory} bytes".format(
                samples=event["sample_points"],
                memory=event["memory_bytes"],
            )
        )
        self.tile_var.set(f"Tile {coord[0]},{coord[1]}  {event['committed']}/{event['total']}")
        self.status_var.set(f"Rasterizing {self.current_glyph}")
        self._draw()

    def _on_done(self, report: Any) -> None:
        self.start_button.configure(state="normal")
        self.cancel_button.configure(state="disabled")
        target_run = report.targets[0]
        result = next(
            (glyph for glyph in target_run.schedule.glyphs if not glyph.plan.glyph.blank),
            target_run.schedule.glyphs[0],
        )
        self.preview = result.coverage
        self.current_glyph = result.glyph_name
        self.completed_tiles = {tile.coord for tile in result.tiles}
        self.status_var.set(
            f"Done: {result.glyph_name}, {result.plan.width}x{result.plan.height}, "
            f"{result.sample_points} samples"
        )
        self.resource_var.set(
            f"Resources: {result.sample_points} samples / {result.memory_bytes} bytes  "
            f"atlas {target_run.atlas.atlas_sha256[:12]}"
        )
        self._draw()

    def _on_error(self, error: dict[str, Any]) -> None:
        self.start_button.configure(state="normal")
        self.cancel_button.configure(state="disabled")
        if error.get("code") == E_CANCELLED:
            self.status_var.set("Cancelled; resume from the committed disk snapshot")
        else:
            details = error.get("details", {})
            self.status_var.set(f"{error.get('code')}: {error.get('message')} {details}")
        self._draw()

    def _draw(self) -> None:
        self.canvas.delete("all")
        width = max(1, self.preview.shape[1])
        height = max(1, self.preview.shape[0])
        canvas_width = max(100, self.canvas.winfo_width())
        canvas_height = max(100, self.canvas.winfo_height())
        scale = max(1, int(min((canvas_width - 40) / width, (canvas_height - 60) / height)))
        left = (canvas_width - width * scale) // 2
        top = (canvas_height - height * scale) // 2
        for row in range(height):
            for column in range(width):
                value = int(self.preview[row, column])
                if value == 0:
                    continue
                color = f"#{value:02x}{value:02x}{value:02x}"
                x0 = left + column * scale
                y0 = top + row * scale
                self.canvas.create_rectangle(
                    x0,
                    y0,
                    x0 + scale,
                    y0 + scale,
                    fill=color,
                    outline="",
                )
        for tile_x, tile_y in sorted(self.completed_tiles):
            x0 = left + tile_x * self.tile_width * scale
            y0 = top + tile_y * self.tile_height * scale
            x1 = left + min(width, (tile_x + 1) * self.tile_width) * scale
            y1 = top + min(height, (tile_y + 1) * self.tile_height) * scale
            self.canvas.create_rectangle(
                x0,
                y0,
                x1,
                y1,
                fill="#36c98f",
                stipple="gray25",
                outline="#36c98f",
                width=2,
            )
        for tile_x in range(0, width + 1, self.tile_width):
            x = left + tile_x * scale
            self.canvas.create_line(x, top, x, top + height * scale, fill="#334155")
        for tile_y in range(0, height + 1, self.tile_height):
            y = top + tile_y * scale
            self.canvas.create_line(left, y, left + width * scale, y, fill="#334155")
        self.canvas.create_text(
            left,
            max(12, top - 12),
            anchor="w",
            fill="#cbd5e1",
            text=f"{self.current_glyph or 'preview'}  {width}x{height}  x{scale}",
        )

