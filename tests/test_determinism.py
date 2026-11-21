from __future__ import annotations

import random
from dataclasses import replace
from pathlib import Path

from tactical_review.model import GENESIS_HASH, InitialState, load_shots
from tactical_review.samplegen import generate_sample
from tactical_review.simulation import Simulation


def test_sample_generation_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    kwargs = {
        "seed": 7788,
        "cover_count": 6,
        "shot_count": 24,
        "end_step": 26,
        "step_ms": 40,
        "compensation_window": 3,
        "include_anomalies": True,
    }
    manifest_a = generate_sample(first, **kwargs)
    manifest_b = generate_sample(second, **kwargs)
    assert manifest_a == manifest_b
    for name in (
        "initial_state.json",
        "shots.json",
        "revocations.json",
        "baseline.json",
        "manifest.json",
    ):
        assert (first / name).read_bytes() == (second / name).read_bytes()


def test_record_list_order_does_not_change_step_hashes(
    sample_dir: Path, loaded_sample
) -> None:
    initial, _, _ = loaded_sample
    clean_a = sample_dir / "clean-a.json"
    clean_b = sample_dir / "clean-b.json"
    generate_sample(
        clean_a,
        seed=3344,
        cover_count=6,
        shot_count=20,
        end_step=22,
        include_anomalies=False,
    )
    generate_sample(
        clean_b,
        seed=3344,
        cover_count=6,
        shot_count=20,
        end_step=22,
        include_anomalies=False,
    )
    initial = InitialState.load(str(clean_a / "initial_state.json"))
    _, ordered = load_shots(str(clean_a / "shots.json"))
    _, second_copy = load_shots(str(clean_b / "shots.json"))
    shuffled = list(second_copy)
    random.Random(99).shuffle(shuffled)

    first = Simulation(initial, ordered)
    second = Simulation(initial, shuffled)
    first.run_to()
    second.run_to()
    assert first.state.hash_chain == second.state.hash_chain
    assert first.state.hash_chain[0]["previous_hash"] == GENESIS_HASH


def test_repeated_replay_is_chain_identical(sample_dir: Path, loaded_sample) -> None:
    initial, shots, revocations = loaded_sample
    first = Simulation(initial, shots, revocations)
    second = Simulation(initial, shots, revocations)
    first.run_to()
    second.run_to()
    assert first.state.hash_chain == second.state.hash_chain
    assert first.state.revocations == second.state.revocations
    assert first.state.operational_reports == second.state.operational_reports


def test_arrival_sequence_permutation_does_not_move_cracks(
    tmp_path: Path,
) -> None:
    sample = tmp_path / "arrival-sample"
    generate_sample(
        sample,
        seed=5150,
        cover_count=6,
        shot_count=18,
        end_step=12,
        include_anomalies=False,
    )
    initial = InitialState.load(str(sample / "initial_state.json"))
    _, shots = load_shots(str(sample / "shots.json"))
    synchronized = [
        replace(shot, arrival_step=6, target_step=6)
        for shot in shots
    ]
    count = len(synchronized)
    forward = [
        replace(shot, arrival_seq=index)
        for index, shot in enumerate(synchronized, start=1)
    ]
    reversed_arrival = [
        replace(shot, arrival_seq=count - index + 1)
        for index, shot in enumerate(synchronized, start=1)
    ]
    first = Simulation(initial, list(reversed(forward)))
    second = Simulation(initial, list(reversed_arrival))
    first.run_to()
    second.run_to()
    assert first.state.hash_chain == second.state.hash_chain
    for cover_id in initial.cover_ids:
        first_cover = first.state.covers[cover_id]
        second_cover = second.state.covers[cover_id]
        assert first_cover.remaining == second_cover.remaining
        assert first_cover.crack_events == second_cover.crack_events


