"""Parallel build scheduler with stable commit order and budget metering."""

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field

from . import budget as budget_mod
from .errors import BudgetExceeded
from .fingerprint import content_hash, fingerprint_signal
from .wavio import read_wav


@dataclass
class SampleTask:
    index: int
    sample_id: str
    path: str
    stage: int = 0
    estimate: int = 0
    params: dict = field(default_factory=dict)
    rejected: bool = False
    reject_info: dict = field(default_factory=dict)


def _compute(task, fp_cfg):
    signal, rate = read_wav(task.path)
    records, stats = fingerprint_signal(
        signal, rate, fp_cfg,
        frame_batch=task.params["frame_batch"],
        hash_chunk=task.params["hash_chunk"],
    )
    usage = budget_mod.actual_usage(stats, fp_cfg, task.params)
    return records, stats, usage


class BuildScheduler:
    # Commits happen strictly in sample-index order; parallelism and budgets
    # come from config and are never silently raised at runtime; runnable
    # tasks are picked in submission order; tasks that would push cumulative
    # memory over the pool budget wait (logged); starting an already-active
    # batch raises E_BUSY.

    def __init__(self, config, library, tracer):
        self.cfg = config
        self.lib = library
        self.tracer = tracer
        sched = config["scheduler"]
        self.parallelism = sched["parallelism"]
        self.per_task_budget = sched["per_task_mem_budget"]
        self.pool_budget = sched["pool_mem_budget"]

    def run_batch(self, batch_id, batch_seq, sample_specs, kill_after=None):
        """sample_specs: ordered list of (sample_id, path). Returns summary."""
        self.lib.acquire_batch(batch_id)
        committed = set(self.lib.committed_ids())
        if committed:
            self.tracer.log("RECOVERY_RESUME", batch_id=batch_id,
                            already_committed=len(committed))
        try:
            tasks = self._prepare(batch_id, batch_seq, sample_specs, committed)
            summary = self._schedule(batch_id, batch_seq, tasks, kill_after)
        finally:
            self.lib.release_batch()
        return summary

    def _prepare(self, batch_id, batch_seq, sample_specs, committed):
        fp_cfg = self.cfg["fingerprint"]
        budget_cfg = self.cfg["budget"]
        tasks = []
        for index, (sample_id, path) in enumerate(sample_specs):
            task = SampleTask(index=index, sample_id=sample_id, path=path)
            if sample_id in committed:
                self.tracer.log("TASK_SKIP_COMMITTED", batch_id=batch_id,
                                batch_seq=batch_seq, sample_id=sample_id,
                                index=index)
                task.rejected = True
                task.reject_info = {"skipped": "already_committed"}
                tasks.append(task)
                continue
            signal, rate = read_wav(path)
            try:
                stage, est, params, attempts = budget_mod.resolve_stage(
                    len(signal), rate, fp_cfg, budget_cfg, self.per_task_budget
                )
            except BudgetExceeded as exc:
                task.rejected = True
                task.reject_info = exc.to_dict()
                self.tracer.log("TASK_REJECT", batch_id=batch_id,
                                batch_seq=batch_seq, sample_id=sample_id,
                                index=index, error="E_BUDGET_EXCEEDED",
                                budget=self.per_task_budget,
                                attempts=exc.ctx.get("attempts"))
                tasks.append(task)
                continue
            task.stage, task.estimate, task.params = stage, est, params
            if stage > 0:
                self.tracer.log("TASK_DEGRADE", batch_id=batch_id,
                                batch_seq=batch_seq, sample_id=sample_id,
                                index=index, degrade_stage=stage,
                                budget_estimated=est, attempts=attempts)
            tasks.append(task)
        return tasks

    def _schedule(self, batch_id, batch_seq, tasks, kill_after):
        fp_cfg = self.cfg["fingerprint"]
        config_version = self.cfg["config_version"]
        time_quant = fp_cfg["time_quant"]
        pending = [t for t in tasks if not t.rejected]
        n_committed = 0
        n_rejected = sum(1 for t in tasks if t.rejected
                         and not t.reject_info.get("skipped"))
        while pending:
            wave, mem_used = [], 0
            deferred = []
            for task in pending:  # submission (index) order
                if (len(wave) < self.parallelism
                        and mem_used + task.estimate <= self.pool_budget):
                    wave.append(task)
                    mem_used += task.estimate
                else:
                    deferred.append(task)
                    self.tracer.log("TASK_WAIT", batch_id=batch_id,
                                    batch_seq=batch_seq,
                                    sample_id=task.sample_id, index=task.index,
                                    budget_estimated=task.estimate,
                                    pool_used=mem_used,
                                    pool_budget=self.pool_budget)
            if not wave:  # deadlock guard: run the lowest-index task alone
                wave = [deferred.pop(0)]
                self.tracer.log("TASK_WAIT_OVERRIDE", batch_id=batch_id,
                                batch_seq=batch_seq,
                                sample_id=wave[0].sample_id,
                                index=wave[0].index,
                                budget_estimated=wave[0].estimate)
            with ThreadPoolExecutor(max_workers=self.parallelism) as pool:
                results = list(pool.map(lambda t: _compute(t, fp_cfg), wave))
            # commit strictly in sample-index order
            for task, (records, stats, usage) in sorted(
                    zip(wave, results), key=lambda tr: tr[0].index):
                if usage > self.per_task_budget:
                    n_rejected += 1
                    self.tracer.log("TASK_REJECT", batch_id=batch_id,
                                    batch_seq=batch_seq,
                                    sample_id=task.sample_id, index=task.index,
                                    error="E_BUDGET_EXCEEDED",
                                    budget=self.per_task_budget,
                                    budget_used=usage,
                                    degrade_stage=task.stage)
                    continue
                fp_hash = content_hash(records)
                self.lib.commit(task.sample_id, records, fp_hash, batch_seq,
                                config_version, time_quant)
                n_committed += 1
                self.tracer.log("TASK_COMMIT", batch_id=batch_id,
                                batch_seq=batch_seq,
                                sample_id=task.sample_id, index=task.index,
                                degrade_stage=task.stage,
                                budget_estimated=task.estimate,
                                budget_used=usage, fp_hash=fp_hash,
                                n_hashes=stats["n_hashes"])
                if kill_after is not None and n_committed >= kill_after:
                    self.tracer.close()
                    import os
                    os._exit(137)  # simulate SIGKILL: no cleanup at all
            pending = deferred
        return {"committed": n_committed, "rejected": n_rejected}
