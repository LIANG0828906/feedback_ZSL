"""脚本化实验编排与确定性轨迹日志。

编排器把加载、双路径转换、误差上界、增量重算、版本发布、
快照恢复串成一个可复现实验；轨迹日志为 JSONL，每行一条事件，
仅记录内容哈希、帧数、误差等确定性字段，不含时间戳或文件大小。
"""

from __future__ import annotations

import json
import os

import numpy as np

from . import color
from .cache import FrameCache, frame_cache_key
from .errorbound import compute_frame_error_bound
from .hashing import array_hash
from .hdrformat import load_sequence
from .pipeline import (
    ConversionConfig,
    FrameResult,
    convert_frame,
    diff_summary,
)
from .snapshot import SnapshotWriter, recover_snapshot
from .versions import FramePayload, VersionBuilder, VersionStore


def bound_summary(report) -> dict:
    return {
        "max_bound_codes": float(np.max(report.bound_codes)),
        "mean_bound_codes": float(np.mean(report.bound_codes)),
        "untrusted_pixels": int(np.sum(report.untrusted)),
        "stage_max_bounds": {
            step: float(np.max(report.stage_bounds[step]))
            for step in report.stage_bounds
        },
    }


def payload_from_result(result: FrameResult) -> FramePayload:
    return FramePayload(
        frame_index=result.frame_index,
        codes=result.codes,
        source_hash=result.source_hash,
        stage_hashes=result.stage_hashes,
        max_diff_codes=result.max_diff_codes,
        over_tolerance=result.over_tolerance,
        bound_summary=result.bound_summary,
    )


class TrajectoryLog:
    """逐行 JSONL 轨迹日志，固定换行与排序键，保证逐字节可复现。"""

    def __init__(self, path: str):
        self.path = path
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        self._handle = open(path, "w", encoding="utf-8", newline="\n")

    def event(self, name: str, **fields) -> None:
        record = {"event": name}
        record.update(fields)
        self._handle.write(json.dumps(record, sort_keys=True) + "\n")

    def close(self) -> None:
        self._handle.flush()
        os.fsync(self._handle.fileno())
        self._handle.close()


class ExperimentRunner:
    """序列 -> 多版本发布 -> 可选崩溃恢复的状态机。"""

    def __init__(self, sequence_path: str, work_dir: str, log: TrajectoryLog):
        self.sequence = load_sequence(sequence_path)
        self.work_dir = work_dir
        self.log = log
        self.store = VersionStore()
        self.cache = FrameCache()
        self.source_hashes = [
            array_hash(frame) for frame in self.sequence.raw_frames
        ]
        self._version_counter = 0
        self._last_config: ConversionConfig | None = None
        self._previous_results: list[FrameResult] | None = None
        os.makedirs(work_dir, exist_ok=True)
        self.log.event(
            "sequence_loaded",
            header=self.sequence.header.as_dict(),
            frame_count=self.sequence.header.frame_count,
            source_hashes=self.source_hashes,
        )

    def _matrix_pair(self, config: ConversionConfig):
        header = self.sequence.header
        matrix = color.color_transform_matrix(
            header.colorspace, config.target_colorspace
        )
        adapt = color.white_adaptation_matrix(
            color.CHROMATICITIES[header.colorspace][3],
            color.CHROMATICITIES[config.target_colorspace][3],
        )
        return matrix, adapt

    def _initial_plan(self):
        return type(
            "Plan",
            (),
            {
                "affected_frames": list(
                    range(self.sequence.header.frame_count)
                ),
                "reused_frames": [],
                "changed_factors": ["initial"],
                "conservative_reasons": {},
            },
        )()

    def run_version(
        self,
        config: ConversionConfig,
        snapshot_dir: str | None = None,
        crash_after: int | None = None,
    ) -> str:
        config.validate()
        if self._last_config is None:
            plan = self._initial_plan()
        else:
            plan = self.cache.plan(
                self.source_hashes, self._last_config, config
            )

        self.log.event(
            "recompute_plan",
            version=self._version_counter + 1,
            changed_factors=plan.changed_factors,
            affected_frames=plan.affected_frames,
            reused_frames=plan.reused_frames,
            conservative_reasons={
                str(key): value
                for key, value in plan.conservative_reasons.items()
            },
        )

        builder = VersionBuilder(
            self._version_counter + 1,
            config,
            self.sequence.header,
            plan.affected_frames,
            plan.reused_frames,
        )
        matrix, adapt = self._matrix_pair(config)
        norm = self.sequence.normalized_frames()

        writer = None
        pre_committed: list[int] = []
        if snapshot_dir is not None:
            os.makedirs(snapshot_dir, exist_ok=True)
            if os.path.isdir(os.path.join(snapshot_dir, "chunks")):
                recovered = recover_snapshot(snapshot_dir)
                pre_committed = recovered["committed_frames"]
            writer = SnapshotWriter(
                snapshot_dir,
                manifest={
                    "version_id": self._version_counter + 1,
                    "factors": config.factor_dict(),
                    "header": self.sequence.header.as_dict(),
                },
                crash_after=crash_after,
            )
            self.log.event(
                "snapshot_resume", committed_frames=pre_committed
            )

        new_results: list[FrameResult] = []
        for index in range(len(self.source_hashes)):
            if index in plan.reused_frames:
                result = self.cache.get(index).result
                self.log.event(
                    "frame_reused",
                    frame=index,
                    cache_key=frame_cache_key(
                        self.source_hashes[index], config
                    ),
                    content_hash=array_hash(result.codes),
                )
            else:
                result = convert_frame(
                    index,
                    norm[index],
                    self.source_hashes[index],
                    self.sequence.header,
                    config,
                )
                report = compute_frame_error_bound(
                    index,
                    norm[index],
                    self.sequence.header,
                    config,
                    matrix,
                    adapt,
                    threshold_codes=config.tolerance_codes,
                )
                result.bound_summary = bound_summary(report)
                result.error_report = report
                self.cache.put(
                    index, self.source_hashes[index], config, result
                )
                self.log.event(
                    "frame_recomputed",
                    frame=index,
                    cache_key=frame_cache_key(
                        self.source_hashes[index], config
                    ),
                    max_diff_codes=result.max_diff_codes,
                    over_tolerance=result.over_tolerance,
                    stage_max_abs=result.stage_max_abs,
                    stage_hashes=result.stage_hashes,
                    error_bound=result.bound_summary,
                )
            new_results.append(result)
            payload = payload_from_result(result)
            builder.add_frame(payload)
            if writer is not None and index not in pre_committed:
                writer.write_frame(
                    index,
                    result.codes,
                    payload.bound_summary,
                    result.source_hash,
                    result.stage_hashes,
                )

        if writer is not None:
            writer.publish_manifest()
            self.log.event(
                "snapshot_published",
                committed_frames=writer.committed_frames,
            )

        version = builder.publish()
        version_hash = self.store.publish(version)
        self._version_counter += 1
        self._last_config = config

        if self._previous_results is not None:
            summary = diff_summary(self._previous_results, new_results)
            self.log.event(
                "version_diff",
                old_version=self._version_counter - 1,
                new_version=self._version_counter,
                changed_frames=summary["changed_frames"],
                overall_max_abs=summary["overall_max_abs"],
                per_frame=summary["per_frame"],
            )
        self._previous_results = new_results
        self.log.event(
            "version_published",
            version=self._version_counter,
            version_hash=version_hash,
            affected_frames=list(version.affected_frames),
            reused_frames=list(version.reused_frames),
            frame_hashes=list(version.frame_hashes),
        )
        return version_hash

