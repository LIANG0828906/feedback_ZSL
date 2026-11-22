"""Tk desktop workbench for deterministic NPR rendering."""

from __future__ import annotations

import json
import math
import os
import queue
import tempfile
from concurrent.futures import Future, ThreadPoolExecutor
from pathlib import Path
from typing import Any

import tkinter as tk
from tkinter import messagebox, ttk

import numpy as np

from .checkpoint import CheckpointStore, RunIdentity
from .consistency import difference_heatmap
from .engine import NprEngine, RenderControl
from .errors import DivergenceError, NprError, VersionConflictError
from .model import Camera, NprFrame, RenderParams, Scene, Snapshot
from .npr import coverage_preview, level_preview, outline_preview
from .scene import load_scene
from .tiles import FaultInjector, make_tiles

BG = "#0b0f16"
PANEL = "#131a24"
PANEL_2 = "#1b2430"
FG = "#eee6d6"
MUTED = "#8591a3"
ACCENT = "#f0b24d"
TEAL = "#58c7b9"
DANGER = "#ee6b68"
PREVIEW_COLOR = "#d39133"
EXACT_COLOR = "#3fa58b"

DISPLAY_WIDTH = 720
DISPLAY_HEIGHT = 540


def _rgb_to_photo(root: tk.Misc, rgb: np.ndarray) -> tk.PhotoImage:
    image = np.ascontiguousarray(np.clip(rgb, 0, 255).astype(np.uint8))
    header = f"P6\n{image.shape[1]} {image.shape[0]}\n255\n".encode("ascii")
    fd, temp_name = tempfile.mkstemp(prefix=".npr-frame.", suffix=".ppm")
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(header)
            handle.write(image.tobytes())
        photo = tk.PhotoImage(master=root, file=temp_name)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
    zoom_x = max(1, DISPLAY_WIDTH // image.shape[1])
    zoom_y = max(1, DISPLAY_HEIGHT // image.shape[0])
    if zoom_x != 1 or zoom_y != 1:
        photo = photo.zoom(zoom_x, zoom_y)
    return photo


class WorkbenchApp:
    def __init__(
        self,
        root: tk.Tk,
        *,
        data_dir: Path,
        scene_name: str,
        persist_dir: Path,
    ) -> None:
        self.root = root
        self.data_dir = data_dir
        self.persist_dir = persist_dir
        self.state_path = persist_dir / "ui_state.json"
        self.diagnostics_dir = Path("diagnostics")
        self.scene_names = self._discover_scenes()
        if scene_name not in self.scene_names and self.scene_names:
            scene_name = self.scene_names[0]
        self.scene_name = scene_name
        self.scene: Scene
        self.engine: NprEngine
        self.current_frame: NprFrame | None = None
        self.preview_frame: NprFrame | None = None
        self.diff_rgb: np.ndarray | None = None
        self.images: dict[str, tk.PhotoImage] = {}
        self.canvases: dict[str, tk.Canvas] = {}
        self.frame_variants: dict[str, np.ndarray] = {}
        self.exact_executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="npr-exact-ui")
        self.result_queue: queue.Queue[tuple[str, int, Future[Any]]] = queue.Queue()
        self.exact_future: Future[Any] | None = None
        self.exact_running = False
        self.exact_token = 0
        self.exact_pending = False
        self.exact_preview_used: NprFrame | None = None
        self.control: RenderControl | None = None
        self.paused_result: Any = None
        self.last_failed_tiles: tuple[int, ...] = ()
        self.fault_injector: FaultInjector | None = None
        self._after_exact: str | None = None
        self._after_state: str | None = None
        self._drag_origin: tuple[int, int] | None = None
        self._closing = False
        self._last_metrics: dict[str, Any] = {}
        self._configure_window()
        self._load_scene_and_engine(scene_name)
        self._build_ui()
        self._restore_controls()
        self.root.protocol("WM_DELETE_WINDOW", self._close)
        self.root.after(40, self._initial_render)
        self.root.after(50, self._poll_results)

    def _configure_window(self) -> None:
        self.root.title("NPR Workbench — deterministic stroke studio")
        self.root.geometry("1180x760")
        self.root.minsize(1040, 700)
        self.root.configure(bg=BG)
        style = ttk.Style(self.root)
        try:
            style.theme_use("clam")
        except tk.TclError:
            pass
        style.configure("App.TFrame", background=BG)
        style.configure("Panel.TFrame", background=PANEL)
        style.configure("Panel2.TFrame", background=PANEL_2)
        style.configure("TLabel", background=PANEL, foreground=FG, font=("TkDefaultFont", 10))
        style.configure(
            "Heading.TLabel",
            background=PANEL,
            foreground=FG,
            font=("TkDefaultFont", 15, "bold"),
        )
        style.configure(
            "Sub.TLabel", background=PANEL, foreground=MUTED, font=("TkDefaultFont", 9)
        )
        style.configure(
            "Value.TLabel", background=PANEL, foreground=ACCENT, font=("TkFixedFont", 10)
        )
        style.configure(
            "Status.TLabel", background=PANEL_2, foreground=FG, font=("TkFixedFont", 10)
        )
        style.configure("TButton", padding=(8, 5), background=PANEL_2, foreground=FG)
        style.map("TButton", background=[("active", "#293545")], foreground=[("disabled", MUTED)])
        style.configure("Accent.TButton", background=ACCENT, foreground="#15100a")
        style.map("Accent.TButton", background=[("active", "#ffd27a")])
        style.configure("Danger.TButton", background="#5a2729", foreground=FG)
        style.map("Danger.TButton", background=[("active", "#7b3336")])
        style.configure("TScale", background=PANEL, troughcolor=PANEL_2)
        style.configure("TNotebook", background=BG, borderwidth=0)
        style.configure("TNotebook.Tab", background=PANEL_2, foreground=MUTED, padding=(12, 7))
        style.map(
            "TNotebook.Tab",
            background=[("selected", BG)],
            foreground=[("selected", ACCENT)],
        )
        style.configure("TCombobox", fieldbackground=PANEL_2, background=PANEL_2, foreground=FG)
        style.map(
            "TCombobox",
            fieldbackground=[("readonly", PANEL_2), ("active", PANEL_2)],
            foreground=[("readonly", FG), ("active", FG)],
            selectbackground=[("readonly", PANEL_2)],
            selectforeground=[("readonly", FG)],
        )
        style.configure("TSpinbox", fieldbackground=PANEL_2, foreground=FG)
        self.root.option_add("*TCombobox*Listbox.background", PANEL_2)
        self.root.option_add("*TCombobox*Listbox.foreground", FG)
        self.root.option_add("*TCombobox*Listbox.selectBackground", ACCENT)
        self.root.option_add("*TCombobox*Listbox.selectForeground", "#15100a")

    def _discover_scenes(self) -> list[str]:
        manifest_path = self.data_dir / "manifest.json"
        if manifest_path.exists():
            data = json.loads(manifest_path.read_text(encoding="utf-8"))
            return [item["scene"] for item in data.get("scenes", [])]
        return sorted(path.stem for path in (self.data_dir / "scenes").glob("*.json"))

    def _load_ui_state(self, scene_name: str) -> dict[str, Any] | None:
        if not self.state_path.exists():
            return None
        try:
            state = json.loads(self.state_path.read_text(encoding="utf-8"))
            if state.get("scene") != scene_name:
                return None
            return state
        except (OSError, ValueError, json.JSONDecodeError):
            return None

    def _load_scene_and_engine(self, scene_name: str) -> None:
        self.scene_name = scene_name
        self.scene = load_scene(self.data_dir / "scenes" / f"{scene_name}.json")
        state = self._load_ui_state(scene_name)
        initial_version = int(state.get("version", 1)) if state else 1
        try:
            camera = (
                Camera.from_dict(state["camera"])
                if state and "camera" in state
                else self.scene.camera_poses[0]
            )
            params = (
                RenderParams.from_dict(state["params"])
                if state and "params" in state
                else RenderParams()
            )
        except (ValueError, KeyError, TypeError):
            initial_version = 1
            camera = self.scene.camera_poses[0]
            params = RenderParams()
        self.engine = NprEngine(
            self.scene,
            params,
            camera,
            persist_dir=self.persist_dir / "checkpoints",
            diagnostics_dir=self.diagnostics_dir,
            initial_version=initial_version,
        )
        self.current_frame = None
        self.preview_frame = None
        self.diff_rgb = None
        self.paused_result = None
        self.last_failed_tiles = ()

    def _build_ui(self) -> None:
        container = ttk.Frame(self.root, style="App.TFrame")
        container.pack(fill="both", expand=True, padx=10, pady=10)
        left = ttk.Frame(container, style="Panel.TFrame", width=272)
        left.pack(side="left", fill="y", padx=(0, 10))
        left.pack_propagate(False)
        right = ttk.Frame(container, style="App.TFrame")
        right.pack(side="left", fill="both", expand=True)

        ttk.Label(left, text="NPR / WORKBENCH", style="Heading.TLabel").pack(
            anchor="w", padx=14, pady=(14, 2)
        )
        ttk.Label(
            left,
            text="deterministic preview · tiled exact · crash-aware",
            style="Sub.TLabel",
            wraplength=240,
            justify="left",
        ).pack(anchor="w", padx=14, pady=(0, 12))

        ttk.Label(left, text="SCENE", style="Sub.TLabel").pack(anchor="w", padx=14)
        self.scene_var = tk.StringVar(value=self.scene_name)
        self.scene_combo = ttk.Combobox(
            left,
            textvariable=self.scene_var,
            values=self.scene_names,
            state="readonly",
        )
        self.scene_combo.pack(fill="x", padx=14, pady=(3, 10))
        self.scene_combo.bind("<<ComboboxSelected>>", self._on_scene_changed)

        ttk.Label(left, text="CAMERA POSE", style="Sub.TLabel").pack(anchor="w", padx=14)
        pose_values = [camera.id for camera in self.scene.camera_poses] + ["custom"]
        current_camera = self.engine.store.latest().camera
        self.pose_var = tk.StringVar(
            value=current_camera.id if current_camera.id in pose_values else "custom"
        )
        self.pose_combo = ttk.Combobox(
            left,
            textvariable=self.pose_var,
            values=pose_values,
            state="readonly",
        )
        self.pose_combo.pack(fill="x", padx=14, pady=(3, 12))
        self.pose_combo.bind("<<ComboboxSelected>>", self._on_pose_changed)

        ttk.Separator(left).pack(fill="x", padx=14, pady=5)
        ttk.Label(left, text="STYLIZATION", style="Sub.TLabel").pack(
            anchor="w", padx=14, pady=(7, 4)
        )
        latest_params = self.engine.store.latest().params
        self.levels_var = tk.IntVar(value=latest_params.levels)
        self.levels_value = tk.StringVar(value=str(latest_params.levels))
        self._slider_row(
            left,
            "Tone levels",
            self.levels_var,
            2,
            12,
            1,
            self.levels_value,
        )
        self.density_var = tk.DoubleVar(value=latest_params.density)
        self.density_value = tk.StringVar(value=f"{latest_params.density:.2f}")
        self._slider_row(
            left,
            "Stroke density",
            self.density_var,
            0.02,
            1.0,
            0.01,
            self.density_value,
        )
        self.length_var = tk.DoubleVar(value=latest_params.length)
        self.length_value = tk.StringVar(value=f"{latest_params.length:.1f}")
        self._slider_row(
            left,
            "Stroke length",
            self.length_var,
            2.0,
            40.0,
            0.5,
            self.length_value,
        )

        ttk.Separator(left).pack(fill="x", padx=14, pady=8)
        ttk.Label(left, text="RENDER CONTROL", style="Sub.TLabel").pack(
            anchor="w", padx=14, pady=(2, 5)
        )
        button_grid = ttk.Frame(left, style="Panel.TFrame")
        button_grid.pack(fill="x", padx=12)
        self.pause_button = ttk.Button(button_grid, text="Pause", command=self._pause)
        self.pause_button.grid(row=0, column=0, sticky="ew", padx=2, pady=2)
        self.resume_button = ttk.Button(
            button_grid, text="Resume", style="Accent.TButton", command=self._resume
        )
        self.resume_button.grid(row=0, column=1, sticky="ew", padx=2, pady=2)
        self.cancel_button = ttk.Button(
            button_grid, text="Cancel", style="Danger.TButton", command=self._cancel
        )
        self.cancel_button.grid(row=1, column=0, sticky="ew", padx=2, pady=2)
        self.retry_button = ttk.Button(
            button_grid, text="Retry failed", command=self._retry_failed
        )
        self.retry_button.grid(row=1, column=1, sticky="ew", padx=2, pady=2)
        self.recalc_button = ttk.Button(
            button_grid, text="Force exact", command=self._force_exact
        )
        self.recalc_button.grid(row=2, column=0, columnspan=2, sticky="ew", padx=2, pady=(5, 2))
        button_grid.columnconfigure(0, weight=1)
        button_grid.columnconfigure(1, weight=1)

        fault_row = ttk.Frame(left, style="Panel.TFrame")
        fault_row.pack(fill="x", padx=12, pady=(6, 0))
        ttk.Label(fault_row, text="Fault", style="Sub.TLabel").pack(side="left")
        self.fault_tile_var = tk.IntVar(value=3)
        self.fault_spin = ttk.Spinbox(
            fault_row,
            from_=0,
            to=999,
            width=5,
            textvariable=self.fault_tile_var,
        )
        self.fault_spin.pack(side="left", padx=(8, 4))
        self.inject_button = ttk.Button(
            fault_row, text="Inject", width=6, command=self._inject_fault
        )
        self.inject_button.pack(side="left", padx=2)
        self.clear_fault_button = ttk.Button(
            fault_row, text="Clear", width=5, command=self._clear_fault
        )
        self.clear_fault_button.pack(side="left", padx=2)

        ttk.Label(left, text="TELEMETRY", style="Sub.TLabel").pack(
            anchor="w", padx=14, pady=(12, 4)
        )
        self.metrics_var = tk.StringVar(value="waiting for first preview…")
        metrics = ttk.Label(
            left,
            textvariable=self.metrics_var,
            style="Value.TLabel",
            justify="left",
            wraplength=240,
        )
        metrics.pack(anchor="w", fill="x", padx=14)
        self.error_var = tk.StringVar(value="")
        self.error_label = tk.Label(
            left,
            textvariable=self.error_var,
            bg=PANEL,
            fg=DANGER,
            justify="left",
            anchor="nw",
            wraplength=240,
            font=("TkFixedFont", 9),
        )
        self.error_label.pack(fill="both", expand=True, padx=14, pady=(10, 4))
        ttk.Label(
            left,
            text="Drag the image to orbit. Release pauses interaction; exact render follows.",
            style="Sub.TLabel",
            wraplength=240,
            justify="left",
        ).pack(anchor="w", padx=14, pady=(4, 12))

        self.notebook = ttk.Notebook(right)
        self.notebook.pack(fill="both", expand=True)
        labels = [
            ("styled", "STYLIZED"),
            ("base", "BASE SHADING"),
            ("poster", "POSTERIZED"),
            ("levels", "LEVEL MAP"),
            ("outline", "OUTLINE MASK"),
            ("brush", "BRUSH LAYER"),
            ("diff", "DIFFERENCE"),
        ]
        for key, label in labels:
            frame = tk.Frame(self.notebook, bg=BG)
            canvas = tk.Canvas(
                frame,
                width=DISPLAY_WIDTH,
                height=DISPLAY_HEIGHT,
                bg="#070a0f",
                highlightthickness=1,
                highlightbackground="#303b49",
                cursor="crosshair",
            )
            canvas.pack(fill="both", expand=True)
            canvas.bind("<ButtonPress-1>", self._drag_start)
            canvas.bind("<B1-Motion>", self._drag_move)
            canvas.bind("<ButtonRelease-1>", self._drag_end)
            canvas.bind(
                "<Configure>",
                lambda _event, tab_key=key: self._on_canvas_configure(tab_key),
            )
            self.notebook.add(frame, text=label)
            self.canvases[key] = canvas
        self.notebook.bind("<<NotebookTabChanged>>", self._on_tab_changed)
        self._refresh_buttons()

        status_frame = ttk.Frame(right, style="Panel2.TFrame")
        status_frame.pack(fill="x", pady=(8, 0))
        self.mode_var = tk.StringVar(value="IDLE")
        self.mode_label = tk.Label(
            status_frame,
            textvariable=self.mode_var,
            bg=PANEL_2,
            fg=MUTED,
            padx=10,
            pady=5,
            font=("TkFixedFont", 10, "bold"),
        )
        self.mode_label.pack(side="left")
        self.status_var = tk.StringVar(value="Loading scene…")
        ttk.Label(status_frame, textvariable=self.status_var, style="Status.TLabel").pack(
            side="left", fill="x", expand=True, padx=10
        )

    def _slider_row(
        self,
        parent: ttk.Frame,
        label: str,
        variable: tk.Variable,
        start: float,
        end: float,
        resolution: float,
        value_text: tk.StringVar,
    ) -> None:
        row = ttk.Frame(parent, style="Panel.TFrame")
        row.pack(fill="x", padx=14, pady=(3, 2))
        ttk.Label(row, text=label, background=PANEL, foreground=FG).pack(side="left")
        ttk.Label(row, textvariable=value_text, style="Value.TLabel").pack(side="right")
        scale = ttk.Scale(
            parent,
            from_=start,
            to=end,
            variable=variable,
            command=lambda _value: self._on_control_changed(),
        )
        scale.pack(fill="x", padx=14, pady=(0, 5))

    def _restore_controls(self) -> None:
        if self.current_frame is None:
            self.metrics_var.set(
                f"version {self.engine.store.version}\n"
                f"preview {self.engine.store.latest().params.preview_width}×"
                f"{self.engine.store.latest().params.preview_height}\n"
                f"exact   {self.engine.store.latest().params.exact_width}×"
                f"{self.engine.store.latest().params.exact_height}"
            )

    def _initial_render(self) -> None:
        if self._closing:
            return
        self.status_var.set("Warming deterministic stroke plan…")
        self.root.update_idletasks()
        self._render_preview_now()

    def _on_scene_changed(self, _event: object = None) -> None:
        if self.exact_running and self.control is not None:
            self.control.request_cancel()
        self.exact_token += 1
        self.exact_running = False
        try:
            self._load_scene_and_engine(self.scene_var.get())
        except NprError as exc:
            self._show_error(exc)
            self.scene_var.set(self.scene_name)
            return
        self._schedule_state_save()
        self.pose_var.set(self.scene.camera_poses[0].id)
        self.levels_var.set(self.engine.store.latest().params.levels)
        self.density_var.set(self.engine.store.latest().params.density)
        self.length_var.set(self.engine.store.latest().params.length)
        self._render_preview_now()

    def _on_pose_changed(self, _event: object = None) -> None:
        pose_id = self.pose_var.get()
        if pose_id == "custom":
            return
        try:
            camera = next(camera for camera in self.scene.camera_poses if camera.id == pose_id)
        except StopIteration:
            return
        snapshot = self.engine.store.update(camera=camera)
        self._schedule_state_save(snapshot)
        self._render_preview_now()

    def _on_control_changed(self) -> None:
        if not all(hasattr(self, name) for name in ("levels_var", "density_var", "length_var")):
            return
        levels = int(round(self.levels_var.get()))
        density = float(self.density_var.get())
        length = float(self.length_var.get())
        self.levels_value.set(str(levels))
        self.density_value.set(f"{density:.2f}")
        self.length_value.set(f"{length:.1f}")
        try:
            params = self.engine.store.latest().params.with_controls(
                levels=levels,
                density=density,
                length=length,
            )
            snapshot = self.engine.store.update(params=params)
        except NprError as exc:
            self._show_error(exc)
            return
        self._schedule_state_save(snapshot)
        self._render_preview_now()

    def _drag_start(self, event: tk.Event[tk.Canvas]) -> None:
        self._drag_origin = (int(event.x), int(event.y))
        self._cancel_scheduled_exact()

    def _drag_move(self, event: tk.Event[tk.Canvas]) -> None:
        if self._drag_origin is None:
            return
        old_x, old_y = self._drag_origin
        dx = int(event.x) - old_x
        dy = int(event.y) - old_y
        if dx == 0 and dy == 0:
            return
        self._drag_origin = (int(event.x), int(event.y))
        camera = self.engine.store.latest().camera
        target = np.asarray(camera.target, dtype=np.float64)
        offset = np.asarray(camera.position, dtype=np.float64) - target
        distance = float(np.linalg.norm(offset))
        azimuth = math.atan2(float(offset[2]), float(offset[0]))
        elevation = math.asin(max(-0.98, min(0.98, float(offset[1]) / max(distance, 1e-9))))
        azimuth += dx * 0.009
        elevation = max(-1.15, min(1.30, elevation - dy * 0.007))
        horizontal = math.cos(elevation) * distance
        position = (
            target[0] + math.cos(azimuth) * horizontal,
            target[1] + math.sin(elevation) * distance,
            target[2] + math.sin(azimuth) * horizontal,
        )
        next_camera = camera.with_pose(position, camera.target, camera_id="custom")
        snapshot = self.engine.store.update(camera=next_camera)
        self.pose_var.set("custom")
        self._schedule_state_save(snapshot)
        self._render_preview_now()

    def _drag_end(self, _event: tk.Event[tk.Canvas]) -> None:
        self._drag_origin = None
        self._schedule_exact()

    def _render_preview_now(self) -> None:
        self._cancel_scheduled_exact()
        try:
            frame = self.engine.render_preview()
        except NprError as exc:
            self._show_error(exc)
            return
        self.preview_frame = frame
        self.current_frame = frame
        self._set_mode("PREVIEW", PREVIEW_COLOR, frame)
        self._present_frame(frame)
        self._update_metrics(frame)
        self._schedule_exact()

    def _present_frame(self, frame: NprFrame) -> None:
        brush_display = frame.brush_rgba[..., :3].copy()
        brush_display[frame.brush_rgba[..., 3] == 0] = np.asarray([10, 13, 18], dtype=np.uint8)
        self.frame_variants = {
            "styled": frame.styled_rgb,
            "base": frame.base_rgb,
            "poster": frame.poster_rgb,
            "levels": level_preview(frame.levels, frame.snapshot.params.levels),
            "outline": outline_preview(frame.outline),
            "brush": brush_display,
            "diff": self.diff_rgb
            if self.diff_rgb is not None
            else np.zeros((frame.height, frame.width, 3), dtype=np.uint8),
        }
        self._render_active_tab(frame)

    def _render_active_tab(self, frame: NprFrame | None = None) -> None:
        frame = frame or self.current_frame
        if frame is None or not self.frame_variants:
            return
        index = self.notebook.index(self.notebook.select())
        keys = ["styled", "base", "poster", "levels", "outline", "brush", "diff"]
        key = keys[index]
        rgb = self.frame_variants[key]
        canvas = self.canvases[key]
        photo = _rgb_to_photo(self.root, rgb)
        self.images[key] = photo
        canvas.delete("all")
        canvas_width = max(canvas.winfo_width(), photo.width())
        canvas_height = max(canvas.winfo_height(), photo.height())
        canvas.create_image(
            canvas_width // 2,
            canvas_height // 2,
            image=photo,
            anchor="center",
        )
        canvas.configure(scrollregion=(0, 0, photo.width(), photo.height()))

    def _on_tab_changed(self, _event: object = None) -> None:
        self._render_active_tab()

    def _on_canvas_configure(self, key: str) -> None:
        if self.current_frame is None:
            return
        index = self.notebook.index(self.notebook.select())
        keys = ["styled", "base", "poster", "levels", "outline", "brush", "diff"]
        if keys[index] == key:
            self._render_active_tab()

    def _set_mode(self, mode: str, color: str, frame: NprFrame) -> None:
        self.mode_var.set(f" {mode} · v{frame.version} ")
        self.mode_label.configure(fg=color)

    def _update_metrics(self, frame: NprFrame) -> None:
        coverage = float(np.mean(frame.brush_rgba[..., 3].astype(np.float64) / 255.0))
        outline = float(np.mean(frame.outline))
        lines = [
            f"version      {frame.version}",
            f"stroke count {frame.stroke_count}",
            f"stroke alpha {coverage:.4f}",
            f"outline      {outline:.4f}",
            f"seed root    {frame.stroke_fingerprint_root[:12]}…",
        ]
        report = self.engine.last_consistency_report
        if report is not None and hasattr(report, "to_dict"):
            data = report.to_dict()
            lines.extend(
                [
                    f"level Δ      {data['level_histogram_deviation']:.4f}",
                    f"outline Δ    {data['outline_coverage_delta']:.4f}",
                    f"stroke Δ     {data['stroke_coverage_delta']:.4f}",
                    f"outline IoU  {data['outline_iou']:.4f}",
                ]
            )
            if data["first_mismatch_pixel"] is not None:
                lines.append(f"first diff   {data['first_mismatch_pixel']}")
        self.metrics_var.set("\n".join(lines))

    def _cancel_scheduled_exact(self) -> None:
        if self._after_exact is not None:
            self.root.after_cancel(self._after_exact)
            self._after_exact = None

    def _schedule_exact(self) -> None:
        self._cancel_scheduled_exact()
        self._after_exact = self.root.after(260, self._start_exact)

    def _force_exact(self) -> None:
        self._start_exact()

    def _start_exact(self) -> None:
        self._after_exact = None
        if self.exact_running:
            self.exact_pending = True
            return
        if self.preview_frame is None:
            self._render_preview_now()
            return
        latest = self.engine.store.latest()
        if self.preview_frame.snapshot.version != latest.version:
            self._render_preview_now()
            return
        self.exact_token += 1
        token = self.exact_token
        self.exact_running = True
        self.exact_pending = False
        self.control = RenderControl()
        self.exact_preview_used = self.preview_frame
        self.mode_var.set(f" EXACT RENDERING · v{latest.version} ")
        self.mode_label.configure(fg=EXACT_COLOR)
        self.status_var.set("Rendering fixed tiles; commits are atomic and ordered…")
        self._refresh_buttons()
        self.exact_future = self.exact_executor.submit(
            self.engine.render_exact,
            latest,
            preview=self.preview_frame,
            control=self.control,
            fault_injector=self.fault_injector,
        )
        self.exact_future.add_done_callback(
            lambda future, run_token=token: self.result_queue.put(("exact", run_token, future))
        )

    def _poll_results(self) -> None:
        if self._closing:
            return
        try:
            while True:
                kind, token, future = self.result_queue.get_nowait()
                if kind == "exact":
                    self._handle_exact_result(token, future)
        except queue.Empty:
            pass
        self.root.after(50, self._poll_results)

    def _handle_exact_result(self, token: int, future: Future[Any]) -> None:
        if token != self.exact_token:
            return
        self.exact_running = False
        self.exact_future = None
        self._refresh_buttons()
        if self._closing:
            return
        try:
            result = future.result()
        except DivergenceError as exc:
            self.diff_rgb = difference_heatmap(exc.preview, exc.exact)
            self.frame_variants["diff"] = self.diff_rgb
            self._render_active_tab(self.current_frame)
            report = exc.report.to_dict()
            self.mode_var.set(f" E_DIVERGE · v{exc.exact.version} ")
            self.mode_label.configure(fg=DANGER)
            self.status_var.set(
                "Preview/exact tolerance exceeded; diagnostics retained. "
                f"levelΔ={report['level_histogram_deviation']:.4f}, "
                f"outlineΔ={report['outline_coverage_delta']:.4f}, "
                f"strokeΔ={report['stroke_coverage_delta']:.4f}"
            )
            self._append_error("E_DIVERGE", report)
            self._refresh_buttons()
            return
        except VersionConflictError as exc:
            self.mode_var.set(" E_VER ")
            self.mode_label.configure(fg=DANGER)
            self.status_var.set(exc.message)
            self._append_error(exc.code, exc.details)
            self.exact_pending = True
            self._refresh_buttons()
        except NprError as exc:
            self.mode_var.set(f" {exc.code} ")
            self.mode_label.configure(fg=DANGER)
            self.status_var.set(exc.message)
            self._append_error(exc.code, exc.details)
            self._refresh_buttons()
        except Exception as exc:  # pragma: no cover - defensive UI boundary
            self.mode_var.set(" INTERNAL ERROR ")
            self.mode_label.configure(fg=DANGER)
            self.status_var.set(str(exc))
            self._append_error("E_INTERNAL", {"message": str(exc)})
            self._refresh_buttons()
        else:
            if not result.complete:
                self.paused_result = result
                self.last_failed_tiles = result.failed_tiles
                if result.canceled:
                    self.mode_var.set(" CANCELED ")
                    self.status_var.set(
                        f"Canceled after {len(result.committed_tiles)}/{result.total_tiles} "
                        "persisted tiles. Resume uses only committed blocks."
                    )
                elif result.paused:
                    self.mode_var.set(f" PAUSED · {len(result.committed_tiles)}/{result.total_tiles} ")
                    self.status_var.set(
                        "Paused after ordered commit. Resume reuses persisted blocks."
                    )
                elif result.failed_tiles:
                    self.mode_var.set(" TILE FAILED ")
                    self.status_var.set(
                        "Failed tiles: "
                        + ", ".join(str(value) for value in result.failed_tiles)
                        + ". Other tiles remain committed; Retry failed is available."
                    )
                    self._append_error("E_TILE", {"failed_tiles": list(result.failed_tiles)})
                else:
                    self.mode_var.set(" INCOMPLETE ")
                    self.status_var.set("Exact render is incomplete; checkpoint can resume.")
                self._refresh_buttons()
                return
            if result.frame is None:
                self.status_var.set("Exact result has no assembled frame.")
                return
            if result.snapshot.version != self.engine.store.version:
                self.status_var.set(
                    f"Exact v{result.snapshot.version} superseded by "
                    f"v{self.engine.store.version}; rerendering latest."
                )
                self.exact_pending = True
            else:
                self.current_frame = result.frame
                self.paused_result = None
                if self.exact_preview_used is not None:
                    self.diff_rgb = difference_heatmap(self.exact_preview_used, result.frame)
                self._set_mode("EXACT", EXACT_COLOR, result.frame)
                exact_ms = result.frame.duration_ms
                self.status_var.set(
                    f"Exact v{result.snapshot.version} committed in {exact_ms:.0f} ms "
                    f"({len(result.commit_order)} tiles, ordered)."
                )
                self._present_frame(result.frame)
                self._update_metrics(result.frame)
                self.error_var.set("")
                self._refresh_buttons()
        if self.exact_pending and not self.exact_running and not self._closing:
            self.exact_pending = False
            self.root.after(10, self._start_exact)

    def _pause(self) -> None:
        if self.control is not None:
            self.control.request_pause()
            self.status_var.set("Pause requested; finishing the current tile commit…")

    def _resume(self) -> None:
        if self.paused_result is None:
            self.status_var.set("No paused checkpoint; starting exact from persisted tiles.")
        self.fault_injector = None
        self._start_exact()

    def _cancel(self) -> None:
        if self.control is not None:
            self.control.request_cancel()
            self.status_var.set(
                "Cancel requested. Only blocks already committed to disk will be reusable."
            )
        else:
            self.status_var.set("No active exact render.")

    def _retry_failed(self) -> None:
        self.fault_injector = None
        self.last_failed_tiles = ()
        self.status_var.set("Retrying failed/missing tiles from the persisted checkpoint…")
        self._start_exact()

    def _inject_fault(self) -> None:
        if self.exact_running:
            self.status_var.set("Wait for the current exact render before injecting a fault.")
            return
        try:
            tile_id = int(self.fault_tile_var.get())
        except (ValueError, tk.TclError):
            self._append_error("E_PARA", {"field": "fault_tile"})
            return
        latest_params = self.engine.store.latest().params
        total_tiles = len(
            make_tiles(
                latest_params.exact_width,
                latest_params.exact_height,
                latest_params.tile_width,
                latest_params.tile_height,
            )
        )
        if not (0 <= tile_id < total_tiles):
            self._append_error(
                "E_PARA",
                {"field": "fault_tile", "reason": f"must be in [0, {total_tiles - 1}]"},
            )
            return
        CheckpointStore(self.engine.checkpoint_root, "session").invalidate(
            f"UI injected tile fault {tile_id}"
        )
        self.fault_injector = FaultInjector({tile_id})
        self.paused_result = None
        self.last_failed_tiles = ()
        self._refresh_buttons()
        self.status_var.set(
            f"Injected local fault in tile {tile_id}; rerendering from a clean checkpoint."
        )
        self._start_exact()

    def _clear_fault(self) -> None:
        self.fault_injector = None
        self.status_var.set("Fault injector cleared. Failed tiles can now be retried.")
        self._refresh_buttons()

    def _append_error(self, code: str, details: dict[str, Any]) -> None:
        message = f"{code}: {json.dumps(details, sort_keys=True, default=str)[:360]}"
        current = self.error_var.get().splitlines()
        current = [message, *current][:4]
        self.error_var.set("\n".join(current))

    def _refresh_buttons(self) -> None:
        if not hasattr(self, "resume_button"):
            return
        running = self.exact_running
        self.pause_button.configure(state="normal" if running else "disabled")
        self.cancel_button.configure(state="normal" if running else "disabled")
        resume_available = (
            self.paused_result is not None
            or bool(self.last_failed_tiles)
            or self._checkpoint_available()
        )
        self.resume_button.configure(state="normal" if resume_available else "disabled")
        self.retry_button.configure(
            state="normal" if bool(self.last_failed_tiles) else "disabled"
        )
        if hasattr(self, "inject_button"):
            self.inject_button.configure(state="disabled" if running else "normal")
            self.clear_fault_button.configure(state="disabled" if running else "normal")

    def _checkpoint_available(self) -> bool:
        manifest_path = self.engine.checkpoint_root / "session" / "manifest.json"
        if not manifest_path.exists():
            return False
        try:
            payload = json.loads(manifest_path.read_text(encoding="utf-8"))
            identity = RunIdentity.from_dict(payload.get("identity"))
            snapshot = self.engine.store.latest()
            current = RunIdentity.from_snapshot(snapshot)
            return identity == current and bool(payload.get("blocks"))
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
            return False

    def _show_error(self, exc: NprError) -> None:
        self.mode_var.set(f" {exc.code} ")
        self.mode_label.configure(fg=DANGER)
        self.status_var.set(exc.message)
        self._append_error(exc.code, exc.details)

    def _save_state(self, snapshot: Snapshot | None = None) -> None:
        snap = snapshot or self.engine.store.latest()
        payload = {
            "schema": 1,
            "scene": self.scene_name,
            "version": snap.version,
            "camera": snap.camera.to_dict(),
            "params": snap.params.to_dict(),
        }
        self.state_path.parent.mkdir(parents=True, exist_ok=True)
        fd, temp_name = tempfile.mkstemp(
            prefix=".ui_state.", suffix=".tmp", dir=self.state_path.parent
        )
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump(payload, handle, indent=2, sort_keys=True)
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_name, self.state_path)
        finally:
            try:
                os.unlink(temp_name)
            except FileNotFoundError:
                pass

    def _schedule_state_save(self, snapshot: Snapshot | None = None) -> None:
        if self._after_state is not None:
            self.root.after_cancel(self._after_state)
        self._after_state = self.root.after(
            240, lambda snap=snapshot: self._flush_state(snap)
        )

    def _flush_state(self, snapshot: Snapshot | None = None) -> None:
        self._after_state = None
        try:
            self._save_state(snapshot or self.engine.store.latest())
        except OSError as exc:
            self._append_error("E_IO", {"message": str(exc)})

    def _close(self) -> None:
        self._closing = True
        self._cancel_scheduled_exact()
        if self._after_state is not None:
            self.root.after_cancel(self._after_state)
            self._after_state = None
        if self.control is not None:
            self.control.request_cancel()
        try:
            self._save_state()
        except OSError:
            pass
        self.exact_executor.shutdown(wait=False, cancel_futures=True)
        self.root.destroy()


def run_app(
    *,
    data_dir: str | Path = "data",
    scene_name: str = "orb_and_crate",
    persist_dir: str | Path = "persist",
) -> int:
    root = tk.Tk()
    try:
        WorkbenchApp(
            root,
            data_dir=Path(data_dir),
            scene_name=scene_name,
            persist_dir=Path(persist_dir),
        )
        root.mainloop()
    except Exception as exc:  # pragma: no cover - desktop launch boundary
        try:
            messagebox.showerror("NPR Workbench", str(exc), parent=root)
        except tk.TclError:
            print(str(exc))
        return 1
    return 0

