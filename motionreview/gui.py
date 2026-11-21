"""Tk desktop workbench.

The GUI deliberately depends only on the Python standard library plus NumPy.
It uses a canvas projection instead of a GPU/OpenGL dependency so it runs on
the same Windows review desktop as the solver.
"""

from __future__ import annotations

import json
import math
import tempfile
import threading
from pathlib import Path
from typing import Any

from .bvh import BVHDocument, evaluate_frame, parse_bvh_file
from .config import RunConfig, load_run_config
from .constraints import ConstraintSet
from .generator import load_case_constraints
from .scheduler import ClipTask, Scheduler
from .solver import FrameSolution, PoseSolver
from .storage import SnapshotStore


def _heat_color(ratio: float) -> str:
    ratio = max(0.0, min(1.0, ratio))
    red = int(40 + 215 * ratio)
    green = int(190 * (1.0 - ratio) + 40 * ratio)
    blue = int(60 * (1.0 - ratio))
    return f"#{red:02x}{green:02x}{blue:02x}"


class MotionReviewApp:
    def __init__(
        self,
        tk: Any,
        ttk: Any,
        *,
        sample_root: Path,
        case_name: str,
        config: RunConfig,
        output_root: Path,
    ) -> None:
        self.tk = tk
        self.ttk = ttk
        self.sample_root = sample_root
        self.case_name = case_name
        self.config = config
        self.output_root = output_root
        self.document, self.constraints = self._load_case()
        self.run_root = (
            output_root
            / self.case_name
            / self.constraints.parameter_version[:10]
        )
        self.solutions: dict[int, FrameSolution] = self._load_committed()
        self.current_frame = 0
        self.playing = False
        self.yaw = 0.62
        self.pitch = -0.42
        self._drag_origin: tuple[int, int] | None = None
        self.scheduler: Scheduler | None = None
        self.future: Any = None
        self._poll_job: str | None = None
        self._play_job: str | None = None
        self._pending_edit: tuple[
            tuple[int, int], dict[int, FrameSolution], str
        ] | None = None
        self._rebuild_window()

    def _load_case(self) -> tuple[BVHDocument, ConstraintSet]:
        manifest = json.loads(
            (self.sample_root / "manifest.json").read_text(
                encoding="utf-8"
            )
        )
        for case in manifest["cases"]:
            if case["name"] == self.case_name:
                document = parse_bvh_file(
                    self.sample_root / case["bvh"],
                    clip_id=self.case_name,
                )
                return document, load_case_constraints(
                    document,
                    self.sample_root / case["constraints"],
                )
        raise ValueError(f"sample case not found: {self.case_name}")

    def _load_committed(self) -> dict[int, FrameSolution]:
        try:
            state = SnapshotStore(self.run_root, self.case_name).recover(
                expected_parameter_version=PoseSolver(
                    self.document,
                    self.constraints,
                    max_iterations=self.config.max_iterations,
                ).parameter_version,
                expected_document_hash=self.document.content_hash,
            )
        except Exception:
            return {}
        return {solution.frame: solution for solution in state.solutions}

    def _rebuild_window(self) -> None:
        tk = self.tk
        ttk = self.ttk
        for child in self.root.winfo_children() if hasattr(self, "root") else []:
            child.destroy()
        if not hasattr(self, "root"):
            self.root = tk.Tk()
            self.root.title("MotionReview — BVH Constraint Workbench")
            self.root.geometry("1240x800")
            self.root.minsize(960, 640)
            self.root.protocol("WM_DELETE_WINDOW", self.close)
        root = self.root
        root.columnconfigure(0, weight=1)
        root.rowconfigure(0, weight=1)

        main = ttk.Frame(root, padding=6)
        main.grid(row=0, column=0, sticky="nsew")
        main.columnconfigure(0, weight=1)
        main.rowconfigure(0, weight=1)

        self.canvas = tk.Canvas(
            main,
            background="#11161d",
            highlightthickness=0,
            width=860,
            height=700,
        )
        self.canvas.grid(row=0, column=0, sticky="nsew")
        self.canvas.bind("<ButtonPress-1>", self._drag_start)
        self.canvas.bind("<B1-Motion>", self._drag_move)

        side = ttk.Frame(main, padding=(10, 0, 0, 0), width=330)
        side.grid(row=0, column=1, sticky="ns")
        side.columnconfigure(0, weight=1)

        title = ttk.Label(
            side,
            text=f"{self.case_name}  •  {self.document.frame_count} frames  •  "
            f"{len(self.document.joints)} joints",
            font=("Segoe UI", 10, "bold"),
        )
        title.grid(row=0, column=0, sticky="w", pady=(0, 8))

        self.frame_var = tk.IntVar(value=self.current_frame)
        frame_row = ttk.Frame(side)
        frame_row.grid(row=1, column=0, sticky="ew")
        frame_row.columnconfigure(1, weight=1)
        ttk.Label(frame_row, text="Frame").grid(row=0, column=0)
        self.frame_scale = ttk.Scale(
            frame_row,
            from_=0,
            to=max(0, self.document.frame_count - 1),
            orient="horizontal",
            variable=self.frame_var,
            command=self._on_frame_scale,
        )
        self.frame_scale.grid(row=0, column=1, sticky="ew", padx=6)
        self.frame_label = ttk.Label(
            frame_row, text=str(self.current_frame), width=6
        )
        self.frame_label.grid(row=0, column=2)

        playback = ttk.Frame(side)
        playback.grid(row=2, column=0, sticky="ew", pady=6)
        self.play_button = ttk.Button(
            playback, text="▶ Play", command=self.toggle_play
        )
        self.play_button.pack(side="left")
        ttk.Button(
            playback, text="◀", command=lambda: self.step_frame(-1)
        ).pack(side="left", padx=(6, 0))
        ttk.Button(
            playback, text="▶", command=lambda: self.step_frame(1)
        ).pack(side="left", padx=(2, 0))

        ttk.Separator(side).grid(row=3, column=0, sticky="ew", pady=6)
        ttk.Label(
            side, text="Constraints (select to edit)", font=("Segoe UI", 10, "bold")
        ).grid(row=4, column=0, sticky="w")
        self.constraint_tree = ttk.Treeview(
            side,
            columns=("id", "kind", "weight", "tol"),
            show="headings",
            height=11,
        )
        self.constraint_tree.heading("id", text="ID")
        self.constraint_tree.heading("kind", text="Kind")
        self.constraint_tree.heading("weight", text="Weight")
        self.constraint_tree.heading("tol", text="Tolerance")
        self.constraint_tree.column("id", width=118, stretch=True)
        self.constraint_tree.column("kind", width=96, stretch=True)
        self.constraint_tree.column("weight", width=54, stretch=False)
        self.constraint_tree.column("tol", width=62, stretch=False)
        self.constraint_tree.grid(row=5, column=0, sticky="ew", pady=4)
        self.constraint_tree.bind(
            "<<TreeviewSelect>>", self._on_constraint_select
        )
        self._refresh_constraint_tree()

        edit = ttk.Frame(side)
        edit.grid(row=6, column=0, sticky="ew", pady=4)
        ttk.Label(edit, text="Weight").grid(row=0, column=0, sticky="w")
        self.weight_var = tk.StringVar()
        self.weight_entry = ttk.Entry(
            edit, textvariable=self.weight_var, width=8
        )
        self.weight_entry.grid(row=0, column=1, padx=(4, 10))
        ttk.Label(edit, text="Tolerance").grid(row=0, column=2, sticky="w")
        self.tolerance_var = tk.StringVar()
        self.tolerance_entry = ttk.Entry(
            edit, textvariable=self.tolerance_var, width=10
        )
        self.tolerance_entry.grid(row=0, column=3, padx=4)
        self.apply_button = ttk.Button(
            edit, text="Apply / Re-solve affected", command=self.apply_edit
        )
        self.apply_button.grid(row=1, column=0, columnspan=4, sticky="ew", pady=4)

        ttk.Separator(side).grid(row=7, column=0, sticky="ew", pady=6)
        run_row = ttk.Frame(side)
        run_row.grid(row=8, column=0, sticky="ew")
        self.start_button = ttk.Button(
            run_row, text="Start", command=self.start_run
        )
        self.start_button.pack(side="left")
        self.pause_button = ttk.Button(
            run_row, text="Pause", command=self.pause_run
        )
        self.pause_button.pack(side="left", padx=4)
        self.cancel_button = ttk.Button(
            run_row, text="Cancel", command=self.cancel_run
        )
        self.cancel_button.pack(side="left")
        self.resume_button = ttk.Button(
            run_row, text="Resume snapshot", command=self.resume_run
        )
        self.resume_button.pack(side="left", padx=(4, 0))

        self.status_var = tk.StringVar(value="Ready")
        self.status_label = ttk.Label(
            side,
            textvariable=self.status_var,
            wraplength=310,
            justify="left",
        )
        self.status_label.grid(row=9, column=0, sticky="ew", pady=(8, 2))
        self.heat_var = tk.StringVar(value="Residual heat: —")
        ttk.Label(
            side,
            textvariable=self.heat_var,
            wraplength=310,
            justify="left",
        ).grid(row=10, column=0, sticky="ew")
        self.detail_text = tk.Text(
            side, height=7, width=38, background="#f4f6f8", relief="flat"
        )
        self.detail_text.grid(row=11, column=0, sticky="ew", pady=(8, 0))
        self.canvas.bind("<Configure>", lambda event: self.render())
        self._update_pose_view()

    def _refresh_constraint_tree(self) -> None:
        for item in self.constraint_tree.get_children():
            self.constraint_tree.delete(item)
        for constraint in self.constraints:
            self.constraint_tree.insert(
                "",
                "end",
                iid=constraint.id,
                values=(
                    constraint.id,
                    constraint.kind,
                    f"{constraint.weight:g}",
                    f"{constraint.tolerance:g}",
                ),
            )

    def _on_constraint_select(self, _event: Any) -> None:
        selection = self.constraint_tree.selection()
        if not selection:
            return
        constraint = self.constraints.by_id(selection[0])
        self.weight_var.set(f"{constraint.weight:.8g}")
        self.tolerance_var.set(f"{constraint.tolerance:.8g}")

    def apply_edit(self) -> None:
        selection = self.constraint_tree.selection()
        if not selection:
            self.status_var.set("Select a constraint first")
            return
        constraint_id = selection[0]
        try:
            weight = float(self.weight_var.get())
            tolerance = float(self.tolerance_var.get())
        except ValueError:
            self.status_var.set("Weight and tolerance must be numeric")
            return
        old = self.constraints.by_id(constraint_id)
        updated = ConstraintSet(
            tuple(
                constraint.with_changes(
                    weight=weight, tolerance=tolerance
                )
                if constraint.id == constraint_id
                else constraint
                for constraint in self.constraints
            ),
            self.document.content_hash,
        )
        affected = updated.changed_frames(
            self.constraints, [constraint_id]
        )
        if not affected:
            self.status_var.set("No affected frames")
            return
        affected_range = (affected[0], affected[-1] + 1)
        self.constraints = updated
        self._refresh_constraint_tree()
        self.constraint_tree.selection_set(constraint_id)
        self.weight_var.set(f"{updated.by_id(constraint_id).weight:.8g}")
        self.status_var.set(
            f"Re-solving affected interval "
            f"[{affected_range[0]}, {affected_range[1]}) only…"
        )
        self._pending_edit = None

        def work() -> None:
            solver = PoseSolver(
                self.document,
                ConstraintSet(
                    tuple(
                        constraint.with_changes(
                            weight=old.weight,
                            tolerance=old.tolerance,
                        )
                        if constraint.id == constraint_id
                        else constraint
                        for constraint in updated
                    ),
                    self.document.content_hash,
                ),
                max_iterations=self.config.max_iterations,
                cache_depth=max(1, affected_range[1] - affected_range[0]),
            )
            _, batch = solver.resolve_affected(updated)
            assert batch is not None
            affected_solutions = {
                solution.frame: solution for solution in batch.solutions
            }
            self._pending_edit = (
                affected_range,
                affected_solutions,
                constraint_id,
            )

        threading.Thread(target=work, daemon=True).start()
        self.root.after(50, self._poll_edit)

    def _poll_edit(self) -> None:
        if self._pending_edit is None:
            self.root.after(50, self._poll_edit)
            return
        affected_range, affected_solutions, constraint_id = (
            self._pending_edit
        )
        self._pending_edit = None
        self._finish_edit(
            affected_range, affected_solutions, constraint_id
        )

    def _finish_edit(
        self,
        affected_range: tuple[int, int],
        affected_solutions: dict[int, FrameSolution],
        constraint_id: str,
    ) -> None:
        self.solutions.update(affected_solutions)
        self.status_var.set(
            f"Re-solved [{affected_range[0]}, {affected_range[1]}) "
            f"for {constraint_id}; other frames reused cached hashes"
        )
        self._update_pose_view()

    def _run_root_for_current(self) -> Path:
        return (
            self.output_root
            / self.case_name
            / self.constraints.parameter_version[:10]
        )

    def start_run(self) -> None:
        if self.scheduler is not None:
            self.status_var.set("A run is already active")
            return
        self._start_task(resume=False)

    def resume_run(self) -> None:
        if self.scheduler is not None:
            self.scheduler.resume_from_pause(self.case_name)
            self.status_var.set("Run resumed from pause checkpoint")
            return
        self._start_task(resume=True)

    def _start_task(self, *, resume: bool) -> None:
        task = ClipTask(
            self.case_name, self.document, self.constraints, resume=resume
        )
        self.scheduler = Scheduler(
            self.config, self._run_root_for_current()
        )
        try:
            self.future = self.scheduler.start_clip(task)
        except Exception as exc:
            self.status_var.set(str(exc))
            self.scheduler.close()
            self.scheduler = None
            return
        self.status_var.set("Running…")
        self._schedule_poll()

    def pause_run(self) -> None:
        if self.scheduler is None:
            self.status_var.set("No active run")
            return
        self.scheduler.pause(self.case_name)
        self.status_var.set("Pause requested at next checkpoint")

    def cancel_run(self) -> None:
        if self.scheduler is None:
            self.status_var.set("No active run")
            return
        self.scheduler.cancel(self.case_name)
        self.status_var.set("Cooperative cancellation requested")

    def _schedule_poll(self) -> None:
        self._poll_job = self.root.after(120, self._poll_run)

    def _poll_run(self) -> None:
        self._load_latest_solutions()
        self._update_pose_view()
        if self.future is not None and self.future.done():
            try:
                report = self.future.result()
                self.status_var.set(
                    f"{report.status}: committed "
                    f"{report.committed_frames} frames, next "
                    f"{report.next_frame}, stage {report.stage_name}"
                )
            except Exception as exc:
                self.status_var.set(f"Run failed: {exc}")
            finally:
                if self.scheduler is not None:
                    self.scheduler.close()
                self.scheduler = None
                self.future = None
                self._poll_job = None
            return
        self._schedule_poll()

    def _load_latest_solutions(self) -> None:
        try:
            store = SnapshotStore(self._run_root_for_current(), self.case_name)
            state = store.recover(
                expected_parameter_version=PoseSolver(
                    self.document,
                    self.constraints,
                    max_iterations=self.config.max_iterations,
                ).parameter_version,
                expected_document_hash=self.document.content_hash,
            )
        except Exception:
            return
        for solution in state.solutions:
            self.solutions[solution.frame] = solution
        if self.solutions:
            self.frame_scale.configure(
                to=max(0, max(self.solutions))
            )

    def toggle_play(self) -> None:
        self.playing = not self.playing
        self.play_button.configure(
            text="❚❚ Pause" if self.playing else "▶ Play"
        )
        if self.playing:
            self._play_tick()
        elif self._play_job is not None:
            self.root.after_cancel(self._play_job)
            self._play_job = None

    def _play_tick(self) -> None:
        if not self.playing:
            return
        self.step_frame(1)
        self._play_job = self.root.after(80, self._play_tick)

    def step_frame(self, delta: int) -> None:
        current = int(self.frame_var.get())
        target = max(
            0,
            min(self.document.frame_count - 1, current + delta),
        )
        self.frame_var.set(target)
        self._on_frame_scale(str(target))

    def _on_frame_scale(self, value: str) -> None:
        try:
            frame = int(round(float(value)))
        except ValueError:
            return
        self.current_frame = frame
        self.frame_label.configure(text=str(frame))
        self._update_pose_view()

    def _current_solution(self) -> FrameSolution | None:
        return self.solutions.get(self.current_frame)

    def _current_transforms(self) -> Any:
        solution = self._current_solution()
        if solution is not None:
            return solution.transforms
        return evaluate_frame(self.document, self.current_frame)

    def _update_pose_view(self) -> None:
        solution = self._current_solution()
        if solution is None:
            self.heat_var.set(
                "Residual heat: baseline FK (not yet committed)"
            )
            if hasattr(self, "detail_text"):
                self.detail_text.delete("1.0", "end")
                self.detail_text.insert(
                    "end",
                    "No committed result for this frame.\n"
                    "Start a run or move to a committed frame.",
                )
        else:
            self.heat_var.set(
                f"Total weighted residual: {solution.total_residual:.8f} "
                f"• {solution.status} • {solution.iterations} iterations"
            )
            if hasattr(self, "detail_text"):
                self.detail_text.delete("1.0", "end")
                for record in solution.residuals:
                    self.detail_text.insert(
                        "end",
                        f"{record['constraint_id']}: "
                        f"{record['residual']:.8f} "
                        f"({'OK' if record['satisfied'] else 'VIOLATED'})\n",
                    )
        self.render()

    def _project(
        self, point: Any
    ) -> tuple[float, float, float]:
        x, y, z = float(point[0]), float(point[1]), float(point[2])
        cos_y, sin_y = math.cos(self.yaw), math.sin(self.yaw)
        x1 = x * cos_y - z * sin_y
        z1 = x * sin_y + z * cos_y
        cos_p, sin_p = math.cos(self.pitch), math.sin(self.pitch)
        y1 = y * cos_p - z1 * sin_p
        depth = y * sin_p + z1 * cos_p
        return x1, y1, depth

    def render(self) -> None:
        if not hasattr(self, "canvas"):
            return
        canvas = self.canvas
        canvas.delete("all")
        width = max(600, canvas.winfo_width())
        height = max(400, canvas.winfo_height())
        transforms = self._current_transforms()
        positions = [
            tuple(float(v) for v in transform[:3, 3])
            for transform in transforms
        ]
        projected = [self._project(position) for position in positions]
        span_x = max(1.0, max(p[0] for p in projected) - min(p[0] for p in projected))
        span_y = max(1.0, max(p[1] for p in projected) - min(p[1] for p in projected))
        scale = min(width * 0.72 / span_x, height * 0.72 / span_y)
        center_x = sum(p[0] for p in projected) / len(projected)
        center_y = sum(p[1] for p in projected) / len(projected)

        def screen(point: tuple[float, float, float]) -> tuple[float, float]:
            return (
                width / 2 + (point[0] - center_x) * scale,
                height * 0.58 - (point[1] - center_y) * scale,
            )

        for grid in range(-6, 7, 2):
            left = screen(self._project((grid * 3.0, -3.0, -18.0)))
            right = screen(self._project((grid * 3.0, -3.0, 18.0)))
            canvas.create_line(
                *left, *right, fill="#1b2733", width=1
            )
        for grid in range(-6, 7, 2):
            left = screen(self._project((-18.0, -3.0, grid * 3.0)))
            right = screen(self._project((18.0, -3.0, grid * 3.0)))
            canvas.create_line(
                *left, *right, fill="#1b2733", width=1
            )

        solution = self._current_solution()
        joint_heat: dict[int, float] = {}
        if solution is not None:
            for record in solution.residuals:
                joint_index = self.document.joint_index(record["joint"])
                ratio = float(record["residual"]) / max(
                    float(record["tolerance"]), 1e-9
                )
                joint_heat[joint_index] = max(
                    joint_heat.get(joint_index, 0.0), ratio
                )
        for joint in self.document.joints:
            if joint.parent is None:
                continue
            start = screen(projected[joint.parent])
            end = screen(projected[joint.index])
            ratio = joint_heat.get(joint.index, 0.0)
            canvas.create_line(
                *start,
                *end,
                fill=_heat_color(ratio),
                width=4,
                smooth=True,
            )
        for joint in self.document.joints:
            x, y = screen(projected[joint.index])
            ratio = joint_heat.get(joint.index, 0.0)
            radius = 5 if ratio < 1.0 else 7
            canvas.create_oval(
                x - radius,
                y - radius,
                x + radius,
                y + radius,
                fill=_heat_color(ratio),
                outline="#d8e2e8",
                width=1,
            )
            if joint.index % 3 == 0:
                canvas.create_text(
                    x + 8,
                    y - 8,
                    text=joint.name,
                    fill="#a9bac7",
                    font=("Segoe UI", 8),
                    anchor="sw",
                )
        if solution is not None:
            for record in solution.residuals:
                if record["kind"] != "effector_position":
                    continue
                target = record["detail"].get("target")
                if not target:
                    continue
                x, y = screen(self._project(target))
                canvas.create_line(
                    x - 7,
                    y,
                    x + 7,
                    y,
                    fill="#ffd166",
                    width=2,
                )
                canvas.create_line(
                    x,
                    y - 7,
                    x,
                    y + 7,
                    fill="#ffd166",
                    width=2,
                )
                canvas.create_text(
                    x + 9,
                    y + 8,
                    text=record["constraint_id"],
                    fill="#ffd166",
                    font=("Segoe UI", 8),
                    anchor="nw",
                )
        canvas.create_text(
            16,
            14,
            text=f"frame {self.current_frame}  |  "
            f"yaw {self.yaw:.2f}  pitch {self.pitch:.2f}  |  "
            f"drag to orbit",
            fill="#8395a5",
            anchor="nw",
            font=("Segoe UI", 9),
        )
        conflict = (
            solution.conflict if solution is not None else None
        )
        if conflict:
            canvas.create_text(
                16,
                height - 34,
                text=(
                    f"CONFLICT frame {conflict['frame']}: "
                    + ", ".join(conflict["constraint_ids"])
                ),
                fill="#ff6b6b",
                anchor="sw",
                font=("Segoe UI", 10, "bold"),
            )

    def _drag_start(self, event: Any) -> None:
        self._drag_origin = (event.x, event.y)

    def _drag_move(self, event: Any) -> None:
        if self._drag_origin is None:
            return
        previous_x, previous_y = self._drag_origin
        self.yaw += (event.x - previous_x) * 0.008
        self.pitch += (event.y - previous_y) * 0.006
        self.pitch = max(-1.2, min(1.2, self.pitch))
        self._drag_origin = (event.x, event.y)
        self.render()

    def mainloop(self) -> None:
        self.root.mainloop()

    def close(self) -> None:
        self.playing = False
        if self.scheduler is not None:
            self.scheduler.close(wait=False)
            self.scheduler = None
        self.root.destroy()


