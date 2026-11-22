"""乐谱工程加载与结构校验.

错误码约定:
  E_SCHEMA  字段缺失或类型非法
  E_REF     引用悬空（声部 / 小节 / 锚点音符 / 拍位越界）
  E_DUR     时值非法（非正整数，或累计时值越过小节边界）
所有错误均携带对象路径（如 parts[0].measures[2].notes[1].duration_ticks）。
"""

from __future__ import annotations

from .errors import DurationError, RefError, SchemaError
from .units import measure_ticks

EVENT_TYPES = ("dynamics", "legato", "pedal")


def _is_int(value) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _require(obj, key, path):
    if not isinstance(obj, dict) or key not in obj:
        raise SchemaError(f"missing required field '{key}'", path=path)
    return obj[key]


def _require_int(obj, key, path):
    value = _require(obj, key, path)
    if not _is_int(value):
        raise SchemaError(f"field '{key}' must be an integer", path=f"{path}.{key}")
    return value


def validate_project(data: dict) -> None:
    """校验完整工程描述；任何违规立即抛出带路径的类型化错误."""
    if not isinstance(data, dict):
        raise SchemaError("project root must be an object", path="$")
    for key in (
        "format_version",
        "project_id",
        "param_version",
        "algo_version",
        "units",
        "tempo_map",
        "measure_structure",
        "parts",
        "expression_events",
        "truth",
    ):
        _require(data, key, "$")

    tpq = _require_int(data["units"], "ticks_per_quarter", "$.units")
    if tpq <= 0:
        raise SchemaError("ticks_per_quarter must be positive", path="$.units.ticks_per_quarter")

    tempo_map = data["tempo_map"]
    if not isinstance(tempo_map, list) or not tempo_map:
        raise SchemaError("tempo_map must be a non-empty list", path="$.tempo_map")
    for i, entry in enumerate(tempo_map):
        path = f"$.tempo_map[{i}]"
        tick = _require_int(entry, "tick", path)
        bpm = _require(entry, "bpm", path)
        if not isinstance(bpm, (int, float)) or isinstance(bpm, bool) or bpm <= 0:
            raise SchemaError("bpm must be a positive number", path=f"{path}.bpm")
        if i == 0 and tick != 0:
            raise SchemaError("first tempo entry must be at tick 0", path=path)

    structure = data["measure_structure"]
    if not isinstance(structure, list) or not structure:
        raise SchemaError("measure_structure must be a non-empty list", path="$.measure_structure")
    for i, entry in enumerate(structure):
        path = f"$.measure_structure[{i}]"
        if _require_int(entry, "index", path) != i:
            raise SchemaError("measure_structure indices must be contiguous from 0", path=path)
        ts = _require(entry, "time_sig", path)
        if (
            not isinstance(ts, list)
            or len(ts) != 2
            or not all(_is_int(x) and x > 0 for x in ts)
        ):
            raise SchemaError(
                "time_sig must be [num, den] of positive ints", path=f"{path}.time_sig"
            )
    n_measures = len(structure)

    parts = data["parts"]
    if not isinstance(parts, list) or not parts:
        raise SchemaError("parts must be a non-empty list", path="$.parts")
    part_ids = set()
    note_index = {}
    for pi, part in enumerate(parts):
        ppath = f"$.parts[{pi}]"
        part_id = _require(part, "part_id", ppath)
        if not isinstance(part_id, str):
            raise SchemaError("part_id must be a string", path=f"{ppath}.part_id")
        if part_id in part_ids:
            raise SchemaError(f"duplicate part_id '{part_id}'", path=f"{ppath}.part_id")
        part_ids.add(part_id)
        measures = _require(part, "measures", ppath)
        if len(measures) != n_measures:
            raise RefError(
                f"part '{part_id}' has {len(measures)} measures, "
                f"expected {n_measures} from measure_structure",
                path=f"{ppath}.measures",
            )
        seen_notes = set()
        for mi, measure in enumerate(measures):
            mpath = f"{ppath}.measures[{mi}]"
            if _require_int(measure, "index", mpath) != mi:
                raise SchemaError("measure indices must be contiguous from 0", path=mpath)
            m_ticks = measure_ticks(structure[mi]["time_sig"])
            notes = _require(measure, "notes", mpath)
            total = 0
            for ni, note in enumerate(notes):
                npath = f"{mpath}.notes[{ni}]"
                note_id = _require(note, "note_id", npath)
                if not isinstance(note_id, str):
                    raise SchemaError("note_id must be a string", path=f"{npath}.note_id")
                if note_id in seen_notes:
                    raise SchemaError(f"duplicate note_id '{note_id}'", path=f"{npath}.note_id")
                seen_notes.add(note_id)
                _require_int(note, "pitch", npath)
                dur = _require(note, "duration_ticks", npath)
                if not _is_int(dur) or dur <= 0:
                    raise DurationError(
                        f"duration_ticks must be a positive integer, got {dur!r}",
                        path=f"{npath}.duration_ticks",
                    )
                total += dur
            if total > m_ticks:
                raise DurationError(
                    f"notes total {total} ticks exceeds measure length {m_ticks}",
                    path=mpath,
                )
        note_index[part_id] = {
            (mi, n["note_id"]) for mi, m in enumerate(measures) for n in m["notes"]
        }

    events = data["expression_events"]
    if not isinstance(events, list):
        raise SchemaError("expression_events must be a list", path="$.expression_events")
    event_ids = set()
    for ei, ev in enumerate(events):
        epath = f"$.expression_events[{ei}]"
        event_id = _require(ev, "event_id", epath)
        if event_id in event_ids:
            raise SchemaError(f"duplicate event_id '{event_id}'", path=f"{epath}.event_id")
        event_ids.add(event_id)
        etype = _require(ev, "type", epath)
        if etype not in EVENT_TYPES:
            raise SchemaError(
                f"unknown event type '{etype}', expected one of {EVENT_TYPES}",
                path=f"{epath}.type",
            )
        part_id = _require(ev, "part_id", epath)
        if part_id not in part_ids:
            raise RefError(f"event references unknown part '{part_id}'", path=f"{epath}.part_id")
        mi = _require_int(ev, "measure", epath)
        if mi < 0 or mi >= n_measures:
            raise RefError(
                f"event references measure {mi}, out of range 0..{n_measures - 1}",
                path=f"{epath}.measure",
            )
        anchor = _require(ev, "anchor_note_id", epath)
        if (mi, anchor) not in note_index[part_id]:
            raise RefError(
                f"event anchor '{anchor}' not found in part '{part_id}' measure {mi}",
                path=f"{epath}.anchor_note_id",
            )
        offset = _require_int(ev, "beat_offset_ticks", epath)
        part = next(p for p in parts if p["part_id"] == part_id)
        notes = part["measures"][mi]["notes"]
        onset = 0
        for n in notes:
            if n["note_id"] == anchor:
                break
            onset += n["duration_ticks"]
        m_ticks = measure_ticks(structure[mi]["time_sig"])
        if offset < 0 or onset + offset >= m_ticks:
            raise RefError(
                f"event beat position {onset + offset} crosses measure boundary "
                f"[0, {m_ticks})",
                path=f"{epath}.beat_offset_ticks",
            )
        _require(ev, "value", epath)

    _validate_truth(data["truth"], part_ids, n_measures, event_ids)


