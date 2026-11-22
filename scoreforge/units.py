"""统一时间单位与速度映射.

全工程只使用一种声明时间单位：tick（每四分音符 TICKS_PER_QUARTER 个）。
绝对物理时间（毫秒）由 TempoMap 统一换算，模型内保留 tick 值以便回溯。
"""

from __future__ import annotations

TICKS_PER_QUARTER = 480
PARAM_VERSION = "1.0"
ALGO_VERSION = "1.0"
DEFAULT_SEED = 42

#: 力度曲线在首个力度事件之前的默认取值
DEFAULT_DYNAMIC = 64.0


def measure_ticks(time_sig) -> int:
    """由拍号 [numerator, denominator] 计算小节的 tick 长度."""
    num, den = int(time_sig[0]), int(time_sig[1])
    quarter_units = TICKS_PER_QUARTER * 4
    if quarter_units % den != 0:
        raise ValueError(f"unsupported time signature denominator: {den}")
    return num * (quarter_units // den)


class TempoMap:
    """分段常速速度映射：[(tick, bpm), ...]，首条必须位于 tick 0."""

    def __init__(self, entries, tpq: int = TICKS_PER_QUARTER):
        entries = [(int(t), float(b)) for t, b in entries]
        entries.sort(key=lambda e: e[0])
        if not entries or entries[0][0] != 0:
            raise ValueError("tempo map must start at tick 0")
        self._entries = entries
        self.tpq = tpq

    @classmethod
    def from_data(cls, data, tpq: int = TICKS_PER_QUARTER) -> "TempoMap":
        return cls([(e["tick"], e["bpm"]) for e in data], tpq=tpq)

    @property
    def entries(self):
        return list(self._entries)

    def _ms_per_tick(self, bpm: float) -> float:
        return 60000.0 / (bpm * self.tpq)

    def ms_at_tick(self, tick: int) -> float:
        """tick -> 绝对毫秒（确定性积分，与参考实现逐段同序累加）."""
        tick = max(0, int(tick))
        total = 0.0
        for i, (start, bpm) in enumerate(self._entries):
            stop = self._entries[i + 1][0] if i + 1 < len(self._entries) else None
            if stop is not None and tick > stop:
                total += (stop - start) * self._ms_per_tick(bpm)
            else:
                total += max(0, tick - start) * self._ms_per_tick(bpm)
                break
        return total

    def tick_at_ms(self, ms: float) -> int:
        """毫秒 -> tick（用于回放定位；逐段反解，保证与 ms_at_tick 互逆）."""
        remaining = max(0.0, float(ms))
        for i, (start, bpm) in enumerate(self._entries):
            stop = self._entries[i + 1][0] if i + 1 < len(self._entries) else None
            mpt = self._ms_per_tick(bpm)
            if stop is None:
                return start + int(round(remaining / mpt))
            seg_ms = (stop - start) * mpt
            if remaining <= seg_ms:
                return start + int(round(remaining / mpt))
            remaining -= seg_ms
        return self._entries[-1][0]
