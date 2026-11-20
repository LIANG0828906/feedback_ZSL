"""Optional Tk desktop workbench.

Tkinter is part of the Python standard library on normal desktop installs, but
some minimal Linux images omit the OS package.  The core and CLI do not import
this module, so metering remains available on such machines.
"""

from __future__ import annotations

import json
import threading
from pathlib import Path
from typing import Any

from .config import load_workbench_config
from .errors import ErrorCode, MeterError, error
from .generator import generate_samples
from .metering import AssetMeter
from .playback import PlaybackController
from .workbench import run_workbench


def run_gui(config_path: str) -> int:
    try:
        import tkinter as tk
        from tkinter import ttk
    except ModuleNotFoundError:
        raise error(
            ErrorCode.E_TASK_FAILED,
            "Tkinter is not installed; use the CLI or install the OS Tk package",
            {"module": "tkinter"},
        ) from None

    config = load_workbench_config(config_path)
    root = tk.Tk()
    root.title("Broadcast Loudness Workbench")
    root.geometry("1120x720")

    meters: dict[str, AssetMeter] = {}
    players: dict[str, PlaybackController] = {}
    for asset in config.assets:
        meter = AssetMeter(
            asset.path,
            config=config.meter,
            asset_id=asset.asset_id,
            enforce_budget=False,
        )
        meters[asset.asset_id] = meter
        players[asset.asset_id] = PlaybackController(meter)

    active = tk.StringVar(value=config.assets[0].asset_id)
    status = tk.StringVar(value="ready")
    summary = tk.Text(root, width=82, height=34, wrap="none")
    summary.configure(font=("TkFixedFont", 10))
    summary.pack(side="right", fill="both", expand=True, padx=8, pady=8)

    side = ttk.Frame(root, padding=8)
    side.pack(side="left", fill="y")
    ttk.Label(side, text="Programs").pack(anchor="w")
    program_list = tk.Listbox(side, height=8, exportselection=False)
    for asset in config.assets:
        program_list.insert("end", asset.asset_id)
    program_list.selection_set(0)
    program_list.pack(fill="x", pady=(0, 8))

    def selected_asset() -> str:
        selection = program_list.curselection()
        if not selection:
            return config.assets[0].asset_id
        return str(program_list.get(selection[0]))

    def refresh(*_: Any) -> None:
        asset_id = selected_asset()
        active.set(asset_id)
        report = meters[asset_id].get_published()
        lines = [
            f"asset: {asset_id}",
            f"state: {players[asset_id].state.value}",
            f"blocks: {report.block_count}/{meters[asset_id].total_blocks}",
            f"parameter_version: {report.parameter_version}",
            f"head_hash: {report.head_hash}",
            f"status: {status.get()}",
            "",
        ]
        if report.blocks:
            block = report.blocks[-1]
            lines.extend(
                [
                    f"sync_marker: {block.sync_marker}",
                    f"window: {block.window_start}..{block.window_end}",
                    f"short_term_lufs: {block.short_term_lufs:.6f}",
                    f"integrated_lufs: {block.integrated_lufs:.6f}",
                    f"true_peak_dbtp: {block.true_peak_dbtp:.6f}",
                    f"target_lufs: {block.target_lufs:.2f}",
                    f"tp_limit_dbtp: {block.true_peak_limit_dbtp:.2f}",
                    f"gain_to_target_db: {block.gain_to_target_db:+.3f}",
                    f"degrade_stage: {block.degrade_stage}",
                    f"memory_bytes: {block.memory_bytes}",
                    f"content_hash: {block.content_hash}",
                    f"chain_hash: {block.chain_hash}",
                ]
            )
        summary.delete("1.0", "end")
        summary.insert("end", "\n".join(lines) + "\n")

    def playback_action(action: str) -> None:
        asset_id = selected_asset()
        player = players[asset_id]
        try:
            if action == "play":
                player.play()
                player.advance(1)
            elif action == "pause":
                player.pause()
            elif action == "seek":
                player.seek_to_block(max(0, player.cursor_block + 1))
            elif action == "restart":
                player.restart()
            refresh()
        except MeterError as exc:
            status.set(f"{exc.code}: {exc.message}")
            refresh()

    buttons = ttk.Frame(side)
    buttons.pack(fill="x")
    for text, action in (
        ("Play", "play"),
        ("Pause", "pause"),
        ("Next/Seek", "seek"),
        ("Restart", "restart"),
    ):
        ttk.Button(
            buttons,
            text=text,
            command=lambda value=action: playback_action(value),
        ).pack(fill="x", pady=2)

    def run_all() -> None:
        status.set("running")
        refresh()

        def worker() -> None:
            try:
                result = run_workbench(config)
                root.after(
                    0,
                    lambda: (
                        status.set("complete" if result.get("ok") else "failed"),
                        refresh(),
                        summary.insert(
                            "end",
                            "\n" + json.dumps(result, sort_keys=True, indent=2),
                        ),
                    ),
                )
            except BaseException as exc:
                root.after(
                    0,
                    lambda: (
                        status.set(str(exc)),
                        refresh(),
                    ),
                )

        threading.Thread(target=worker, daemon=True).start()

    ttk.Button(side, text="Run all + verify", command=run_all).pack(
        fill="x",
        pady=(8, 2),
    )
    ttk.Button(
        side,
        text="Generate samples",
        command=lambda: generate_samples(
            config.sample_dir,
            seed=config.seed,
            config_path=Path(config.sample_dir) / "config.json",
        ),
    ).pack(fill="x", pady=2)
    ttk.Label(side, textvariable=status, wraplength=220).pack(
        fill="x",
        pady=(8, 0),
    )
    program_list.bind("<<ListboxSelect>>", refresh)
    refresh()
    root.mainloop()
    return 0
