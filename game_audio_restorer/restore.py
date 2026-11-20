"""Checkpointed restoration engine and atomic final export."""

from __future__ import annotations

import json
import os
import shutil
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .errors import BusinessError, ErrorCode
from .locks import TaskLock
from .metrics import audio_metrics
from .processing import DEFAULT_SEGMENT_SECONDS, process_segment, segment_bounds
from .recipes import Recipe
from .snapshot import (
    ENGINE_VERSION,
    SNAPSHOT_VERSION,
    load_and_validate,
    manifest_path,
    validate_manifest,
    write_manifest,
)
from .utils import canonical_json, sha256_bytes, sha256_json
from .wav_io import atomic_write_bytes, encode_wav, read_wav


@dataclass
class RunResult:
    status: str
    snapshot_dir: str
    output_wav: str | None
    report_path: str | None
    trace_path: str | None
    committed_segments: int
    total_segments: int
    hash_chain_head: str
    cancelled_at_segment: int | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "snapshot_dir": self.snapshot_dir,
            "output_wav": self.output_wav,
            "report_path": self.report_path,
            "trace_path": self.trace_path,
            "committed_segments": self.committed_segments,
            "total_segments": self.total_segments,
            "hash_chain_head": self.hash_chain_head,
            "cancelled_at_segment": self.cancelled_at_segment,
        }


SegmentCallback = Callable[[dict[str, Any]], None]


