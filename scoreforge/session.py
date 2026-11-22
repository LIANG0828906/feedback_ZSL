"""按小节推进的编辑会话与提交边界.

规则:
  - 用户编辑只作用于工作副本 (working copy), 未提交更改不影响导出与回放;
  - 提交按小节批次原子写回内部模型并刷新依赖索引;
  - 提交失败的批次整体回滚, 已提交模型保持提交前状态;
  - 已提交小节被冻结, 后续编辑 / 重复提交返回 E_BATCH。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .errors import BatchError, DanglingEventError, DurationError, RefError, ScoreError
from .hashing import content_hash
from .model import Score
from .units import ALGO_VERSION, PARAM_VERSION
from .validation import EVENT_TYPES


@dataclass
class PreparedCommit:
    """提交准备阶段的产物（纯数据，可在工作线程中计算）."""

    part_id: str
    measure_index: int
    fragment: dict
    fragment_hash: str


@dataclass
class CommitRecord:
    part_id: str
    measure_index: int
    seq: int
    content_hash: str
    fragment: dict = field(repr=False, default=None)


class EditSession:
    def __init__(self, score: Score, store=None):
        self.score = score
        self.working = score.clone()
        self.store = store
        self.committed: set[tuple[str, int]] = set()
        self.commit_log: list[CommitRecord] = []
        self._staged: set[tuple[str, int]] = set()

    def _check_editable(self, part_id: str, measure: int) -> None:
        if (part_id, measure) in self.committed:
            raise BatchError(
                f"measure ({part_id}, {measure}) is frozen after commit",
                path=f"parts[{part_id}].measures[{measure}]",
            )

    def set_note_duration(self, part_id: str, measure: int, note_id: str,
                          duration_ticks: int) -> None:
        self._check_editable(part_id, measure)
        m = self.working.part(part_id).measures[measure]
        note = m.note(note_id)
        if note is None:
            raise RefError(f"note '{note_id}' not found",
                           path=f"parts[{part_id}].measures[{measure}]")
        note.duration_ticks = duration_ticks
        self._staged.add((part_id, measure))

    def set_event_value(self, event_id: str, value) -> None:
        ev = self.working.events.get(event_id)
        if ev is None:
            raise RefError(f"event '{event_id}' not found", path=f"events[{event_id}]")
        self._check_editable(ev.part_id, ev.measure)
        ev.value = value
        self._staged.add((ev.part_id, ev.measure))

    def add_event(self, event_dict: dict) -> None:
        for key in ("event_id", "type", "part_id", "measure",
                    "anchor_note_id", "beat_offset_ticks", "value"):
            if key not in event_dict:
                raise BatchError(f"new event missing field '{key}'", path="events[new]")
        if event_dict["type"] not in EVENT_TYPES:
            raise BatchError(f"unknown event type '{event_dict['type']}'")
        part_id, measure = event_dict["part_id"], event_dict["measure"]
        self._check_editable(part_id, measure)
        if event_dict["event_id"] in self.working.events:
            raise BatchError(f"duplicate event_id '{event_dict['event_id']}'")
        from .model import ExpressionEvent
        self.working.events[event_dict["event_id"]] = ExpressionEvent(
            event_dict["event_id"], event_dict["type"], part_id, measure,
            event_dict["anchor_note_id"], event_dict["beat_offset_ticks"],
            event_dict["value"],
        )
        self.working.rebuild_index()
        self._staged.add((part_id, measure))

    def delete_note(self, part_id: str, measure: int, note_id: str,
                    reanchor: dict | None = None, drop_events: list | None = None) -> None:
        """删除音符。锚定其上的事件须显式 reanchor 或 drop，否则提交时 E_DANGLING."""
        self._check_editable(part_id, measure)
        m = self.working.part(part_id).measures[measure]
        if m.note(note_id) is None:
            raise RefError(f"note '{note_id}' not found",
                           path=f"parts[{part_id}].measures[{measure}]")
        reanchor = reanchor or {}
        drop_events = set(drop_events or [])
        m.notes = [n for n in m.notes if n.note_id != note_id]
        for ev in self.working.events_of_note(note_id):
            if ev.event_id in reanchor:
                ev.anchor_note_id = reanchor[ev.event_id]
            elif ev.event_id in drop_events:
                self.working.events.pop(ev.event_id, None)
        self.working.rebuild_index()
        self._staged.add((part_id, measure))

    def prepare_commit(self, part_id: str, measure: int) -> PreparedCommit:
        """准备阶段：只读工作副本，可在工作线程中并行执行."""
        key = (part_id, measure)
        if key in self.committed:
            raise BatchError(
                f"measure ({part_id}, {measure}) already committed",
                path=f"parts[{part_id}].measures[{measure}]",
            )
        if key not in self._staged:
            raise BatchError(
                f"no staged edits for measure ({part_id}, {measure})",
                path=f"parts[{part_id}].measures[{measure}]",
            )
        m = self.working.part(part_id).measures[measure]
        for ni, note in enumerate(m.notes):
            if not isinstance(note.duration_ticks, int) or note.duration_ticks <= 0:
                raise DurationError(
                    f"duration_ticks must be a positive integer, got {note.duration_ticks!r}",
                    path=f"parts[{part_id}].measures[{measure}].notes[{ni}].duration_ticks",
                )
        m_ticks = self.working.measure_ticks(measure)
        if m.total_ticks > m_ticks:
            raise DurationError(
                f"notes total {m.total_ticks} ticks exceeds measure length {m_ticks}",
                path=f"parts[{part_id}].measures[{measure}]",
            )
        dangling = self.working.find_dangling_events(part_id, measure)
        if dangling:
            raise DanglingEventError(
                f"events left dangling after edit: {dangling}",
                path=f"parts[{part_id}].measures[{measure}]",
                event_id=dangling[0],
            )
        for ev in self.working.events_of_measure(part_id, measure):
            onset = m.onset_of(ev.anchor_note_id)
            if ev.beat_offset_ticks < 0 or onset + ev.beat_offset_ticks >= m_ticks:
                raise RefError(
                    f"event '{ev.event_id}' beat position crosses measure boundary",
                    path=f"events[{ev.event_id}].beat_offset_ticks",
                )
        fragment = self.working.measure_fragment(part_id, measure)
        return PreparedCommit(part_id, measure, fragment, content_hash(fragment))

    def apply_commit(self, prepared: PreparedCommit) -> CommitRecord:
        """应用阶段：原子写回内部模型、刷新索引、串行写检查点（主线程顺序执行）."""
        key = (prepared.part_id, prepared.measure_index)
        try:
            self.score.replace_measure(prepared.fragment)
        except ScoreError:
            raise
        except Exception as exc:
            raise BatchError(f"commit apply failed: {exc}",
                             path=f"parts[{key[0]}].measures[{key[1]}]") from exc
        self.committed.add(key)
        self._staged.discard(key)
        record = CommitRecord(
            prepared.part_id, prepared.measure_index,
            seq=len(self.commit_log), content_hash=prepared.fragment_hash,
            fragment=prepared.fragment,
        )
        self.commit_log.append(record)
        if self.store is not None:
            self.store.write_fragment({
                "seq": record.seq,
                "part_id": prepared.part_id,
                "measure_index": prepared.measure_index,
                "fragment": prepared.fragment,
                "content_hash": prepared.fragment_hash,
                "param_version": PARAM_VERSION,
                "algo_version": ALGO_VERSION,
            })
        return record

    def commit_measure(self, part_id: str, measure: int) -> CommitRecord:
        """串行提交：准备 + 应用。失败时内部模型保持提交前状态."""
        prepared = self.prepare_commit(part_id, measure)
        return self.apply_commit(prepared)

    def pending_keys(self) -> list[tuple[str, int]]:
        return [k for k in self.score.measure_keys() if k not in self.committed]

    @property
    def next_seq(self) -> int:
        return len(self.commit_log)
