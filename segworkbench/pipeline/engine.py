"""分块阶段编排引擎。

每个块按 ``grow -> clean -> stats`` 顺序推进，阶段提交后结果冻结；
引擎只通过 :class:`CheckpointStore` 持久化，恢复时不依赖任何内存状态。
并发由 :class:`concurrency.BoundedRunner` 提供，检查点写入在存储层
串行化，块序/阶段序由确定性的块编号与阶段顺序保证。
"""
from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

import numpy as np

from segworkbench.core.errors import ErrorCode, WorkbenchError, fail
from segworkbench.core.versioning import ALGORITHM_VERSION
from segworkbench.persistence.checkpoint import CheckpointStore
from segworkbench.pipeline.blocks import (
    all_block_ids,
    apply_bad_block_injection,
    crop_block,
    localize_truth,
    truth_for_block,
    verify_block_baseline,
)
from segworkbench.pipeline.geometry import deviation_report, region_statistics
from segworkbench.pipeline.growing import detect_spill, region_grow
from segworkbench.pipeline.loader import LoadedSample, observation_baseline
from segworkbench.pipeline.parameters import SegmentationParameters
from segworkbench.pipeline.postprocess import clean_mask
from segworkbench.pipeline.states import COMMITTED, PHASES, RUNNING, BlockStateMachine
from segworkbench.utils.hashing import sha256_array
from segworkbench.utils.logging_setup import get_logger

LOG = get_logger()


@dataclass
class BlockFailure:
    block_id: str
    phase: str
    code: str
    message: str
    context: Dict[str, Any] = field(default_factory=dict)


