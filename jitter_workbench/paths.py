"""The three execution paths that must produce identical results.

  * realtime  - threaded receiver + virtual-clock engine + checkpoints
  * resume    - realtime run stopped after K windows, then a brand new
                engine restores from the checkpoint and finishes
  * offline   - single-threaded full replay of the whole arrival stream
"""
from __future__ import annotations

import os

from .checkpoint import CheckpointManager
from .controller import ControllerParams
from .engine import JitterEngine, RunResult
from .loader import Sample
from .receiver import OfflineIngress, ThreadedIngress


def run_realtime(sample: Sample, params: ControllerParams,
                 ckpt_dir: str | None = None, *,
                 n_workers: int = 3, max_pending_frames: int = 64,
                 stop_after_windows: int | None = None,
                 engine: JitterEngine | None = None) -> RunResult:
    ckpt = CheckpointManager(ckpt_dir) if ckpt_dir else None
    if engine is None:
        engine = JitterEngine(sample, params, checkpoint=ckpt,
                              max_pending_frames=max_pending_frames)
        skip = 0
        if ckpt is not None:
            state = ckpt.load(content_hash=sample.content_hash,
                              policy_version=params.version)
            if state is not None:
                engine.restore(state)
                skip = state["ingress_pos"]
    else:
        skip = engine.ingress_pos
    ingress = ThreadedIngress(sample.arrivals, n_workers=n_workers,
                              max_pending_frames=max_pending_frames,
                              skip=skip)
    result = engine.run(ingress, stop_after_windows=stop_after_windows)
    result.drop_records.extend(ingress.drop_records)
    return result


def run_offline(sample: Sample, params: ControllerParams) -> RunResult:
    engine = JitterEngine(sample, params)
    ingress = OfflineIngress(sample.arrivals)
    return engine.run(ingress)


def run_resume(sample: Sample, params: ControllerParams,
               ckpt_dir: str, *, stop_after_windows: int,
               n_workers: int = 3) -> RunResult:
    """Run to ``stop_after_windows``, then resume from checkpoint with a
    completely new engine instance (process-restart semantics)."""
    mgr = CheckpointManager(ckpt_dir)
    mgr.discard()
    # phase 1: partial run, checkpoint written after every window
    run_realtime(sample, params, ckpt_dir, n_workers=n_workers,
                 stop_after_windows=stop_after_windows)
    # phase 2: new engine restores and finishes
    return run_realtime(sample, params, ckpt_dir, n_workers=n_workers)
