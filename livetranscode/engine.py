from __future__ import annotations

import bisect
import os
import tempfile
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .audio import AudioSlice, deterministic_resample_slice, output_sample_bounds
from .budget import BudgetDecision, choose_budget_stage
from .config import parse_config
from .errors import ErrorCode, build_error
from .index import (
    LoadedTrackIndex,
    compute_chain_hash,
    compute_content_hash,
    load_track_index,
    new_index_value,
    write_track_index,
)
from .models import AppConfig, SourceAudio, Timeline, TrackConfig, VideoDescription
from .scheduler import PreparedTask, SliceScheduler, SliceTask
from .snapshot import build_snapshot, load_snapshot, save_snapshot
from .source import build_timeline, read_source_audio, read_video_description
from .util import (
    canonical_json_text,
    ensure_dir,
    read_json,
    remove_tree_if_exists,
    sha256_file,
    sha256_obj,
    write_bytes_atomic,
    write_json_atomic,
    write_jsonl_atomic,
)
from .video import VideoSlice, deterministic_video_slice

IMPLEMENTATION_ID = "live-transcode-sim/1"


@dataclass(frozen=True)
class PreparedSlice:
    task: SliceTask
    budget: BudgetDecision
    audio: AudioSlice
    video: VideoSlice
    frame_count: int

    @property
    def output_bytes(self) -> int:
        return len(self.audio.data) + len(self.video.data)


