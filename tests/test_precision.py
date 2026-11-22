import pytest

from scoreforge.errors import PrecisionError
from scoreforge.precision import verify_truth


def test_unedited_project_passes(loaded):
    score, truth = loaded
    report = verify_truth(score, truth)
    assert report.passed
    assert report.measure_reports
    assert all(r.abs_dev == 0 for r in report.measure_reports)
    assert all(r.abs_dev <= r.tolerance for r in report.sample_reports)
    assert all(r.abs_dev <= r.tolerance_ms for r in report.event_reports)


def test_unverifiable_measures_marked(loaded):
    score, truth = loaded
    report = verify_truth(score, truth)
    covered = {(r.part_id, r.measure) for r in report.measure_reports}
    expected = [k for k in score.measure_keys() if k not in covered]
    assert sorted(report.unverifiable) == sorted(expected)
    assert report.unverifiable  # 真值缺失的小节必须显式列出


def test_duration_deviation_returns_e_prec(loaded):
    score, truth = loaded
    entry = truth["measures"][0]
    pid, mi = entry["part_id"], entry["measure"]
    score.part(pid).measures[mi].notes[0].duration_ticks += 240
    report = verify_truth(score, truth)
    assert not report.passed
    with pytest.raises(PrecisionError) as exc:
        report.raise_if_failed()
    assert exc.value.code == "E_PREC"
    assert exc.value.measure == mi


def test_dynamics_deviation_reports_measure_and_beat(loaded):
    score, truth = loaded
    sample = truth["dynamics_samples"][0]
    pid = sample["part_id"]
    for ev in score.events.values():
        if ev.part_id == pid and ev.type == "dynamics":
            ev.value = float(ev.value) + 50.0
    report = verify_truth(score, truth)
    assert not report.passed
    with pytest.raises(PrecisionError) as exc:
        report.raise_if_failed()
    assert exc.value.measure is not None
    assert exc.value.beat_ticks is not None
