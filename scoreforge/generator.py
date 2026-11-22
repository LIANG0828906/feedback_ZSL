"""确定性样本生成器.

按固定种子在本地产出多组乐谱工程，每组包含：
  - 乐谱结构描述（声部 / 小节 / 音符，时值为声明 tick 单位下的正整数）
  - 逐音符演奏表达事件（力度 / 连奏 / 踏板，带锚点与拍位）
  - 解析真值（指定小节期望总时值、力度曲线采样点期望值、事件绝对时间戳）

真值由本模块内的参考实现独立计算，是后续观测与一致性验证的唯一依据。
"""

from __future__ import annotations

import json
import random
from pathlib import Path

from .units import (
    ALGO_VERSION,
    DEFAULT_DYNAMIC,
    DEFAULT_SEED,
    PARAM_VERSION,
    TICKS_PER_QUARTER,
    measure_ticks,
)

FORMAT_VERSION = 1
TIME_SIGNATURES = [(4, 4), (3, 4), (6, 8)]


def ref_measure_starts(structure):
    """参考实现：各小节起始 tick."""
    starts = []
    acc = 0
    for entry in structure:
        starts.append(acc)
        acc += measure_ticks(entry["time_sig"])
    return starts


def ref_note_onsets(notes):
    """参考实现：小节内各音符起始拍位（tick）."""
    onsets = []
    acc = 0
    for note in notes:
        onsets.append(acc)
        acc += note["duration_ticks"]
    return onsets


def ref_event_abs_tick(structure, part, event):
    starts = ref_measure_starts(structure)
    measure = part["measures"][event["measure"]]
    onsets = ref_note_onsets(measure["notes"])
    idx = next(
        i for i, n in enumerate(measure["notes"]) if n["note_id"] == event["anchor_note_id"]
    )
    return starts[event["measure"]] + onsets[idx] + event["beat_offset_ticks"]


def ref_ms_at_tick(tempo_map, tick):
    entries = sorted(((int(e["tick"]), float(e["bpm"])) for e in tempo_map), key=lambda x: x[0])
    total = 0.0
    for i, (start, bpm) in enumerate(entries):
        stop = entries[i + 1][0] if i + 1 < len(entries) else None
        ms_per_tick = 60000.0 / (bpm * TICKS_PER_QUARTER)
        if stop is not None and tick > stop:
            total += (stop - start) * ms_per_tick
        else:
            total += max(0, tick - start) * ms_per_tick
            break
    return total


def collapse_dynamics(events_abs):
    """同一绝对拍位存在多个力度事件时后者优先（规范语义，模型侧一致）."""
    out = []
    for t, v in events_abs:
        if out and out[-1][0] == t:
            out[-1] = (t, v)
        else:
            out.append((t, v))
    return out


def ref_dynamics_at(events_abs, tick):
    """events_abs 为按绝对 tick 升序的 [(abs_tick, value)]；分段线性插值."""
    events_abs = collapse_dynamics(events_abs)
    if not events_abs:
        return DEFAULT_DYNAMIC
    if tick < events_abs[0][0]:
        return DEFAULT_DYNAMIC
    for (t0, v0), (t1, v1) in zip(events_abs, events_abs[1:]):
        if t0 <= tick <= t1:
            if t1 == t0:
                return v1
            return v0 + (v1 - v0) * (tick - t0) / (t1 - t0)
    return events_abs[-1][1]


def _split_measure(rng, total_ticks):
    """把小节总时值拆成 2..5 个正整数时值（120 tick 的整数倍）."""
    unit = 120
    units = total_ticks // unit
    count = rng.randint(2, min(5, units))
    cuts = sorted(rng.sample(range(1, units), count - 1))
    parts = []
    prev = 0
    for c in cuts + [units]:
        parts.append((c - prev) * unit)
        prev = c
    return parts


