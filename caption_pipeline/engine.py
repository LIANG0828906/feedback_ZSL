# 管线引擎：包络检测 -> 窗口推导 -> 并行折行排布 -> 串行提交。
from __future__ import annotations

import time
from dataclasses import dataclass, field

import numpy as np

from .checkpoint import CheckpointStore
from .envelope import detect_segments
from .errors import E_FIT, E_SEG, PipelineError
from .fit import merge_fit_errors, reconcile
from .layout import split_screens, wrap_text
from .scheduler import Budget, Scheduler
from .states import LineState, LineStateMachine
from .windows import Window, derive_windows

ALGO_VERSION = "1.0.0"


@dataclass
class LineResult:
    line_id: str
    ord: int
    start: int
    end: int
    screens: list
    tradeoffs: list = field(default_factory=list)

    def key(self):
        # 用于串行/并行结果一致性比对的规范化元组。
        return (self.line_id, self.start, self.end,
                tuple(tuple(sc) for sc in self.screens),
                tuple((t["line_id"], t["means"], t["reason"])
                      for t in self.tradeoffs))


class Engine:
    # hooks 可选回调：on_commit(line_id, committed, total)、
    # should_stop() -> bool、commit_delay_s（演示/测试用）。
    def __init__(self, project, store: CheckpointStore, budget: Budget,
                 hooks=None):
        self.project = project
        self.store = store
        self.budget = budget
        self.hooks = hooks or {}

    def run(self, max_commits=None):
        project = self.project
        pres = project.presentation
        committed_ids = self.store.begin(project.content_hash,
                                         pres.param_version, ALGO_VERSION)

        samples = np.load(project.audio_path)
        detected = detect_segments(samples, project.frame_len, project.hop)
        if len(detected) != len(project.segments):
            raise PipelineError(
                E_SEG, "检测到的语音段数量与清单声明不一致",
                details={"declared": len(project.segments),
                         "detected": len(detected)})

        windows = derive_windows(project, detected, total_samples=len(samples))
        order = project.timeline_order()
        ord_of = {line.id: idx for idx, line in enumerate(order)}

        # 每句台词在取舍阶段可回收的空隙：下一句窗口起点之前的余量。
        gaps = {}
        for idx, line in enumerate(order):
            w = windows[line.id]
            if idx + 1 < len(order):
                boundary = windows[order[idx + 1].id].start
            else:
                boundary = len(samples)
            gaps[line.id] = max(0, boundary - w.end)

        machines = {line.id: LineStateMachine() for line in order}
        for line in order:
            machines[line.id].deps_met = True  # 包络与窗口推导已完成

        todo = [line for line in order if line.id not in committed_ids]

        def make_task(line):
            def work():
                sm = machines[line.id]
                sm.transition(LineState.LAYING_OUT)
                lines = wrap_text(line.text, pres.max_chars_per_line,
                                  pres.wrap_weight)
                screens = split_screens(lines, pres.line_height, pres.margin,
                                        pres.screen_height)
                w0 = windows[line.id]
                window = Window(w0.start, w0.end, w0.deficit)
                new_window, new_screens, tradeoffs = reconcile(
                    line, window, screens, pres, project.sample_rate,
                    gaps[line.id])
                return LineResult(
                    line_id=line.id, ord=ord_of[line.id],
                    start=new_window.start, end=new_window.end,
                    screens=new_screens,
                    tradeoffs=[t.to_dict() for t in tradeoffs])
            return work

        seg_map = project.segment_map()
        tasks = []
        for line in todo:
            seg = seg_map[line.segment_id]
            sample_bytes = int((seg.end - seg.start) * samples.dtype.itemsize)
            text_bytes = len(line.text.encode("utf-8"))
            tasks.append((line.id, text_bytes, sample_bytes, make_task(line)))

        scheduler = Scheduler(self.budget)
        futures = scheduler.submit_all(tasks) if tasks else {}

        results_committed_now = 0
        fit_errors = []
        committed_count = len(committed_ids)
        total = len(order)
        try:
            for line in todo:
                if self._should_stop():
                    break
                if max_commits is not None \
                        and results_committed_now >= max_commits:
                    break
                sm = machines[line.id]
                try:
                    res = futures[line.id].result()
                except PipelineError as exc:
                    sm.transition(LineState.FAILED)
                    self.store.mark_failed(line.id, ord_of[line.id], str(exc))
                    if exc.code == E_FIT:
                        fit_errors.append(exc)
                        continue
                    raise
                sm.transition(LineState.COMMITTED)
                self.store.commit_line({
                    "line_id": res.line_id, "ord": res.ord,
                    "win_start": res.start, "win_end": res.end,
                    "screens": res.screens, "tradeoffs": res.tradeoffs})
                committed_count += 1
                results_committed_now += 1
                on_commit = self.hooks.get("on_commit")
                if on_commit:
                    on_commit(line.id, committed_count, total)
                delay = self.hooks.get("commit_delay_s", 0)
                if delay:
                    time.sleep(delay)
        finally:
            scheduler.shutdown()

        if fit_errors:
            raise merge_fit_errors(fit_errors)
        return {"results": self._collect_results(),
                "committed": committed_count, "total": total}

    def _collect_results(self):
        _, records = self.store.inspect()
        results = {}
        for rec in records:
            if rec["state"] != "COMMITTED":
                continue
            results[rec["line_id"]] = LineResult(
                line_id=rec["line_id"], ord=rec["ord"],
                start=rec["win_start"], end=rec["win_end"],
                screens=rec["screens"], tradeoffs=rec["tradeoffs"])
        return results

    def _should_stop(self):
        should_stop = self.hooks.get("should_stop")
        return bool(should_stop and should_stop())


def run_project(manifest_path, persist_dir, workers=4, hooks=None,
                max_commits=None, budget_overrides=None):
    # 便捷入口：加载工程并运行管线，返回运行汇总。
    from .loader import load_project
    project = load_project(manifest_path)
    opts = {"max_workers": workers}
    if budget_overrides:
        opts.update(budget_overrides)
    store = CheckpointStore(persist_dir)
    try:
        engine = Engine(project, store, Budget(**opts), hooks=hooks)
        return engine.run(max_commits=max_commits)
    finally:
        store.close()
