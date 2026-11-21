from __future__ import annotations

from collections import Counter

from tactical_review.model import (
    InitialState,
    Projectile,
    RevocationCommand,
    ShotRecord,
)
from tactical_review.physics import compute_damage, locate_hit
from tactical_review.simulation import Simulation


def test_projectile_damage_and_chain_spread_are_fixed_point(
    loaded_sample,
) -> None:
    initial, _, _ = loaded_sample
    cover_id = initial.cover_ids[0]
    cover = initial.covers[cover_id]
    projectile = Projectile(
        hit_x=cover.x + cover.width / 2,
        hit_y=cover.y + cover.height / 2,
        incident_angle_deg=0,
        momentum=110,
    )
    assert locate_hit(initial, projectile) == cover_id
    result = compute_damage(initial, projectile)
    assert result.direct_cover_id == cover_id
    assert result.raw_damage[cover_id] > 0
    assert all(isinstance(value, int) for value in result.raw_damage.values())
    assert len(result.raw_damage) >= 1


def test_every_step_balances_damage_and_pending_ledger(
    loaded_sample,
) -> None:
    initial, shots, revocations = loaded_sample
    simulation = Simulation(initial, shots, revocations)
    summaries = simulation.run_to()
    for summary in summaries:
        assert summary["remaining_total"] + summary["applied_damage_total"] == (
            summary["initial_total"]
        )
        for cover in summary["covers"]:
            assert (
                cover["remaining"] + cover["damage"]
                == cover["max_durability"]
            )
        for ledger in summary["ledger"]:
            assert ledger["conserved"] is True
            assert (
                ledger["display_remaining"] + ledger["pending"]
                == ledger["authority_remaining"]
            )
    assert not any(
        item["code"] == "E_DAMAGE_IMBALANCE"
        for item in simulation.state.operational_reports
    )


def test_requested_sample_business_codes_are_present(loaded_sample) -> None:
    initial, shots, revocations = loaded_sample
    simulation = Simulation(initial, shots, revocations)
    simulation.run_to()
    counts = Counter(
        item["code"] for item in simulation.state.operational_reports
    )
    assert counts["E_OP_LATE"] >= 2
    assert counts["E_OP_DUPLICATE"] >= 2
    assert counts["E_REVOKE_CLOSED"] >= 1
    assert counts["E_DAMAGE_CONFLICT"] >= 1
    assert simulation.state.duplicate_count >= 2
    crack_keys = [
        (event["cover_id"], event["segment_after"])
        for cover in simulation.state.covers.values()
        for event in cover.crack_events
    ]
    assert len(crack_keys) == len(set(crack_keys))

    late = [
        item
        for item in simulation.state.operational_reports
        if item["code"] == "E_OP_LATE"
    ]
    assert all(
        item["lag_steps"] > initial.compensation_window_steps
        for item in late
    )
    conflicts = [
        item
        for item in simulation.state.operational_reports
        if item["code"] == "E_DAMAGE_CONFLICT"
    ]
    assert all(
        {"step", "cover_id", "predicted", "authoritative"} <= set(item)
        for item in conflicts
    )


def test_revocation_compensation_and_closed_rejection(loaded_sample) -> None:
    initial, shots, revocations = loaded_sample
    simulation = Simulation(initial, shots, revocations)
    simulation.run_to()
    accepted = [
        item
        for item in simulation.state.revocations
        if item["status"] == "ACCEPTED"
    ]
    closed = [
        item
        for item in simulation.state.revocations
        if item.get("reason_code") == "E_REVOKE_CLOSED"
    ]
    assert accepted
    assert closed
    for item in accepted:
        changed = sum(
            int(item["before_pending"].get(cover_id, 0))
            - int(item["after_pending"].get(cover_id, 0))
            for cover_id in set(item["before_pending"]) | set(item["after_pending"])
        )
        assert changed == item["restore_amount"]
        assert changed > 0


def test_same_step_revocation_uses_arrival_order() -> None:
    initial = InitialState.from_dict(
        {
            "schema_version": 1,
            "match_id": "same-step",
            "step_ms": 50,
            "observation": {
                "start_step": 1,
                "end_step": 4,
                "sample_rule": "one per step",
            },
            "truth_source": "initial_state.json",
            "compensation_window_steps": 3,
            "spread_radius": 1.0,
            "chain_decay": 0.5,
            "max_chain_depth": 1,
            "covers": [
                {
                    "cover_id": "C01",
                    "x": 0,
                    "y": 0,
                    "width": 1,
                    "height": 1,
                    "hardness": 1.0,
                    "max_durability": 100,
                    "segment_threshold": 20,
                }
            ],
        }
    )
    shot = ShotRecord.from_dict(
        {
            "record_id": "same-step-shot",
            "player_id": "P1",
            "client_seq": 1,
            "local_time_ms": 150,
            "target_step": 3,
            "arrival_step": 3,
            "arrival_seq": 1,
            "projectile": {
                "hit_position": [0.5, 0.5],
                "incident_angle_deg": 0,
                "momentum": 50,
            },
            "prediction": {"C01": 40},
        }
    )
    revoke_after_shot = RevocationCommand.from_dict(
        {
            "revocation_id": "same-step-revoke",
            "player_id": "P1",
            "client_seq": 1,
            "arrival_step": 3,
            "arrival_seq": 2,
        }
    )
    accepted = Simulation(initial, [shot], [revoke_after_shot])
    accepted.run_to()
    assert accepted.state.revocations[0]["status"] == "ACCEPTED"
    assert accepted.state.covers["C01"].damage == 0
    assert accepted.state.pending == {}

    revoke_before_shot = RevocationCommand.from_dict(
        {
            "revocation_id": "same-step-revoke",
            "player_id": "P1",
            "client_seq": 1,
            "arrival_step": 3,
            "arrival_seq": 0,
        }
    )
    rejected = Simulation(initial, [shot], [revoke_before_shot])
    rejected.run_to()
    assert rejected.state.revocations[0]["reason_code"] == "E_REVOKE_NOT_FOUND"
    assert rejected.state.covers["C01"].damage > 0