def _validate_truth(truth, part_ids, n_measures, event_ids) -> None:
    if not isinstance(truth, dict):
        raise SchemaError("truth must be an object", path="$.truth")
    for key in ("measures", "dynamics_samples", "event_timestamps"):
        _require(truth, key, "$.truth")
    for i, entry in enumerate(truth["measures"]):
        path = f"$.truth.measures[{i}]"
        part_id = _require(entry, "part_id", path)
        if part_id not in part_ids:
            raise RefError(f"truth references unknown part '{part_id}'", path=f"{path}.part_id")
        mi = _require_int(entry, "measure", path)
        if mi < 0 or mi >= n_measures:
            raise RefError(f"truth references unknown measure {mi}", path=f"{path}.measure")
        _require_int(entry, "expected_total_ticks", path)
    for i, entry in enumerate(truth["dynamics_samples"]):
        path = f"$.truth.dynamics_samples[{i}]"
        part_id = _require(entry, "part_id", path)
        if part_id not in part_ids:
            raise RefError(f"truth references unknown part '{part_id}'", path=f"{path}.part_id")
        mi = _require_int(entry, "measure", path)
        if mi < 0 or mi >= n_measures:
            raise RefError(f"truth references unknown measure {mi}", path=f"{path}.measure")
        _require_int(entry, "beat_ticks", path)
        _require(entry, "expected_value", path)
    for i, entry in enumerate(truth["event_timestamps"]):
        path = f"$.truth.event_timestamps[{i}]"
        event_id = _require(entry, "event_id", path)
        if event_id not in event_ids:
            raise RefError(
                f"truth references unknown event '{event_id}'", path=f"{path}.event_id"
            )
        _require(entry, "expected_ms", path)
