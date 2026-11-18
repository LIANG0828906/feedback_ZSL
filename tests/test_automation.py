from __future__ import annotations

import numpy as np

from mastering.automation import apply_automation_smoothing, base_gain_db
from mastering.manifest import AutomationConfig, AutomationNode


def test_piecewise_linear_curve_and_hold_ends():
    nodes = (
        AutomationNode(1.0, -3.0),
        AutomationNode(2.0, 3.0),
        AutomationNode(4.0, 0.0),
    )
    times = np.array([0.0, 1.0, 1.5, 2.0, 3.0, 4.0, 5.0])
    values = base_gain_db(times, nodes)
    assert np.allclose(values, [-3.0, -3.0, 0.0, 3.0, 1.5, 0.0, 0.0])


def test_short_steep_segments_are_smoothed_deterministically():
    config = AutomationConfig(-48.0, 6.0, 3.0, 0.05)
    nodes = (
        AutomationNode(0.0, -3.0),
        AutomationNode(1.0, -3.0),
        AutomationNode(1.001, 3.0),
        AutomationNode(1.2, 3.0),
        AutomationNode(1.201, -6.0),
        AutomationNode(1.5, -6.0),
    )
    times = np.arange(0, 1.5 * 48000 + 1) / 48000.0
    first, segments = apply_automation_smoothing(times, nodes, config)
    second, second_segments = apply_automation_smoothing(times, nodes, config)
    assert len(segments) == 2
    assert segments == second_segments
    assert np.array_equal(first, second)
    assert abs(sum(segment.duration_seconds for segment in segments) - 0.1) < 1e-12
    assert np.max(first) <= 3.0
    assert np.min(first) >= -6.0


def test_smoothing_is_clipped_to_curve_time_range():
    config = AutomationConfig(-48.0, 6.0, 2.0, 0.5)
    nodes = (
        AutomationNode(0.0, -3.0),
        AutomationNode(0.01, 3.0),
        AutomationNode(0.2, 3.0),
        AutomationNode(0.21, -3.0),
    )
    times = np.arange(0, 0.21 * 48000 + 1) / 48000.0
    _, segments = apply_automation_smoothing(times, nodes, config)
    assert segments
    for segment in segments:
        assert segment.start_seconds >= nodes[0].time
        assert (
            segment.start_seconds + segment.duration_seconds
            <= nodes[-1].time + 1e-12
        )
