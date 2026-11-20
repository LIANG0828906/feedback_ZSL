from __future__ import annotations

import json
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from .cache import TierCache, TierCacheState
from .config import load_project
from .errors import BusinessError, ErrorCode, fail
from .generator import atomic_write, parse_slice_file
from .models import (
    GENESIS_HASH,
    ProjectConfig,
    SliceArtifact,
    SourceTimeline,
    canonical_json,
)
from .scheduler import RunControl, SchedulerResult, SliceScheduler
from .snapshot import (
    RestoreResult,
    build_snapshot,
    load_snapshot,
    save_snapshot,
)
from .timebase import build_timeline


@dataclass
class RunResult:
    status: str
    project: ProjectConfig
    timeline: SourceTimeline
    scheduler: SchedulerResult
    snapshot_path: Path
    trace_path: Path
    snapshot_document: dict[str, Any]
    reused_tiers: list[str] = field(default_factory=list)
    recomputed_tiers: list[str] = field(default_factory=list)
    restored: bool = False

    @property
    def artifacts(self) -> dict[str, list[SliceArtifact]]:
        return self.scheduler.artifacts()

    @property
    def errors(self) -> list[dict[str, Any]]:
        return list(self.scheduler.errors)

    def to_dict(self) -> dict[str, Any]:
        return {
            "errors": self.errors,
            "memory_peak_bytes": self.scheduler.peak_bytes,
            "recomputed_tiers": self.recomputed_tiers,
            "restored": self.restored,
            "reused_tiers": self.reused_tiers,
            "status": self.status,
            "tiers": {
                tier_id: {
                    "completed": len(work.state.artifacts),
                    "error": work.error,
                    "status": work.state.status,
                }
                for tier_id, work in self.scheduler.tiers.items()
            },
            "trace": str(self.trace_path),
        }


