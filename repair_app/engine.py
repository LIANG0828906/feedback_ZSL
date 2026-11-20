from __future__ import annotations

import threading
import time
from pathlib import Path
from typing import Any, Callable, Iterable

from .audio import sha256_bytes, sha256_file
from .errors import AppError
from .locking import SessionLock
from .models import ChainConfig, Interval, validate_intervals
from .processors import config_sha256, process_block
from .session import SessionSnapshot, SessionStore, block_payload

ProgressCallback = Callable[[dict[str, Any]], None]
TraceCallback = Callable[[dict[str, Any]], None]


def _emit(callback: TraceCallback | None, event: dict[str, Any]) -> None:
    if callback is not None:
        callback(event)


class SessionEngine:
    def __init__(self, session_path: str | Path) -> None:
        self.store = SessionStore(session_path)

    @property
    def path(self) -> Path:
        return self.store.path

    def create(
        self,
        *,
        input_path: str | Path,
        output_path: str | Path,
        block_size: int,
        intervals: Iterable[Interval] = (),
        chain: ChainConfig | None = None,
        force: bool = False,
    ) -> dict[str, Any]:
        with SessionLock(self.store.lock_path):
            snapshot = self.store.create(
                input_path=input_path,
                output_path=output_path,
                block_size=block_size,
                intervals=intervals,
                chain=chain,
                force=force,
            )
            return self.store.status(snapshot)

    def status(self) -> dict[str, Any]:
        return self.store.status()

    def save(self) -> dict[str, Any]:
        with SessionLock(self.store.lock_path):
            snapshot = self.store.load()
            self.store.save(snapshot.raw)
            report = self.store.status(snapshot)
            report["status"] = "saved"
            return report

    def recover(
        self,
        *,
        trace: TraceCallback | None = None,
    ) -> dict[str, Any]:
        with SessionLock(self.store.lock_path):
            snapshot = self.store.load()
            committed_before = len(snapshot.completed)
            missing = [
                index
                for index in range(snapshot.block_count)
                if str(index) not in snapshot.completed
            ]
            output_sha, complete = self.store.finalize(
                snapshot, mark_complete=True
            )
            _emit(
                trace,
                {
                    "event": "recover",
                    "status": "recovered",
                    "committed_blocks": committed_before,
                    "rebuild_blocks": missing,
                    "complete": complete,
                    "output_sha256": output_sha,
                },
            )
            report = self.store.status(snapshot)
            report.update(
                {
                    "status": "recovered",
                    "recovered_blocks": committed_before,
                    "remaining_blocks": len(missing),
                    "output_sha256": output_sha,
                }
            )
            return report

    def update_chain(
        self,
        updates: dict[str, Any],
        *,
        trace: TraceCallback | None = None,
    ) -> dict[str, Any]:
        with SessionLock(self.store.lock_path):
            snapshot = self.store.load()
            new_chain = snapshot.chain.merged(updates)
            snapshot.chain = new_chain
            snapshot.raw["processing_chain"] = new_chain.to_dict()
            dirty = self.store.refresh_dirty(snapshot)
            self.store.save(snapshot.raw)
            _emit(
                trace,
                {
                    "event": "update_chain",
                    "status": "invalidated" if dirty else "unchanged",
                    "dirty_blocks": dirty,
                },
            )
            report = self.store.status(snapshot)
            report.update({"status": "updated", "dirty_blocks": dirty})
            return report

    def update_intervals(
        self,
        intervals: Iterable[Interval | dict[str, Any]],
        *,
        trace: TraceCallback | None = None,
    ) -> dict[str, Any]:
        with SessionLock(self.store.lock_path):
            snapshot = self.store.load()
            parsed = [
                item
                if isinstance(item, Interval)
                else Interval.from_dict(item, total_frames=snapshot.info.frames)
                for item in intervals
            ]
            interval_list = validate_intervals(parsed, snapshot.info.frames)
            snapshot.intervals = interval_list
            snapshot.raw["intervals"] = [item.to_dict() for item in interval_list]
            dirty = self.store.refresh_dirty(snapshot)
            self.store.save(snapshot.raw)
            _emit(
                trace,
                {
                    "event": "update_intervals",
                    "status": "invalidated" if dirty else "unchanged",
                    "dirty_blocks": dirty,
                },
            )
            report = self.store.status(snapshot)
            report.update({"status": "updated", "dirty_blocks": dirty})
            return report

    def process(
        self,
        *,
        max_new_blocks: int | None = None,
        abort_event: threading.Event | None = None,
        progress: ProgressCallback | None = None,
        trace: TraceCallback | None = None,
        post_commit_delay: float = 0.0,
    ) -> dict[str, Any]:
        if max_new_blocks is not None and max_new_blocks < 0:
            raise AppError("E_INVALID_ARGUMENT", "max_new_blocks 不能为负数")
        if post_commit_delay < 0:
            raise AppError("E_INVALID_ARGUMENT", "post_commit_delay 不能为负数")
        with SessionLock(self.store.lock_path):
            snapshot = self.store.load()
            total = snapshot.block_count
            reused = 0
            processed = 0
            stopped = False

            baseline_progress = {
                "event": "progress",
                "current_block": -1,
                "total_blocks": total,
                "committed_blocks": len(snapshot.completed),
                "reused_blocks": reused,
                "processed_blocks": processed,
                "phase": "started",
            }
            if progress is not None:
                progress(baseline_progress)

            for block_index in range(total):
                if abort_event is not None and abort_event.is_set():
                    stopped = True
                    break
                key = str(block_index)
                if key in snapshot.completed:
                    reused += 1
                    summary = snapshot.completed[key]
                    _emit(
                        trace,
                        {
                            "event": "block",
                            "index": block_index,
                            "status": "reused",
                            "input_sha256": summary["input_sha256"],
                            "config_sha256": summary["config_sha256"],
                            "output_sha256": summary["output_sha256"],
                        },
                    )
                    continue
                if max_new_blocks is not None and processed >= max_new_blocks:
                    stopped = True
                    break

                start, frames, _ = snapshot.block_bounds(block_index)
                source = snapshot.input_samples[start : start + frames]
                source_sha = sha256_bytes(block_payload(source))
                cfg_sha = config_sha256(
                    chain=snapshot.chain,
                    intervals=snapshot.intervals,
                    start_frame=start,
                    frames=frames,
                    sample_rate=snapshot.info.sample_rate,
                    channels=snapshot.info.channels,
                )
                output = process_block(
                    samples=source,
                    start_frame=start,
                    sample_rate=snapshot.info.sample_rate,
                    chain=snapshot.chain,
                    intervals=snapshot.intervals,
                )
                output_payload = block_payload(output)
                output_sha = self.store.write_completed_block(
                    snapshot, block_index, output_payload
                )
                self.store.commit_summary(
                    snapshot,
                    block_index,
                    source_sha,
                    cfg_sha,
                    output_payload,
                    output_sha,
                )
                processed += 1
                _emit(
                    trace,
                    {
                        "event": "block",
                        "index": block_index,
                        "status": "processed",
                        "input_sha256": source_sha,
                        "config_sha256": cfg_sha,
                        "output_sha256": output_sha,
                    },
                )
                if progress is not None:
                    progress(
                        {
                            "event": "progress",
                            "current_block": block_index,
                            "total_blocks": total,
                            "committed_blocks": len(snapshot.completed),
                            "reused_blocks": reused,
                            "processed_blocks": processed,
                            "phase": "processing",
                        }
                    )
                if post_commit_delay:
                    time.sleep(post_commit_delay)

            output_sha, complete = self.store.finalize(
                snapshot, mark_complete=True
            )
            phase = "completed" if complete else ("stopped" if stopped else "paused")
            if abort_event is not None and abort_event.is_set():
                phase = "stopped"
            _emit(
                trace,
                {
                    "event": "run_end",
                    "status": phase,
                    "committed_blocks": len(snapshot.completed),
                    "reused_blocks": reused,
                    "processed_blocks": processed,
                    "total_blocks": total,
                    "complete": complete,
                    "output_sha256": output_sha,
                },
            )
            if progress is not None:
                progress(
                    {
                        "event": "finished",
                        "current_block": total - 1,
                        "total_blocks": total,
                        "committed_blocks": len(snapshot.completed),
                        "reused_blocks": reused,
                        "processed_blocks": processed,
                        "phase": phase,
                        "output_sha256": output_sha,
                    }
                )
            report = self.store.status(snapshot)
            report.update(
                {
                    "status": phase,
                    "reused_blocks": reused,
                    "processed_blocks": processed,
                    "output_sha256": output_sha,
                }
            )
            return report

    def output_hash(self) -> str:
        snapshot = self.store.load()
        return sha256_file(snapshot.output_path)

