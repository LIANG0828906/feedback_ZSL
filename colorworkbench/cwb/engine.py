"""工作台门面：串联配置解析、转换管线、编辑、并发、快照与一致性比对。"""

from __future__ import annotations

import os

import numpy as np

from .editor import ProfileEditor
from .errors import CwbError, E_DIFF
from .executor import BatchExecutor
from .image_io import decode_image
from .pipeline import ConversionPipeline
from .profile_io import load_profile
from .snapshot import SnapshotStore, sha256_image
from .model import TransformConfig


class Workbench:
    def __init__(self, source_profile_path: str, target_profile_path: str,
                 intent: str, output_depth: int, batch_rows: int = 8,
                 max_workers: int = 4,
                 pixel_buffer_cap: int = 4 * 1024 * 1024,
                 snapshot_dir: str | None = None):
        self.source_profile = load_profile(source_profile_path)
        self.target_profile = load_profile(target_profile_path)
        self.config = TransformConfig(
            source=self.source_profile, target=self.target_profile,
            intent=intent, output_depth=output_depth,
            batch_rows=batch_rows)
        self.max_workers = max_workers
        self.pixel_buffer_cap = pixel_buffer_cap
        self.snapshot_dir = snapshot_dir
        self.image: np.ndarray | None = None
        self.pipeline: ConversionPipeline | None = None
        self.editor: ProfileEditor | None = None
        self.executor: BatchExecutor | None = None
        self.store: SnapshotStore | None = None
        self.reuse_count = 0

    # ---- 加载与完整运行 ----
    def load_image(self, path: str, expected_depth: int | None = None
                   ) -> np.ndarray:
        self.image = decode_image(path, expected_depth)
        if self.config.output_depth is None:
            self.config.output_depth = 16 if self.image.dtype == np.uint16 \
                else 8
        self._build_pipeline()
        return self.image

    def _build_pipeline(self) -> None:
        self.pipeline = ConversionPipeline(self.config)
        self.pipeline.reset_batches(self.image.shape[0])
        self.editor = ProfileEditor(self.config, self.pipeline)
        self.executor = BatchExecutor(self.pipeline, self.max_workers,
                                      self.pixel_buffer_cap)
        if self.snapshot_dir:
            self.store = SnapshotStore(self.snapshot_dir)

    def run_full(self, parallel: bool = True, use_snapshot: bool = True
                 ) -> np.ndarray:
        assert self.image is not None and self.pipeline is not None
        source_hash = sha256_image(self.image)
        config_version = (f"{self.source_profile.source_version}->"
                          f"{self.target_profile.source_version}")
        writer = None
        if self.store is not None and use_snapshot:
            # 先尝试断点恢复
            try:
                cached = self.store.load(
                    source_hash, config_version, self.editor.param_version)
                return self._restore_from_snapshot(cached)
            except CwbError:
                pass
            self.store.init_run(
                source_hash, config_version, self.editor.param_version,
                len(self.pipeline.batches), self.config.output_depth,
                self.image.shape)

            def writer(bid: int, result):
                data = self.pipeline.output_of(bid)
                self.store.write_batch(bid, result.row_start,
                                       result.row_end, data)

        self.executor.run(self.image, parallel=parallel,
                          snapshot_writer=writer)
        self._raise_if_failed()
        if self.store is not None and use_snapshot:
            self.store.commit_manifest()
        self.reuse_count = sum(b.cache_hits for b in self.pipeline.batches)
        return self.pipeline.assemble(*self.image.shape)

    def _restore_from_snapshot(self, cached: dict[int, np.ndarray]) -> np.ndarray:
        out = np.empty_like(self.image,
                            dtype=np.uint16 if self.config.output_depth == 16
                            else np.uint8)
        for bid, data in cached.items():
            batch = self.pipeline.batches[bid]
            batch.stages["S5_output_encode"] = data
            batch.status = "done"
            out[batch.row_start:batch.row_end] = data
        return out

    # ---- 编辑后增量重算 ----
    def edit_and_recompute(self, edit_fn, affected_batches=None,
                           parallel: bool = True) -> np.ndarray:
        before_hits = self._total_hits()
        edit_fn(self.editor)
        inv = self.editor.commit(affected_batches)
        # 受影响批重算，未受影响批因 stage 缓存保留而被复用
        results = self.executor.run(
            self.image, batch_ids=inv.batch_ids or None, parallel=parallel)
        self._raise_if_failed()
        self.reuse_count = self._total_hits() - before_hits
        return self.pipeline.assemble(*self.image.shape)

    def _raise_if_failed(self) -> None:
        if self.executor.failed_batches:
            bid = next(iter(self.executor.failed_batches))
            code = self.executor.failed_batches[bid]
            raise CwbError(code, f"像素批 {bid} 转换失败: {code}")

    def _total_hits(self) -> int:
        return sum(b.cache_hits for b in self.pipeline.batches)

    # ---- 增量 vs 全量一致性比对 ----
    def verify_equivalence(self, reference_image: np.ndarray,
                           reference_stats: dict, reference_hist: dict
                           ) -> dict:
        current = self.pipeline.assemble(*self.image.shape)
        if current.shape != reference_image.shape or \
                current.dtype != reference_image.dtype:
            raise CwbError(E_DIFF, "输出形状/位深与全量基线不一致")
        mismatch = np.argwhere(current != reference_image)
        if mismatch.size:
            coord = tuple(int(v) for v in mismatch[0])
            bid = next(b.batch_id for b in self.pipeline.batches
                       if b.row_start <= coord[0] < b.row_end)
            stage = self.editor.last_invalidation.stage \
                if self.editor.last_invalidation else "unknown"
            raise CwbError(E_DIFF,
                           "增量与全量结果不一致",
                           detail={"first_mismatch": coord, "batch": bid,
                                   "stage": stage})
        stats = ConversionPipeline.diff_statistics(self.image, current)
        hist = ConversionPipeline.channel_histogram(current)
        if stats != reference_stats or hist != reference_hist:
            raise CwbError(E_DIFF, "差异统计/直方图与全量基线不一致")
        return {"equal": True, "pixels": int(current.size),
                "reuse_count": self._total_hits()}

    def stats_and_histogram(self, output: np.ndarray) -> tuple[dict, dict]:
        stats = ConversionPipeline.diff_statistics(self.image, output)
        hist = ConversionPipeline.channel_histogram(output)
        return stats, hist

    def heatmap(self, output: np.ndarray) -> np.ndarray:
        return ConversionPipeline.heatmap(self.image, output)

    def retry_failed(self, parallel: bool = True) -> np.ndarray:
        ids = list(self.executor.failed_batches)
        self.executor.retry(self.image, ids, parallel=parallel)
        return self.pipeline.assemble(*self.image.shape)
