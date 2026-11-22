import pytest

from scoreforge.errors import BatchError, DanglingEventError, DurationError


def _first_key(session):
    return session.score.measure_keys()[0]


def test_edits_only_touch_working_copy(session):
    pid, mi = _first_key(session)
    note = session.score.part(pid).measures[mi].notes[0]
    original = note.duration_ticks
    session.set_note_duration(pid, mi, note.note_id, original - 120)
    assert session.score.part(pid).measures[mi].notes[0].duration_ticks == original
    assert session.working.part(pid).measures[mi].notes[0].duration_ticks \
        == original - 120


def test_commit_applies_atomically_and_freezes(session):
    pid, mi = _first_key(session)
    notes = session.score.part(pid).measures[mi].notes
    session.set_note_duration(pid, mi, notes[0].note_id,
                              notes[0].duration_ticks + 120)
    session.set_note_duration(pid, mi, notes[1].note_id,
                              notes[1].duration_ticks - 120)
    record = session.commit_measure(pid, mi)
    assert record.seq == 0
    assert (pid, mi) in session.committed
    assert session.score.part(pid).measures[mi].notes[0].duration_ticks \
        == notes[0].duration_ticks + 120
    with pytest.raises(BatchError):
        session.set_note_duration(pid, mi, notes[0].note_id, 120)
    with pytest.raises(BatchError):
        session.commit_measure(pid, mi)


def test_empty_commit_returns_e_batch(session):
    pid, mi = _first_key(session)
    with pytest.raises(BatchError) as exc:
        session.commit_measure(pid, mi)
    assert exc.value.code == "E_BATCH"


def test_failed_commit_rolls_back_model(session):
    pid, mi = _first_key(session)
    before = session.score.part(pid).measures[mi].to_dict()
    note = session.working.part(pid).measures[mi].notes[0]
    session.set_note_duration(pid, mi, note.note_id, -10)
    with pytest.raises(DurationError):
        session.commit_measure(pid, mi)
    assert session.score.part(pid).measures[mi].to_dict() == before
    assert (pid, mi) not in session.committed


def test_delete_note_without_reanchor_returns_e_dangling(session):
    pid, mi = _first_key(session)
    events = session.working.events_of_measure(pid, mi)
    if not events:
        pytest.skip("样本该小节无事件")
    ev = events[0]
    session.delete_note(pid, mi, ev.anchor_note_id)
    with pytest.raises(DanglingEventError) as exc:
        session.commit_measure(pid, mi)
    assert exc.value.code == "E_DANGLING"
    assert exc.value.event_id == ev.event_id


def test_delete_note_with_reanchor_commits(session):
    pid, mi = _first_key(session)
    measure = session.working.part(pid).measures[mi]
    events = session.working.events_of_measure(pid, mi)
    if not events or len(measure.notes) < 2:
        pytest.skip("样本条件不足")
    ev = events[0]
    doomed = ev.anchor_note_id
    survivor = next(n.note_id for n in measure.notes if n.note_id != doomed)
    dropped_duration = measure.note(doomed).duration_ticks
    survivor_note = measure.note(survivor)
    session.set_note_duration(pid, mi, survivor,
                              survivor_note.duration_ticks + dropped_duration)
    session.delete_note(pid, mi, doomed, reanchor={ev.event_id: survivor})
    record = session.commit_measure(pid, mi)
    assert record.seq == 0
    assert session.score.events[ev.event_id].anchor_note_id == survivor


def test_uncommitted_changes_do_not_affect_export(session):
    pid, mi = _first_key(session)
    before = session.score.export_timeline()
    note = session.working.part(pid).measures[mi].notes[0]
    session.set_note_duration(pid, mi, note.note_id, note.duration_ticks - 120)
    assert session.score.export_timeline() == before
