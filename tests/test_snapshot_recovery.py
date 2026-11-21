from __future__ import annotations

from pathlib import Path

import pytest

from tactical_review.errors import SnapshotError
from tactical_review.simulation import Simulation
from tactical_review.snapshot import SnapshotStore, inject_snapshot_fault


def test_resume_from_snapshot_does_not_reapply_steps(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    interrupted = Simulation(initial, shots, revocations)
    for _ in range(17):
        interrupted.advance()
        store.write(interrupted)
    recovered = store.load_simulation(initial, shots, revocations)
    assert recovered.simulation.state.step == 17
    assert recovered.simulation.next_step == 18

    recovered.simulation.run_to()
    full = Simulation(initial, shots, revocations)
    full.run_to()
    assert recovered.simulation.state.hash_chain == full.state.hash_chain
    assert recovered.simulation.state.step == full.state.step
    assert recovered.simulation.state.duplicate_count == full.state.duplicate_count
    assert (
        recovered.simulation.state.operational_reports
        == full.state.operational_reports
    )


@pytest.mark.parametrize(
    "fault",
    ["missing_field", "hash_break", "durability_tamper", "prefix_digest"],
)
def test_recovery_faults_report_first_broken_step(
    tmp_path: Path, loaded_sample, fault: str
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / f"snapshots-{fault}")
    simulation = Simulation(initial, shots, revocations)
    for _ in range(7):
        simulation.advance()
        store.write(simulation)
    with pytest.raises(SnapshotError) as info:
        store.load_simulation(
            initial,
            shots,
            revocations,
            fault=fault,
            fault_step=7,
        )
    assert info.value.code == "E_RECOVERY_INVALID"
    assert info.value.details.get("step") == 7


def test_disk_corruption_is_detected_and_located(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    simulation = Simulation(initial, shots, revocations)
    for _ in range(5):
        simulation.advance()
        store.write(simulation)
    inject_snapshot_fault(store, "hash_break", step=5)
    with pytest.raises(SnapshotError) as info:
        store.load_simulation(initial, shots, revocations)
    assert info.value.code == "E_RECOVERY_INVALID"
    assert info.value.details.get("step") == 5


def test_snapshot_publication_is_atomic_and_complete(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    simulation = Simulation(initial, shots, revocations)
    for _ in range(4):
        simulation.advance()
        store.write(simulation)
    assert store.list_steps() == [1, 2, 3, 4]
    assert not list(store.directory.glob("*.tmp"))
    assert not list(store.directory.glob(".*.tmp"))
    data, _ = store.load_latest(initial)
    assert {
        "step",
        "state",
        "summary",
        "hash_chain",
        "chain_prefix_digest",
        "buffer_watermark",
    } <= set(data)