class TranscodeEngine:
    def __init__(
        self,
        workspace: Path,
        config: AppConfig,
        timeline: Timeline,
        audio: SourceAudio,
        video: VideoDescription,
        config_version: str,
    ) -> None:
        self.workspace = workspace.resolve()
        self.config = config
        self.timeline = timeline
        self.audio = audio
        self.video = video
        self.config_version = config_version
        self.tracks_by_id = config.tracks_by_id
        self.index_paths = {
            track.track_id: self.workspace / "tracks" / track.track_id / "index.json"
            for track in config.tracks
        }
        self.slice_roots = {
            track.track_id: self.workspace / "tracks" / track.track_id / "slices"
            for track in config.tracks
        }
        self._indexes: dict[str, dict[str, Any]] = {}
        self._snapshot = None
        self._scheduler: SliceScheduler | None = None
        self._run_lock = threading.Lock()
        self._state_lock = threading.Lock()
        self._running = False
        self._loaded = self._load_state()

    @property
    def boundaries_sha256(self) -> str:
        return self.timeline.boundaries_sha256

    @property
    def snapshot_path(self) -> Path:
        return self.workspace / "snapshot.json"

    @property
    def trace_path(self) -> Path:
        return self.workspace / "logs" / "scheduler_trace.jsonl"

    @property
    def run_log_path(self) -> Path:
        return self.workspace / "logs" / "run.log"

    @property
    def exit_report_path(self) -> Path:
        return self.workspace / "exit_report.json"

    def _track_params_versions(self) -> dict[str, str]:
        return {track.track_id: track.params_version for track in self.config.tracks}

    @staticmethod
    def compute_config_version(
        config: AppConfig, audio: SourceAudio, video: VideoDescription, timeline: Timeline
    ) -> str:
        return sha256_obj(
            {
                "implementation": IMPLEMENTATION_ID,
                "config": config.raw,
                "audio_sha256": audio.sha256,
                "video_sha256": video.sha256,
                "boundaries_sha256": timeline.boundaries_sha256,
            }
        )

    @classmethod
    def prepare_workspace(
        cls,
        workspace: str | Path,
        audio_path: str | Path,
        frames_path: str | Path,
        config_path: str | Path,
    ) -> TranscodeEngine:
        target = Path(workspace).resolve()
        incoming_config = parse_config(read_json(config_path))
        incoming_audio = read_source_audio(audio_path)
        incoming_video = read_video_description(frames_path)
        incoming_timeline = build_timeline(incoming_audio, incoming_video, incoming_config)
        incoming_version = cls.compute_config_version(
            incoming_config, incoming_audio, incoming_video, incoming_timeline
        )
        manifest_path = target / "manifest.json"
        if manifest_path.exists():
            return cls.load_workspace(target, expected_version=incoming_version)

        ensure_dir(target)
        ensure_dir(target / "source")
        ensure_dir(target / "logs")
        ensure_dir(target / "tracks")
        copied_audio = target / "source" / "audio.wav"
        copied_frames = target / "source" / "frames.json"
        write_bytes_atomic(copied_audio, Path(audio_path).read_bytes())
        write_bytes_atomic(copied_frames, Path(frames_path).read_bytes())
        workspace_audio = read_source_audio(copied_audio)
        workspace_video = read_video_description(copied_frames)
        timeline = build_timeline(workspace_audio, workspace_video, incoming_config)
        config_version = cls.compute_config_version(
            incoming_config, workspace_audio, workspace_video, timeline
        )
        if config_version != incoming_version:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "source files changed while preparing the workspace",
                field="source_hash",
            )
        write_json_atomic(target / "config.json", incoming_config.raw)
        write_json_atomic(target / "boundaries.json", timeline.boundary_table())
        manifest = {
            "schema_version": 1,
            "implementation": IMPLEMENTATION_ID,
            "config_version": config_version,
            "boundaries_sha256": timeline.boundaries_sha256,
            "source": {
                "audio": "source/audio.wav",
                "frames": "source/frames.json",
                "audio_sha256": workspace_audio.sha256,
                "frames_sha256": workspace_video.sha256,
            },
        }
        write_json_atomic(manifest_path, manifest)
        for track in incoming_config.tracks:
            index_path = target / "tracks" / track.track_id / "index.json"
            slices_root = target / "tracks" / track.track_id / "slices"
            ensure_dir(slices_root)
            ensure_dir(slices_root / ".staging")
            write_track_index(
                index_path,
                new_index_value(track, timeline.boundaries_sha256, config_version),
            )
        return cls(target, incoming_config, timeline, workspace_audio, workspace_video, config_version)

    @classmethod
    def load_workspace(
        cls,
        workspace: str | Path,
        expected_version: str | None = None,
    ) -> TranscodeEngine:
        target = Path(workspace).resolve()
        manifest = read_json(target / "manifest.json")
        if not isinstance(manifest, dict) or manifest.get("schema_version") != 1:
            raise build_error(ErrorCode.SNAPSHOT_MISMATCH, "workspace manifest is invalid")
        if manifest.get("implementation") != IMPLEMENTATION_ID:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace implementation version changed",
                expected=IMPLEMENTATION_ID,
                found=manifest.get("implementation"),
                field="implementation",
            )
        config = parse_config(read_json(target / "config.json"))
        audio = read_source_audio(target / "source" / "audio.wav")
        video = read_video_description(target / "source" / "frames.json")
        timeline = build_timeline(audio, video, config)
        config_version = cls.compute_config_version(config, audio, video, timeline)
        if expected_version is not None and config_version != expected_version:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace configuration or source differs from the requested run",
                expected=expected_version,
                found=config_version,
                field="config_version",
            )
        if manifest.get("config_version") != config_version:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace manifest configuration version changed",
                expected=manifest.get("config_version"),
                found=config_version,
                field="config_version",
            )
        if manifest.get("boundaries_sha256") != timeline.boundaries_sha256:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace manifest boundary hash changed",
                expected=manifest.get("boundaries_sha256"),
                found=timeline.boundaries_sha256,
                field="boundaries_sha256",
            )
        boundaries_path = target / "boundaries.json"
        if not boundaries_path.exists():
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace boundary table is missing",
                path=str(boundaries_path),
                field="boundaries",
            )
        if sha256_obj(read_json(boundaries_path)) != timeline.boundaries_sha256:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace boundary table changed on disk",
                expected=timeline.boundaries_sha256,
                path=str(boundaries_path),
                field="boundaries",
            )
        source_manifest = manifest.get("source", {})
        if source_manifest.get("audio_sha256") != audio.sha256 or source_manifest.get("frames_sha256") != video.sha256:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "workspace source hash differs from its manifest",
                field="source_hash",
            )
        return cls(target, config, timeline, audio, video, config_version)

    def _cleanup_uncommitted(self, track: TrackConfig, committed: int) -> None:
        slices_root = self.slice_roots[track.track_id]
        ensure_dir(slices_root)
        staging = slices_root / ".staging"
        ensure_dir(staging)
        for child in list(staging.iterdir()):
            remove_tree_if_exists(child)
        for child in list(slices_root.iterdir()):
            if not child.is_dir() or child.name == ".staging":
                continue
            if not child.name.isdigit():
                continue
            if int(child.name) >= committed:
                remove_tree_if_exists(child)

    def _load_state(self) -> bool:
        for temp_path in self.workspace.rglob(".*.tmp"):
            if temp_path.is_file():
                temp_path.unlink(missing_ok=True)
        loaded: dict[str, LoadedTrackIndex] = {}
        for track in self.config.tracks:
            loaded[track.track_id] = load_track_index(
                self.index_paths[track.track_id],
                track,
                self.timeline.boundaries_sha256,
                self.config_version,
                self.slice_roots[track.track_id],
                verify_files=True,
            )
        self._indexes = {}
        for track in self.config.tracks:
            value = new_index_value(track, self.timeline.boundaries_sha256, self.config_version)
            loaded_track = loaded[track.track_id]
            value["entries"] = [dict(entry) for entry in loaded_track.entries]
            value["chain_head"] = loaded_track.chain_head
            value["status"] = loaded_track.status
            value["failure"] = loaded_track.failure
            self._indexes[track.track_id] = value
            self._cleanup_uncommitted(track, len(loaded_track.entries))
        snapshot = load_snapshot(
            self.snapshot_path,
            self.config_version,
            self.timeline.boundaries_sha256,
            self._track_params_versions(),
        )
        if snapshot is not None:
            self._validate_snapshot_against_disk(snapshot)
        self._snapshot = snapshot
        return True

    def _validate_snapshot_against_disk(self, snapshot: dict[str, Any]) -> None:
        for track in self.config.tracks:
            snapshot_track = snapshot["tracks"][track.track_id]
            snapshot_entries = snapshot_track.get("entries", [])
            disk_entries = self._indexes[track.track_id]["entries"]
            if len(snapshot_entries) > len(disk_entries):
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "snapshot claims slices that are not committed in the disk index",
                    track_id=track.track_id,
                    snapshot_slices=len(snapshot_entries),
                    disk_slices=len(disk_entries),
                    field="entries",
                )
            for index, snapshot_entry in enumerate(snapshot_entries):
                if snapshot_entry.get("chain_hash") != disk_entries[index].get("chain_hash"):
                    raise build_error(
                        ErrorCode.SNAPSHOT_MISMATCH,
                        "snapshot and disk index disagree",
                        track_id=track.track_id,
                        slice_index=index,
                        field="chain_hash",
                    )

    def _save_snapshot(self) -> None:
        scheduler = self._scheduler
        memory = {
            "global_budget_bytes": self.config.memory_budget_bytes,
            "reserved_bytes": scheduler.memory_reserved() if scheduler else 0,
            "high_water_bytes": scheduler.high_water if scheduler else 0,
        }
        snapshot = build_snapshot(
            self.config_version,
            self.timeline.boundaries_sha256,
            self.timeline.boundary_table(),
            self._track_params_versions(),
            self._indexes,
            memory,
            scheduler.queue_snapshot() if scheduler else [],
            sum(len(item["entries"]) for item in self._indexes.values()),
        )
        save_snapshot(self.snapshot_path, snapshot)
        self._snapshot = snapshot

    def _write_index(self, track_id: str) -> None:
        value = self._indexes[track_id]
        track = self.tracks_by_id[track_id]
        value["track_params_version"] = track.params_version
        value["track_config"] = track.raw
        value["boundaries_sha256"] = self.timeline.boundaries_sha256
        value["config_version"] = self.config_version
        write_track_index(self.index_paths[track_id], value)

    def _frame_count(self, boundary_index: int) -> tuple[int, int]:
        boundary = self.timeline.boundaries[boundary_index]
        factor = self.timeline.ticks_per_second // self.video.timescale
        starts = [frame.pts_ticks * factor for frame in self.video.frames]
        first = max(0, bisect.bisect_right(starts, boundary.start_ticks) - 1)
        count = 0
        for frame in self.video.frames[first:]:
            start = frame.pts_ticks * factor
            end = frame.end_ticks * factor
            if end <= boundary.start_ticks:
                continue
            if start >= boundary.end_ticks:
                break
            count += 1
        return count, first

    def _choose_budget(self, task: SliceTask) -> BudgetDecision:
        frame_count, _ = self._frame_count(task.slice_index)
        return choose_budget_stage(
            self.tracks_by_id[task.track_id],
            task.boundary,
            frame_count,
            self.audio.channels,
        )

    def _prepare_task(
        self,
        task: SliceTask,
        budget: BudgetDecision,
        cancel_event: threading.Event,
    ) -> PreparedSlice:
        if cancel_event.is_set():
            raise build_error(
                ErrorCode.CANCELLED,
                "slice preparation cancelled at a boundary checkpoint",
                track_id=task.track_id,
                slice_index=task.slice_index,
            )
        injected = self.config.failure_injection.get(f"{task.track_id}:{task.slice_index}")
        if injected is not None and injected not in {"commit", "commit_error"}:
            if injected == "E_BUDGET_EXCEEDED":
                raise build_error(
                    ErrorCode.BUDGET_EXCEEDED,
                    "injected budget rejection",
                    track_id=task.track_id,
                    slice_index=task.slice_index,
                )
            raise build_error(
                injected if injected.startswith("E_") else ErrorCode.INTERNAL,
                "injected slice failure",
                track_id=task.track_id,
                slice_index=task.slice_index,
            )
        track = self.tracks_by_id[task.track_id]
        audio_slice = deterministic_resample_slice(
            self.audio,
            task.boundary,
            track,
            self.timeline.ticks_per_second,
            budget.stage.block_size,
        )
        video_slice = deterministic_video_slice(
            self.video,
            task.boundary,
            track,
            self.timeline.ticks_per_second,
        )
        if cancel_event.is_set():
            raise build_error(
                ErrorCode.CANCELLED,
                "slice preparation cancelled before publication",
                track_id=task.track_id,
                slice_index=task.slice_index,
            )
        return PreparedSlice(
            task=task,
            budget=budget,
            audio=audio_slice,
            video=video_slice,
            frame_count=len(video_slice.records),
        )

    def _commit_ready(self, prepared: PreparedTask) -> None:
        artifact: PreparedSlice = prepared.artifacts
        task = artifact.task
        index_value = self._indexes[task.track_id]
        entries = index_value["entries"]
        injected = self.config.failure_injection.get(f"{task.track_id}:{task.slice_index}")
        if injected in {"commit", "commit_error"}:
            raise build_error(
                ErrorCode.INTERNAL,
                "injected commit failure",
                track_id=task.track_id,
                slice_index=task.slice_index,
            )
        if task.slice_index != len(entries):
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "publication order would skip a slice",
                track_id=task.track_id,
                expected_slice=len(entries),
                found_slice=task.slice_index,
            )
        slice_relpath = f"{task.slice_index:06d}"
        slices_root = self.slice_roots[task.track_id]
        final_dir = slices_root / slice_relpath
        if final_dir.exists():
            remove_tree_if_exists(final_dir)
        staging_root = slices_root / ".staging"
        ensure_dir(staging_root)
        stage_dir = Path(tempfile.mkdtemp(prefix=f".{slice_relpath}.", dir=staging_root))
        old_chain = index_value["chain_head"]
        old_status = index_value["status"]
        old_failure = index_value["failure"]
        index_written = False
        try:
            write_bytes_atomic(stage_dir / "audio.wav", artifact.audio.data)
            write_bytes_atomic(stage_dir / "video.json", artifact.video.data)
            os.replace(stage_dir, final_dir)
            boundary = task.boundary
            start_output, end_output = output_sample_bounds(
                boundary, self.tracks_by_id[task.track_id], self.timeline.ticks_per_second
            )
            entry = {
                "track_id": task.track_id,
                "slice_index": task.slice_index,
                "start_ticks": boundary.start_ticks,
                "end_ticks": boundary.end_ticks,
                "start_source_sample": boundary.start_source_sample,
                "end_source_sample": boundary.end_source_sample,
                "start_output_sample": start_output,
                "end_output_sample": end_output,
                "output_sample_count": end_output - start_output,
                "audio_start_ticks": boundary.start_ticks,
                "audio_end_ticks": boundary.end_ticks,
                "video_start_ticks": boundary.start_ticks,
                "video_end_ticks": boundary.end_ticks,
                "first_source_frame_pts": artifact.video.records[0]["source_pts_ticks"],
                "last_source_frame_end_pts": artifact.video.records[-1]["source_end_ticks"],
                "frame_count": artifact.frame_count,
                "audio_sha256": artifact.audio.sha256,
                "video_sha256": artifact.video.sha256,
                "track_params_version": self.tracks_by_id[task.track_id].params_version,
                "boundaries_sha256": self.timeline.boundaries_sha256,
                "budget_stage": artifact.budget.stage.name,
                "estimated_bytes": artifact.budget.estimated_bytes,
                "slice_relpath": slice_relpath,
            }
            entry["content_hash"] = compute_content_hash(entry)
            entry["chain_hash"] = compute_chain_hash(
                index_value["chain_head"],
                entry["content_hash"],
                entry["track_params_version"],
                self.timeline.boundaries_sha256,
            )
            entries.append(entry)
            index_value["chain_head"] = entry["chain_hash"]
            index_value["status"] = "running"
            index_value["failure"] = None
            self._write_index(task.track_id)
            index_written = True
            self._save_snapshot()
        except Exception:
            if not index_written:
                if len(entries) > task.slice_index:
                    del entries[task.slice_index:]
                index_value["chain_head"] = old_chain
                index_value["status"] = old_status
                index_value["failure"] = old_failure
                remove_tree_if_exists(final_dir)
            remove_tree_if_exists(stage_dir)
            raise
        self._commits_this_run += 1
        crash_after = getattr(self, "_crash_after_commits", None)
        if crash_after is not None and self._commits_this_run >= crash_after:
            os._exit(137)

    def _build_tasks(self, selected: list[str], next_indices: dict[str, int]) -> list[SliceTask]:
        tasks: list[SliceTask] = []
        min_slice = min(next_indices.values(), default=0)
        submission = 0
        for slice_index in range(min_slice, len(self.timeline.boundaries)):
            for track_index, track in enumerate(self.config.tracks):
                if track.track_id not in selected:
                    continue
                if slice_index < next_indices[track.track_id]:
                    continue
                tasks.append(
                    SliceTask(
                        submission_seq=submission,
                        track_id=track.track_id,
                        track_index=track_index,
                        slice_index=slice_index,
                        boundary=self.timeline.boundaries[slice_index],
                    )
                )
                submission += 1
        return tasks

    def _write_trace(self, events: list[dict[str, Any]]) -> None:
        write_jsonl_atomic(self.trace_path, events)
        lines = [canonical_json_text(event) for event in events]
        write_bytes_atomic(self.run_log_path, ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8"))

    def _write_exit_report(self, report: dict[str, Any]) -> None:
        report = dict(report)
        report["trace_sha256"] = sha256_file(self.trace_path) if self.trace_path.exists() else None
        report["snapshot_sha256"] = sha256_file(self.snapshot_path) if self.snapshot_path.exists() else None
        write_json_atomic(self.exit_report_path, report)

    def run(
        self,
        track_ids: list[str] | None = None,
        crash_after_commits: int | None = None,
    ) -> dict[str, Any]:
        if not self._run_lock.acquire(blocking=False):
            with self._state_lock:
                active = sorted(getattr(self, "_active_tracks", set()))
            raise build_error(
                ErrorCode.BUSY,
                "a transcode run is already active",
                track_ids=track_ids,
                active_tracks=active,
            )
        try:
            with self._state_lock:
                if getattr(self, "_running", False):
                    raise build_error(ErrorCode.BUSY, "engine is already running", track_ids=track_ids)
                self._running = True
            self.exit_report_path.unlink(missing_ok=True)
            selected = list(track_ids) if track_ids is not None else [track.track_id for track in self.config.tracks]
            unknown = [track_id for track_id in selected if track_id not in self.tracks_by_id]
            if unknown:
                raise build_error(ErrorCode.CONFIG_INVALID, "unknown track ids", track_ids=unknown)
            if len(set(selected)) != len(selected):
                raise build_error(ErrorCode.BUSY, "duplicate track requested in the same run", track_ids=selected)
            with self._state_lock:
                self._active_tracks = set(selected)
            self._crash_after_commits = crash_after_commits
            self._commits_this_run = 0
            self._load_state()
            next_indices = {
                track_id: len(self._indexes[track_id]["entries"])
                for track_id in selected
            }
            tasks = self._build_tasks(selected, next_indices)
            scheduler = SliceScheduler(
                self.config,
                tasks,
                prepare=self._prepare_task,
                commit=self._commit_ready,
                choose_budget=self._choose_budget,
            )
            self._scheduler = scheduler
            self._save_snapshot()
            result = scheduler.run()
            statuses: dict[str, str] = {}
            failures: dict[str, Any] = {}
            for track_id in selected:
                index_value = self._indexes[track_id]
                if track_id in result.failed_tracks:
                    index_value["status"] = "failed"
                    index_value["failure"] = result.failed_tracks[track_id]
                    statuses[track_id] = "failed"
                    failures[track_id] = result.failed_tracks[track_id]
                elif len(index_value["entries"]) == len(self.timeline.boundaries):
                    index_value["status"] = "complete"
                    index_value["failure"] = None
                    statuses[track_id] = "complete"
                elif scheduler.cancel_event.is_set():
                    index_value["status"] = "cancelled"
                    statuses[track_id] = "cancelled"
                elif scheduler.pause_event.is_set():
                    index_value["status"] = "paused"
                    statuses[track_id] = "paused"
                else:
                    index_value["status"] = "running"
                    statuses[track_id] = "running"
                self._write_index(track_id)
            self._save_snapshot()
            self._write_trace(result.events)
            report = {
                "ok": not result.failed_tracks,
                "status": "paused"
                if scheduler.pause_event.is_set() and not scheduler.cancel_event.is_set()
                else ("cancelled" if result.cancelled else ("partial" if result.failed_tracks else "complete")),
                "workspace": str(self.workspace),
                "config_version": self.config_version,
                "boundaries_sha256": self.timeline.boundaries_sha256,
                "slice_count": len(self.timeline.boundaries),
                "track_status": dict(sorted(statuses.items())),
                "failed_tracks": dict(sorted(failures.items())),
                "scheduler": result.to_dict(),
                "snapshot": str(self.snapshot_path),
                "trace": str(self.trace_path),
            }
            self._write_exit_report(report)
            return report
        finally:
            with self._state_lock:
                self._running = False
                self._active_tracks = set()
            self._crash_after_commits = None
            self._run_lock.release()

    def pause(self) -> None:
        scheduler = self._scheduler
        if scheduler:
            scheduler.pause()

    def resume_run(self) -> None:
        scheduler = self._scheduler
        if scheduler:
            scheduler.resume()

    def cancel(self) -> None:
        scheduler = self._scheduler
        if scheduler:
            scheduler.cancel()

    def state_summary(self) -> dict[str, Any]:
        tracks = {}
        for track in self.config.tracks:
            value = self._indexes.get(track.track_id, {})
            tracks[track.track_id] = {
                "completed_slices": len(value.get("entries", [])),
                "slice_count": len(self.timeline.boundaries),
                "status": value.get("status", "pending"),
                "failure": value.get("failure"),
            }
        scheduler = self._scheduler
        return {
            "running": self._running,
            "tracks": tracks,
            "memory_reserved_bytes": scheduler.memory_reserved() if scheduler else 0,
            "memory_high_water_bytes": scheduler.high_water if scheduler else 0,
            "waiting_queue": scheduler.queue_snapshot() if scheduler else [],
        }