class SegmentationEngine:
    def __init__(self, sample: LoadedSample, params: SegmentationParameters, store: CheckpointStore,
                 block_ids: Optional[List[str]] = None) -> None:
        params.validate()
        self.sample = sample
        self.params = params
        self.store = store
        self.block_ids = block_ids or all_block_ids()
        self.states = BlockStateMachine(self.block_ids)
        self.failures: Dict[str, BlockFailure] = {}
        self._cancel = threading.Event()
        self._header = {
            "sample_id": sample.sample_id,
            "image_sha256": sample.image_hash,
            "truth_fingerprint": sample.truth_fingerprint,
            "param_version": params.param_version,
            "algorithm_version": ALGORITHM_VERSION,
            "parameters": params.to_dict(),
            "block_grid": sample.block_grid,
            "baseline": observation_baseline(sample),
        }

    def request_cancel(self) -> None:
        self._cancel.set()

    def cancelled(self) -> bool:
        return self._cancel.is_set()

    def _raise_if_cancelled(self) -> None:
        if self._cancel.is_set():
            raise fail(ErrorCode.E_CANCELLED, "收到取消请求", phase_boundary=True)

    # ----- 恢复 -----------------------------------------------------------
    def restore(self) -> Dict[str, List[str]]:
        """仅依据已持久化且版本匹配的块阶段恢复内存状态。"""
        with self.store.locked():
            header = self.store.verify_header(self.sample.image_hash, self.params.param_version)
            committed = self.store.committed_phases()
        for block_id, phases in committed.items():
            if block_id not in self.block_ids:
                continue
            for phase in phases:
                record = self.store.load_phase(block_id, phase)
                if record is None:
                    continue
                self.states.get(block_id, phase).state = COMMITTED
        LOG.info("恢复完成: %s", committed)
        return committed

    def initialize(self) -> None:
        """幂等初始化：快照不存在则创建；版本不匹配则全量重算。"""
        with self.store.locked():
            header = self.store.read_header()
            if header is None:
                self.store.init_snapshot(self._header)
                return
        try:
            self.store.verify_header(self.sample.image_hash, self.params.param_version)
        except WorkbenchError as exc:
            if exc.code != ErrorCode.E_VERSION:
                raise
            LOG.warning("快照作废: %s", exc.context)
        with self.store.locked():
            if self.store.read_header() is None:
                self.store.init_snapshot(self._header)

    # ----- 单块三阶段 -----------------------------------------------------
    def process_block(self, block_id: str) -> Dict[str, Any]:
        """处理单个块；任一阶段失败记录 E_* 并把该阶段置 FAILED。"""
        if block_id not in self.block_ids:
            raise fail(ErrorCode.E_PARA, "未知块编号", field="block_id", value=block_id)
        try:
            grow = self._phase_grow(block_id)
            clean = self._phase_clean(block_id, grow)
            stats = self._phase_stats(block_id, clean)
            self.failures.pop(block_id, None)
            return stats["metadata"]
        except WorkbenchError as exc:
            phase = exc.context.get("phase", "grow")
            with self.store.locked():
                self.states.mark_failed(block_id, phase, exc.code)
            self.failures[block_id] = BlockFailure(
                block_id, phase, exc.code, exc.message, dict(exc.context)
            )
            LOG.error("块失败 block=%s phase=%s code=%s", block_id, phase, exc.code)
            raise

    def _enter_phase(self, block_id: str, phase: str) -> None:
        self._raise_if_cancelled()
        state = self.states.get(block_id, phase)
        if state.state == COMMITTED:
            return
        state.transition(RUNNING)

    def _exit_phase(self, block_id: str, phase: str) -> None:
        self.states.move(block_id, phase, COMMITTED)

    def _phase_grow(self, block_id: str) -> Dict[str, Any]:
        phase = "grow"
        if self.states.is_committed(block_id, phase):
            return self.store.load_phase(block_id, phase)
        self._enter_phase(block_id, phase)
        block_image = crop_block(self.sample.image, block_id)
        try:
            block_image = apply_bad_block_injection(block_id, block_image, self.params.injections)
            self._detect_bad_block(block_id, block_image)
            image_truth = truth_for_block(self.sample.regions, block_id)
            truth = localize_truth(image_truth, block_id)
            grown = region_grow(block_id, block_image,
                                self._block_origin(block_id), self.params, image_truth)
            detect_spill(block_id, grown["raw_mask"], truth, self.params)
        except WorkbenchError as exc:
            exc.context.setdefault("phase", phase)
            raise
        metadata = {
            "block_id": block_id,
            "phase": phase,
            "seed_xy": list(self._seed_xy(block_id)),
            "raw_pixel_count": int(grown["raw_mask"].sum()),
        }
        with self.store.locked():
            record = self.store.commit_phase(
                block_id, phase, grown["raw_mask"].astype(np.uint8), metadata, self._header
            )
            self._exit_phase(block_id, phase)
        record["array"] = grown["raw_mask"].astype(np.uint8)
        record["metadata"] = metadata
        return record

    def _detect_bad_block(self, block_id: str, block_image: np.ndarray) -> None:
        """E_BLOCK：输入注入的坏块（全零/损坏/块哈希偏离基线）。"""
        spec = self.params.injections.get("bad_block", {})
        if block_id not in spec.get("blocks", []):
            return
        mode = spec.get("mode", "corrupt")
        if mode in ("zero", "corrupt"):
            raise fail(ErrorCode.E_BLOCK, "检测到坏块输入注入", block_id=block_id,
                       injection="bad_block", mode=mode,
                       block_sha256=sha256_array(block_image))
        if mode == "hash_break":
            expected = sha256_array(crop_block(self.sample.image, block_id))
            verify_block_baseline(block_id, block_image, expected)

    def _phase_clean(self, block_id: str, grow_record: Dict[str, Any]) -> Dict[str, Any]:
        phase = "clean"
        if self.states.is_committed(block_id, phase):
            return self.store.load_phase(block_id, phase)
        self._enter_phase(block_id, phase)
        raw_mask = grow_record["array"].astype(bool)
        result = clean_mask(raw_mask, self.params)
        metadata = {
            "block_id": block_id,
            "phase": phase,
            "final_pixel_count": int(result["final_mask"].sum()),
            "kept_component_count": int(result["kept_component_count"][0]),
            "struct_radius": self.params.struct_radius,
        }
        with self.store.locked():
            record = self.store.commit_phase(
                block_id, phase, result["final_mask"].astype(np.uint8), metadata, self._header
            )
            self._exit_phase(block_id, phase)
        record["array"] = result["final_mask"].astype(np.uint8)
        record["metadata"] = metadata
        return record

    def _phase_stats(self, block_id: str, clean_record: Dict[str, Any]) -> Dict[str, Any]:
        phase = "stats"
        if self.states.is_committed(block_id, phase):
            return self.store.load_phase(block_id, phase)
        self._enter_phase(block_id, phase)
        final_mask = clean_record["array"].astype(bool)
        local_stats = region_statistics(final_mask)
        origin_x, origin_y = self._block_origin(block_id)
        stats = [self._to_global_coordinates(item, origin_x, origin_y) for item in local_stats]
        truth = truth_for_block(self.sample.regions, block_id)
        # E_REGION：块内连通域数量不等于该块真值区域数量（每块设计为 1 个）。
        if len(stats) != 1:
            missing = [block_id] if len(stats) == 0 else []
            extra = [f"{block_id}#c{idx}" for idx in range(1, len(stats))] if len(stats) > 1 else []
            raise fail(ErrorCode.E_REGION, "区域数量与真值不一致", phase=phase, block_id=block_id,
                       measured_count=len(stats), expected_count=1, missing=missing, extra=extra)
        measured = stats[0]
        report = deviation_report(measured, truth)
        metadata = {
            "block_id": block_id,
            "phase": phase,
            "statistics": measured,
            "deviation": report,
            "truth_region_id": next(
                region.region_id for region in self.sample.regions if region.block_id == block_id
            ),
        }
        with self.store.locked():
            record = self.store.commit_phase(block_id, phase, None, metadata, self._header)
            self._exit_phase(block_id, phase)
        record["metadata"] = metadata
        return record

    @staticmethod
    def _to_global_coordinates(local: Dict[str, Any], origin_x: int, origin_y: int) -> Dict[str, Any]:
        x0, y0, x1, y1 = local["bbox_xyxy"]
        cx, cy = local["centroid_xy"]
        global_contour = [[x + origin_x, y + origin_y] for x, y in local["contour"]]
        return {
            **local,
            "centroid_xy": [cx + origin_x, cy + origin_y],
            "bbox_xyxy": [x0 + origin_x, y0 + origin_y, x1 + origin_x, y1 + origin_y],
            "contour": global_contour,
            "coordinate_space": "image",
        }

    def _block_origin(self, block_id: str) -> tuple[int, int]:
        from segworkbench.pipeline.blocks import block_rect

        x0, y0, _, _ = block_rect(block_id)
        return x0, y0

    def _seed_xy(self, block_id: str) -> tuple[int, int]:
        from segworkbench.pipeline.growing import resolve_seed

        return resolve_seed(block_id, self.params)

    # ----- 全量执行 -------------------------------------------------------
    def run_all(self, runner: Optional["BoundedRunner"] = None,
                on_block: Optional[Callable[[str], None]] = None) -> Dict[str, Any]:
        self.initialize()
        if runner is None:
            for block_id in self.block_ids:
                self.process_block(block_id)
                if on_block:
                    on_block(block_id)
        else:
            runner.map(self.process_block, list(self.block_ids))
        return self.summarize()

    def summarize(self) -> Dict[str, Any]:
        committed = {
            block_id: [phase for phase in PHASES if self.states.is_committed(block_id, phase)]
            for block_id in self.block_ids
        }
        return {
            "sample_id": self.sample.sample_id,
            "state_machine": self.states.snapshot(),
            "committed": committed,
            "failures": {key: value.__dict__ for key, value in self.failures.items()},
            "image_sha256": self.sample.image_hash,
            "truth_fingerprint": self.sample.truth_fingerprint,
            "algorithm_version": ALGORITHM_VERSION,
            "param_version": self.params.param_version,
        }
