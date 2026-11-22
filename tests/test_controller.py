"""Adaptive controller: E_PARA, bounds, hysteresis, observed-only decisions."""
import pytest

from jitter_workbench.controller import AdaptiveController, ControllerParams
from jitter_workbench.errors import ParamError


def test_e_para_bad_init():
    with pytest.raises(ParamError) as ei:
        ControllerParams(init_delay_ms=1000).validate()
    assert ei.value.code == "E_PARA"
    assert "init_delay_ms" in str(ei.value)


def test_e_para_bad_bounds():
    with pytest.raises(ParamError):
        ControllerParams(min_delay_ms=300, max_delay_ms=100).validate()


def test_e_para_bad_step():
    with pytest.raises(ParamError):
        ControllerParams(step_ms=0).validate()


def test_e_para_bad_thresholds():
    with pytest.raises(ParamError):
        ControllerParams(low_late_ratio=0.5, high_late_ratio=0.1).validate()


def test_clamp_bounds():
    p = ControllerParams(min_delay_ms=40, max_delay_ms=120, step_ms=50,
                         init_delay_ms=100, adjust_interval_ticks=1)
    c = AdaptiveController(p)
    # force repeated raises
    for i in range(10):
        c.observe(i, i * 10, water_frames=0, water_trend=-1.0,
                  late_ratio=0.9, drift_ms=0, recent_delays=[10.0],
                  recent_gap_count=5, recent_queue_delays=[1.0])
    assert c.target_delay_ms == 120  # clamped at max
    # force repeated lowers
    c._last_direction = 0
    for i in range(20, 40):
        c.observe(i, i * 10, water_frames=99, water_trend=1.0,
                  late_ratio=0.0, drift_ms=0, recent_delays=[10.0],
                  recent_gap_count=0, recent_queue_delays=[1.0])
    assert c.target_delay_ms == 40  # clamped at min


def test_hysteresis_cooldown():
    p = ControllerParams(adjust_interval_ticks=5, step_ms=20,
                         init_delay_ms=100, max_delay_ms=400)
    c = AdaptiveController(p)
    d0 = c.observe(0, 0, water_frames=0, water_trend=0.0, late_ratio=0.5,
                   drift_ms=0, recent_delays=[1.0], recent_gap_count=3,
                   recent_queue_delays=[1.0])
    assert d0 == 120
    # within cooldown: no further adjustment even with high late ratio
    for i in range(1, 5):
        d = c.observe(i, i * 10, water_frames=0, water_trend=0.0,
                      late_ratio=0.9, drift_ms=0, recent_delays=[1.0],
                      recent_gap_count=9, recent_queue_delays=[1.0])
        assert d == 120
    # after cooldown: adjustment allowed
    d = c.observe(6, 60, water_frames=0, water_trend=0.0, late_ratio=0.9,
                  drift_ms=0, recent_delays=[1.0], recent_gap_count=9,
                  recent_queue_delays=[1.0])
    assert d == 140
    reasons = [x.reason for x in c.decisions]
    assert reasons == ["late_ratio_high", "late_ratio_high"]


def test_decision_log_fields():
    p = ControllerParams(adjust_interval_ticks=1)
    c = AdaptiveController(p)
    c.observe(0, 0, water_frames=3, water_trend=-1.0, late_ratio=0.5,
              drift_ms=7, recent_delays=[1.0], recent_gap_count=2,
              recent_queue_delays=[1.0])
    d = c.decisions[0]
    assert d.old_delay_ms == 120 and d.new_delay_ms == 140
    assert d.reason == "late_ratio_high"
    assert d.water_frames == 3 and d.drift_ms == 7
