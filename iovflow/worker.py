from __future__ import annotations

import multiprocessing as mp
import os
import queue
import signal
import time
from dataclasses import dataclass
from typing import Any

from .errors import EngineError
from .stages import execute_filter_task, execute_window_task
from .storage import open_state, record_recovery, record_worker
from .util import process_alive


def _suspend_process(proc) -> None:
    if os.name == "nt":
        import ctypes

        PROCESS_SUSPEND_RESUME = 0x0800
        handle = ctypes.windll.kernel32.OpenProcess(
            PROCESS_SUSPEND_RESUME, False, int(proc.pid)
        )
        if not handle:
            raise OSError(f"OpenProcess failed for pid {proc.pid}")
        try:
            status = ctypes.windll.ntdll.NtSuspendProcess(handle)
            if status != 0:
                raise OSError(f"NtSuspendProcess failed: {status}")
        finally:
            ctypes.windll.kernel32.CloseHandle(handle)
    else:
        os.kill(int(proc.pid), signal.SIGSTOP)


def worker_main(
    worker_id: str,
    task_queue,
    result_queue,
    state_db: str,
    job_id: str,
    parent_pid: int,
) -> None:
    conn = open_state(state_db)
    record_worker(conn, job_id, worker_id, os.getpid(), "idle", None)
    try:
        while True:
            try:
                task = task_queue.get(timeout=1.0)
            except queue.Empty:
                if not process_alive(parent_pid):
                    break
                continue
            except (EOFError, OSError):
                break
            if task is None:
                break
            if not process_alive(parent_pid):
                break
            tid = task.get("tid")
            record_worker(
                conn,
                job_id,
                worker_id,
                os.getpid(),
                "running",
                f"{task.get('stage_id')}:{task.get('batch_id')}",
            )
            try:
                if task.get("kind") == "filter":
                    result = execute_filter_task(task)
                elif task.get("kind") == "window":
                    result = execute_window_task(task)
                else:
                    raise EngineError("IOV_INTERNAL", f"unknown worker task kind {task.get('kind')!r}")
                result_queue.put(
                    {
                        "type": "result",
                        "worker_id": worker_id,
                        "tid": tid,
                        "ok": True,
                        "result": result,
                    }
                )
            except BaseException as exc:  # noqa: BLE001 - must cross the process boundary
                err = _serialize_error(exc)
                result_queue.put(
                    {
                        "type": "result",
                        "worker_id": worker_id,
                        "tid": tid,
                        "ok": False,
                        "error": err,
                    }
                )
            finally:
                try:
                    record_worker(conn, job_id, worker_id, os.getpid(), "idle", None)
                except Exception:
                    pass
    finally:
        try:
            record_worker(conn, job_id, worker_id, os.getpid(), "stopped", None)
        except Exception:
            pass
        conn.close()


def _serialize_error(exc: BaseException) -> dict[str, Any]:
    if isinstance(exc, EngineError):
        return {
            "code": exc.code,
            "message": exc.message,
            "fragment": exc.fragment,
            "stage": exc.stage,
            "context": exc.context,
        }
    return {
        "code": "IOV_INTERNAL",
        "message": f"{type(exc).__name__}: {exc}",
        "fragment": None,
        "stage": None,
        "context": {},
    }


def _deserialize_error(data: dict[str, Any]) -> EngineError:
    return EngineError(
        str(data.get("code") or "IOV_INTERNAL"),
        str(data.get("message") or "worker failed"),
        fragment=data.get("fragment"),
        stage=data.get("stage"),
        context=dict(data.get("context") or {}),
    )


@dataclass
class _Dispatch:
    tid: int
    task: dict[str, Any]
    started: float
    attempts: int


