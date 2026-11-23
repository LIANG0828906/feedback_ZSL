"""全局响度归一与受控增益分配。

约束：

* 节目段必须按时间轴顺序提交，第 k 段的增益只依赖已提交的 0..k 段统计
* 归一增益 = 目标响度 - 当前已提交整体积分响度
* 施加增益后若真峰超过限幅上限，执行二次线性增益修正（非裁剪），
  修正量受限并完整记录
"""
from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from ..core.config import NormalizeConfig
from ..core.errors import ParameterError, StateError
from ..dsp.kweighting import true_peak_db
from ..dsp.loudness import measure_loudness, FrameConfig


@dataclass
class SegmentGainRecord:
    segment_index: int
    input_lufs: float
    primary_gain_db: float
    secondary_correction_db: float
    total_gain_db: float
    pre_tp_db: float
    post_tp_db: float
    output_lufs: float


@dataclass
class NormalizerState:
    """可序列化的归一器全局状态（进入检查点）。"""

    submitted_count: int = 0
    accumulated_energy: float = 0.0
    accumulated_weight: int = 0
    records: list[dict] = field(default_factory=list)


class GlobalNormalizer:
    """按提交顺序维护全局能量统计与增益分配。"""

    def __init__(self, cfg: NormalizeConfig, frame_cfg: FrameConfig,
                 ceiling_db: float):
        cfg.validate()
        self.cfg = cfg
        self.frame_cfg = frame_cfg
        self.ceiling_db = ceiling_db
        self._state = NormalizerState()

    @property
    def state(self) -> NormalizerState:
        return self._state

    def load_state(self, state: NormalizerState) -> None:
        self._state = state

    def submit_restored(self, segment_index: int,
                        record: SegmentGainRecord) -> None:
        """断点恢复：按检查点中的增益记录重建顺序与追溯链。"""
        if segment_index != self._state.submitted_count:
            raise StateError(
                f"恢复段序乱序：期望 {self._state.submitted_count}，"
                f"实际 {segment_index}", field="segment_index")
        self._state.records.append(record.__dict__.copy())
        self._state.submitted_count += 1

    @property
    def submitted_count(self) -> int:
        return self._state.submitted_count

    def current_integrated_lufs(self) -> float:
        if self._state.accumulated_weight == 0:
            return -np.inf
        return -0.691 + 10.0 * np.log10(
            self._state.accumulated_energy / self._state.accumulated_weight)

    def _frame_mean_energy(self, x: np.ndarray, fs: int) -> float:
        result = measure_loudness(x, fs, self.frame_cfg)
        # 重新取门限后能量：直接复用测量内部口径
        frame_length = int(round(self.frame_cfg.frame_seconds * fs))
        hop = int(round(self.frame_cfg.hop_seconds * fs))
        from ..dsp.loudness import frame_energy, STEREO_WEIGHT
        from ..dsp.kweighting import apply_k_weighting
        weighted = apply_k_weighting(x, fs)
        energies = frame_energy(weighted, frame_length, hop)
        if x.ndim == 2:
            energies = energies * STEREO_WEIGHT
        active = result.gate_active
        if not np.any(active):
            return 0.0
        return float(np.mean(energies[active]))

    def submit(self, segment_index: int, x: np.ndarray, fs: int
               ) -> tuple[np.ndarray, SegmentGainRecord]:
        """提交一段已压缩/限幅的音频，返回归一后音频与增益记录。

        必须严格按 0,1,2... 顺序提交，否则 E_STATE。
        """
        if segment_index != self._state.submitted_count:
            raise StateError(
                f"归一提交乱序：期望段 {self._state.submitted_count}，"
                f"实际段 {segment_index}（前置段响度尚未提交）",
                field="segment_index")
        if x.ndim not in (1, 2) or (x.ndim == 2 and x.shape[1] != 2):
            raise ParameterError("仅支持单声道或立体声输入",
                                 field="channels")

        measured = measure_loudness(x, fs, self.frame_cfg)
        pre_tp = measured.true_peak_db
        seg_energy = self._frame_mean_energy(x, fs)
        seg_weight = max(measured.gated_frame_count, 1)

        # 一次增益：把本段积分响度拉到目标响度。提交顺序仍是全局约束
        # （后段必须等待前置段提交），累计统计用于整体验收与全程追溯。
        if not np.isfinite(measured.integrated_lufs):
            primary_db = 0.0
        else:
            primary_db = self.cfg.target_lufs - measured.integrated_lufs
        primary_db = float(np.clip(primary_db, -24.0, 24.0))

        out = x * (10.0 ** (primary_db / 20.0))

        # 二次修正：归一后真峰超限则受控衰减（不裁剪）
        post_tp = true_peak_db(out, self.frame_cfg.true_peak_oversample)
        secondary_db = 0.0
        if np.isfinite(post_tp) and post_tp > self.ceiling_db:
            # 必须衰减到上限以内；max_correction_db 仅作为追溯告警阈值，
            # 不允许因截断而违反真峰硬约束（不得用裁剪替代受控增益）
            secondary_db = self.ceiling_db - post_tp - 1e-6
            out = out * (10.0 ** (secondary_db / 20.0))
            post_tp = true_peak_db(
                out, self.frame_cfg.true_peak_oversample)

        total_db = primary_db + secondary_db
        output_lufs = measure_loudness(out, fs, self.frame_cfg).integrated_lufs

        # 更新全局累计（能量按门限帧数加权）
        gain_linear_sq = 10.0 ** (total_db / 10.0)
        self._state.accumulated_energy += seg_energy * gain_linear_sq * seg_weight
        self._state.accumulated_weight += seg_weight

        record = SegmentGainRecord(
            segment_index=segment_index,
            input_lufs=measured.integrated_lufs,
            primary_gain_db=primary_db,
            secondary_correction_db=secondary_db,
            total_gain_db=total_db,
            pre_tp_db=pre_tp,
            post_tp_db=post_tp,
            output_lufs=output_lufs,
        )
        self._state.records.append(record.__dict__.copy())
        self._state.submitted_count += 1
        return out, record

    def verify_target(self) -> tuple[bool, float]:
        """整体积分响度是否落在目标容差内。"""
        current = self.current_integrated_lufs()
        if not np.isfinite(current):
            return False, current
        ok = abs(current - self.cfg.target_lufs) <= self.cfg.tolerance_lu
        return ok, current
