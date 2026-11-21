from __future__ import annotations

from dataclasses import replace

import numpy as np
import pytest

from lightprobe.bake import Baker
from lightprobe.errors import ProbeError

from conftest import fast_config


def test_occluder_move_rebakes_only_influence_closure(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    lamp = room.occluder("street_lamp")
    center = lamp.center
    moved = room.with_updates(
        {
            "occluders": {
                "street_lamp": {
                    "center": [center[0] - 0.75, center[1] + 0.25, center[2] + 0.5]
                }
            }
        }
    )
    second = baker.bake(moved, fast_config(moved), base_state=first.state)
    assert second.report.influence["changed_factors"] == ["occluder_geometry"]
    assert 0 < second.report.recomputed_count < first.report.probe_count
    assert second.report.reused_count == first.report.probe_count - second.report.recomputed_count
    assert second.report.max_coefficient_deviation > 0.0
    affected = set(second.report.influence["affected_probe_ids"])
    for probe_id in first.state.records:
        if probe_id not in affected:
            assert (
                second.state.records[probe_id].content_hash
                == first.state.records[probe_id].content_hash
            )


def test_material_albedo_change_partially_reuses(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    changed = room.with_updates({"materials": {"metal": {"albedo": [0.8, 0.2, 0.1]}}})
    second = baker.bake(changed, fast_config(changed), base_state=first.state)
    assert second.report.influence["changed_factors"] == ["material_albedo"]
    assert 0 < second.report.recomputed_count < first.report.probe_count
    assert second.report.reused_count > 0


def test_compensation_change_rebuilds_leaks_and_dependencies(room):
    baker = Baker()
    sparse_config = fast_config(room, strategy="sparse")
    first = baker.bake(room, sparse_config)
    changed = room.with_updates(
        {"settings": {"compensation_strength": 0.9, "strategy": "sparse"}}
    )
    second = baker.bake(
        changed, fast_config(changed, strategy="sparse"), base_state=first.state
    )
    assert second.report.influence["changed_factors"] == ["compensation_strength"]
    assert second.report.recomputed_count > 0
    assert second.report.reused_count > 0
    assert second.report.max_coefficient_deviation > 0.0


def test_sh_order_4_recomputes_and_reports_dropped_band(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    changed = room.with_updates({"settings": {"sh_order": 4}})
    second = baker.bake(changed, fast_config(changed), base_state=first.state)
    assert second.report.influence["changed_factors"] == ["sh_order"]
    assert second.report.recomputed_count == first.report.probe_count
    assert second.report.max_coefficient_deviation > 1.0e-4


def test_sky_parameter_change_is_conservative_all_probe(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    changed = room.with_updates({"sky": {"intensity": 1.3}})
    second = baker.bake(changed, fast_config(changed), base_state=first.state)
    assert second.report.influence["changed_factors"] == ["sky_light"]
    assert second.report.recomputed_count == first.report.probe_count
    assert second.report.reused_count == 0
    assert second.report.max_coefficient_deviation > 0.0


def test_corrupt_content_hash_is_never_silently_reused(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    bad_probe = next(iter(first.state.records))
    records = dict(first.state.records)
    records[bad_probe] = replace(records[bad_probe], content_hash="stale")
    corrupted = replace(first.state, records=records)
    second = baker.bake(room, fast_config(room), base_state=corrupted)
    assert bad_probe in second.report.influence["affected_probe_ids"]
    assert (
        second.report.influence["reasons"][bad_probe] == "content_hash_mismatch"
    )
    assert second.report.recomputed_count >= 1


def test_negative_compensated_irradiance_returns_interp_failed(room, monkeypatch):
    def negative_irradiance(raw, order):
        return -np.ones(((order + 1) ** 2, 3), dtype=np.float64)

    monkeypatch.setattr(
        "lightprobe.bake.radiance_to_irradiance", negative_irradiance
    )
    with pytest.raises(ProbeError) as caught:
        Baker().bake(room, fast_config(room))
    assert caught.value.code == "E_INTERP_FAILED"
    assert caught.value.details["probe_ids"]

