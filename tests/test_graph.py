"""要求 4：参数级依赖图谱与增量重算判定。"""

from __future__ import annotations

from mediaaudit.graph import DependencyGraph, ParamComponent
from mediaaudit.parameters import TierParams
from tests.test_slicing import _source


def _graph() -> DependencyGraph:
    return DependencyGraph(_source())


def test_first_version_recomputes_all() -> None:
    graph = _graph()
    params = TierParams(16000, 256, 1000, 250_000)
    plan = graph.plan_recompute("tier", None, params, 0, 4)
    assert plan.recompute == [0, 1, 2, 3]
    assert plan.reuse == []


def test_gain_only_change_recomputes_all_audio_keeps_grid() -> None:
    graph = _graph()
    old = TierParams(16000, 256, 1000, 250_000)
    new = TierParams(16000, 320, 1000, 250_000)
    plan = graph.plan_recompute("tier", old, new, 4, 4)
    assert ParamComponent.GAIN in plan.changed_components
    assert plan.recompute == [0, 1, 2, 3]
    assert all("gain" in plan.reasons[i] for i in range(4))


def test_quantum_change_conservatively_recomputes_with_reason() -> None:
    graph = _graph()
    old = TierParams(16000, 256, 1000, 250_000)
    new = TierParams(16000, 256, 500, 250_000)
    plan = graph.plan_recompute("tier", old, new, 4, 8)
    assert ParamComponent.QUANTUM in plan.changed_components
    assert len(plan.recompute) == 8
    assert "边界" in plan.reasons[0]


def test_unchanged_params_with_proven_clean_reuses() -> None:
    graph = _graph()
    params = TierParams(16000, 256, 1000, 250_000)
    plan = graph.plan_recompute(
        "tier", params, params, 4, 4, proven_clean={0, 1, 2, 3}
    )
    assert plan.recompute == []
    assert plan.reuse == [0, 1, 2, 3]


def test_unproven_slice_conservatively_recomputed() -> None:
    graph = _graph()
    params = TierParams(16000, 256, 1000, 250_000)
    plan = graph.plan_recompute(
        "tier", params, params, 4, 4, proven_clean={0, 2}
    )
    assert set(plan.recompute) == {1, 3}
    assert "保守重算" in plan.reasons[1]


def test_graph_version_bumps_on_structure_change() -> None:
    graph = _graph()
    before = graph.graph_version
    graph.ensure_source()
    graph.ensure_param_nodes("tier")
    graph.register_slice("tier", 1, 0, TierParams(16000, 256, 1000, 250_000))
    assert graph.graph_version > before
