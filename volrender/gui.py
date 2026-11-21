"""Tkinter preview UI with tile grid, stage labels, and budget curve."""

from __future__ import annotations

import os
from typing import TYPE_CHECKING

from .errors import AppError, ErrorCode

if TYPE_CHECKING:
    from .pipeline import PreviewResult


def _hex_color(rgb: list[float] | tuple[float, float, float]) -> str:
    channels = [
        max(
            0,
            min(
                255,
                int(
                    round(
                        (1.0 - 2.718281828459045 ** (-4.0 * float(value)))
                        ** (1.0 / 2.2)
                        * 255.0
                    )
                ),
            ),
        )
        for value in rgb
    ]
    return "#{:02x}{:02x}{:02x}".format(*channels)


def _stage_color(stage_index: int) -> str:
    return ("#2f9e44", "#f08c00", "#c92a2a", "#7048e8")[
        stage_index % 4
    ]


def launch_gui(preview: "PreviewResult") -> None:
    try:
        import tkinter as tk
    except ImportError as exc:
        raise AppError(
            ErrorCode.E_DISPLAY_UNAVAILABLE,
            "tkinter is not installed in this Python environment",
            field="gui",
        ) from exc

    prefer_fixed = "fixed" in preview.renders
    artifact = preview.renders["fixed" if prefer_fixed else "float64"]
    scale = max(4, min(10, 480 // max(1, preview.scene.width)))
    image_width = preview.scene.width * scale
    image_height = preview.scene.height * scale
    panel_width = 360

    try:
        root = tk.Tk()
    except tk.TclError as exc:
        raise AppError(
            ErrorCode.E_DISPLAY_UNAVAILABLE,
            f"cannot open a display for the GUI: {exc}",
            field="gui",
        ) from exc

    root.title(
        f"Volumetric Preview - {preview.scene.scene_id} "
        f"({artifact.precision})"
    )
    root.resizable(False, False)
    canvas = tk.Canvas(
        root,
        width=image_width + panel_width,
        height=max(image_height + 30, 420),
        background="#111318",
        highlightthickness=0,
    )
    canvas.pack()

    for y in range(preview.scene.height):
        for x in range(preview.scene.width):
            canvas.create_rectangle(
                x * scale,
                y * scale,
                (x + 1) * scale,
                (y + 1) * scale,
                fill=_hex_color(artifact.color[y, x]),
                outline="",
            )
    for result in artifact.tiles:
        tile = result.tile
        canvas.create_rectangle(
            tile.x0 * scale,
            tile.y0 * scale,
            tile.x1 * scale,
            tile.y1 * scale,
            outline="#d8dee9",
            width=2,
        )
        if result.stage_index > 0:
            canvas.create_text(
                tile.x0 * scale + 6,
                tile.y0 * scale + 6,
                anchor="nw",
                text=result.stage,
                fill="white",
                font=("TkDefaultFont", 8, "bold"),
            )

    panel_x = image_width + 18
    canvas.create_text(
        panel_x,
        14,
        anchor="nw",
        text=f"{preview.scene.width}x{preview.scene.height}  "
        f"tile={preview.config.tile_width}x{preview.config.tile_height}",
        fill="#f8f9fa",
        font=("TkDefaultFont", 10, "bold"),
    )
    status_lines = [
        f"scene: {preview.scene.scene_id}",
        f"precision: {artifact.precision}",
        f"budget/tile: {preview.config.sample_budget}",
        f"budget used: {artifact.summary['budget']['used']}",
        f"degraded tiles: {artifact.summary['degraded_tile_indices']}",
        f"seam max delta: "
        f"{preview.seam_checks[artifact.precision].max_color_delta:.3g}",
    ]
    if preview.precision_error is not None:
        status_lines.append(
            "fixed error max: "
            f"{preview.precision_error['max_abs_error']:.3g}"
        )
    for index, line in enumerate(status_lines):
        canvas.create_text(
            panel_x,
            40 + index * 18,
            anchor="nw",
            text=line,
            fill="#ced4da",
            font=("TkDefaultFont", 9),
        )

    chart_x = panel_x
    chart_y = 190
    chart_w = panel_width - 30
    chart_h = 150
    canvas.create_text(
        chart_x,
        chart_y - 20,
        anchor="nw",
        text="Budget occupancy by tile sequence",
        fill="#f8f9fa",
        font=("TkDefaultFont", 9, "bold"),
    )
    canvas.create_rectangle(
        chart_x,
        chart_y,
        chart_x + chart_w,
        chart_y + chart_h,
        outline="#495057",
    )
    results = sorted(artifact.tiles, key=lambda result: result.tile.seq)
    if results:
        points: list[float] = []
        for index, result in enumerate(results):
            x = chart_x + chart_w * (
                (index + 0.5) / len(results)
            )
            ratio = min(
                1.0,
                result.samples_used / preview.config.sample_budget,
            )
            y = chart_y + chart_h * (1.0 - ratio)
            points.extend([x, y])
            canvas.create_oval(
                x - 3,
                y - 3,
                x + 3,
                y + 3,
                fill=_stage_color(result.stage_index),
                outline="",
            )
        if len(points) >= 4:
            canvas.create_line(*points, fill="#74c0fc", width=2)
    canvas.create_text(
        chart_x,
        chart_y + chart_h + 10,
        anchor="nw",
        text="green=full  orange=balanced  red=draft",
        fill="#adb5bd",
        font=("TkDefaultFont", 8),
    )

    legend_y = 390
    for index, stage in enumerate(preview.config.degradation_sequence):
        canvas.create_rectangle(
            panel_x,
            legend_y + index * 20,
            panel_x + 12,
            legend_y + 12 + index * 20,
            fill=_stage_color(index),
            outline="",
        )
        canvas.create_text(
            panel_x + 18,
            legend_y + index * 20,
            anchor="nw",
            text=f"{stage.name}  step_scale={stage.step_scale:g}",
            fill="#ced4da",
            font=("TkDefaultFont", 9),
        )

    autoclose = os.environ.get("VOLRENDER_GUI_AUTOCLOSE_MS")
    if autoclose:
        root.after(max(1, int(autoclose)), root.destroy)
    root.mainloop()
