"""统一定点时间基。

全链路只使用一种整型时间单位: 采样点数 (samples)。
本模块提供与微秒之间的纯整型换算(有理数运算, 不引入浮点误差),
以及时间基版本常量 —— 检查点快照须携带该版本, 不匹配则整体作废。
"""

from __future__ import annotations

from fractions import Fraction

TIME_UNIT = "samples"          # 全链路统一时间单位
TIME_UNIT_VERSION = 1          # 时间单位参数版本
ALGO_VERSION = 1               # 算法版本(检测/推导/排程规则)


def samples_to_us(samples: int, sample_rate: int) -> int:
    """采样点 -> 微秒, 最近取整, 纯整型有理运算。"""
    return int(Fraction(samples * 1_000_000, sample_rate) + Fraction(1, 2))


def us_to_samples(us: int, sample_rate: int) -> int:
    """微秒 -> 采样点, 最近取整, 纯整型有理运算。"""
    return int(Fraction(us * sample_rate, 1_000_000) + Fraction(1, 2))


def samples_to_seconds(samples: int, sample_rate: int) -> Fraction:
    """采样点 -> 秒(精确有理数, 仅用于展示)。"""
    return Fraction(samples, sample_rate)
