"""跨档同片段起止样本一致性校验。

两个档位共享同一源时间轴：对同一源时间区间，两侧切片覆盖的源起止样本
必须一致。边界若因量化刻度不整除而产生偏移，输出受牵连档位、切片清单
与偏移量，并以 ``E_SPAN_MISMATCH`` 定位首个不一致区间。
"""

from __future__ import annotations

from dataclasses import dataclass

from .errors import AuditError, ErrorCode
from .parameters import TierParams
from .slicing import quantized_slice_bounds
from .timebase import SourceTimebase


@dataclass(frozen=True)
class SpanRecord:
    tier: str
    param_version: int
    slice_index: int
    start_tick: int
    end_tick: int
    source_start_sample: int
    source_end_sample: int
    offset_start: int
    offset_end: int

    def to_dict(self) -> dict:
        return {
            "tier": self.tier,
            "param_version": self.param_version,
            "slice_index": self.slice_index,
            "start_tick": self.start_tick,
            "end_tick": self.end_tick,
            "source_start_sample": self.source_start_sample,
            "source_end_sample": self.source_end_sample,
            "offset_start": self.offset_start,
            "offset_end": self.offset_end,
        }


def tier_span_records(
    source: SourceTimebase, tier: str, param_version: int, params: TierParams
) -> list[SpanRecord]:
    """生成单档切片在共享源时基上的起止样本记录。"""
    records: list[SpanRecord] = []
    for index, (start_tick, end_tick) in enumerate(
        quantized_slice_bounds(source, params)
    ):
        start_sample = source.sample_at_tick(start_tick)
        end_sample = source.sample_at_tick(end_tick)
        records.append(
            SpanRecord(
                tier=tier,
                param_version=param_version,
                slice_index=index,
                start_tick=start_tick,
                end_tick=end_tick,
                source_start_sample=start_sample,
                source_end_sample=end_sample,
                offset_start=0,
                offset_end=0,
            )
        )
    return records


@dataclass(frozen=True)
class SpanMismatch:
    first_window_index: int
    window_start_tick: int
    window_end_tick: int
    involved_tiers: tuple[str, str]
    records: tuple[SpanRecord, SpanRecord]

    def to_dict(self) -> dict:
        return {
            "first_mismatch_window": self.first_window_index,
            "window_start_tick": self.window_start_tick,
            "window_end_tick": self.window_end_tick,
            "involved_tiers": list(self.involved_tiers),
            "slices": [record.to_dict() for record in self.records],
        }


def compare_tier_spans(
    source: SourceTimebase,
    tier_a: str,
    version_a: int,
    params_a: TierParams,
    tier_b: str,
    version_b: int,
    params_b: TierParams,
) -> list[SpanMismatch]:
    """按共享源时间窗口比较两档源起止样本，返回全部不一致。

    两档切片数量可能不同，因此以所有切片边界的并集划分共享窗口，逐窗
    定位两侧覆盖样本与相对偏移；首个不一致窗口即 ``E_SPAN_MISMATCH``
    的定位结果。
    """
    records_a = tier_span_records(source, tier_a, version_a, params_a)
    records_b = tier_span_records(source, tier_b, version_b, params_b)
    boundaries = sorted(
        {0, source.duration_ticks}
        | {r.start_tick for r in records_a}
        | {r.end_tick for r in records_a}
        | {r.start_tick for r in records_b}
        | {r.end_tick for r in records_b}
    )
    mismatches: list[SpanMismatch] = []
    for index in range(len(boundaries) - 1):
        win_start, win_end = boundaries[index], boundaries[index + 1]
        ra = _covering_record(records_a, win_start, win_end)
        rb = _covering_record(records_b, win_start, win_end)
        win_start_sample = source.sample_at_tick(win_start)
        win_end_sample = source.sample_at_tick(win_end)
        marked_a = _mark(
            ra, tier_a, version_a, index, win_start, win_end,
            win_start_sample, win_end_sample,
        )
        marked_b = _mark(
            rb, tier_b, version_b, index, win_start, win_end,
            win_start_sample, win_end_sample,
        )
        if (
            marked_a.source_start_sample != marked_b.source_start_sample
            or marked_a.source_end_sample != marked_b.source_end_sample
            or marked_a.offset_start != marked_b.offset_start
            or marked_a.offset_end != marked_b.offset_end
        ):
            mismatches.append(
                SpanMismatch(
                    first_window_index=index,
                    window_start_tick=win_start,
                    window_end_tick=win_end,
                    involved_tiers=(tier_a, tier_b),
                    records=(marked_a, marked_b),
                )
            )
    return mismatches


def _covering_record(
    records: list[SpanRecord], start_tick: int, end_tick: int
) -> SpanRecord | None:
    for record in records:
        if record.start_tick <= start_tick and record.end_tick >= end_tick:
            return record
    return None


def _mark(
    record: SpanRecord | None,
    tier: str,
    version: int,
    index: int,
    win_start: int,
    win_end: int,
    win_start_sample: int,
    win_end_sample: int,
) -> SpanRecord:
    if record is None:
        return SpanRecord(tier, version, index, win_start, win_end, -1, -1, 0, 0)
    return SpanRecord(
        record.tier,
        record.param_version,
        record.slice_index,
        record.start_tick,
        record.end_tick,
        record.source_start_sample,
        record.source_end_sample,
        record.source_start_sample - win_start_sample,
        record.source_end_sample - win_end_sample,
    )


def assert_spans_consistent(
    source: SourceTimebase,
    tier_a: str,
    version_a: int,
    params_a: TierParams,
    tier_b: str,
    version_b: int,
    params_b: TierParams,
) -> None:
    """一致时静默；首个不一致区间抛 ``E_SPAN_MISMATCH``。"""
    mismatches = compare_tier_spans(
        source,
        tier_a, version_a, params_a,
        tier_b, version_b, params_b,
    )
    if mismatches:
        first = mismatches[0]
        raise AuditError(
            ErrorCode.E_SPAN_MISMATCH,
            (
                f"档位 {tier_a} 与 {tier_b} 在窗口 {first.first_window_index} "
                "起止样本不一致"
            ),
            (
                "span",
                f"window[{first.first_window_index}]",
            ),
            {
                "first_mismatch": first.to_dict(),
                "total_mismatches": len(mismatches),
            },
        )
