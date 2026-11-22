"""配置参数编辑、工作副本、原子提交与依赖失效。

编辑只作用于工作副本；``commit`` 按批次原子写回内部模型，
任一参数非法则整批回滚、模型保持提交前状态。
提交成功后按参数 -> 阶段依赖，失效相关像素批的下游缓存。
"""

from __future__ import annotations

import copy
from dataclasses import dataclass

import numpy as np

from .errors import CwbError, E_PARA
from .model import (
    INTENTS,
    TRC,
    TransformConfig,
)
from .pipeline import STAGES, ConversionPipeline


# 参数编辑影响的起始阶段（下游全部失效）
DEPENDENCY = {
    "trc_table": "S1_trc_invert",
    "trc_gamma": "S1_trc_invert",
    "matrix": "S2_to_pcs",
    "lut3d": "S2_to_pcs",
    "white_point": "S3_adapt_intent",
    "intent": "S3_adapt_intent",
    "output_depth": "S5_output_encode",
}


@dataclass
class Invalidation:
    batch_ids: list[int]
    stage: str
    reasons: list[str]


class ProfileEditor:
    """持有内部模型工作副本，收集编辑并原子提交。"""

    def __init__(self, config: TransformConfig, pipeline: ConversionPipeline):
        self._config = config
        self._pipeline = pipeline
        self._working_source = config.source.clone()
        self._working_target = config.target.clone()
        self._working_intent = config.intent
        self._working_depth = config.output_depth
        self.param_version = 1
        # 每次提交的失效记录（供失效范围查询）
        self.last_invalidation: Invalidation | None = None
        self.history: list[Invalidation] = []

    # ---- 工作副本编辑（不触碰已提交模型） ----
    def edit_trc_table(self, samples: list[float],
                       side: str = "source") -> None:
        if len(samples) < 2:
            raise CwbError(E_PARA, "表格采样点至少需要 2 个",
                           tag="trc_table")
        if any(v < 0 or v > 1 for v in samples):
            raise CwbError(E_PARA, "表格采样值必须落在 [0,1]",
                           tag="trc_table")
        if samples[0] != 0.0 or samples[-1] != 1.0:
            raise CwbError(E_PARA, "表格首点须为 0、末点须为 1",
                           tag="trc_table")
        if any(b < a for a, b in zip(samples, samples[1:])):
            raise CwbError(E_PARA, "表格采样必须单调非减",
                           tag="trc_table")
        profile = self._profile(side)
        profile.trc = TRC(kind="table", table=tuple(float(v) for v in samples))

    def edit_trc_gamma(self, gamma: float, side: str = "source") -> None:
        if not isinstance(gamma, (int, float)) or not 0.2 <= gamma <= 5.0:
            raise CwbError(E_PARA,
                           f"gamma={gamma} 越界 [0.2,5.0]", tag="trc_gamma")
        profile = self._profile(side)
        profile.trc = TRC(kind="parametric", curve="gamma",
                          gamma=float(gamma))

    def edit_matrix(self, matrix9: list[float], side: str = "source") -> None:
        if len(matrix9) != 9:
            raise CwbError(E_PARA, "矩阵必须是 9 个系数", tag="matrix")
        if any(not np.isfinite(v) or abs(v) > 4.0 for v in matrix9):
            raise CwbError(E_PARA, "矩阵系数越界或非有限", tag="matrix")
        self._profile(side).matrix = tuple(float(v) for v in matrix9)

    def edit_white_point(self, wp3: list[float], side: str = "source") -> None:
        if len(wp3) != 3 or any(not np.isfinite(v) or abs(v) > 4.0
                                for v in wp3):
            raise CwbError(E_PARA, "白点必须是 3 个有限值且 |x|<=4",
                           tag="white_point")
        self._profile(side).white_point = tuple(float(v) for v in wp3)

    def edit_intent(self, intent: str) -> None:
        if intent not in INTENTS:
            raise CwbError(E_PARA, f"渲染意图 {intent!r} 不受支持",
                           tag="intent")
        self._working_intent = intent

    def edit_output_depth(self, depth: int) -> None:
        if depth not in (8, 16):
            raise CwbError(E_PARA, "输出位深只允许 8 或 16", tag="depth")
        self._working_depth = depth

    def _profile(self, side: str):
        if side == "source":
            return self._working_source
        if side == "target":
            return self._working_target
        raise CwbError(E_PARA, f"未知配置侧 {side}", tag="side")

    # ---- 原子提交 ----
    def commit(self, affected_batches: list[int] | None = None
               ) -> Invalidation:
        # 先在暂存区做最终校验，任何异常都不修改已提交模型
        snapshot = self._snapshot_committed()
        try:
            self._validate_working()
        except CwbError:
            self._restore_committed(snapshot)
            raise

        changed = self._diff_changed(snapshot)
        if not changed:
            inv = Invalidation([], "S1_trc_invert", [])
            self.last_invalidation = inv
            return inv

        # 校验通过，原子写回
        self._config.source = self._working_source
        self._config.target = self._working_target
        self._config.intent = self._working_intent
        self._config.output_depth = self._working_depth
        self.param_version += 1

        # 计算失效：取受影响参数中最靠前的阶段
        earliest = min(STAGES.index(DEPENDENCY[key]) for key in changed)
        stage = STAGES[earliest]
        batch_ids = (affected_batches if affected_batches is not None
                     else [b.batch_id for b in self._pipeline.batches])
        self._pipeline.invalidate(batch_ids, stage)
        inv = Invalidation(batch_ids=batch_ids, stage=stage,
                           reasons=[f"{k}->{DEPENDENCY[k]}" for k in changed])
        self.last_invalidation = inv
        self.history.append(inv)
        # 提交后工作副本与已提交模型重新对齐
        self._working_source = self._config.source.clone()
        self._working_target = self._config.target.clone()
        return inv

    def _validate_working(self) -> None:
        for profile in (self._working_source, self._working_target):
            np.linalg.inv(np.asarray(profile.matrix,
                                     dtype=np.float64).reshape(3, 3))
            if profile.trc.kind == "table":
                self.edit_trc_table(list(profile.trc.table))
        if self._working_intent not in INTENTS:
            raise CwbError(E_PARA, "意图非法", tag="intent")

    def _snapshot_committed(self) -> dict:
        return {
            "source": copy.deepcopy(self._config.source),
            "target": copy.deepcopy(self._config.target),
            "intent": self._config.intent,
            "depth": self._config.output_depth,
        }

    def _restore_committed(self, snap: dict) -> None:
        self._working_source = snap["source"].clone()
        self._working_target = snap["target"].clone()
        self._working_intent = snap["intent"]
        self._working_depth = snap["depth"]

    def _diff_changed(self, snap: dict) -> list[str]:
        changed: list[str] = []
        if self._working_source.trc != snap["source"].trc or \
                self._working_target.trc != snap["target"].trc:
            kind = ("trc_table"
                    if self._working_source.trc.kind == "table"
                    or self._working_target.trc.kind == "table"
                    else "trc_gamma")
            changed.append(kind)
        if self._working_source.matrix != snap["source"].matrix or \
                self._working_target.matrix != snap["target"].matrix:
            changed.append("matrix")
        if self._working_source.lut3d != snap["source"].lut3d:
            changed.append("lut3d")
        if self._working_source.white_point != snap["source"].white_point or \
                self._working_target.white_point != snap["target"].white_point:
            changed.append("white_point")
        if self._working_intent != snap["intent"]:
            changed.append("intent")
        if self._working_depth != snap["depth"]:
            changed.append("output_depth")
        return changed

    def query_invalidation(self) -> dict:
        """失效范围查询：受影响像素批编号与原因。"""
        inv = self.last_invalidation
        if inv is None:
            return {"batch_ids": [], "stage": None, "reasons": []}
        return {"batch_ids": list(inv.batch_ids), "stage": inv.stage,
                "reasons": list(inv.reasons)}
