"""Tkinter 复核界面。

展示多场演练进度、各玩家累计配额曲线、透支/惩罚事件与分叉差异，
支持启动、暂停、协作式取消与检查点续算。无显示环境或缺少 tkinter
时抛出带说明的 RuntimeError，脚本化入口不依赖本模块。
"""

from __future__ import annotations

import os
import queue
import threading

from .baseline import compare_with_baseline, read_baseline
from .parser import parse_input_file
from .models import canonical_dumps
from .runner import BatchItem, CancellationToken, replay_batch


def launch(sample_dir: str, work_dir: str, parallelism: int = 2):
    try:
        import tkinter as tk
        from tkinter import ttk
    except ImportError as exc:
        raise RuntimeError(
            "当前环境缺少 Tkinter（Debian/Ubuntu 需安装 python3-tk）；"
            "无显示环境请使用 CLI 脚本化入口。"
        ) from exc

    scenarios = sorted(
        name
        for name in os.listdir(sample_dir)
        if os.path.isdir(os.path.join(sample_dir, name))
    )
    if not scenarios:
        raise RuntimeError(f"样本目录 {sample_dir} 下没有演练场景")

    class ReplayApp:
        def __init__(self, root):
            self.root = root
            root.title("战术演练配额复核台")
            self.token = CancellationToken()
            self.worker = None
            self.ui_queue: queue.Queue = queue.Queue()
            self._build_widgets(ttk, tk, scenarios)
            self.root.after(150, self._drain_queue)

        def _build_widgets(self, ttk, tk, scenarios):
            bar = ttk.Frame(self.root)
            bar.pack(fill="x", padx=8, pady=6)
            self.start_btn = ttk.Button(bar, text="启动", command=self.start)
            self.start_btn.pack(side="left")
            self.pause_btn = ttk.Button(
                bar, text="暂停", command=self.toggle_pause, state="disabled"
            )
            self.pause_btn.pack(side="left", padx=4)
            self.cancel_btn = ttk.Button(
                bar, text="取消", command=self.cancel, state="disabled"
            )
            self.cancel_btn.pack(side="left")
            ttk.Label(bar, text=f"并行度固定输入: {parallelism}").pack(side="right")

            self.progress = ttk.Progressbar(
                self.root, maximum=len(scenarios), mode="determinate"
            )
            self.progress.pack(fill="x", padx=8)

            body = ttk.Panedwindow(self.root, orient="horizontal")
            body.pack(fill="both", expand=True, padx=8, pady=6)

            left = ttk.Frame(body)
            self.tree = ttk.Treeview(
                left,
                columns=("exercise", "steps", "penalty", "hash"),
                show="headings",
            )
            for col, text, width in (
                ("exercise", "演练", 160),
                ("steps", "已结算步", 80),
                ("penalty", "累计扣减", 100),
                ("hash", "末步哈希前缀", 140),
            ):
                self.tree.heading(col, text=text)
                self.tree.column(col, width=width)
            self.tree.pack(fill="both", expand=True)
            body.add(left, weight=2)

            right = ttk.Frame(body)
            ttk.Label(right, text="透支 / 惩罚 / 分叉事件").pack(anchor="w")
            self.events = tk.Listbox(right)
            self.events.pack(fill="both", expand=True)
            body.add(right, weight=3)

            self.status = ttk.Label(self.root, text="就绪")
            self.status.pack(fill="x", padx=8, pady=4)

        def start(self):
            self.start_btn.config(state="disabled")
            self.pause_btn.config(state="normal")
            self.cancel_btn.config(state="normal")
            self.status.config(text="重演中…")
            self.token = CancellationToken()
            self.worker = threading.Thread(target=self._run_batch, daemon=True)
            self.worker.start()

        def toggle_pause(self):
            if self.pause_btn.config("text")[-1] == "暂停":
                self.token.pause()
                self.pause_btn.config(text="继续")
                self.status.config(text="已暂停")
            else:
                self.token.resume()
                self.pause_btn.config(text="暂停")
                self.status.config(text="重演中…")

        def cancel(self):
            self.token.cancel()
            self.status.config(text="取消中…")

        def _run_batch(self):
            items = []
            self.item_scenarios = []
            for scenario in scenarios:
                scenario_dir = os.path.join(sample_dir, scenario)
                config, operations = parse_input_file(
                    os.path.join(scenario_dir, "input.json")
                )
                cp_dir = os.path.join(work_dir, "checkpoints", scenario)
                traj = os.path.join(work_dir, "trajectories", f"{scenario}.jsonl")
                items.append(
                    BatchItem(
                        name=scenario,
                        config=config,
                        operations=operations,
                        checkpoint_dir=cp_dir,
                        trajectory_path=traj,
                    )
                )
                self.item_scenarios.append(scenario)
            try:
                outcomes = replay_batch(
                    items,
                    parallelism=parallelism,
                    token=self.token,
                    on_progress=lambda idx, outcome: self.ui_queue.put(
                        ("progress", idx, outcome)
                    ),
                )
                for idx, outcome in enumerate(outcomes):
                    self.ui_queue.put(("done", idx, outcome))
                self.ui_queue.put(("all_done", None, None))
            except Exception as exc:  # noqa: BLE001 - GUI 需要展示全部失败
                self.ui_queue.put(("error", None, exc))

        def _drain_queue(self):
            try:
                while True:
                    kind, idx, payload = self.ui_queue.get_nowait()
                    if kind == "progress":
                        self.progress.step(1)
                    elif kind == "done":
                        self._render_outcome(idx, payload)
                    elif kind == "all_done":
                        self.status.config(text="全部完成，可再次启动以检查点续算")
                        self._reset_buttons()
                    elif kind == "error":
                        self.status.config(text=f"失败: {payload}")
                        self._reset_buttons()
            except queue.Empty:
                pass
            self.root.after(150, self._drain_queue)

        def _reset_buttons(self):
            self.start_btn.config(state="normal")
            self.pause_btn.config(state="disabled", text="暂停")
            self.cancel_btn.config(state="disabled")

        def _render_outcome(self, idx, outcome):
            results = outcome.results
            total_penalty = (
                sum(results[-1].cumulative_penalty.values()) if results else 0.0
            )
            hash_prefix = results[-1].step_hash[:16] if results else "-"
            self.tree.insert(
                "",
                "end",
                values=(outcome.exercise_id, len(results), total_penalty, hash_prefix),
            )
            for result in results:
                for event in result.quota_events:
                    self.events.insert(
                        "end",
                        f"[透支] 步{event.step} {event.player_id} "
                        f"尝试{event.attempted_total} > 上限{event.quota} "
                        f"策略={event.policy}",
                    )
                for penalty in result.penalties:
                    self.events.insert(
                        "end",
                        f"[惩罚] 步{penalty.step} {penalty.player_id} "
                        f"{penalty.kind} 扣减={penalty.deduction}",
                    )
            scenario = self.item_scenarios[idx]
            baseline_path = os.path.join(sample_dir, scenario, "baseline.json")
            if os.path.exists(baseline_path):
                try:
                    divergence = compare_with_baseline(
                        read_baseline(baseline_path),
                        self._config_for(idx),
                        results,
                    )
                except Exception as exc:  # noqa: BLE001
                    self.events.insert("end", f"[基线错误] {exc}")
                    divergence = None
                if divergence is not None:
                    self.events.insert(
                        "end", f"[分叉] {canonical_dumps(divergence.to_dict())}"
                    )
                else:
                    self.events.insert("end", f"[一致] {scenario} 与基线无分叉")

        def _config_for(self, idx):
            scenario = self.item_scenarios[idx]
            config, _ = parse_input_file(
                os.path.join(sample_dir, scenario, "input.json")
            )
            return config

    root = tk.Tk()
    ReplayApp(root)
    root.mainloop()
