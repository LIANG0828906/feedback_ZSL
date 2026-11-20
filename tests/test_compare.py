"""基线逐帧比对与分叉诊断。"""

import copy

import pytest

from replay.compare import compare_trace
from replay.engine import ReplayEngine
from replay.errors import BaselineVersionError
from replay.generator import generate_sample


def _trace_and_baseline():
    sample = generate_sample(seed=23, players=4, frames=24, disorder=2)
    engine = ReplayEngine(sample)
    engine.run()
    return engine.trace_lines(), engine.baseline()


def test_match():
    trace, baseline = _trace_and_baseline()
    result = compare_trace(trace, baseline)
    assert result == {"status": "match", "frames_compared": 24}


def test_divergence_reports_first_frame_and_field():
    trace, baseline = _trace_and_baseline()
    tampered = copy.deepcopy(baseline)
    tampered["frames"][5]["quota_used"]["p1"] += 1
    result = compare_trace(trace, tampered)
    assert result["status"] == "diverged"
    assert result["frame"] == 5
    assert result["field"] == "quota_used.p1"
    assert result["local"] == baseline["frames"][5]["quota_used"]["p1"]
    assert result["baseline"] == \
        baseline["frames"][5]["quota_used"]["p1"] + 1
    assert result["applied"] == trace[5]["applied"]
    assert result["buffer_watermark"] == trace[5]["buffer_watermark"]


def test_divergence_in_events_path():
    trace, baseline = _trace_and_baseline()
    tampered = copy.deepcopy(baseline)
    tampered["frames"][2]["events"][0]["type"] = "E_TAMPERED"
    result = compare_trace(trace, tampered)
    assert result["status"] == "diverged"
    assert result["frame"] == 2
    assert result["field"] == "events[0].type"


def test_baseline_version_mismatch():
    trace, baseline = _trace_and_baseline()
    tampered = copy.deepcopy(baseline)
    tampered["baseline_version"] = 999
    with pytest.raises(BaselineVersionError):
        compare_trace(trace, tampered)
