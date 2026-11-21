from __future__ import annotations

import json
import threading
import tkinter as tk
from collections.abc import Callable
from pathlib import Path
from tkinter import filedialog, messagebox, ttk
from typing import Any

from .archive import ArchiveService, CancellationToken
from .config import Config
from .errors import E_CANCELLED, AppError
from .loudness import LoudnessAnalyzer
from .metadata import derive_boundaries, load_metadata
from .publish import FileVersionPublisher
from .versions import downgrade_document, upgrade_to_v3
from .wav import read_wav


class WorkbenchApp:
    def __init__(self, root: tk.Tk, config: Config, workspace: str | Path):
        self.root = root
        self.config = config
        self.workspace = Path(workspace)
        self.workspace.mkdir(parents=True, exist_ok=True)
        self.wav = None
        self.metadata = None
        self.internal = None
        self.preview_rows: list[dict[str, Any]] = []
        self.mapping: list[dict[str, Any]] = []
        self.loss_manifest: dict[str, Any] | None = None
        self.cancel_token: CancellationToken | None = None
        self.worker: threading.Thread | None = None
        self.current_preview_token = 0
        self.root.title("Game Voice Loudness Archive Workbench")
        self.root.geometry("1440x900")
        self.root.minsize(1100, 700)
        self._build_style()
        self._build_ui()
        self._refresh_trace()

    def _build_style(self) -> None:
        style = ttk.Style(self.root)
        if "clam" in style.theme_names():
            style.theme_use("clam")
        style.configure("Treeview", rowheight=24)
        style.configure("Over.TLabel", foreground="#a40000")

    def _build_ui(self) -> None:
        toolbar = ttk.Frame(self.root, padding=(8, 6))
        toolbar.pack(fill=tk.X)
        ttk.Button(toolbar, text="Load WAV", command=self.load_wav).pack(side=tk.LEFT, padx=2)
        ttk.Button(toolbar, text="Load Metadata", command=self.load_metadata).pack(
            side=tk.LEFT, padx=2
        )
        ttk.Label(toolbar, text="Target version:").pack(side=tk.LEFT, padx=(14, 4))
        self.target_var = tk.StringVar(value="v3")
        target = ttk.Combobox(
            toolbar,
            textvariable=self.target_var,
            values=("v1", "v2", "v3"),
            state="readonly",
            width=5,
        )
        target.pack(side=tk.LEFT)
        target.bind("<<ComboboxSelected>>", lambda _event: self.refresh_preview())
        ttk.Button(toolbar, text="Analyze Preview", command=self.refresh_preview).pack(
            side=tk.LEFT, padx=(14, 2)
        )
        self.start_button = ttk.Button(
            toolbar,
            text="Archive / Recover",
            command=self.start_archive,
            state=tk.DISABLED,
        )
        self.start_button.pack(side=tk.LEFT, padx=2)
        self.cancel_button = ttk.Button(
            toolbar,
            text="Cancel",
            command=self.cancel_work,
            state=tk.DISABLED,
        )
        self.cancel_button.pack(side=tk.LEFT, padx=2)
        ttk.Button(toolbar, text="Query Latest Version", command=self.query_latest).pack(
            side=tk.LEFT, padx=(14, 2)
        )
        self.status_var = tk.StringVar(value="Load one WAV and one v1/v2/v3 metadata file.")
        ttk.Label(toolbar, textvariable=self.status_var).pack(side=tk.LEFT, padx=(16, 4))

        self.progress = ttk.Progressbar(self.root, mode="determinate", maximum=1.0)
        self.progress.pack(fill=tk.X, padx=8, pady=(0, 6))

        paned = ttk.Panedwindow(self.root, orient=tk.VERTICAL)
        paned.pack(fill=tk.BOTH, expand=True, padx=8, pady=(0, 8))
        self.notebook = ttk.Notebook(paned)
        paned.add(self.notebook, weight=5)

        self.segment_tree = self._make_tree_tab(
            "Segment Grid",
            (
                ("index", "Segment", 70),
                ("start", "Start", 95),
                ("end", "End", 95),
                ("st", "Short-term LUFS", 135),
                ("tp", "True Peak dBTP", 135),
                ("limit", "Limit dBTP", 110),
                ("excess", "Excess dB", 95),
                ("over", "Rework", 85),
                ("source", "Boundary Source", 230),
            ),
        )
        self.segment_tree.tag_configure("over", background="#ffe2e2", foreground="#8b0000")
        self.mapping_tree = self._make_tree_tab(
            "Field Mapping",
            (
                ("decision", "Decision ID", 190),
                ("field", "Field", 280),
                ("action", "Action", 100),
                ("source", "Source", 260),
                ("algorithm", "Algorithm", 330),
                ("result", "Result", 260),
            ),
        )
        self.recovery_tree = self._make_tree_tab(
            "Recovery Progress",
            (
                ("event", "Event", 150),
                ("segment", "Segment", 80),
                ("committed", "Committed", 100),
                ("total", "Total", 80),
                ("hash", "Hash / Detail", 700),
            ),
        )
        self.loss_text = self._make_text_tab("Loss Report")
        self.report_text = self._make_text_tab("Review / Version Result")
        self.trace_text = self._make_text_tab("Trace")

        bottom = ttk.Frame(self.root, padding=(8, 2))
        bottom.pack(fill=tk.X)
        self.wav_label = ttk.Label(bottom, text="WAV: -")
        self.wav_label.pack(side=tk.LEFT)
        self.meta_label = ttk.Label(bottom, text="Metadata: -")
        self.meta_label.pack(side=tk.LEFT, padx=24)
        self.hash_label = ttk.Label(bottom, text="Version: -")
        self.hash_label.pack(side=tk.RIGHT)

    def _make_tree_tab(
        self,
        title: str,
        columns: tuple[tuple[str, str, int], ...],
    ) -> ttk.Treeview:
        frame = ttk.Frame(self.notebook)
        self.notebook.add(frame, text=title)
        tree = ttk.Treeview(
            frame,
            columns=[column[0] for column in columns],
            show="headings",
        )
        for key, label, width in columns:
            tree.heading(key, text=label)
            tree.column(key, width=width, anchor=tk.W, stretch=True)
        y_scroll = ttk.Scrollbar(frame, orient=tk.VERTICAL, command=tree.yview)
        x_scroll = ttk.Scrollbar(frame, orient=tk.HORIZONTAL, command=tree.xview)
        tree.configure(yscrollcommand=y_scroll.set, xscrollcommand=x_scroll.set)
        tree.grid(row=0, column=0, sticky="nsew")
        y_scroll.grid(row=0, column=1, sticky="ns")
        x_scroll.grid(row=1, column=0, sticky="ew")
        frame.rowconfigure(0, weight=1)
        frame.columnconfigure(0, weight=1)
        return tree

    def _make_text_tab(self, title: str) -> tk.Text:
        frame = ttk.Frame(self.notebook)
        self.notebook.add(frame, text=title)
        text = tk.Text(frame, wrap=tk.NONE, font=("TkFixedFont", 9))
        y_scroll = ttk.Scrollbar(frame, orient=tk.VERTICAL, command=text.yview)
        x_scroll = ttk.Scrollbar(frame, orient=tk.HORIZONTAL, command=text.xview)
        text.configure(yscrollcommand=y_scroll.set, xscrollcommand=x_scroll.set)
        text.grid(row=0, column=0, sticky="nsew")
        y_scroll.grid(row=0, column=1, sticky="ns")
        x_scroll.grid(row=1, column=0, sticky="ew")
        frame.rowconfigure(0, weight=1)
        frame.columnconfigure(0, weight=1)
        return text

    def _post(self, callback: Callable[..., None], *args: Any) -> None:
        self.root.after(0, lambda: callback(*args))

    def _set_status(self, message: str) -> None:
        self.status_var.set(message)

    def _set_text(self, widget: tk.Text, value: Any) -> None:
        widget.delete("1.0", tk.END)
        widget.insert("1.0", json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))

    def load_wav(self) -> None:
        path = filedialog.askopenfilename(
            title="Load PCM WAV",
            filetypes=(("WAV files", "*.wav"), ("All files", "*")),
        )
        if not path:
            return
        try:
            self.wav = read_wav(path)
        except AppError as exc:
            messagebox.showerror("WAV rejected", str(exc))
            return
        self.wav_label.configure(text=f"WAV: {Path(path).name}")
        self._maybe_preview()

    def load_metadata(self) -> None:
        path = filedialog.askopenfilename(
            title="Load loudness metadata",
            filetypes=(("JSON metadata", "*.json"), ("All files", "*")),
        )
        if not path:
            return
        try:
            self.metadata = load_metadata(path, wav=self.wav)
        except AppError as exc:
            messagebox.showerror("Metadata rejected", str(exc))
            return
        self.meta_label.configure(text=f"Metadata: {Path(path).name} (v{self.metadata.version})")
        self._maybe_preview()

    def _maybe_preview(self) -> None:
        if self.wav is not None and self.metadata is not None:
            self.refresh_preview()

    def refresh_preview(self) -> None:
        if self.wav is None or self.metadata is None:
            self._set_status("Load both WAV and metadata before analysis.")
            return
        self.current_preview_token += 1
        token = self.current_preview_token
        target_version = int(self.target_var.get().lstrip("v"))
        self._set_status("Analyzing preview...")

        def work() -> None:
            try:
                upgraded = upgrade_to_v3(self.metadata, self.wav, self.config)
                derived = derive_boundaries(upgraded.document, self.config)
                analyzer = LoudnessAnalyzer(
                    self.wav,
                    self.config,
                    upgraded.document.downmix_weights,
                )
                analyses = analyzer.analyze_boundaries(
                    derived.segments,
                    upgraded.document.limits(),
                    derived.source,
                )
                downgrade = downgrade_document(
                    upgraded.document,
                    target_version,
                    self.wav,
                    self.config,
                    allow_loss=True,
                )
                mapping = list(upgraded.field_mapping) + list(downgrade.field_mapping)
                self._post(
                    self._apply_preview,
                    token,
                    upgraded,
                    analyses,
                    downgrade,
                    mapping,
                )
            except Exception as exc:  # noqa: BLE001 - GUI boundary
                self._post(self._preview_failed, token, exc)

        threading.Thread(target=work, name="loudness-preview", daemon=True).start()

    def _apply_preview(
        self,
        token: int,
        upgraded: Any,
        analyses: tuple[Any, ...],
        downgrade: Any,
        mapping: list[dict[str, Any]],
    ) -> None:
        if token != self.current_preview_token:
            return
        self.internal = upgraded.document
        self.preview_rows = [analysis.to_dict() for analysis in analyses]
        self.mapping = mapping
        self.loss_manifest = downgrade.loss_manifest
        self.segment_tree.delete(*self.segment_tree.get_children())
        for row in self.preview_rows:
            tag = ("over",) if row["over_limit"] else ()
            self.segment_tree.insert(
                "",
                tk.END,
                values=(
                    row["index"],
                    row["start_sample"],
                    row["end_sample"],
                    f'{row["short_term_lufs"]:.3f}',
                    f'{row["true_peak_dbtp"]:.3f}',
                    f'{row["true_peak_limit_dbtp"]:.3f}',
                    f'{row["excess_db"]:.3f}',
                    "REWORK" if row["over_limit"] else "ok",
                    row["boundary_source"],
                ),
                tags=tag,
            )
        self.mapping_tree.delete(*self.mapping_tree.get_children())
        for entry in mapping:
            self.mapping_tree.insert(
                "",
                tk.END,
                values=(
                    entry.get("decision_id", ""),
                    entry.get("field", ""),
                    entry.get("action", ""),
                    entry.get("source", ""),
                    entry.get("algorithm", ""),
                    json.dumps(entry.get("result"), ensure_ascii=False)[:500],
                ),
            )
        self._set_text(self.loss_text, self.loss_manifest)
        self._set_text(
            self.report_text,
            {
                "internal_version": 3,
                "target_version": int(self.target_var.get().lstrip("v")),
                "source_version": self.metadata.version,
                "boundary_derivation": upgraded.derived_boundaries.__dict__,
                "boundary_diff": upgraded.boundary_diff.to_dict(),
                "segments": self.preview_rows,
            },
        )
        if self.loss_manifest and self.loss_manifest.get("losses"):
            self._set_status(
                f"Preview ready. Target v{self.target_var.get()[1:]} will report E_VERSION_LOSS."
            )
        else:
            self._set_status("Preview ready. Segment grid and mapping table are current.")
        self.start_button.configure(state=tk.NORMAL)

    def _preview_failed(self, token: int, exc: Exception) -> None:
        if token != self.current_preview_token:
            return
        self._set_status("Preview failed.")
        messagebox.showerror("Preview failed", str(exc))

    def start_archive(self) -> None:
        if self.wav is None or self.metadata is None:
            messagebox.showwarning("Inputs missing", "Load a WAV and metadata file first.")
            return
        if self.worker is not None and self.worker.is_alive():
            messagebox.showwarning("Busy", "An archival task is already running.")
            return
        self.cancel_token = CancellationToken()
        target_version = int(self.target_var.get().lstrip("v"))
        self.start_button.configure(state=tk.DISABLED)
        self.cancel_button.configure(state=tk.NORMAL)
        self.recovery_tree.delete(*self.recovery_tree.get_children())
        self.progress.configure(value=0, maximum=1)
        self._set_status("Archiving/recovering segments...")

        def work() -> None:
            try:
                service = ArchiveService(self.workspace, self.config)
                result = service.execute(
                    self.wav,
                    self.metadata,
                    target_version=target_version,
                    cancel_token=self.cancel_token,
                    progress_callback=lambda event: self._post(self._progress, event),
                )
                publisher = FileVersionPublisher(self.workspace)
                published = publisher.publish(
                    result.manifest,
                    [analysis.to_dict() for analysis in result.analyses],
                    target_version=result.target_version,
                    loss_manifest=result.loss_manifest,
                    field_mapping=list(result.field_mapping),
                    expected_manifest_chain_head=result.manifest["chain_head"],
                )
                query = publisher.query(
                    published["version_id"],
                    expected_version_hash=published["content_hash"],
                )
                self._post(self._archive_complete, result, published, query)
            except Exception as exc:  # noqa: BLE001 - GUI boundary
                self._post(self._archive_failed, exc)

        self.worker = threading.Thread(target=work, name="loudness-archive", daemon=True)
        self.worker.start()

    def _progress(self, event: dict[str, Any]) -> None:
        committed = int(event.get("committed", 0) or 0)
        total = int(event.get("total_segments", 0) or 0)
        if total:
            self.progress.configure(maximum=max(1, total), value=min(committed, total))
        hash_value = event.get("chain_head") or event.get("hash") or ""
        self.recovery_tree.insert(
            "",
            tk.END,
            values=(
                event.get("event", ""),
                event.get("segment_index", ""),
                committed,
                total,
                hash_value,
            ),
        )
        self.recovery_tree.see(self.recovery_tree.get_children()[-1])
        self._set_status(
            f'{event.get("event", "progress")}: {committed}/{total or "?"} segments committed'
        )
        self._refresh_trace()

    def _archive_complete(
        self,
        result: Any,
        published: dict[str, Any],
        query: dict[str, Any],
    ) -> None:
        self.start_button.configure(state=tk.NORMAL)
        self.cancel_button.configure(state=tk.DISABLED)
        self.hash_label.configure(
            text=f'Version {published["version_id"]}  {published["content_hash"]}'
        )
        self._set_text(
            self.report_text,
            {
                "archive": result.to_dict(),
                "published": published,
                "query": {
                    "hit_version": query["hit_version"],
                    "content_hash": query["content_hash"],
                    "manifest_chain_head": query["manifest_chain_head"],
                },
            },
        )
        self.notebook.select(2)
        revision = result.manifest["manifest_revision"]
        self._set_status(
            f"Archive complete at manifest revision {revision}; published {published['version_id']}."
        )
        self._refresh_trace()

    def _archive_failed(self, exc: Exception) -> None:
        self.start_button.configure(state=tk.NORMAL)
        self.cancel_button.configure(state=tk.DISABLED)
        if isinstance(exc, AppError) and exc.code == E_CANCELLED:
            self._set_status("Cancelled. Committed segments remain recoverable.")
            return
        self._set_status("Archive failed.")
        messagebox.showerror("Archive failed", str(exc))

    def cancel_work(self) -> None:
        if self.cancel_token is not None:
            self.cancel_token.cancel()
            self.cancel_button.configure(state=tk.DISABLED)
            self._set_status("Cancellation requested; waiting for the current segment boundary.")

    def query_latest(self) -> None:
        try:
            result = FileVersionPublisher(self.workspace).query()
        except AppError as exc:
            messagebox.showerror("Version query failed", str(exc))
            return
        data = result["data"]
        self.hash_label.configure(
            text=f'Version {result["hit_version"]}  {result["content_hash"]}'
        )
        self._set_text(
            self.report_text,
            {
                "hit_version": result["hit_version"],
                "content_hash": result["content_hash"],
                "manifest_chain_head": result["manifest_chain_head"],
                "metadata_version": data.get("metadata_version"),
                "manifest_revision": data.get("manifest_revision"),
                "segments": data.get("segments"),
                "rework": data.get("rework"),
            },
        )
        self.notebook.select(4)
        self._set_status(
            f'Queried immutable version {result["hit_version"]} with complete snapshot.'
        )

    def _refresh_trace(self) -> None:
        path = self.workspace / "trace.jsonl"
        if not path.exists():
            return
        try:
            lines = path.read_text(encoding="utf-8").splitlines()[-300:]
        except OSError:
            return
        self.trace_text.delete("1.0", tk.END)
        self.trace_text.insert("1.0", "\n".join(lines))
        self.trace_text.see(tk.END)


def run_gui(config: Config, workspace: str = "workbench_data") -> None:
    root = tk.Tk()
    WorkbenchApp(root, config, workspace)
    root.mainloop()

