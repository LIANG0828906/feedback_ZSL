"""受限预算下的分块并行渲染与串行提交。"""
from __future__ import annotations

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

import numpy as np

from .checkpoint import CheckpointStore
from .errors import E_MEM, E_TILE, TileError
from .renderer import SoftwareRasterizer
from .versioning import VersionStore


@dataclass
class TileJob:
    tile_id: int
    tx: int
    ty: int
    x0: int
    y0: int
    w: int
    h: int


@dataclass
class TileStatus:
    tile_id: int
    state: str = "pending"          # pending|queued|running|committed|failed
    version: int = 0
    attempts: int = 0
    wait_ms: float = 0.0
    run_ms: float = 0.0
    queue_reason: Optional[str] = None
    error: Optional[str] = None


@dataclass
class ScheduleResult:
    order: List[int] = field(default_factory=list)
    statuses: Dict[int, TileStatus] = field(default_factory=dict)
    queue_events: List[Dict[str, Any]] = field(default_factory=list)
    latency_ms: List[float] = field(default_factory=list)
    recovered: List[int] = field(default_factory=list)
    failed: List[int] = field(default_factory=list)

    def quantiles(self) -> Dict[str, float]:
        if not self.latency_ms:
            return {"p50": 0.0, "p90": 0.0, "p99": 0.0}
        a = np.asarray(self.latency_ms)
        return {
            "p50": float(np.percentile(a, 50)),
            "p90": float(np.percentile(a, 90)),
            "p99": float(np.percentile(a, 99)),
        }


