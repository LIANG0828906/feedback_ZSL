"""设备 -> PCS -> 目标空间的逐像素批转换管线。

管线按像素批（连续若干行）与阶段组织，阶段产物可缓存，
参数编辑后由 editor 决定哪些 (批, 阶段) 失效并增量重算。

阶段编号：
  S1 输入归一化 + 源 TRC 反演
  S2 矩阵 / 3D LUT 进入 PCS
  S3 白点适配 + 渲染意图
  S4 目标空间逆矩阵（PCS -> 目标线性）
  S5 目标 TRC 编码 + 位深量化
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .color_math import (
    PCS_D50,
    adapt_and_render,
    apply_lut3d,
    apply_matrix,
    resolve_white_point,
)
from .errors import CwbError, E_BATCH, E_DEPTH
from .model import TraceRecord, TransformConfig
from .trc import encode as trc_encode
from .trc import linearize

STAGES = ("S1_trc_invert", "S2_to_pcs", "S3_adapt_intent",
          "S4_from_pcs", "S5_output_encode")


@dataclass
class BatchResult:
    batch_id: int
    row_start: int
    row_end: int
    stages: dict[str, np.ndarray] = field(default_factory=dict)
    status: str = "pending"          # pending|done|failed
    error: str | None = None
    cache_hits: int = 0


@dataclass
class PipelineStats:
    diff_stats: dict
    histogram: dict
    traces: list[TraceRecord]


def _max_value(depth: int) -> int:
    return 65535 if depth == 16 else 255


def _split_color_alpha(data: np.ndarray):
    c = data.shape[2]
    if c == 1:
        rgb = np.repeat(data, 3, axis=2)
        return rgb, None, 1
    if c == 2:
        rgb = np.repeat(data[..., :1], 3, axis=2)
        return rgb, data[..., 1:2], 1
    if c >= 3:
        return data[..., :3], (data[..., 3:4] if c == 4 else None), 3
    raise CwbError(E_DEPTH, f"不支持的通道数 {c}")


class ConversionPipeline:
    """无状态算法 + 有状态批缓存的转换引擎。"""

    def __init__(self, config: TransformConfig):
        self.config = config
        self.batches: list[BatchResult] = []
        self.traces: list[TraceRecord] = []
        self._prepared = False

    # ---- 预处理：白点解析（只解析一次并留痕） ----
    def prepare(self) -> None:
        if self._prepared:
            return
        src_wp, trace1 = resolve_white_point(self.config.source)
        dst_wp, trace2 = resolve_white_point(self.config.target)
        for tr in (trace1, trace2):
            if tr is not None:
                self.traces.append(tr)
        self._src_wp = src_wp
        self._dst_wp = dst_wp
        self._prepared = True

    def reset_batches(self, height: int) -> None:
        rows = self.config.batch_rows
        self.batches = [
            BatchResult(bid, start, min(start + rows, height))
            for bid, start in enumerate(range(0, height, rows))
        ]

    # ---- 单批各阶段 ----
    def _stage1(self, batch: BatchResult, raw: np.ndarray) -> np.ndarray:
        depth = self._input_depth(raw)
        color, _, _ = _split_color_alpha(raw)
        norm = color.astype(np.float64) / _max_value(depth)
        return linearize(self.config.source.trc, norm)

    def _stage2(self, linear: np.ndarray) -> np.ndarray:
        src = self.config.source
        if src.lut3d is not None:
            return apply_lut3d(src.lut3d, linear)
        return apply_matrix(src.matrix, linear)

    def _stage3(self, pcs: np.ndarray) -> np.ndarray:
        return adapt_and_render(pcs, self._src_wp, self._dst_wp,
                                self.config.intent)

    def _stage4(self, adapted: np.ndarray) -> np.ndarray:
        inv = np.linalg.inv(
            np.asarray(self.config.target.matrix,
                       dtype=np.float64).reshape(3, 3))
        return adapted @ inv.T

    def _stage5(self, target_linear: np.ndarray, raw: np.ndarray) -> np.ndarray:
        encoded = trc_encode(self.config.target.trc, target_linear)
        if self.config.output_depth not in (8, 16):
            raise CwbError(E_DEPTH,
                           f"输出位深 {self.config.output_depth} 非法",
                           tag="output_depth")
        q = np.rint(encoded * _max_value(self.config.output_depth))
        q = np.clip(q, 0, _max_value(self.config.output_depth))
        dtype = np.uint16 if self.config.output_depth == 16 else np.uint8
        color = q.astype(dtype)
        return self._restore_channels(raw, color)

    def _restore_channels(self, raw: np.ndarray, color: np.ndarray) -> np.ndarray:
        c = raw.shape[2]
        depth = self._input_depth(raw)
        dtype = np.uint16 if self.config.output_depth == 16 else np.uint8
        if c == 1:
            return color[..., :1]
        if c == 2:
            alpha = raw[..., 1:2].astype(dtype)
            if depth != self.config.output_depth:
                alpha = self._resample_alpha(alpha, depth)
            return np.concatenate([color[..., :1], alpha], axis=2)
        if c == 3:
            return color
        alpha = raw[..., 3:4].astype(dtype)
        if depth != self.config.output_depth:
            alpha = self._resample_alpha(alpha, depth)
        return np.concatenate([color, alpha], axis=2)

    def _resample_alpha(self, alpha: np.ndarray, src_depth: int) -> np.ndarray:
        if self.config.output_depth == 16 and src_depth == 8:
            return (alpha.astype(np.uint16) * 257).astype(np.uint16)
        if self.config.output_depth == 8 and src_depth == 16:
            return (alpha.astype(np.uint16) // 257).astype(np.uint8)
        return alpha

    @staticmethod
    def _input_depth(raw: np.ndarray) -> int:
        if raw.dtype == np.uint8:
            return 8
        if raw.dtype == np.uint16:
            return 16
        raise CwbError(E_DEPTH, f"输入 dtype {raw.dtype} 非 8/16 位",
                       tag="input")

    def run_batch(self, batch_id: int, raw_batch: np.ndarray,
                  reuse: bool = True) -> BatchResult:
        """执行（或从缓存复用）一个像素批，单批异常只标记该批失败。"""
        self.prepare()
        batch = self.batches[batch_id]
        if reuse and batch.status == "done":
            batch.cache_hits += 1
            return batch
        try:
            chain = [
                ("S1_trc_invert", lambda prev: self._stage1(batch, raw_batch)),
                ("S2_to_pcs", self._stage2),
                ("S3_adapt_intent", self._stage3),
                ("S4_from_pcs", self._stage4),
                ("S5_output_encode",
                 lambda prev: self._stage5(prev, raw_batch)),
            ]
            current = None
            for name, fn in chain:
                if reuse and name in batch.stages:
                    current = batch.stages[name]
                    batch.cache_hits += 1
                    continue
                current = fn(current)
                # 仅缓存中间浮点阶段；最终阶段随结果保留
                batch.stages[name] = current
            batch.status = "done"
            batch.error = None
            return batch
        except CwbError as exc:
            batch.status = "failed"
            batch.error = exc.code
            raise
        except Exception as exc:  # 单批内部异常隔离
            batch.status = "failed"
            batch.error = E_BATCH
            raise CwbError(E_BATCH,
                           f"批 {batch_id} 转换失败: {exc}") from exc

    def invalidate(self, batch_ids, stage: str) -> None:
        """失效指定批从某阶段起的下游缓存。"""
        idx = STAGES.index(stage)
        for bid in batch_ids:
            batch = self.batches[bid]
            for name in STAGES[idx:]:
                batch.stages.pop(name, None)
            batch.status = "pending"
            batch.error = None

    def output_of(self, batch_id: int) -> np.ndarray:
        return self.batches[batch_id].stages["S5_output_encode"]

    def assemble(self, height: int, width: int, channels: int) -> np.ndarray:
        dtype = np.uint16 if self.config.output_depth == 16 else np.uint8
        out = np.empty((height, width, channels), dtype=dtype)
        for batch in self.batches:
            out[batch.row_start:batch.row_end] = self.output_of(batch.batch_id)
        return out

    # ---- 统计 ----
    @staticmethod
    def diff_statistics(source: np.ndarray, target: np.ndarray) -> dict:
        a = source.astype(np.float64)
        b = target.astype(np.float64)
        scale = _max_value(16 if target.dtype == np.uint16 else 8)
        diff = np.abs(a - b) / scale
        flat = diff.reshape(-1)
        return {
            "max_abs": float(diff.max()),
            "mean_abs": float(diff.mean()),
            "rmse": float(np.sqrt(np.mean(diff ** 2))),
            "nonzero_ratio": float(np.count_nonzero(diff) / flat.size),
            "per_channel_max": [float(x) for x in
                                diff.max(axis=(0, 1))],
        }

    @staticmethod
    def channel_histogram(image: np.ndarray, bins: int = 64) -> dict:
        depth = 16 if image.dtype == np.uint16 else 8
        edges = np.linspace(0, _max_value(depth), bins + 1)
        result = {}
        for ch in range(image.shape[2]):
            hist, _ = np.histogram(image[..., ch], bins=edges)
            result[f"ch{ch}"] = [int(x) for x in hist]
        result["bins"] = bins
        result["depth"] = depth
        return result

    @staticmethod
    def heatmap(source: np.ndarray, target: np.ndarray) -> np.ndarray:
        """逐像素差异热力图（HxW，0..1 浮点）。"""
        a = source.astype(np.float64)
        b = target.astype(np.float64)
        scale = _max_value(16 if target.dtype == np.uint16 else 8)
        return np.clip(np.abs(a - b).mean(axis=2) / scale, 0.0, 1.0)
