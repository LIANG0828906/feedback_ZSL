"""参数修改后的增量重算与依赖判定。

影响因子（需求 5）：

- ``target_colorspace`` 目标色彩空间
- ``target_peak_nits``  峰值亮度
- ``tone_curve``        色调映射曲线
- ``frac_bits``         定点小数位

另外 target_transfer / mode / rounding / seed / 目标位深同样进入缓存键，
保证任何配置变化都不会错误复用。

每帧缓存键只依赖该帧源内容哈希与完整因子集，因此可以逐帧证明
“未受影响”；无法取得源哈希或配置不可比较时保守重算并记录原因。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .hashing import dict_hash
from .pipeline import FrameResult

AFFECTED_FACTORS = (
    "target_colorspace",
    "target_peak_nits",
    "tone_curve",
    "frac_bits",
)


def factor_fingerprint(config) -> str:
    """配置因子的内容指纹，等价配置必然同指纹。"""
    return dict_hash(config.factor_dict())


def frame_cache_key(source_hash: str, config) -> str:
    return dict_hash(
        {"source": source_hash, "factors": config.factor_dict()}
    )


@dataclass
class CacheEntry:
    key: str
    result: FrameResult
    factor_fingerprint: str


@dataclass
class RecomputePlan:
    affected_frames: list
    reused_frames: list
    changed_factors: list
    conservative_reasons: dict = field(default_factory=dict)

    @property
    def closure_complete(self) -> bool:
        return len(self.affected_frames) + len(self.reused_frames) > 0


class FrameCache:
    """按帧缓存编码结果；依赖闭包 = 受影响帧集合 ∪ 保守重算帧。"""

    def __init__(self):
        self._entries: dict[int, CacheEntry] = {}

    def put(
        self,
        frame_index: int,
        source_hash: str,
        config,
        result: FrameResult,
    ) -> str:
        key = frame_cache_key(source_hash, config)
        self._entries[frame_index] = CacheEntry(
            key=key,
            result=result,
            factor_fingerprint=factor_fingerprint(config),
        )
        return key

    def get(self, frame_index: int) -> CacheEntry | None:
        return self._entries.get(frame_index)

    def plan(
        self,
        source_hashes: list[str],
        old_config,
        new_config,
    ) -> RecomputePlan:
        old_factors = old_config.factor_dict()
        new_factors = new_config.factor_dict()
        changed = [
            name
            for name in AFFECTED_FACTORS
            if old_factors.get(name) != new_factors.get(name)
        ]
        # 其余配置变化（传递函数、模式、舍入、位深等）同样必须触发重算
        changed.extend(
            name
            for name in new_factors
            if name not in AFFECTED_FACTORS
            and old_factors.get(name) != new_factors.get(name)
        )

        affected, reused, reasons = [], [], {}
        new_fp = factor_fingerprint(new_config)
        for index, source_hash in enumerate(source_hashes):
            entry = self._entries.get(index)
            if entry is None:
                affected.append(index)
                reasons[index] = "no_cache_entry"
                continue
            if not source_hash:
                affected.append(index)
                reasons[index] = "missing_source_hash"
                continue
            expected_key = frame_cache_key(source_hash, new_config)
            if entry.key == expected_key and entry.factor_fingerprint == new_fp:
                reused.append(index)
            else:
                affected.append(index)
                reasons[index] = "factor_changed:" + ",".join(
                    sorted(set(changed))
                )
        return RecomputePlan(
            affected_frames=affected,
            reused_frames=reused,
            changed_factors=sorted(set(changed)),
            conservative_reasons=reasons,
        )

    def snapshot(self) -> dict:
        return {
            index: {
                "key": entry.key,
                "factor_fingerprint": entry.factor_fingerprint,
                "source_hash": entry.result.source_hash,
            }
            for index, entry in self._entries.items()
        }

