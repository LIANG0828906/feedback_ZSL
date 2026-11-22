"""Optional tkinter result viewer; no network, GPU, or external service."""

from __future__ import annotations

import json
from pathlib import Path

import tkinter as tk
from PIL import Image, ImageTk
from tkinter import ttk

from .io_utils import read_json


def launch_gui(dataset_path: Path = Path("samples/dataset.json"), output_root: Path = Path("outputs/paths")) -> None:
    dataset_path = Path(dataset_path)
    output_root = Path(output_root)
    root = tk.Tk()
    root.title("MSHDR Workbench")
    root.geometry("980x680")

    frame = ttk.Frame(root, padding=8)
    frame.pack(fill=tk.BOTH, expand=True)
    header = ttk.Label(
        frame,
        text="多光谱 HDR 合成与增强工作台 — 本地确定性结果查看器",
        font=("TkDefaultFont", 13, "bold"),
    )
    header.pack(anchor=tk.W, pady=(0, 8))

    panes = ttk.Panedwindow(frame, orient=tk.HORIZONTAL)
    panes.pack(fill=tk.BOTH, expand=True)
    left = ttk.Frame(panes, width=260)
    right = ttk.Frame(panes)
    panes.add(left, weight=1)
    panes.add(right, weight=4)

    scene_ids: list[str] = []
    if dataset_path.exists():
        dataset = read_json(dataset_path, field="dataset")
        scene_ids = [str(record["scene_id"]) for record in dataset.get("scenes", [])]
    listbox = tk.Listbox(left, exportselection=False)
    listbox.pack(fill=tk.BOTH, expand=True)
    for scene_id in scene_ids:
        listbox.insert(tk.END, scene_id)

    info = tk.Text(right, height=12, wrap=tk.WORD)
    info.pack(side=tk.BOTTOM, fill=tk.X)
    canvas = tk.Canvas(right, background="#202020", highlightthickness=0)
    canvas.pack(side=tk.TOP, fill=tk.BOTH, expand=True)
    photo: dict[str, ImageTk.PhotoImage] = {}

    def show_scene(_event: object | None = None) -> None:
        selection = listbox.curselection()
        if not selection:
            return
        scene_id = scene_ids[selection[0]]
        image_path = output_root / "full" / scene_id / "synthetic_preview.png"
        stats_path = output_root / "full" / scene_id / "stats.json"
        canvas.delete("all")
        if image_path.exists():
            image = Image.open(image_path)
            photo["current"] = ImageTk.PhotoImage(image)
            canvas.create_image(0, 0, anchor=tk.NW, image=photo["current"])
        if stats_path.exists():
            stats = json.loads(stats_path.read_text(encoding="utf-8"))
            summary = {
                "scene": scene_id,
                "output_bit_depth": stats.get("output_bit_depth"),
                "progress": f"{stats.get('committed_count')}/{stats.get('segment_count')}",
                "residual_std": stats.get("filtering_quality", {}).get("residual_std"),
                "edge_fidelity": stats.get("filtering_quality", {}).get("edge_fidelity"),
                "out_of_gamut_count": stats.get("gamut", {}).get("out_of_gamut_count"),
                "params_version": stats.get("params_version"),
            }
            info.delete("1.0", tk.END)
            info.insert(tk.END, json.dumps(summary, indent=2, ensure_ascii=False))
        else:
            info.delete("1.0", tk.END)
            info.insert(tk.END, f"No full-path result at:\n{image_path}")

    listbox.bind("<<ListboxSelect>>", show_scene)
    if scene_ids:
        listbox.selection_set(0)
        show_scene()
    ttk.Button(left, text="Reload", command=show_scene).pack(fill=tk.X, pady=(6, 0))
    root.mainloop()

