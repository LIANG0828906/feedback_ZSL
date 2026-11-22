"""内部领域模型与演奏事件时间轴.

一致性约束:
  1. 表达事件通过 (anchor_note_id, beat_offset_ticks) 绑定到具体音符;
     音符时值修改后, 下游音符 onset 自动迁移, 事件绝对拍位随之迁移。
  2. 删除 / 替换音符后由 find_dangling_events 检测悬空事件 (E_DANGLING)。
  3. 绝对时间统一由 TempoMap 换算, 模型内始终保留 tick 值以便回溯。
"""

from __future__ import annotations

import copy

import numpy as np

from .errors import RefError
from .units import DEFAULT_DYNAMIC, TICKS_PER_QUARTER, TempoMap, measure_ticks
from .validation import validate_project


class Note:
    __slots__ = ("note_id", "pitch", "duration_ticks")

    def __init__(self, note_id: str, pitch: int, duration_ticks: int):
        self.note_id = note_id
        self.pitch = pitch
        self.duration_ticks = duration_ticks

    def to_dict(self) -> dict:
        return {
            "note_id": self.note_id,
            "pitch": self.pitch,
            "duration_ticks": self.duration_ticks,
        }


class ExpressionEvent:
    __slots__ = ("event_id", "type", "part_id", "measure", "anchor_note_id",
                 "beat_offset_ticks", "value")

    def __init__(self, event_id, type, part_id, measure, anchor_note_id,
                 beat_offset_ticks, value):
        self.event_id = event_id
        self.type = type
        self.part_id = part_id
        self.measure = measure
        self.anchor_note_id = anchor_note_id
        self.beat_offset_ticks = beat_offset_ticks
        self.value = value

    def to_dict(self) -> dict:
        return {
            "event_id": self.event_id,
            "type": self.type,
            "part_id": self.part_id,
            "measure": self.measure,
            "anchor_note_id": self.anchor_note_id,
            "beat_offset_ticks": self.beat_offset_ticks,
            "value": self.value,
        }


class Measure:
    __slots__ = ("index", "notes")

    def __init__(self, index: int, notes: list[Note]):
        self.index = index
        self.notes = notes

    @property
    def total_ticks(self) -> int:
        return sum(n.duration_ticks for n in self.notes)

    def note(self, note_id: str) -> Note | None:
        for n in self.notes:
            if n.note_id == note_id:
                return n
        return None

    def onset_of(self, note_id: str) -> int | None:
        acc = 0
        for n in self.notes:
            if n.note_id == note_id:
                return acc
            acc += n.duration_ticks
        return None

    def to_dict(self) -> dict:
        return {"index": self.index, "notes": [n.to_dict() for n in self.notes]}


class Part:
    __slots__ = ("part_id", "name", "measures")

    def __init__(self, part_id: str, name: str, measures: list[Measure]):
        self.part_id = part_id
        self.name = name
        self.measures = measures

    def to_dict(self) -> dict:
        return {
            "part_id": self.part_id,
            "name": self.name,
            "measures": [m.to_dict() for m in self.measures],
        }