class RestorationEngine:
    def __init__(
        self,
        source_wav: str | os.PathLike[str],
        recipe: Recipe,
        work_dir: str | os.PathLike[str],
        *,
        output_wav: str | os.PathLike[str] | None = None,
        report_path: str | os.PathLike[str] | None = None,
        trace_path: str | os.PathLike[str] | None = None,
        segment_seconds: float = DEFAULT_SEGMENT_SECONDS,
        resume: bool = True,
    ) -> None:
        self.source_wav = Path(source_wav)
        self.recipe = recipe
        self.work_dir = Path(work_dir)
        self.output_wav = (
            Path(output_wav)
            if output_wav is not None
            else self.work_dir / "restored.wav"
        )
        self.report_path = (
            Path(report_path)
            if report_path is not None
            else self.work_dir / "report.json"
        )
        self.trace_path = (
            Path(trace_path) if trace_path is not None else self.work_dir / "trace.jsonl"
        )
        self.segment_seconds = float(segment_seconds)
        self.resume = bool(resume)

    def run(
        self,
        *,
        cancel_event: threading.Event | None = None,
        on_segment: SegmentCallback | None = None,
        finalize: bool = True,
    ) -> RunResult:
        if self.segment_seconds <= 0:
            raise ValueError("segment_seconds must be positive")
        cancel = cancel_event or threading.Event()
        with TaskLock(self.source_wav):
            audio = read_wav(self.source_wav)
            bounds = segment_bounds(audio.frames, audio.sample_rate, self.segment_seconds)
            manifest, start_index = self._load_or_initialize(audio, len(bounds))
            output_channels = self._output_channels(audio.channels)
            if len(manifest["segments"]) < len(bounds):
                self._remove_stale_final_outputs()

            committed = len(manifest["segments"])
            if committed >= len(bounds):
                if finalize:
                    result = self._export(audio, manifest)
                    return result
                return self._result(
                    "completed",
                    manifest,
                    committed,
                    len(bounds),
                    None,
                )

            for index in range(start_index, len(bounds)):
                if cancel.is_set():
                    result = self._result(
                        "cancelled",
                        manifest,
                        len(manifest["segments"]),
                        len(bounds),
                        index,
                    )
                    return result
                start, end = bounds[index]
                try:
                    processed, metrics, parameters = process_segment(
                        audio.samples[start:end],
                        audio.sample_rate,
                        self.recipe,
                        index,
                    )
                except BusinessError:
                    raise
                except Exception as exc:
                    raise BusinessError(
                        ErrorCode.PROCESSING,
                        f"片段 {index} 处理异常，未提交任何声道或片段文件",
                        {
                            "segment": index,
                            "error_type": type(exc).__name__,
                            "error": str(exc),
                        },
                    ) from exc
                metrics = dict(metrics)
                final_stage = parameters["stages"][-1]
                applied_gain = float(final_stage["total_gain_db"]) + float(
                    final_stage["limiter_applied_db"]
                )
                metrics["applied_gain_db"] = round(applied_gain, 6)
                if manifest["segments"]:
                    previous_stage = manifest["segments"][-1]["parameters"]["stages"][-1]
                    previous_gain = float(previous_stage["total_gain_db"]) + float(
                        previous_stage["limiter_applied_db"]
                    )
                    jump = applied_gain - previous_gain
                else:
                    jump = 0.0
                metrics["gain_jump_db"] = round(jump, 6)
                if processed.shape[1] != output_channels:
                    raise BusinessError(
                        ErrorCode.PROCESSING,
                        "片段输出声道数与任务声明不一致",
                        {
                            "segment": index,
                            "actual": int(processed.shape[1]),
                            "expected": output_channels,
                        },
                    )
                segment_bytes = encode_wav(processed, audio.sample_rate)
                relative = Path("segments") / f"{index:06d}.wav"
                segment_path = self.work_dir / relative
                atomic_write_bytes(segment_path, segment_bytes)
                updated = self._append(
                    manifest,
                    index=index,
                    start=start,
                    end=end,
                    relative=relative,
                    file_sha256=sha256_bytes(segment_bytes),
                    metrics=metrics,
                    parameters=parameters,
                )
                write_manifest(self.work_dir, updated)
                manifest = updated
                entry = manifest["segments"][-1]
                if on_segment is not None:
                    on_segment(entry)

            if finalize:
                return self._export(audio, manifest)
            return self._result(
                "completed",
                manifest,
                len(manifest["segments"]),
                len(bounds),
                None,
            )

    def _load_or_initialize(
        self, audio: Any, total_segments: int
    ) -> tuple[dict[str, Any], int]:
        path = manifest_path(self.work_dir)
        if path.exists():
            if not self.resume:
                raise BusinessError(
                    ErrorCode.BUSY,
                    "恢复目录已有快照，且当前未启用恢复",
                    {"snapshot_dir": str(self.work_dir)},
                )
            manifest = load_and_validate(
                self.work_dir,
                expected_source_sha256=audio.sha256,
                expected_recipe_hash=self.recipe.hash(),
            )
            if manifest["source"].get("segment_size") != self._segment_size(audio.sample_rate):
                raise BusinessError(
                    ErrorCode.VERSION_STALE,
                    "恢复快照的分段边界参数与当前任务不一致",
                    {
                        "snapshot_segment_size": manifest["source"].get("segment_size"),
                        "expected_segment_size": self._segment_size(audio.sample_rate),
                    },
                )
            expected_output = {
                "sample_rate": audio.sample_rate,
                "channels": self._output_channels(audio.channels),
            }
            if manifest["output"] != expected_output:
                raise BusinessError(
                    ErrorCode.RECOVERY_INVALID,
                    "恢复快照的输出布局与当前 WAV/配方不一致",
                    {
                        "first_break_segment": len(manifest["segments"]),
                        "snapshot_output": manifest["output"],
                        "expected_output": expected_output,
                    },
                )
            return manifest, len(manifest["segments"])

        self.work_dir.mkdir(parents=True, exist_ok=True)
        (self.work_dir / "segments").mkdir(parents=True, exist_ok=True)
        manifest: dict[str, Any] = {
            "snapshot_version": SNAPSHOT_VERSION,
            "engine_version": ENGINE_VERSION,
            "task_id": sha256_json(
                {
                    "source": audio.sha256,
                    "recipe": self.recipe.hash(),
                    "segment_size": self._segment_size(audio.sample_rate),
                }
            )[:24],
            "source": {
                "path": audio.source_path,
                "sha256": audio.sha256,
                "byte_length": audio.byte_length,
                "sample_rate": audio.sample_rate,
                "channels": audio.channels,
                "frames": audio.frames,
                "segment_seconds": self.segment_seconds,
                "segment_size": self._segment_size(audio.sample_rate),
                "total_segments": total_segments,
            },
            "recipe_version": self.recipe.version,
            "recipe_hash": self.recipe.hash(),
            "recipe": {
                **self.recipe.semantic_dict(),
                "field_mapping": list(self.recipe.field_mapping),
                "lost_fields": list(self.recipe.lost_fields),
            },
            "output": {
                "sample_rate": audio.sample_rate,
                "channels": self._output_channels(audio.channels),
            },
            "segments": [],
        }
        write_manifest(self.work_dir, manifest)
        return manifest, 0

    def _remove_stale_final_outputs(self) -> None:
        source = self.source_wav.resolve()
        for path in (self.output_wav, self.report_path, self.trace_path):
            try:
                if path.resolve() == source:
                    continue
            except OSError:
                pass
            try:
                path.unlink(missing_ok=True)
            except OSError as exc:
                raise BusinessError(
                    ErrorCode.PROCESSING,
                    "无法清理旧修复产物，拒绝在可能误导的目录中续跑",
                    {"path": str(path), "error": str(exc)},
                ) from exc

    def _append(
        self,
        manifest: dict[str, Any],
        *,
        index: int,
        start: int,
        end: int,
        relative: Path,
        file_sha256: str,
        metrics: dict[str, Any],
        parameters: dict[str, Any],
    ) -> dict[str, Any]:
        updated = dict(manifest)
        segments = list(manifest["segments"])
        previous_chain = segments[-1]["chain_hash"] if segments else sha256_bytes(
            b"GAME_AUDIO_RESTORER_CHAIN_V1"
        )
        entry = {
            "index": index,
            "start": start,
            "end": end,
            "path": relative.as_posix(),
            "sha256": file_sha256,
            "metrics": metrics,
            "parameters": parameters,
            "recipe_hash": self.recipe.hash(),
            "recipe_version": self.recipe.version,
            "prev_chain_hash": previous_chain,
        }
        from .snapshot import chain_hash

        entry["chain_hash"] = chain_hash(previous_chain, entry)
        updated["segments"] = [*segments, entry]
        return updated

    def _output_channels(self, input_channels: int) -> int:
        if self.recipe.channel_merge_mode == "preserve":
            return input_channels
        return 1

    def _segment_size(self, sample_rate: int) -> int:
        return max(1, int(round(sample_rate * self.segment_seconds)))

    def _export(
        self, audio: Any, manifest: dict[str, Any]
    ) -> RunResult:
        current_source = read_wav(self.source_wav)
        if current_source.sha256 != audio.sha256:
            raise BusinessError(
                ErrorCode.VERSION_STALE,
                "导出前源 WAV 已变化，拒绝提交与快照不一致的产物",
                {
                    "snapshot_source_sha256": audio.sha256,
                    "current_source_sha256": current_source.sha256,
                },
            )
        # Revalidate immediately before export; the current recipe and every
        # committed segment hash must still agree with the manifest.
        validate_manifest(
            self.work_dir,
            manifest,
            expected_source_sha256=audio.sha256,
            expected_recipe_hash=self.recipe.hash(),
        )
        total_segments = int(manifest["source"]["total_segments"])
        if len(manifest["segments"]) != total_segments:
            raise BusinessError(
                ErrorCode.RECOVERY_INVALID,
                "片段清单未完成，不能导出最终产物",
                {
                    "first_break_segment": len(manifest["segments"]),
                    "committed": len(manifest["segments"]),
                    "expected": total_segments,
                },
            )

        chunks = []
        for index, entry in enumerate(manifest["segments"]):
            segment_audio = read_wav(self.work_dir / entry["path"])
            if segment_audio.sample_rate != audio.sample_rate:
                raise BusinessError(
                    ErrorCode.RECOVERY_INVALID,
                    "片段采样率不一致",
                    {"first_break_segment": index},
                )
            if segment_audio.channels != int(manifest["output"]["channels"]):
                raise BusinessError(
                    ErrorCode.RECOVERY_INVALID,
                    "片段声道数与快照输出布局不一致",
                    {
                        "first_break_segment": index,
                        "actual": segment_audio.channels,
                        "expected": manifest["output"]["channels"],
                    },
                )
            chunks.append(segment_audio.samples)
        output_channels = int(manifest["output"]["channels"])
        if chunks:
            output_samples = np.concatenate(chunks, axis=0)
        else:
            output_samples = np.empty((0, output_channels), dtype=np.float64)
        if output_samples.shape[1] != output_channels:
            raise BusinessError(
                ErrorCode.RECOVERY_INVALID,
                "合并后的输出声道数不一致",
                {
                    "first_break_segment": 0,
                    "actual": int(output_samples.shape[1]),
                    "expected": output_channels,
                },
            )
        output_bytes = encode_wav(output_samples, audio.sample_rate)
        output_hash = sha256_bytes(output_bytes)

        trace_bytes = self._trace_bytes(manifest)
        report = self._report(manifest, output_samples, output_bytes, output_hash)
        report_bytes = (
            json.dumps(
                report,
                ensure_ascii=False,
                sort_keys=True,
                indent=2,
                allow_nan=False,
            ).encode("utf-8")
            + b"\n"
        )

        atomic_write_bytes(self.output_wav, output_bytes)
        atomic_write_bytes(self.trace_path, trace_bytes)
        atomic_write_bytes(self.report_path, report_bytes)
        return RunResult(
            status="completed",
            snapshot_dir=str(self.work_dir),
            output_wav=str(self.output_wav),
            report_path=str(self.report_path),
            trace_path=str(self.trace_path),
            committed_segments=len(manifest["segments"]),
            total_segments=total_segments,
            hash_chain_head=(
                manifest["segments"][-1]["chain_hash"] if manifest["segments"] else ""
            ),
        )

    def _trace_bytes(self, manifest: dict[str, Any]) -> bytes:
        lines: list[bytes] = []
        header = {
            "trace_version": 1,
            "source_sha256": manifest["source"]["sha256"],
            "sample_rate": manifest["source"]["sample_rate"],
            "input_channels": manifest["source"]["channels"],
            "output_channels": manifest["output"]["channels"],
            "recipe_version": manifest["recipe_version"],
            "recipe_hash": manifest["recipe_hash"],
            "field_mapping": manifest["recipe"]["field_mapping"],
            "lost_fields": manifest["recipe"]["lost_fields"],
        }
        lines.append((canonical_json(header) + "\n").encode("utf-8"))
        for entry in manifest["segments"]:
            row = {
                "segment": entry["index"],
                "start_frame": entry["start"],
                "end_frame": entry["end"],
                "recipe_version": entry["recipe_version"],
                "recipe_hash": entry["recipe_hash"],
                "field_mapping": manifest["recipe"]["field_mapping"],
                "metrics": entry["metrics"],
                "content_sha256": entry["sha256"],
                "prev_chain_hash": entry["prev_chain_hash"],
                "chain_hash": entry["chain_hash"],
            }
            lines.append((canonical_json(row) + "\n").encode("utf-8"))
        return b"".join(lines)

    def _report(
        self,
        manifest: dict[str, Any],
        output_samples: np.ndarray,
        output_bytes: bytes,
        output_hash: str,
    ) -> dict[str, Any]:
        return {
            "report_version": 1,
            "engine_version": ENGINE_VERSION,
            "status": "completed",
            "task_id": manifest["task_id"],
            "source": {
                "path": manifest["source"]["path"],
                "sha256": manifest["source"]["sha256"],
                "byte_length": manifest["source"]["byte_length"],
                "sample_rate": manifest["source"]["sample_rate"],
                "channels": manifest["source"]["channels"],
                "frames": manifest["source"]["frames"],
                "duration_s": round(
                    manifest["source"]["frames"] / manifest["source"]["sample_rate"], 6
                ),
            },
            "recipe": {
                "version": manifest["recipe_version"],
                "hash": manifest["recipe_hash"],
                "normalized": manifest["recipe"],
                "field_mapping": manifest["recipe"]["field_mapping"],
                "lost_fields": manifest["recipe"]["lost_fields"],
                "lost_field_count": len(manifest["recipe"]["lost_fields"]),
            },
            "segments": [
                {
                    "index": entry["index"],
                    "start_frame": entry["start"],
                    "end_frame": entry["end"],
                    "start_s": round(
                        entry["start"] / manifest["source"]["sample_rate"], 6
                    ),
                    "end_s": round(
                        entry["end"] / manifest["source"]["sample_rate"], 6
                    ),
                    "recipe_version": entry["recipe_version"],
                    "recipe_hash": entry["recipe_hash"],
                    "metrics": entry["metrics"],
                    "parameters": entry["parameters"],
                    "content_sha256": entry["sha256"],
                    "prev_chain_hash": entry["prev_chain_hash"],
                    "chain_hash": entry["chain_hash"],
                }
                for entry in manifest["segments"]
            ],
            "aggregate_metrics": audio_metrics(
                output_samples, manifest["output"]["sample_rate"]
            ),
            "output": {
                "path": str(self.output_wav),
                "sha256": output_hash,
                "byte_length": len(output_bytes),
                "sample_rate": manifest["output"]["sample_rate"],
                "channels": manifest["output"]["channels"],
                "frames": int(output_samples.shape[0]),
            },
            "snapshot": {
                "version": manifest["snapshot_version"],
                "segment_count": len(manifest["segments"]),
                "hash_chain_head": (
                    manifest["segments"][-1]["chain_hash"]
                    if manifest["segments"]
                    else ""
                ),
            },
        }

    def _result(
        self,
        status: str,
        manifest: dict[str, Any],
        committed: int,
        total: int,
        cancelled_at: int | None,
    ) -> RunResult:
        return RunResult(
            status=status,
            snapshot_dir=str(self.work_dir),
            output_wav=None,
            report_path=None,
            trace_path=None,
            committed_segments=committed,
            total_segments=total,
            hash_chain_head=(
                manifest["segments"][-1]["chain_hash"] if manifest["segments"] else ""
            ),
            cancelled_at_segment=cancelled_at,
        )


def export_existing(
    source_wav: str | os.PathLike[str],
    recipe: Recipe,
    work_dir: str | os.PathLike[str],
    *,
    output_wav: str | os.PathLike[str] | None = None,
    report_path: str | os.PathLike[str] | None = None,
    trace_path: str | os.PathLike[str] | None = None,
) -> RunResult:
    engine = RestorationEngine(
        source_wav,
        recipe,
        work_dir,
        output_wav=output_wav,
        report_path=report_path,
        trace_path=trace_path,
    )
    with TaskLock(source_wav):
        audio = read_wav(source_wav)
        manifest = load_and_validate(
            work_dir,
            expected_source_sha256=audio.sha256,
            expected_recipe_hash=recipe.hash(),
        )
        return engine._export(audio, manifest)


def remove_work_dir(path: str | os.PathLike[str]) -> None:
    """Used by tests/demo cleanup; never called automatically by the engine."""

    target = Path(path)
    if target.exists():
        shutil.rmtree(target)