def run_gui(
    *,
    sample_root: str | None,
    case_name: str,
    config_path: str | None,
    output_root: str,
) -> int:
    try:
        import tkinter as tk
        from tkinter import ttk
    except ImportError:
        print(
            "E_GUI_UNAVAILABLE: tkinter is not installed. "
            "Install the Python Tk component (for example "
            "'sudo apt install python3-tk' on Debian/Ubuntu or the "
            "official Windows Python installer with Tcl/Tk).",
            file=__import__("sys").stderr,
        )
        return 6

    if config_path:
        config = load_run_config(config_path)
    else:
        config = RunConfig(
            max_parallelism=2,
            per_task_memory_bytes=64 * 1024 * 1024,
            total_memory_bytes=128 * 1024 * 1024,
            max_iterations=40,
            checkpoint_frames=16,
        )
    if sample_root is None:
        temporary = tempfile.mkdtemp(prefix="motionreview-gui-samples-")
        root = Path(temporary)
        from .generator import SampleCase, generate_case

        case_manifest = generate_case(
            root,
            SampleCase(case_name, 96, 7),
            seed=42,
            write_baseline=False,
            max_iterations=config.max_iterations,
        )
        manifest_path = root / "manifest.json"
        manifest = {
            "schema": "sample-dataset-v1",
            "seed": 42,
            "cases": [case_manifest],
        }
        manifest_path.write_text(
            json.dumps(manifest, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        sample_root = str(root)

    app = MotionReviewApp(
        tk,
        ttk,
        sample_root=Path(sample_root),
        case_name=case_name,
        config=config,
        output_root=Path(output_root),
    )
    app.mainloop()
    return 0
