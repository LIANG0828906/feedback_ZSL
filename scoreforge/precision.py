"""基于真值的演奏表达精度校验.

按真值清单比对内部模型换算结果：小节期望总时值、力度曲线采样点、
事件绝对时间戳。偏差超出声明容差返回 E_PREC 并指出小节编号与采样点拍位；
真值缺失的小节显式标记为 UNVERIFIABLE 而非静默通过。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .errors import PrecisionError
from .model import Score


@dataclass
class MeasureDeviation:
    part_id: str
    measure: int
    expected: int
    actual: int
    abs_dev: float
    tolerance: float
    ok: bool


@dataclass
class SampleDeviation:
    part_id: str
    measure: int
    beat_ticks: int
    expected: float
    actual: float
    abs_dev: float
    tolerance: float
    ok: bool


@dataclass
class EventDeviation:
    event_id: str
    expected_ms: float
    actual_ms: float
    abs_dev: float
    tolerance_ms: float
    ok: bool


@dataclass
class PrecisionReport:
    measure_reports: list[MeasureDeviation] = field(default_factory=list)
    sample_reports: list[SampleDeviation] = field(default_factory=list)
    event_reports: list[EventDeviation] = field(default_factory=list)
    unverifiable: list[tuple[str, int]] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        return (
            all(r.ok for r in self.measure_reports)
            and all(r.ok for r in self.sample_reports)
            and all(r.ok for r in self.event_reports)
        )

    def raise_if_failed(self) -> None:
        for r in self.measure_reports:
            if not r.ok:
                raise PrecisionError(
                    f"measure total duration deviates: expected {r.expected}, "
                    f"actual {r.actual}, tolerance {r.tolerance}",
                    path=f"parts[{r.part_id}].measures[{r.measure}]",
                    measure=r.measure, expected=r.expected,
                    actual=r.actual, tolerance=r.tolerance,
                )
        for r in self.sample_reports:
            if not r.ok:
                raise PrecisionError(
                    f"dynamics sample deviates: expected {r.expected:.4f}, "
                    f"actual {r.actual:.4f}, tolerance {r.tolerance}",
                    path=f"parts[{r.part_id}].measures[{r.measure}]"
                         f".dynamics@{r.beat_ticks}",
                    measure=r.measure, beat_ticks=r.beat_ticks,
                    expected=r.expected, actual=r.actual, tolerance=r.tolerance,
                )
        for r in self.event_reports:
            if not r.ok:
                raise PrecisionError(
                    f"event timestamp deviates: expected {r.expected_ms:.4f}ms, "
                    f"actual {r.actual_ms:.4f}ms, tolerance {r.tolerance_ms}ms",
                    path=f"events[{r.event_id}].abs_ms",
                    expected=r.expected_ms, actual=r.actual_ms,
                    tolerance=r.tolerance_ms,
                )

    def summary(self) -> str:
        lines = [
            f"measures checked: {len(self.measure_reports)} "
            f"(failed: {sum(1 for r in self.measure_reports if not r.ok)})",
            f"dynamics samples checked: {len(self.sample_reports)} "
            f"(failed: {sum(1 for r in self.sample_reports if not r.ok)})",
            f"event timestamps checked: {len(self.event_reports)} "
            f"(failed: {sum(1 for r in self.event_reports if not r.ok)})",
            f"unverifiable measures (no truth): {len(self.unverifiable)}",
        ]
        return "\n".join(lines)


def verify_truth(score: Score, truth: dict) -> PrecisionReport:
    """比对内部模型与真值清单，返回完整偏差报告（不抛异常）."""
    report = PrecisionReport()
    covered = set()
    for entry in truth.get("measures", []):
        pid, mi = entry["part_id"], entry["measure"]
        covered.add((pid, mi))
        expected = entry["expected_total_ticks"]
        actual = score.measure_total_ticks(pid, mi)
        tol = entry.get("tolerance_ticks", 0)
        dev = abs(actual - expected)
        report.measure_reports.append(MeasureDeviation(
            pid, mi, expected, actual, dev, tol, dev <= tol,
        ))
    for entry in truth.get("dynamics_samples", []):
        pid, mi, beat = entry["part_id"], entry["measure"], entry["beat_ticks"]
        abs_tick = score.measure_start(mi) + beat
        expected = float(entry["expected_value"])
        actual = score.dynamics_value_at(pid, abs_tick)
        tol = float(entry.get("tolerance", 0.0))
        dev = abs(actual - expected)
        report.sample_reports.append(SampleDeviation(
            pid, mi, beat, expected, actual, dev, tol, dev <= tol,
        ))
    for entry in truth.get("event_timestamps", []):
        ev = score.events[entry["event_id"]]
        expected = float(entry["expected_ms"])
        actual = score.event_abs_ms(ev)
        tol = float(entry.get("tolerance_ms", 0.0))
        dev = abs(actual - expected)
        report.event_reports.append(EventDeviation(
            ev.event_id, expected, actual, dev, tol, dev <= tol,
        ))
    for key in score.measure_keys():
        if key not in covered:
            report.unverifiable.append(key)
    return report
