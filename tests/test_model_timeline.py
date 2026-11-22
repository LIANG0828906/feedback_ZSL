from scoreforge.generator import (
    ref_dynamics_at,
    ref_event_abs_tick,
    ref_measure_starts,
    ref_ms_at_tick,
)
from scoreforge.units import DEFAULT_DYNAMIC, TempoMap, measure_ticks


def test_measure_starts_match_reference(project_data):
    score, _ = __import__("scoreforge.model", fromlist=["load_project"]).load_project(
        project_data)
    starts = ref_measure_starts(project_data["measure_structure"])
    for i, start in enumerate(starts):
        assert score.measure_start(i) == start
        assert score.measure_ticks(i) == measure_ticks(
            project_data["measure_structure"][i]["time_sig"])


def test_event_abs_time_matches_reference(project_data):
    score, _ = __import__("scoreforge.model", fromlist=["load_project"]).load_project(
        project_data)
    parts = {p["part_id"]: p for p in project_data["parts"]}
    for ev in project_data["expression_events"]:
        model_ev = score.events[ev["event_id"]]
        expected_tick = ref_event_abs_tick(
            project_data["measure_structure"], parts[ev["part_id"]], ev)
        assert score.event_abs_tick(model_ev) == expected_tick
        expected_ms = ref_ms_at_tick(project_data["tempo_map"], expected_tick)
        assert abs(score.event_abs_ms(model_ev) - expected_ms) < 1e-9


def test_dynamics_curve_matches_reference(project_data):
    score, _ = __import__("scoreforge.model", fromlist=["load_project"]).load_project(
        project_data)
    parts = {p["part_id"]: p for p in project_data["parts"]}
    for pid in score.part_ids:
        dyn_abs = []
        for ev in project_data["expression_events"]:
            if ev["part_id"] == pid and ev["type"] == "dynamics":
                dyn_abs.append((ref_event_abs_tick(
                    project_data["measure_structure"], parts[pid], ev), ev["value"]))
        dyn_abs.sort(key=lambda x: x[0])
        for tick in range(0, score.total_ticks, 137):
            assert abs(score.dynamics_value_at(pid, tick)
                       - ref_dynamics_at(dyn_abs, tick)) < 1e-9


def test_dynamics_default_before_first_event(loaded):
    score, _ = loaded
    pid = score.part_ids[0]
    ticks, _ = score.dynamics_curve(pid)
    if ticks.size and ticks[0] > 0:
        assert score.dynamics_value_at(pid, 0) == DEFAULT_DYNAMIC


def test_tempo_map_roundtrip(project_data):
    tempo = TempoMap.from_data(project_data["tempo_map"])
    for tick in (0, 480, 1920, 5000, 12345):
        assert abs(tempo.ms_at_tick(tempo.tick_at_ms(tempo.ms_at_tick(tick)))
                   - tempo.ms_at_tick(tick)) < 1.0


def test_anchor_migration_after_duration_edit(session):
    pid = session.score.part_ids[0]
    measure = session.working.part(pid).measures[0]
    n0, n1 = measure.notes[0], measure.notes[1]
    events_on_n1 = session.working.events_of_note(n1.note_id)
    if not events_on_n1:
        return
    before = {ev.event_id: session.working.event_abs_tick(ev) for ev in events_on_n1}
    delta = 120
    session.set_note_duration(pid, 0, n0.note_id, n0.duration_ticks + delta)
    session.set_note_duration(pid, 0, n1.note_id, n1.duration_ticks - delta)
    for ev in events_on_n1:
        assert session.working.event_abs_tick(ev) == before[ev.event_id] + delta
