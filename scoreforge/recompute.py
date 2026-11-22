"""三路径一致性与自动比对.

三条路径:
  A. 实时编辑  —— EditSession 逐操作推进并提交;
  B. 断点恢复  —— 提交若干小节后模拟强杀, 从检查点恢复后续跑;
  C. 全量重算  —— 从源样本一次性应用同一编辑序列。
compare_scores 定位首个不一致对象路径与字段名, 不一致返回 E_DIVERGE。
"""

from __future__ import annotations

import math

from .errors import DivergenceError
from .model import Score

FLOAT_ABS_TOL = 1e-9


def make_edit_script(project_data: dict) -> list[dict]:
    """由工程内容确定性地派生编辑序列（同一工程永远得到同一序列）。

    每个小节：交换前两个音符时值（保持小节总时值不变，驱动下游锚点迁移），
    并将该小节首个力度事件取值 +5（不超过 127），随后提交该小节。
    若交换会把任一锚定事件推出小节边界，则该小节跳过交换（确定性规则）。
    """
    script = []
    dyn_by_measure: dict[tuple[str, int], str] = {}
    for ev in project_data["expression_events"]:
        if ev["type"] == "dynamics":
            dyn_by_measure.setdefault((ev["part_id"], ev["measure"]), ev["event_id"])
    from .units import measure_ticks as _m_ticks
    structure = project_data["measure_structure"]
    for part in project_data["parts"]:
        pid = part["part_id"]
        for m in part["measures"]:
            mi = m["index"]
            notes = m["notes"]
            if len(notes) >= 2:
                d0 = notes[0]["duration_ticks"]
                d1 = notes[1]["duration_ticks"]
                m_ticks = _m_ticks(structure[mi]["time_sig"])
                safe = all(
                    not (ev["part_id"] == pid and ev["measure"] == mi
                         and ev["anchor_note_id"] == notes[1]["note_id"]
                         and d1 + ev["beat_offset_ticks"] >= m_ticks)
                    for ev in project_data["expression_events"]
                )
                if safe:
                    script.append({"op": "set_duration", "part": pid, "measure": mi,
                                   "note_id": notes[0]["note_id"], "value": d1})
                    script.append({"op": "set_duration", "part": pid, "measure": mi,
                                   "note_id": notes[1]["note_id"], "value": d0})
            ev_id = dyn_by_measure.get((pid, mi))
            if ev_id is not None:
                cur = next(e for e in project_data["expression_events"]
                           if e["event_id"] == ev_id)
                script.append({"op": "set_dynamics", "event_id": ev_id,
                               "value": min(127.0, float(cur["value"]) + 5.0)})
            script.append({"op": "commit", "part": pid, "measure": mi})
    return script


def apply_op(session, op: dict) -> None:
    kind = op["op"]
    if kind == "set_duration":
        session.set_note_duration(op["part"], op["measure"], op["note_id"], op["value"])
    elif kind == "set_dynamics":
        session.set_event_value(op["event_id"], op["value"])
    elif kind == "commit":
        session.commit_measure(op["part"], op["measure"])
    else:  # pragma: no cover
        raise ValueError(f"unknown op {kind}")


def run_script(session, script: list[dict], stop_after_commits: int | None = None):
    """按顺序应用编辑序列；stop_after_commits 用于模拟进程强杀."""
    commits = 0
    for op in script:
        if op["op"] == "commit" and stop_after_commits is not None \
                and commits >= stop_after_commits:
            return session
        apply_op(session, op)
        if op["op"] == "commit":
            commits += 1
    return session


def full_recompute(project_data: dict, script: list[dict]) -> Score:
    """路径 C：从源样本一次性全量重算（无检查点）."""
    from .session import EditSession

    session = EditSession(Score(project_data))
    run_script(session, script)
    return session.score


def realtime_run(project_data: dict, script: list[dict], store=None) -> Score:
    """路径 A：实时编辑会话."""
    from .session import EditSession

    session = EditSession(Score(project_data), store=store)
    run_script(session, script)
    return session.score