class TileScheduler:
    """以固定分块网格并行渲染，提交严格按 tile_id 升序串行执行。"""

    def __init__(self, rasterizer: SoftwareRasterizer,
                 store: CheckpointStore, versions: VersionStore,
                 max_workers: int = 2, tile_size: int = 32,
                 max_inflight_pixels: int = 250_000) -> None:
        if max_workers < 1:
            raise ValueError("max_workers 必须 >= 1")
        self.rast = rasterizer
        self.store = store
        self.versions = versions
        self.max_workers = max_workers
        self.tile_size = tile_size
        # 信号量同时充当“在途像素缓冲预算”，预算耗尽则阻塞排队
        self._slots = threading.BoundedSemaphore(max_workers)
        self._pixel_lock = threading.Lock()
        self._inflight_pixels = 0
        self.max_inflight_pixels = max_inflight_pixels
        self._commit_lock = threading.Lock()

    def build_jobs(self) -> List[TileJob]:
        W, H = self.rast.camera.width, self.rast.camera.height
        jobs: List[TileJob] = []
        tid = 0
        for ty in range(0, H, self.tile_size):
            for tx in range(0, W, self.tile_size):
                w = min(self.tile_size, W - tx)
                h = min(self.tile_size, H - ty)
                jobs.append(TileJob(tid, tx // self.tile_size,
                                    ty // self.tile_size, tx, ty, w, h))
                tid += 1
        return jobs

    def _acquire_budget(self, pixels: int, result: ScheduleResult,
                        tile_id: int) -> float:
        """获取并发槽与像素预算；无法立即满足时阻塞并记录排队原因。"""
        t0 = time.perf_counter()
        reason = None
        while True:
            self._slots.acquire()
            with self._pixel_lock:
                if self._inflight_pixels + pixels <= self.max_inflight_pixels:
                    self._inflight_pixels += pixels
                    waited = (time.perf_counter() - t0) * 1000.0
                    if reason or waited > 0.5:
                        result.queue_events.append({
                            "tile_id": tile_id,
                            "reason": reason or "worker_slot_budget",
                            "wait_ms": waited,
                        })
                    return waited
                reason = "pixel_buffer_budget"
            self._slots.release()
            result.queue_events.append({
                "tile_id": tile_id, "reason": reason,
                "wait_ms": 0.0,
            })
            time.sleep(0.002)

    def _release_budget(self, pixels: int) -> None:
        with self._pixel_lock:
            self._inflight_pixels -= pixels
        self._slots.release()

    def run(self, meshes, mode: str, version: int, content_hash: str,
            *, parallel: bool = True,
            cancel_event: Optional[threading.Event] = None,
            max_retries: int = 1) -> ScheduleResult:
        snap = self.versions.lock_for_precise(version)
        jobs = self.build_jobs()
        result = ScheduleResult()
        for job in jobs:
            result.statuses[job.tile_id] = TileStatus(job.tile_id,
                                                      version=version)

        recovered = self.store.recover(version, content_hash)
        for tid, rec in recovered.items():
            st = result.statuses.get(tid)
            if st is not None:
                st.state = "committed"
                result.order.append(tid)
                result.recovered.append(tid)
                self.versions.freeze_tile(version, tid)

        pending = [j for j in jobs if j.tile_id not in recovered]
        next_commit = 0
        commit_cond = threading.Condition()
        ready: Dict[int, Tuple[np.ndarray, np.ndarray]] = {}

        def render_one(job: TileJob) -> Tuple[int, np.ndarray, np.ndarray,
                                               float, float]:
            st = result.statuses[job.tile_id]
            pixels = job.w * job.h
            waited = self._acquire_budget(pixels, result, job.tile_id)
            st.state = "running"
            st.wait_ms = waited
            run_t0 = time.perf_counter()
            try:
                color, depth = self.rast.render_tile(
                    meshes, job.x0, job.y0, job.w, job.h, mode, version)
            finally:
                self._release_budget(pixels)
            run_ms = (time.perf_counter() - run_t0) * 1000.0
            return job.tile_id, color, depth, waited, run_ms

        def commit_loop() -> None:
            nonlocal next_commit
            committed_ids = set(recovered.keys())
            total = len(jobs)
            next_commit = 0
            while next_commit in committed_ids and next_commit < total:
                next_commit += 1
            while len(committed_ids) < total:
                with commit_cond:
                    if next_commit >= total:
                        return
                    if next_commit not in ready:
                        commit_cond.wait(timeout=0.05)
                        if cancel_event is not None and cancel_event.is_set():
                            return
                        continue
                    color, depth = ready.pop(next_commit)
                tid = next_commit
                with self._commit_lock:
                    job = jobs[tid]
                    self.store.commit_tile(tid, job.tx, job.ty, version,
                                           content_hash, color, depth)
                    self.versions.freeze_tile(version, tid)
                    result.statuses[tid].state = "committed"
                    result.order.append(tid)
                with commit_cond:
                    committed_ids.add(tid)
                    next_commit += 1
                    commit_cond.notify_all()

        def worker(job: TileJob) -> None:
            st = result.statuses[job.tile_id]
            for attempt in range(max_retries + 1):
                if cancel_event is not None and cancel_event.is_set():
                    st.state = "pending"
                    return
                st.attempts = attempt + 1
                try:
                    tid, color, depth, waited, run_ms = render_one(job)
                    result.latency_ms.append(waited + run_ms)
                    st.run_ms = run_ms
                    with commit_cond:
                        ready[tid] = (color, depth)
                        commit_cond.notify_all()
                    return
                except Exception as exc:  # 单块失败不污染其他块
                    st.error = f"{type(exc).__name__}: {exc}"
                    if attempt == max_retries:
                        st.state = "failed"
                        result.failed.append(job.tile_id)
                        with commit_cond:
                            # 用空结果占位，保证提交序列不被卡死
                            color = np.zeros((job.h, job.w, 3), np.float32)
                            depth = np.full((job.h, job.w), np.inf,
                                            np.float32)
                            ready[job.tile_id] = (color, depth)
                            commit_cond.notify_all()
                        return

        if not parallel or self.max_workers == 1:
            committer = threading.Thread(target=commit_loop, daemon=True)
            committer.start()
            for job in pending:
                if cancel_event is not None and cancel_event.is_set():
                    break
                result.statuses[job.tile_id].state = "queued"
                worker(job)
            committer.join(timeout=30)
        else:
            committer = threading.Thread(target=commit_loop, daemon=True)
            committer.start()
            with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
                futures = []
                for job in pending:
                    if cancel_event is not None and cancel_event.is_set():
                        break
                    result.statuses[job.tile_id].state = "queued"
                    futures.append(pool.submit(worker, job))
                for f in futures:
                    f.result()
            committer.join(timeout=30)

        result.order.sort()
        return result

    def assemble_frame(self, version: int, content_hash: str,
                       mode: str) -> "RenderedFrameLike":
        """从已持久化分块重组整帧；崩溃后只能基于已提交分块。"""
        records = self.store.recover(version, content_hash)
        W, H = self.rast.camera.width, self.rast.camera.height
        color = np.zeros((H, W, 3), dtype=np.float32)
        depth = np.full((H, W), np.inf, dtype=np.float32)
        complete = len(records) == len(self.build_jobs())
        for tid, rec in sorted(records.items()):
            c, d = self.store.load_tile(rec.to_meta())
            x0, y0 = rec.tx * self.tile_size, rec.ty * self.tile_size
            color[y0:y0 + c.shape[0], x0:x0 + c.shape[1]] = c
            depth[y0:y0 + d.shape[0], x0:x0 + d.shape[1]] = d
        frame = self.rast._finalize(color, depth, mode, version,
                                    (W // self.tile_size, H // self.tile_size),
                                    self.tile_size)
        frame.content_hash = content_hash
        frame.complete = complete  # type: ignore[attr-defined]
        return frame