class WorkerPool:
    """A small persistent pool limited to two compute processes.

    Each worker has a private task queue, which lets the supervisor know
    exactly which shard was in flight when a worker was killed.  The supervisor
    requeues that shard to a surviving worker or a replacement instance.
    """

    def __init__(
        self,
        state_db: str,
        job_id: str,
        size: int,
        no_progress_timeout: float,
        max_takeovers: int,
        coordinator_conn,
        inject: str | None = None,
        inject_delay_ms: int = 40,
    ) -> None:
        self.state_db = state_db
        self.job_id = job_id
        self.size = max(1, int(size))
        self.timeout = float(no_progress_timeout)
        self.max_takeovers = int(max_takeovers)
        self.conn = coordinator_conn
        self.inject = self._parse_inject(inject)
        self.inject_delay = max(0.0, inject_delay_ms / 1000.0)
        # Use the portable spawn path everywhere.  This mirrors Windows and
        # avoids forking a multi-threaded coordinator (the memory monitor is a
        # thread), which can deadlock on POSIX.  The pool is persistent, so the
        # startup cost is paid once per job rather than once per shard.
        method = os.environ.get("IOVFLOW_START_METHOD") or "spawn"
        self.ctx = mp.get_context(method)
        self.result_queue = self.ctx.Queue()
        self.queues: dict[str, Any] = {}
        self.procs: dict[str, Any] = {}
        self._next_tid = 1
        self._started = False

    def start(self) -> None:
        if self._started:
            return
        for i in range(self.size):
            self._spawn(f"w{i}")
        self._started = True

    def _spawn(self, worker_id: str) -> None:
        q = self.ctx.Queue()
        proc = self.ctx.Process(
            target=worker_main,
            args=(worker_id, q, self.result_queue, self.state_db, self.job_id, os.getpid()),
            name=f"iovflow-{worker_id}",
            daemon=True,
        )
        proc.start()
        self.queues[worker_id] = q
        self.procs[worker_id] = proc
        try:
            record_worker(self.conn, self.job_id, worker_id, proc.pid, "starting", None)
        except Exception:
            pass

    def _respwan_if_dead(self, worker_id: str) -> None:
        proc = self.procs.get(worker_id)
        if proc is None or proc.is_alive():
            return
        old_pid = proc.pid
        try:
            proc.join(timeout=0.1)
        except Exception:
            pass
        self._spawn(worker_id)

    _current_job_id = ""
    _current_stage = ""
    _current_batch = -1

    def run_tasks(
        self,
        tasks: list[dict[str, Any]],
        job_id: str,
        stage_id: str,
        batch_id: int,
    ) -> dict[int, dict[str, Any]]:
        self._current_job_id = job_id
        self._current_stage = stage_id
        self._current_batch = batch_id
        if not tasks:
            return {}
        pending: list[_Dispatch] = []
        for task in tasks:
            tid = self._next_tid
            self._next_tid += 1
            item = dict(task)
            item["tid"] = tid
            pending.append(_Dispatch(tid=tid, task=item, started=0.0, attempts=0))
        completed: dict[int, dict[str, Any]] = {}
        in_flight: dict[str, _Dispatch] = {}
        inject_done: set[int] = set()
        while pending or in_flight:
            now = time.monotonic()
            # Fill idle workers.
            for worker_id in list(self.procs):
                if not pending:
                    break
                if worker_id in in_flight:
                    continue
                if not self.procs[worker_id].is_alive():
                    continue
                item = pending.pop(0)
                item.started = now
                in_flight[worker_id] = item
                item.task["worker_id"] = worker_id
                try:
                    self.queues[worker_id].put(item.task)
                except Exception:
                    pending.insert(0, item)
                    in_flight.pop(worker_id, None)
                    continue
                if (
                    self.inject
                    and self.inject_delay <= 0
                    and self._matches(item.task)
                    and item.tid not in inject_done
                ):
                    inject_done.add(item.tid)
                    proc = self.procs.get(worker_id)
                    if proc is not None:
                        try:
                            if self.inject.get("mode") == "hang":
                                _suspend_process(proc)
                            else:
                                proc.kill()
                        except Exception:
                            pass
            # Collect results without blocking so process deaths are observed.
            while True:
                try:
                    msg = self.result_queue.get(timeout=0.03)
                except queue.Empty:
                    break
                if msg.get("type") != "result":
                    continue
                worker_id = msg.get("worker_id")
                tid = int(msg.get("tid", -1))
                item = in_flight.pop(worker_id, None)
                if item is None:
                    # A stale result from an aborted task; ignore it.
                    continue
                if msg.get("ok"):
                    completed[tid] = msg.get("result") or {}
                else:
                    raise _deserialize_error(msg.get("error") or {})
            # Watchdog / external-kill handling.
            for worker_id, item in list(in_flight.items()):
                proc = self.procs.get(worker_id)
                dead = proc is None or not proc.is_alive()
                timed_out = (time.monotonic() - item.started) > self.timeout
                inject_kill = self._should_inject(item, inject_done, now)
                if dead or timed_out or inject_kill:
                    reason = "external_kill" if dead else ("no_progress_timeout" if timed_out else "injected_kill")
                    old_pid = proc.pid if proc is not None else None
                    if proc is not None and proc.is_alive():
                        try:
                            proc.kill()
                        except Exception:
                            pass
                        try:
                            proc.join(timeout=1.0)
                        except Exception:
                            pass
                    in_flight.pop(worker_id, None)
                    item.attempts += 1
                    if item.attempts > self.max_takeovers:
                        raise EngineError(
                            "IOV_STAGE_TAKEOVER_EXCEEDED",
                            f"stage {stage_id} shard {item.task.get('shard_id')} exceeded takeover limit",
                            stage=stage_id,
                            context={"attempts": item.attempts, "batch_id": batch_id},
                        )
                    try:
                        record_recovery(
                            self.conn,
                            job_id,
                            stage_id,
                            int(item.task.get("shard_id", 0)),
                            batch_id,
                            reason,
                            old_pid,
                            None,
                            item.attempts,
                        )
                    except Exception:
                        pass
                    item.started = 0.0
                    pending.insert(0, item)
                    if worker_id not in self.procs or not self.procs[worker_id].is_alive():
                        self._spawn(worker_id)
            # Respawn idle dead processes too.
            for worker_id in list(self.procs):
                if worker_id not in in_flight:
                    self._respwan_if_dead(worker_id)
            if not pending and not in_flight:
                break
        return completed

    def _should_inject(self, item: _Dispatch, done: set[int], now: float) -> bool:
        if not self.inject:
            return False
        if item.tid in done:
            return False
        if not self._matches(item.task):
            return False
        if now - item.started >= self.inject_delay:
            done.add(item.tid)
            if self.inject.get("mode") == "hang" and os.name != "nt":
                # Freeze the worker; the regular no-progress watchdog will
                # terminate it at the configured threshold.
                return False
            return True
        return False

    def _matches(self, task: dict[str, Any]) -> bool:
        if not self.inject:
            return False
        spec = self.inject
        if spec.get("stage") and spec["stage"] != task.get("stage_id"):
            return False
        if spec.get("batch") is not None and int(spec["batch"]) != int(task.get("batch_id", -1)):
            return False
        if spec.get("shard") is not None and int(spec["shard"]) != int(task.get("shard_id", -1)):
            return False
        if spec.get("kind") and spec["kind"] != task.get("kind"):
            return False
        return True

    @staticmethod
    def _parse_inject(spec: str | None) -> dict[str, Any] | None:
        if not spec:
            return None
        out: dict[str, Any] = {}
        for part in spec.split(","):
            if not part.strip():
                continue
            k, _, v = part.partition("=")
            k = k.strip()
            v = v.strip()
            if k in ("batch", "shard"):
                out[k] = int(v)
            else:
                out[k] = v
        return out

    def stop(self) -> None:
        for wid, q in self.queues.items():
            try:
                q.put(None)
            except Exception:
                pass
        deadline = time.monotonic() + 2.0
        for wid, proc in self.procs.items():
            try:
                proc.join(timeout=max(0.0, deadline - time.monotonic()))
            except Exception:
                pass
            if proc.is_alive():
                try:
                    proc.kill()
                except Exception:
                    pass