def resume_run(project_data: dict, script: list[dict], store,
               crash_after_commits: int) -> Score:
    """路径 B：提交若干小节后模拟强杀，重启从检查点恢复续跑."""
    from .checkpoint import restore_session
    from .session import EditSession

    session = EditSession(Score(project_data), store=store)
    run_script(session, script, stop_after_commits=crash_after_commits)
    del session  # 模拟进程退出（强杀：工作副本中未提交内容全部丢失）
    restored = restore_session(project_data, store)
    run_script(restored, _skip_committed_ops(restored, script))
    return restored.score


def _skip_committed_ops(session, script: list[dict]) -> list[dict]:
    """恢复续跑时过滤掉已提交（冻结）小节的编辑与提交操作."""
    out = []
    for op in script:
        if op["op"] in ("set_duration", "commit"):
            if (op["part"], op["measure"]) in session.committed:
                continue
        elif op["op"] == "set_dynamics":
            ev = session.working.events.get(op["event_id"])
            if ev is not None and (ev.part_id, ev.measure) in session.committed:
                continue
        out.append(op)
    return out


def _close(a, b) -> bool:
    if isinstance(a, float) or isinstance(b, float):
        return math.isclose(float(a), float(b), rel_tol=0.0, abs_tol=FLOAT_ABS_TOL)
    return a == b


def compare_scores(left: Score, right: Score) -> None:
    """逐音符时值、逐事件绝对时间戳、逐采样点力度值三方比对."""
    if left.part_ids != right.part_ids:
        raise DivergenceError("parts", "part_ids", left.part_ids, right.part_ids)
    for pid in left.part_ids:
        lp, rp = left.part(pid), right.part(pid)
        for mi, (lm, rm) in enumerate(zip(lp.measures, rp.measures)):
            mpath = f"parts[{pid}].measures[{mi}]"
            if len(lm.notes) != len(rm.notes):
                raise DivergenceError(mpath, "note_count", len(lm.notes), len(rm.notes))
            for ln, rn in zip(lm.notes, rm.notes):
                npath = f"{mpath}.notes[{ln.note_id}]"
                if ln.note_id != rn.note_id:
                    raise DivergenceError(mpath, "note_id", ln.note_id, rn.note_id)
                if ln.duration_ticks != rn.duration_ticks:
                    raise DivergenceError(npath, "duration_ticks",
                                          ln.duration_ticks, rn.duration_ticks)
                if ln.pitch != rn.pitch:
                    raise DivergenceError(npath, "pitch", ln.pitch, rn.pitch)
    if sorted(left.events) != sorted(right.events):
        raise DivergenceError("events", "event_ids",
                              sorted(left.events), sorted(right.events))
    for eid in sorted(left.events):
        lev, rev = left.events[eid], right.events[eid]
        epath = f"events[{eid}]"
        for field_name in ("type", "part_id", "measure", "anchor_note_id",
                           "beat_offset_ticks"):
            lv, rv = getattr(lev, field_name), getattr(rev, field_name)
            if lv != rv:
                raise DivergenceError(epath, field_name, lv, rv)
        if not _close(lev.value, rev.value):
            raise DivergenceError(epath, "value", lev.value, rev.value)
        lt, rt = left.event_abs_tick(lev), right.event_abs_tick(rev)
        if lt != rt:
            raise DivergenceError(epath, "abs_tick", lt, rt)
        lms, rms = left.event_abs_ms(lev), right.event_abs_ms(rev)
        if not _close(lms, rms):
            raise DivergenceError(epath, "abs_ms", lms, rms)
    grid = 120
    for pid in left.part_ids:
        for tick in range(0, left.total_ticks, grid):
            lv = left.dynamics_value_at(pid, tick)
            rv = right.dynamics_value_at(pid, tick)
            if not _close(lv, rv):
                raise DivergenceError(
                    f"parts[{pid}].dynamics@{tick}", "dynamics_value", lv, rv)