class Score:
    """已提交内部模型。编辑会话的工作副本是它的深拷贝."""

    def __init__(self, data: dict, validate: bool = True):
        if validate:
            validate_project(data)
        self.project_id = data["project_id"]
        self.param_version = data["param_version"]
        self.algo_version = data["algo_version"]
        self.tpq = data["units"]["ticks_per_quarter"]
        self.tempo_map = TempoMap.from_data(data["tempo_map"], tpq=self.tpq)
        self.measure_structure = copy.deepcopy(data["measure_structure"])
        self.parts: list[Part] = []
        for p in data["parts"]:
            measures = [
                Measure(m["index"], [Note(n["note_id"], n["pitch"], n["duration_ticks"])
                                     for n in m["notes"]])
                for m in p["measures"]
            ]
            self.parts.append(Part(p["part_id"], p.get("name", p["part_id"]), measures))
        self.events: dict[str, ExpressionEvent] = {}
        for ev in data["expression_events"]:
            self.events[ev["event_id"]] = ExpressionEvent(
                ev["event_id"], ev["type"], ev["part_id"], ev["measure"],
                ev["anchor_note_id"], ev["beat_offset_ticks"], ev["value"],
            )
        self._measure_starts = self._compute_measure_starts()
        self.rebuild_index()

    def _compute_measure_starts(self) -> list[int]:
        starts = []
        acc = 0
        for entry in self.measure_structure:
            starts.append(acc)
            acc += measure_ticks(entry["time_sig"])
        return starts

    @property
    def total_ticks(self) -> int:
        return self._measure_starts[-1] + self.measure_ticks(len(self._measure_starts) - 1)

    def measure_ticks(self, index: int) -> int:
        return measure_ticks(self.measure_structure[index]["time_sig"])

    def measure_start(self, index: int) -> int:
        return self._measure_starts[index]

    def part(self, part_id: str) -> Part:
        for p in self.parts:
            if p.part_id == part_id:
                return p
        raise RefError(f"unknown part '{part_id}'", path=f"parts[{part_id}]")

    @property
    def part_ids(self) -> list[str]:
        return [p.part_id for p in self.parts]

    def measure_keys(self) -> list[tuple[str, int]]:
        return [(p.part_id, m.index) for p in self.parts for m in p.measures]

    def rebuild_index(self) -> None:
        """重建 音符 -> 事件 与 小节 -> 事件 依赖索引."""
        self.events_by_note: dict[str, list[str]] = {}
        self.events_by_measure: dict[tuple[str, int], list[str]] = {}
        for ev in self.events.values():
            self.events_by_note.setdefault(ev.anchor_note_id, []).append(ev.event_id)
            key = (ev.part_id, ev.measure)
            self.events_by_measure.setdefault(key, []).append(ev.event_id)
        for ids in self.events_by_note.values():
            ids.sort()
        for ids in self.events_by_measure.values():
            ids.sort()

    def events_of_measure(self, part_id: str, measure: int) -> list[ExpressionEvent]:
        return [self.events[i] for i in self.events_by_measure.get((part_id, measure), [])]

    def events_of_note(self, note_id: str) -> list[ExpressionEvent]:
        return [self.events[i] for i in self.events_by_note.get(note_id, [])]

    def find_dangling_events(self, part_id: str, measure: int) -> list[str]:
        """返回指定小节内锚点已失效的事件 id 列表."""
        part = self.part(part_id)
        m = part.measures[measure]
        dangling = []
        for ev in self.events_of_measure(part_id, measure):
            if m.note(ev.anchor_note_id) is None:
                dangling.append(ev.event_id)
        return dangling

    def note_abs_onset(self, part_id: str, measure: int, note_id: str) -> int | None:
        m = self.part(part_id).measures[measure]
        onset = m.onset_of(note_id)
        if onset is None:
            return None
        return self.measure_start(measure) + onset

    def event_abs_tick(self, ev: ExpressionEvent) -> int:
        onset = self.note_abs_onset(ev.part_id, ev.measure, ev.anchor_note_id)
        if onset is None:
            raise RefError(
                f"anchor '{ev.anchor_note_id}' not found",
                path=f"events[{ev.event_id}].anchor_note_id",
            )
        return onset + ev.beat_offset_ticks

    def event_abs_ms(self, ev: ExpressionEvent) -> float:
        return self.tempo_map.ms_at_tick(self.event_abs_tick(ev))

    def measure_total_ticks(self, part_id: str, measure: int) -> int:
        return self.part(part_id).measures[measure].total_ticks

    def dynamics_curve(self, part_id: str) -> tuple[np.ndarray, np.ndarray]:
        """返回该声部力度事件的 (abs_ticks, values) 升序数组."""
        from .generator import collapse_dynamics

        points = [
            (self.event_abs_tick(ev), float(ev.value))
            for ev in self.events.values()
            if ev.part_id == part_id and ev.type == "dynamics"
        ]
        points.sort(key=lambda x: x[0])
        points = collapse_dynamics(points)
        if not points:
            return np.array([], dtype=float), np.array([], dtype=float)
        ticks, values = zip(*points)
        return np.array(ticks, dtype=float), np.array(values, dtype=float)

    def dynamics_value_at(self, part_id: str, abs_tick: float) -> float:
        """分段线性插值；首个事件之前取 DEFAULT_DYNAMIC，末尾之后保持末值."""
        ticks, values = self.dynamics_curve(part_id)
        if ticks.size == 0:
            return DEFAULT_DYNAMIC
        return float(
            np.interp(abs_tick, ticks, values, left=DEFAULT_DYNAMIC, right=values[-1])
        )

    def measure_fragment(self, part_id: str, measure: int) -> dict:
        """导出小节模型片段 + 表达事件快照（检查点与提交共用格式）."""
        m = self.part(part_id).measures[measure]
        return {
            "part_id": part_id,
            "measure_index": measure,
            "measure": m.to_dict(),
            "events": [ev.to_dict() for ev in self.events_of_measure(part_id, measure)],
        }

    def replace_measure(self, fragment: dict) -> None:
        """原子地用片段替换小节内容与事件，并刷新依赖索引."""
        part_id = fragment["part_id"]
        mi = fragment["measure_index"]
        new_measure = Measure(
            fragment["measure"]["index"],
            [Note(n["note_id"], n["pitch"], n["duration_ticks"])
             for n in fragment["measure"]["notes"]],
        )
        new_events = {}
        for ev in fragment["events"]:
            new_events[ev["event_id"]] = ExpressionEvent(
                ev["event_id"], ev["type"], part_id, mi,
                ev["anchor_note_id"], ev["beat_offset_ticks"], ev["value"],
            )
        part = self.part(part_id)
        old_event_ids = set(self.events_by_measure.get((part_id, mi), []))
        part.measures[mi] = new_measure
        for eid in old_event_ids:
            self.events.pop(eid, None)
        self.events.update(new_events)
        self.rebuild_index()

    def clone(self) -> "Score":
        return Score(self.to_data(), validate=False)

    def to_data(self) -> dict:
        return {
            "project_id": self.project_id,
            "param_version": self.param_version,
            "algo_version": self.algo_version,
            "units": {"ticks_per_quarter": self.tpq},
            "tempo_map": [{"tick": t, "bpm": b} for t, b in self.tempo_map.entries],
            "measure_structure": copy.deepcopy(self.measure_structure),
            "parts": [p.to_dict() for p in self.parts],
            "expression_events": [self.events[k].to_dict() for k in sorted(self.events)],
        }

    def export_timeline(self) -> list[dict]:
        """导出（只读已提交模型）: 全部事件按绝对时间排序的时间轴."""
        entries = [
            {
                "event_id": ev.event_id,
                "type": ev.type,
                "part_id": ev.part_id,
                "abs_tick": self.event_abs_tick(ev),
                "abs_ms": self.event_abs_ms(ev),
                "value": ev.value,
            }
            for ev in self.events.values()
        ]
        entries.sort(key=lambda e: (e["abs_tick"], e["event_id"]))
        return entries


def load_project(data: dict) -> tuple[Score, dict]:
    """加载并校验工程，返回 (内部模型, 真值清单)."""
    validate_project(data)
    return Score(data, validate=False), data["truth"]