def generate_project(seed=DEFAULT_SEED, project_index=0):
    rng = random.Random((seed, project_index, "scoreforge").__repr__())
    project_id = f"proj-{seed}-{project_index}"

    n_measures = rng.randint(4, 8)
    structure = [
        {"index": i, "time_sig": list(rng.choice(TIME_SIGNATURES))} for i in range(n_measures)
    ]
    starts = ref_measure_starts(structure)
    total_ticks = starts[-1] + measure_ticks(structure[-1]["time_sig"])

    tempo_map = [{"tick": 0, "bpm": float(rng.choice([90, 100, 120, 132, 140]))}]
    if rng.random() < 0.5 and n_measures > 2:
        change_measure = rng.randint(1, n_measures - 1)
        tempo_map.append(
            {"tick": starts[change_measure], "bpm": float(rng.choice([80, 96, 108, 150]))}
        )

    n_parts = rng.randint(2, 3)
    parts = []
    events = []
    event_counter = 0
    pitch = 60
    for p in range(n_parts):
        part_id = f"P{p + 1}"
        measures = []
        pedal_on = False
        for m in structure:
            mi = m["index"]
            m_ticks = measure_ticks(m["time_sig"])
            durations = _split_measure(rng, m_ticks)
            notes = []
            for k, dur in enumerate(durations):
                pitch = max(48, min(84, pitch + rng.randint(-5, 5)))
                notes.append(
                    {
                        "note_id": f"{part_id}-M{mi}-N{k}",
                        "pitch": pitch,
                        "duration_ticks": dur,
                    }
                )
            measures.append({"index": mi, "notes": notes})

            def add_event(etype, anchor_idx, offset, value, _notes=notes, _mi=mi):
                nonlocal event_counter
                events.append(
                    {
                        "event_id": f"E{event_counter:04d}",
                        "type": etype,
                        "part_id": part_id,
                        "measure": _mi,
                        "anchor_note_id": _notes[anchor_idx]["note_id"],
                        "beat_offset_ticks": offset,
                        "value": value,
                    }
                )
                event_counter += 1

            if rng.random() < 0.75:
                for _ in range(rng.randint(1, 2)):
                    ai = rng.randrange(len(notes))
                    offset = rng.choice([0, notes[ai]["duration_ticks"] // 2])
                    add_event("dynamics", ai, offset, float(rng.randrange(40, 112)))
            if mi % 2 == 0 or pedal_on:
                pedal_on = not pedal_on
                add_event("pedal", 0, 0, 127 if pedal_on else 0)
            if rng.random() < 0.4:
                ai = rng.randrange(len(notes))
                add_event("legato", ai, 0, 1)
        parts.append({"part_id": part_id, "name": f"Part {part_id}", "measures": measures})

    truth_measures = []
    truth_samples = []
    truth_timestamps = []
    for part in parts:
        part_id = part["part_id"]
        dyn_abs = []
        for ev in events:
            if ev["part_id"] == part_id and ev["type"] == "dynamics":
                dyn_abs.append((ref_event_abs_tick(structure, part, ev), ev["value"]))
        dyn_abs.sort(key=lambda x: x[0])
        for m in part["measures"]:
            mi = m["index"]
            m_ticks = measure_ticks(structure[mi]["time_sig"])
            if rng.random() < 0.6:
                truth_measures.append(
                    {
                        "part_id": part_id,
                        "measure": mi,
                        "expected_total_ticks": sum(n["duration_ticks"] for n in m["notes"]),
                        "tolerance_ticks": 0,
                    }
                )
                for _ in range(2):
                    beat = rng.randrange(0, m_ticks)
                    truth_samples.append(
                        {
                            "part_id": part_id,
                            "measure": mi,
                            "beat_ticks": beat,
                            "expected_value": ref_dynamics_at(dyn_abs, starts[mi] + beat),
                            "tolerance": 0.25,
                        }
                    )
    for ev in events:
        if rng.random() < 0.5:
            part = next(p for p in parts if p["part_id"] == ev["part_id"])
            truth_timestamps.append(
                {
                    "event_id": ev["event_id"],
                    "expected_ms": ref_ms_at_tick(
                        tempo_map, ref_event_abs_tick(structure, part, ev)
                    ),
                    "tolerance_ms": 0.5,
                }
            )

    return {
        "format_version": FORMAT_VERSION,
        "project_id": project_id,
        "seed": seed,
        "param_version": PARAM_VERSION,
        "algo_version": ALGO_VERSION,
        "units": {"ticks_per_quarter": TICKS_PER_QUARTER},
        "tempo_map": tempo_map,
        "measure_structure": structure,
        "parts": parts,
        "expression_events": events,
        "truth": {
            "measures": truth_measures,
            "dynamics_samples": truth_samples,
            "event_timestamps": truth_timestamps,
        },
        "total_ticks": total_ticks,
    }


def generate_suite(seed=DEFAULT_SEED, count=3):
    return [generate_project(seed, i) for i in range(count)]


def write_samples(directory, seed=DEFAULT_SEED, count=3):
    out = Path(directory)
    out.mkdir(parents=True, exist_ok=True)
    paths = []
    for project in generate_suite(seed, count):
        path = out / f"{project['project_id']}.json"
        path.write_text(json.dumps(project, indent=2, ensure_ascii=False), encoding="utf-8")
        paths.append(path)
    return paths


def main(argv=None):
    import argparse

    parser = argparse.ArgumentParser(description="生成确定性乐谱样本")
    parser.add_argument("--out", default="samples")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--count", type=int, default=3)
    args = parser.parse_args(argv)
    for path in write_samples(args.out, args.seed, args.count):
        print(f"written: {path}")


if __name__ == "__main__":
    main()