class Pipeline:
    def __init__(
        self,
        *,
        config_path: Path,
        budget_path: Path,
        output_root: Path,
        control: RunControl | None = None,
        on_event: Callable[[dict[str, Any]], None] | None = None,
        on_progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> None:
        self.project = load_project(config_path, budget_path)
        self.timeline = build_timeline(
            audio_path=self.project.audio_path,
            video_path=self.project.video_path,
            slice_duration_ms=self.project.slice_duration_ms,
            tolerance_ms=self.project.tolerance_ms,
        )
        self.output_root = output_root.resolve()
        self.output_root.mkdir(parents=True, exist_ok=True)
        self.snapshot_path = self.output_root / "snapshot.json"
        self.trace_path = self.output_root / "scheduler_trace.jsonl"
        self.control = control or RunControl()
        self.on_event = on_event
        self.on_progress = on_progress
        self._run_lock = threading.Lock()
        self._running = False
        self._last_result: RunResult | None = None

    def _load_local_states(self) -> dict[str, TierCacheState]:
        states: dict[str, TierCacheState] = {}
        for tier in self.project.tiers:
            cache = TierCache(self.output_root, tier, self.timeline)
            states[tier.tier_id] = cache.load()
        return states

    def _restore_states(
        self, snapshot: Path
    ) -> tuple[dict[str, TierCacheState], RestoreResult]:
        restore = load_snapshot(
            snapshot,
            project=self.project,
            timeline=self.timeline,
            output_root=self.output_root,
        )
        states = dict(restore.states)
        for tier_id, state in states.items():
            if tier_id not in restore.dirty_tiers and state.artifacts:
                state.reused = True
        for tier_id in restore.dirty_tiers:
            cache = TierCache(
                self.output_root, self.project.tier_map()[tier_id], self.timeline
            )
            cache.clear()
            states[tier_id] = TierCacheState()
        return states, restore

    def _write_trace(self, result: SchedulerResult) -> None:
        lines = [
            canonical_json(event)
            for event in result.events
        ]
        for artifact in (
            artifact
            for artifacts in result.artifacts().values()
            for artifact in artifacts
        ):
            lines.append(
                canonical_json(
                    {
                        "event": "slice_record",
                        "tier": artifact.tier_id,
                        "slice_index": artifact.index,
                        "start_ns": artifact.start_ns,
                        "end_ns": artifact.end_ns,
                        "estimated_bytes": artifact.memory.estimated_bytes,
                        "content_hash": artifact.content_hash,
                        "prev_hash": artifact.prev_hash,
                    }
                )
            )
        atomic_write(
            self.trace_path,
            b"\n".join(lines) + (b"\n" if lines else b""),
        )

    def run(
        self,
        *,
        restore_snapshot: Path | None = None,
        fault_at: tuple[str, int] | None = None,
    ) -> RunResult:
        with self._run_lock:
            if self._running:
                raise fail(
                    ErrorCode.BUSY,
                    "pipeline is already running",
                )
            self._running = True
        try:
            restored = restore_snapshot is not None
            restore_info: RestoreResult | None = None
            if restore_snapshot is not None:
                states, restore_info = self._restore_states(restore_snapshot)
            else:
                states = self._load_local_states()
            reused_tiers = sorted(
                tier_id
                for tier_id, state in states.items()
                if state.reused and state.artifacts
            )
            scheduler = SliceScheduler(
                timeline=self.timeline,
                tiers=list(self.project.tiers),
                budget=self.project.budget,
                output_root=self.output_root,
                states=states,
                control=self.control,
                on_event=self.on_event,
                on_progress=self.on_progress,
                fault_at=fault_at,
            )
            scheduler.set_config_version(self.project.config_version)
            result = scheduler.run()
            restored_peak = (
                int(restore_info.memory.get("peak_bytes", 0))
                if restore_info is not None
                else 0
            )
            restored_samples = (
                list(restore_info.memory.get("samples", []))
                if restore_info is not None
                else []
            )
            combined_samples = restored_samples + result.memory_samples
            combined_peak = max(restored_peak, result.peak_bytes)
            states_after = {
                tier_id: work.state for tier_id, work in result.tiers.items()
            }
            next_submit_seq = len(self.timeline.boundaries) * len(
                self.project.tiers
            )
            snapshot_document = build_snapshot(
                project=self.project,
                timeline=self.timeline,
                states=states_after,
                memory_samples=combined_samples,
                peak_bytes=combined_peak,
                waiting_queue=result.waiting_queue,
                output_root=self.output_root,
                next_submit_seq=next_submit_seq,
            )
            save_snapshot(self.snapshot_path, snapshot_document)
            self._write_trace(result)
            recomputed = sorted(
                tier_id
                for tier_id, work in result.tiers.items()
                if tier_id not in reused_tiers
                or work.next_index > len(states[tier_id].artifacts)
            )
            status = (
                "cancelled"
                if result.cancelled
                else "failed"
                if result.errors
                else "complete"
            )
            run_result = RunResult(
                status=status,
                project=self.project,
                timeline=self.timeline,
                scheduler=result,
                snapshot_path=self.snapshot_path,
                trace_path=self.trace_path,
                snapshot_document=snapshot_document,
                reused_tiers=reused_tiers,
                recomputed_tiers=recomputed,
                restored=restored,
            )
            self._last_result = run_result
            return run_result
        finally:
            with self._run_lock:
                self._running = False

    def pause(self) -> None:
        self.control.pause()

    def resume(self) -> None:
        self.control.resume()

    def cancel(self) -> None:
        self.control.cancel()


def load_artifacts_from_output(
    output_root: Path, project: ProjectConfig
) -> dict[str, list[SliceArtifact]]:
    result: dict[str, list[SliceArtifact]] = {}
    for tier in project.tiers:
        directory = output_root / tier.tier_id
        paths = sorted(directory.glob("slice-*.fslice"))
        artifacts = [parse_slice_file(path, verify=True) for path in paths]
        previous = GENESIS_HASH
        for expected, artifact in enumerate(artifacts):
            if (
                artifact.index != expected
                or artifact.tier_id != tier.tier_id
                or artifact.prev_hash != previous
            ):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "output artifact chain is inconsistent",
                    tier=tier.tier_id,
                    index=expected,
                )
            previous = artifact.content_hash
        result[tier.tier_id] = artifacts
    return result


def write_json(path: Path, document: dict[str, Any]) -> None:
    atomic_write(path, canonical_json(document) + b"\n")


def read_json(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))
