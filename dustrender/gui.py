"""Local interactive preview with deterministic rendering controls."""

from __future__ import annotations

import queue
import subprocess
import sys
import tempfile
import threading
from pathlib import Path
from typing import Any

from .chunks import Chunk
from .config import RenderConfig
from .errors import DustRenderError
from .scene import Scene
from .scheduler import RenderRun, RenderScheduler
from .util import write_ppm


class PreviewApp:
    def __init__(self, root: Any, scene: Scene, config: RenderConfig) -> None:
        import tkinter as tk
        from tkinter import ttk

        self.tk = tk
        self.ttk = ttk
        self.root = root
        self.scene = scene
        self.config = config
        self.scheduler = RenderScheduler()
        self.events: queue.Queue[tuple[str, Any]] = queue.Queue()
        self.run: RenderRun | None = None
        self._tempdir = tempfile.TemporaryDirectory(prefix="dustrender-gui-")
        self._photo = None

        root.title(f"DustRender - {scene.scene_id}")
        root.protocol("WM_DELETE_WINDOW", self._close)
        main = ttk.Frame(root, padding=8)
        main.pack(fill="both", expand=True)

        self.zoom = max(1, min(16, 720 // max(1, scene.width), 540 // max(1, scene.height)))
        self.canvas = tk.Canvas(
            main,
            width=min(720, scene.width * self.zoom),
            height=min(540, scene.height * self.zoom),
            background="#11151d",
            highlightthickness=0,
        )
        self.canvas.grid(row=0, column=0, columnspan=4, sticky="nsew")
        main.columnconfigure(0, weight=1)
        main.rowconfigure(0, weight=1)

        self.status = tk.StringVar(value="ready")
        ttk.Label(main, textvariable=self.status).grid(
            row=1, column=0, columnspan=4, sticky="w", pady=(6, 4)
        )

        controls = ttk.Frame(main)
        controls.grid(row=2, column=0, columnspan=4, sticky="ew")
        self.chunk_w = self._scale(controls, "Chunk W", 2, 64, config.chunk_width, 0)
        self.chunk_h = self._scale(controls, "Chunk H", 2, 64, config.chunk_height, 1)
        self.parallelism = self._scale(controls, "Parallel", 1, 16, config.parallelism, 2)
        self.budget = self._scale(
            controls, "Budget", 100, 2_000_000, min(2_000_000, config.task_budget), 3
        )
        ttk.Label(controls, text="Precision").grid(row=0, column=4, padx=4, sticky="w")
        self.precision = tk.StringVar(value=config.precision)
        ttk.Combobox(
            controls,
            textvariable=self.precision,
            values=["float", "fixed"],
            width=8,
            state="readonly",
        ).grid(row=1, column=4, padx=4, sticky="ew")

        buttons = ttk.Frame(main)
        buttons.grid(row=3, column=0, columnspan=4, sticky="ew", pady=(6, 0))
        self.render_button = ttk.Button(buttons, text="Render", command=self._render)
        self.render_button.pack(side="left")
        self.cancel_button = ttk.Button(
            buttons, text="Cancel", command=self._cancel, state="disabled"
        )
        self.cancel_button.pack(side="left", padx=4)
        ttk.Button(buttons, text="OpenGL", command=self._open_gl).pack(side="left", padx=4)

        self.curve = tk.Canvas(main, height=90, background="#151b24", highlightthickness=0)
        self.curve.grid(row=4, column=0, columnspan=4, sticky="ew", pady=(8, 0))
        self.completed: set[str] = set()
        self.budget_points: list[tuple[int, int]] = []
        self.chunks: list[Chunk] = []
        self._draw_empty()
        root.after(40, self._pump)

    def _scale(
        self,
        parent: Any,
        label: str,
        start: int,
        end: int,
        value: int,
        column: int,
    ) -> Any:
        frame = self.ttk.Frame(parent)
        frame.grid(row=0, column=column, rowspan=2, padx=4, sticky="w")
        self.ttk.Label(frame, text=label).pack(anchor="w")
        variable = self.tk.IntVar(value=max(start, min(end, int(value))))
        self.ttk.Scale(
            frame,
            from_=start,
            to=end,
            orient="horizontal",
            variable=variable,
            length=120,
        ).pack()
        return variable

    def _draw_empty(self) -> None:
        self.canvas.delete("all")
        self.canvas.create_text(
            self.canvas.winfo_reqwidth() // 2,
            self.canvas.winfo_reqheight() // 2,
            text="Render to preview",
            fill="#8892a0",
        )

    def _current_config(self) -> RenderConfig:
        return RenderConfig.from_dict(
            {
                **self.config.to_dict(),
                "chunk_width": int(self.chunk_w.get()),
                "chunk_height": int(self.chunk_h.get()),
                "parallelism": int(self.parallelism.get()),
                "task_budget": int(self.budget.get()),
                "precision": self.precision.get(),
            }
        )

    def _render(self) -> None:
        if self.render_button["state"] == "disabled":
            return
        self.config = self._current_config()
        self.render_button.configure(state="disabled")
        self.cancel_button.configure(state="normal")
        self.completed.clear()
        self.budget_points.clear()
        self.status.set("rendering (old frame remains visible until atomic publish)")

        def work() -> None:
            try:
                def progress(results, statuses, step) -> None:
                    self.events.put(("progress", (set(statuses), list(results.values()), step)))

                run = self.scheduler.render(self.scene, self.config, on_progress=progress)
                self.events.put(("done", run))
            except Exception as exc:
                self.events.put(("error", exc))

        threading.Thread(target=work, daemon=True).start()

    def _cancel(self) -> None:
        self.scheduler.request_cancel()
        self.status.set("cancellation requested")

    def _pump(self) -> None:
        try:
            while True:
                kind, payload = self.events.get_nowait()
                if kind == "progress":
                    statuses, results, step = payload
                    self.completed = {chunk_id for chunk_id in statuses if chunk_id.startswith("c")}
                    self.budget_points = [
                        (index, result.samples) for index, result in enumerate(results)
                    ]
                    self._draw_curve()
                    self.status.set(
                        f"rendering: {len(self.completed)} chunks, "
                        f"{sum(item.samples for item in results)} samples, step {step:.6g}"
                    )
                elif kind == "done":
                    self._finish(payload)
                elif kind == "error":
                    self.status.set(f"error: {payload}")
                    self._enable_controls()
        except queue.Empty:
            pass
        self.root.after(40, self._pump)

    def _finish(self, run: RenderRun) -> None:
        self.run = run
        self.completed = {
            chunk_id for chunk_id, status in run.statuses.items() if status == "completed"
        }
        self.chunks = run.chunks
        ppm = Path(self._tempdir.name) / "preview.ppm"
        write_ppm(ppm, run.image)
        self._photo = self.tk.PhotoImage(file=str(ppm))
        if self.zoom > 1:
            self._photo = self._photo.zoom(self.zoom, self.zoom)
        self.canvas.delete("all")
        self.canvas.create_image(
            0,
            0,
            anchor="nw",
            image=self._photo,
            tags="image",
        )
        xscale = float(self.zoom)
        yscale = float(self.zoom)
        for chunk in run.chunks:
            color = "#44ffaa" if chunk.id in self.completed else "#ff5566"
            self.canvas.create_rectangle(
                chunk.x0 * xscale,
                chunk.y0 * yscale,
                chunk.x1 * xscale,
                chunk.y1 * yscale,
                outline=color,
                width=1,
            )
            if chunk.id in self.completed:
                self.canvas.create_rectangle(
                    chunk.x0 * xscale,
                    chunk.y0 * yscale,
                    chunk.x1 * xscale,
                    chunk.y1 * yscale,
                    fill="#29c98c",
                    stipple="gray50",
                    outline="",
                )
        self.budget_points = [
            (index, result.samples)
            for index, result in enumerate(run.results.values())
        ]
        self._draw_curve()
        if run.error is None:
            self.status.set(
                f"published v{len(self.budget_points)} | "
                f"{run.summary()['samples']['total']} samples | "
                f"step {run.effective_step:.6g}"
            )
        else:
            self.status.set(
                f"render stopped with {run.error.code}; completed chunks preserved"
            )
        self._enable_controls()

    def _enable_controls(self) -> None:
        self.render_button.configure(state="normal")
        self.cancel_button.configure(state="disabled")

    def _draw_curve(self) -> None:
        self.curve.delete("all")
        width = max(1, self.curve.winfo_width())
        height = self.curve.winfo_height()
        if not self.budget_points:
            self.curve.create_text(8, height // 2, anchor="w", text="budget usage", fill="#778395")
            return
        maximum = max(1, max(value for _, value in self.budget_points))
        count = max(2, len(self.budget_points))
        points = []
        for index, value in self.budget_points:
            x = 8 + (width - 16) * index / max(1, count - 1)
            y = height - 8 - (height - 16) * value / maximum
            points.extend([x, y])
        self.curve.create_line(*points, fill="#62b6ff", width=2)
        budget = int(self.budget.get())
        budget_y = height - 8 - (height - 16) * min(1.0, budget / maximum)
        self.curve.create_line(8, budget_y, width - 8, budget_y, fill="#ffcc66", dash=(4, 3))
        self.curve.create_text(8, 4, anchor="nw", text=f"max {maximum}", fill="#9aa6b6")

    def _open_gl(self) -> None:
        if self.run is None:
            self.status.set("render first, then open the OpenGL window")
            return
        path = Path(self._tempdir.name) / "opengl-preview.npy"
        import numpy as np

        np.save(path, self.run.image)
        subprocess.Popen([sys.executable, "-m", "dustrender.glview", str(path)])

    def _close(self) -> None:
        self.scheduler.request_cancel()
        self._tempdir.cleanup()
        self.root.destroy()


def launch_gui(scene_path: str, config: RenderConfig) -> None:
    try:
        import tkinter as tk
    except ImportError as exc:
        raise DustRenderError(
            "E_GUI_UNAVAILABLE", "tkinter is not available in this Python installation", "$gui"
        ) from exc
    scene = Scene.load(scene_path)
    root = tk.Tk()
    PreviewApp(root, scene, config)
    root.mainloop()
