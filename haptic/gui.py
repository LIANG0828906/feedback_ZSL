from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np

from .engine import BuildState, compile_signals
from .errors import E_BUSY, HapticError
from .model import (
    HapticProject,
    WaveSegment,
    load_project,
    parse_segment,
)
from .samples import write_manifest, write_samples
from .snapshot import SnapshotManager
from .tasks import RecomputeTaskManager, TaskSnapshot
from .trace import TraceLogger
from .util import canonical_json_text
from .versioning import Publisher

try:
    import tkinter as tk
    from tkinter import filedialog, messagebox, ttk
    from tkinter.scrolledtext import ScrolledText

    import matplotlib

    matplotlib.use("TkAgg")
    from matplotlib.backends.backend_tkagg import FigureCanvasTkAgg
    from matplotlib.figure import Figure

    GUI_IMPORT_ERROR: Exception | None = None
except Exception as exc:  # noqa: BLE001  # pragma: no cover
    tk = None
    ttk = None
    filedialog = None
    messagebox = None
    ScrolledText = None
    FigureCanvasTkAgg = None
    Figure = None
    GUI_IMPORT_ERROR = exc


ENVELOPE_VALUES = ("rectangular", "linear_decay", "sine")


class HapticEditorApp:
    def __init__(
        self,
        root: tk.Tk,
        *,
        sample_path: str | Path | None = None,
        outdir: str | Path = "artifacts/gui",
    ) -> None:
        if GUI_IMPORT_ERROR is not None:
            raise RuntimeError(f"GUI dependencies are unavailable: {GUI_IMPORT_ERROR}")
        self.root = root
        self.outdir = Path(outdir)
        self.outdir.mkdir(parents=True, exist_ok=True)
        self.sample_dir = Path("samples")
        if not (self.sample_dir / "mixed_envelopes.json").exists():
            write_samples(self.sample_dir)
            write_manifest(self.sample_dir)

        self.project: HapticProject | None = None
        self.publisher: Publisher | None = None
        self.manager: RecomputeTaskManager | None = None
        self.trace_path = self.outdir / "trace.jsonl"
        self.trace = TraceLogger(self.trace_path, reset=True)
        self.snapshot_default = self.outdir / "snapshot.json"
        self.selected_index = 0
        self._last_status: TaskSnapshot | None = None
        self._last_trace_sequence = 0
        self._pending_modified: tuple[int, ...] = ()
        self._syncing_tree_selection = False

        self.root.title("Haptic Waveform Studio")
        self.root.geometry("1440x900")
        self.root.minsize(1100, 700)
        self._build_ui()
        self._load_initial(
            Path(sample_path)
            if sample_path is not None
            else self.sample_dir / "mixed_envelopes.json"
        )
        self.root.protocol("WM_DELETE_WINDOW", self.shutdown)
        self.root.after(80, self._poll_task)

    def _build_ui(self) -> None:
        style = ttk.Style(self.root)
        try:
            style.theme_use("clam")
        except tk.TclError:
            pass
        style.configure("Title.TLabel", font=("TkDefaultFont", 13, "bold"))
        style.configure("Status.TLabel", padding=6)

        toolbar = ttk.Frame(self.root, padding=(8, 6))
        toolbar.pack(side=tk.TOP, fill=tk.X)
        ttk.Button(
            toolbar,
            text="Load JSON",
            command=self._choose_project,
        ).pack(side=tk.LEFT, padx=(0, 5))
        ttk.Button(
            toolbar,
            text="Generate Samples",
            command=self._generate_samples,
        ).pack(side=tk.LEFT, padx=5)
        ttk.Button(
            toolbar,
            text="Save Snapshot",
            command=self._save_snapshot,
        ).pack(side=tk.LEFT, padx=5)
        ttk.Button(
            toolbar,
            text="Recover Snapshot",
            command=self._recover_snapshot,
        ).pack(side=tk.LEFT, padx=5)
        self.trace_label = ttk.Label(
            toolbar,
            text=f"Trace: {self.trace_path}",
        )
        self.trace_label.pack(side=tk.RIGHT)

        paned = ttk.Panedwindow(self.root, orient=tk.HORIZONTAL)
        paned.pack(fill=tk.BOTH, expand=True, padx=8, pady=(0, 8))

        left = ttk.Frame(paned, padding=4)
        right = ttk.Frame(paned, padding=4)
        paned.add(left, weight=1)
        paned.add(right, weight=4)

        ttk.Label(left, text="Waveform Segments", style="Title.TLabel").pack(
            anchor=tk.W,
            pady=(0, 4),
        )
        columns = (
            "index",
            "start",
            "duration",
            "freq",
            "amp",
            "envelope",
            "left",
            "right",
        )
        self.tree = ttk.Treeview(
            left,
            columns=columns,
            show="headings",
            height=12,
            selectmode="browse",
        )
        headings = {
            "index": "#",
            "start": "Start ms",
            "duration": "Dur ms",
            "freq": "Hz",
            "amp": "Amp",
            "envelope": "Envelope",
            "left": "L ratio",
            "right": "R ratio",
        }
        widths = {
            "index": 32,
            "start": 66,
            "duration": 62,
            "freq": 52,
            "amp": 52,
            "envelope": 94,
            "left": 55,
            "right": 55,
        }
        for column in columns:
            self.tree.heading(column, text=headings[column])
            self.tree.column(
                column,
                width=widths[column],
                minwidth=32,
                anchor=tk.CENTER,
                stretch=True,
            )
        tree_scroll = ttk.Scrollbar(
            left,
            orient=tk.VERTICAL,
            command=self.tree.yview,
        )
        self.tree.configure(yscrollcommand=tree_scroll.set)
        self.tree.pack(side=tk.TOP, fill=tk.BOTH, expand=False)
        tree_scroll.place(relx=1.0, rely=0.0, relheight=0.3, anchor="ne")
        self.tree.bind("<<TreeviewSelect>>", self._on_select)

        editor = ttk.LabelFrame(left, text="Edit Selected Segment", padding=8)
        editor.pack(fill=tk.X, pady=(10, 0))

        self.var_start = tk.StringVar()
        self.var_duration = tk.StringVar()
        self.var_frequency = tk.StringVar()
        self.var_amplitude = tk.StringVar()
        self.var_envelope = tk.StringVar(value=ENVELOPE_VALUES[0])
        self.var_left_ratio = tk.StringVar()
        self.var_right_ratio = tk.StringVar()
        self.var_left_polarity = tk.StringVar(value="1")
        self.var_right_polarity = tk.StringVar(value="1")

        fields = [
            ("Start ms", self.var_start),
            ("Duration ms", self.var_duration),
            ("Frequency Hz", self.var_frequency),
            ("Amplitude", self.var_amplitude),
            ("Left ratio", self.var_left_ratio),
            ("Right ratio", self.var_right_ratio),
        ]
        for row, (label, variable) in enumerate(fields):
            ttk.Label(editor, text=label).grid(
                row=row,
                column=0,
                sticky=tk.W,
                padx=(0, 8),
                pady=2,
            )
            ttk.Entry(editor, textvariable=variable, width=16).grid(
                row=row,
                column=1,
                sticky=tk.EW,
                pady=2,
            )
        ttk.Label(editor, text="Envelope").grid(
            row=6,
            column=0,
            sticky=tk.W,
            padx=(0, 8),
            pady=2,
        )
        ttk.Combobox(
            editor,
            textvariable=self.var_envelope,
            values=ENVELOPE_VALUES,
            state="readonly",
            width=14,
        ).grid(row=6, column=1, sticky=tk.EW, pady=2)
        ttk.Label(editor, text="L polarity").grid(
            row=7,
            column=0,
            sticky=tk.W,
            padx=(0, 8),
            pady=2,
        )
        ttk.Combobox(
            editor,
            textvariable=self.var_left_polarity,
            values=("1", "-1"),
            state="readonly",
            width=14,
        ).grid(row=7, column=1, sticky=tk.EW, pady=2)
        ttk.Label(editor, text="R polarity").grid(
            row=8,
            column=0,
            sticky=tk.W,
            padx=(0, 8),
            pady=2,
        )
        ttk.Combobox(
            editor,
            textvariable=self.var_right_polarity,
            values=("1", "-1"),
            state="readonly",
            width=14,
        ).grid(row=8, column=1, sticky=tk.EW, pady=2)
        editor.columnconfigure(1, weight=1)

        action_row = ttk.Frame(editor)
        action_row.grid(row=9, column=0, columnspan=2, pady=(10, 0), sticky=tk.EW)
        ttk.Button(
            action_row,
            text="Apply & Recompute",
            command=self._apply_and_recompute,
        ).pack(side=tk.LEFT, fill=tk.X, expand=True)
        ttk.Button(
            action_row,
            text="Cancel",
            command=self._cancel_recompute,
        ).pack(side=tk.LEFT, padx=(6, 0))

        self.status_var = tk.StringVar(value="Ready")
        ttk.Label(
            left,
            textvariable=self.status_var,
            style="Status.TLabel",
            wraplength=420,
        ).pack(fill=tk.X, pady=(8, 2))

        ttk.Label(left, text="Publish Trace", style="Title.TLabel").pack(
            anchor=tk.W,
            pady=(6, 2),
        )
        self.trace_text = ScrolledText(left, height=12, wrap=tk.WORD)
        self.trace_text.pack(fill=tk.BOTH, expand=True)
        self.trace_text.configure(state=tk.DISABLED)

        self.figure = Figure(figsize=(9, 6), dpi=100)
        self.canvas = FigureCanvasTkAgg(self.figure, master=right)
        self.canvas.get_tk_widget().pack(fill=tk.BOTH, expand=True)

    def _load_initial(self, path: Path) -> None:
        if self.manager is not None and self.manager.status().state in (
            "running",
            "cancelling",
        ):
            self._show_error(
                HapticError(E_BUSY, "cannot load a project during recompute")
            )
            return
        try:
            project = load_project(path)
            initial = compile_signals(project.segments, project.config)
            publisher = Publisher()
            publisher.publish(initial.version)
            trace = TraceLogger(self.trace_path, reset=True)
            trace.record_build(initial)
            manager = RecomputeTaskManager(publisher, initial.state, trace)
            self.project = project
            self.publisher = publisher
            self.manager = manager
            self.trace = trace
            self._last_trace_sequence = 0
            self._last_status = manager.status()
            self.selected_index = 0
            self.status_var.set(
                f"Loaded {path.name}; published version {initial.version.version}"
            )
            self._refresh_all()
        except HapticError as exc:
            self._show_error(exc)

    def _active_state(self) -> BuildState | None:
        if self.manager is None:
            return None
        return self.manager.state()

    def _refresh_all(self) -> None:
        state = self._active_state()
        if state is None:
            return
        self._refresh_tree(state.version.segments)
        self._refresh_plot(state)
        self._refresh_trace()
        self._select_index(
            min(self.selected_index, max(0, len(state.version.segments) - 1))
        )

    def _refresh_tree(self, segments: tuple[WaveSegment, ...]) -> None:
        selected = self.selected_index
        self._syncing_tree_selection = True
        try:
            self.tree.delete(*self.tree.get_children())
            for segment in segments:
                self.tree.insert(
                    "",
                    tk.END,
                    iid=str(segment.index),
                    values=(
                        segment.index,
                        f"{segment.start_ms:g}",
                        f"{segment.duration_ms:g}",
                        f"{segment.frequency_hz:g}",
                        f"{segment.amplitude:g}",
                        segment.envelope,
                        f"{segment.left_ratio:g}",
                        f"{segment.right_ratio:g}",
                    ),
                )
            if segments and self.tree.exists(str(selected)):
                self.tree.selection_set(str(selected))
                self.tree.see(str(selected))
        finally:
            self._syncing_tree_selection = False

    def _refresh_trace(self) -> None:
        if not self.trace_path.exists():
            return
        lines = self.trace_path.read_text(encoding="utf-8").splitlines()
        text = "\n".join(lines[-12:])
        try:
            self.trace_text.configure(state=tk.NORMAL)
            self.trace_text.delete("1.0", tk.END)
            self.trace_text.insert(tk.END, text)
            self.trace_text.see(tk.END)
            self.trace_text.configure(state=tk.DISABLED)
        except tk.TclError:
            pass

    def _refresh_plot(self, state: BuildState) -> None:
        version = state.version
        self.figure.clear()
        figure = self.figure
        if version.sample_count == 0:
            axis = figure.add_subplot(111)
            axis.text(0.5, 0.5, "No samples", ha="center", va="center")
            axis.set_axis_off()
            self.canvas.draw_idle()
            return

        axis_segments = figure.add_subplot(211)
        axis_drivers = figure.add_subplot(212, sharex=axis_segments)
        time_ms = np.arange(version.sample_count) / version.sample_rate * 1000.0
        colors = ("#1f77b4", "#d62728", "#2ca02c", "#9467bd", "#ff7f0e")

        for signal in state.segment_signals:
            segment_time = (
                np.arange(signal.sample_count) / version.sample_rate * 1000.0
                + signal.start_sample / version.sample_rate * 1000.0
            )
            axis_segments.plot(
                segment_time,
                signal.left,
                linewidth=1.0,
                alpha=0.62,
                color=colors[signal.index % len(colors)],
                label=f"S{signal.index} {signal.segment.envelope}",
            )
            axis_segments.axvline(
                signal.start_sample / version.sample_rate * 1000.0,
                color="#aaaaaa",
                linewidth=0.6,
                linestyle=":",
            )
        axis_segments.set_ylabel("Segment L")
        axis_segments.set_title(
            f"Version {version.version} | hash {version.content_hash[:12]} | "
            f"{version.sample_rate} Hz | {version.config.limiter} limiter"
        )
        axis_segments.grid(True, alpha=0.25)
        if state.segment_signals:
            axis_segments.legend(
                loc="upper right",
                fontsize=7,
                ncol=min(3, len(state.segment_signals)),
            )

        axis_drivers.plot(time_ms, version.left, label="Left motor", linewidth=1.2)
        axis_drivers.plot(
            time_ms,
            version.right,
            label="Right motor",
            linewidth=1.2,
            alpha=0.82,
        )
        limited = np.flatnonzero(version.limited_mask)
        if limited.size:
            if limited.size > 1800:
                limited = limited[
                    np.linspace(0, limited.size - 1, 1800, dtype=np.int64)
                ]
            axis_drivers.scatter(
                time_ms[limited],
                version.left[limited],
                marker="x",
                s=16,
                color="#d62728",
                label=f"limited ({version.limited_samples})",
            )
        axis_drivers.axhline(
            version.config.device_max,
            color="#d62728",
            linewidth=0.7,
            linestyle="--",
            alpha=0.55,
        )
        axis_drivers.axhline(
            version.config.device_min,
            color="#d62728",
            linewidth=0.7,
            linestyle="--",
            alpha=0.55,
        )
        axis_drivers.set_ylabel("Driver output")
        axis_drivers.set_xlabel("Time (ms)")
        axis_drivers.grid(True, alpha=0.25)
        axis_drivers.legend(loc="upper right", fontsize=8)
        figure.tight_layout()
        self.canvas.draw_idle()

    def _select_index(self, index: int) -> None:
        state = self._active_state()
        if state is None or not state.version.segments:
            return
        self.selected_index = index
        segment = state.version.segments[index]
        if self.tree.exists(str(index)) and self.tree.selection() != (str(index),):
            self._syncing_tree_selection = True
            try:
                self.tree.selection_set(str(index))
                self.tree.see(str(index))
            finally:
                self._syncing_tree_selection = False
        self.var_start.set(f"{segment.start_ms:g}")
        self.var_duration.set(f"{segment.duration_ms:g}")
        self.var_frequency.set(f"{segment.frequency_hz:g}")
        self.var_amplitude.set(f"{segment.amplitude:g}")
        self.var_envelope.set(segment.envelope)
        self.var_left_ratio.set(f"{segment.left_ratio:g}")
        self.var_right_ratio.set(f"{segment.right_ratio:g}")
        self.var_left_polarity.set(str(segment.left_polarity))
        self.var_right_polarity.set(str(segment.right_polarity))

    def _on_select(self, _event: object) -> None:
        if self._syncing_tree_selection:
            return
        selection = self.tree.selection()
        if selection:
            self._select_index(int(selection[0]))

    def _parse_editor_segment(self) -> WaveSegment:
        if self.project is None:
            raise HapticError("E_WAVE_INVALID", "no project is loaded")
        raw = {
            "start_ms": self.var_start.get(),
            "duration_ms": self.var_duration.get(),
            "frequency_hz": self.var_frequency.get(),
            "amplitude": self.var_amplitude.get(),
            "envelope": self.var_envelope.get(),
            "left_ratio": self.var_left_ratio.get(),
            "right_ratio": self.var_right_ratio.get(),
            "left_polarity": self.var_left_polarity.get(),
            "right_polarity": self.var_right_polarity.get(),
        }
        try:
            converted = {
                **raw,
                "start_ms": float(raw["start_ms"]),
                "duration_ms": float(raw["duration_ms"]),
                "frequency_hz": float(raw["frequency_hz"]),
                "amplitude": float(raw["amplitude"]),
                "left_ratio": float(raw["left_ratio"]),
                "right_ratio": float(raw["right_ratio"]),
                "left_polarity": int(raw["left_polarity"]),
                "right_polarity": int(raw["right_polarity"]),
            }
        except (TypeError, ValueError) as exc:
            raise HapticError(
                "E_WAVE_INVALID",
                f"editor value is not numeric: {exc}",
                segment_index=self.selected_index,
            ) from exc
        return parse_segment(converted, self.selected_index, self.project.config)

    def _apply_and_recompute(self) -> None:
        state = self._active_state()
        if state is None or self.manager is None:
            return
        try:
            edited = self._parse_editor_segment()
            candidate = [
                edited if segment.index == self.selected_index else segment
                for segment in state.version.segments
            ]
            self.manager.submit(
                candidate,
                state.version.config,
                modified_indices=[self.selected_index],
            )
            self._pending_modified = (self.selected_index,)
            self.status_var.set(f"Recomputing segment {self.selected_index}...")
        except HapticError as exc:
            self._show_error(exc)

    def _cancel_recompute(self) -> None:
        if self.manager is not None and self.manager.cancel():
            self.status_var.set("Cancellation requested...")

    def _poll_task(self) -> None:
        if self.manager is not None:
            status = self.manager.status()
            if status != self._last_status:
                self._last_status = status
                if status.state == "succeeded":
                    self.status_var.set(
                        f"Published version {status.published_version} "
                        f"({status.content_hash[:12]})"
                    )
                    self._refresh_all()
                elif status.state == "cancelled":
                    self.status_var.set(
                        f"Recompute cancelled; version "
                        f"{status.published_version} remains published"
                    )
                    self._refresh_all()
                elif status.state == "failed":
                    self.status_var.set(f"Recompute failed: {status.message}")
        self.root.after(80, self._poll_task)

    def _choose_project(self) -> None:
        path = filedialog.askopenfilename(
            title="Load haptic waveform JSON",
            initialdir=str(self.sample_dir),
            filetypes=(("JSON", "*.json"), ("All files", "*")),
        )
        if path:
            self._load_initial(Path(path))

    def _generate_samples(self) -> None:
        try:
            write_samples(self.sample_dir)
            write_manifest(self.sample_dir)
            self.status_var.set(f"Samples written to {self.sample_dir}")
        except OSError as exc:
            self._show_error(HapticError("E_IO", str(exc)))

    def _save_snapshot(self) -> None:
        if self.manager is None or self.publisher is None:
            return
        if self.manager.status().state in ("running", "cancelling"):
            self._show_error(HapticError(E_BUSY, "cannot snapshot during recompute"))
            return
        path = filedialog.asksaveasfilename(
            title="Save atomic snapshot",
            initialdir=str(self.outdir),
            initialfile=self.snapshot_default.name,
            defaultextension=".json",
            filetypes=(("JSON", "*.json"),),
        )
        if not path:
            return
        try:
            SnapshotManager.save(path, self.publisher, self.manager.state())
            self.status_var.set(f"Snapshot saved: {path}")
        except HapticError as exc:
            self._show_error(exc)

    def _recover_snapshot(self) -> None:
        if self.manager is not None and self.manager.status().state in (
            "running",
            "cancelling",
        ):
            self._show_error(HapticError(E_BUSY, "cannot recover during recompute"))
            return
        path = filedialog.askopenfilename(
            title="Recover snapshot",
            initialdir=str(self.outdir),
            filetypes=(("JSON", "*.json"),),
        )
        if not path:
            return
        try:
            recovered = SnapshotManager.load(path)
            self.publisher = recovered.publisher()
            self.manager = RecomputeTaskManager(
                self.publisher,
                recovered.state,
                self.trace,
            )
            self.project = HapticProject(
                config=recovered.state.version.config,
                segments=recovered.state.version.segments,
            )
            self._last_status = self.manager.status()
            self.trace.append(
                {
                    "schema": "haptic-trace-v1",
                    "event": "recover",
                    "version": recovered.state.version.version,
                    "content_hash": recovered.state.version.content_hash,
                    "recomputed_chunk_intervals": [
                        list(interval) for interval in recovered.recomputed_chunk_ranges
                    ],
                    "recomputed_segment_indices": list(
                        recovered.recomputed_segment_indices
                    ),
                }
            )
            self.status_var.set(
                f"Recovered version {recovered.state.version.version}; "
                f"rebuilt {recovered.recomputed_chunks} chunks"
            )
            self._refresh_all()
        except HapticError as exc:
            self._show_error(exc)

    def _show_error(self, exc: HapticError) -> None:
        message = str(exc)
        self.status_var.set(message)
        try:
            messagebox.showerror("Haptic Waveform Studio", message)
        except tk.TclError:
            print(message, file=sys.stderr)

    def shutdown(self) -> None:
        if self.manager is not None:
            self.manager.cancel()
            self.manager.wait(1.0)
        self.root.destroy()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Haptic waveform desktop app")
    parser.add_argument(
        "--sample",
        type=Path,
        default=None,
        help="local project JSON to open",
    )
    parser.add_argument(
        "--outdir",
        type=Path,
        default=Path("artifacts/gui"),
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help="report Tk/Matplotlib availability",
    )
    parser.add_argument(
        "--smoke-test",
        action="store_true",
        help="create and destroy the window once",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.check:
        print(
            canonical_json_text(
                {
                    "tkinter_available": tk is not None,
                    "tk_version": getattr(tk, "TkVersion", None),
                    "matplotlib_tkagg_available": FigureCanvasTkAgg is not None,
                    "error": str(GUI_IMPORT_ERROR) if GUI_IMPORT_ERROR else None,
                }
            )
        )
        return 0 if GUI_IMPORT_ERROR is None else 2
    if GUI_IMPORT_ERROR is not None:
        print(
            canonical_json_text(
                {
                    "code": "E_GUI_UNAVAILABLE",
                    "message": str(GUI_IMPORT_ERROR),
                }
            ),
            file=sys.stderr,
        )
        return 2
    root = tk.Tk()
    app = HapticEditorApp(
        root,
        sample_path=args.sample,
        outdir=args.outdir,
    )
    if args.smoke_test:
        root.update_idletasks()
        root.update()
        app.shutdown()
        return 0
    root.mainloop()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
