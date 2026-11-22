from __future__ import annotations

import pytest
import torch

from diffrender.blocks import (
    BlockLedger,
    BlockRecord,
    execute_block_round,
    make_full_image_specs,
)
from diffrender.errors import AppError
from diffrender.serialization import tensor_content_hash


def _specs(count: int = 2):
    entries = [
        (index, f"target{index}", f"cam{index}", 4, 4)
        for index in range(count)
    ]
    return make_full_image_specs(entries, base_seed=42)


def _record(state, spec, snapshot_hash: str, status: str = "committed"):
    return BlockRecord(
        round_index=state.round_index,
        sequence=spec.sequence,
        block_id=spec.block_id,
        status=status,
        snapshot_hash=snapshot_hash,
        parameter_version=state.parameter_version,
        shadow_seed=spec.shadow_seed,
        attempts=1,
        loss=0.0,
        gradient_norm=0.0,
        duration_ms=0.0,
    )


def test_illegal_order_and_snapshot_drift_preserve_state():
    ledger = BlockLedger()
    specs = _specs(2)
    snapshot_hash = tensor_content_hash(torch.zeros(3))
    state = ledger.begin_round(0, specs, snapshot_hash, 0)
    with pytest.raises(AppError) as caught:
        ledger.submit(
            _record(state, specs[1], snapshot_hash),
            torch.zeros(3),
        )
    assert caught.value.code == "E_STATE"
    assert state.committed_count == 0
    assert not state.records

    ledger.submit(_record(state, specs[0], snapshot_hash), torch.zeros(3))
    before_count = state.committed_count
    drifted = _record(state, specs[1], "different-snapshot")
    with pytest.raises(AppError) as caught:
        ledger.submit(drifted, torch.zeros(3))
    assert caught.value.code == "E_STATE"
    assert caught.value.field_path == "snapshot_hash"
    assert state.committed_count == before_count
    assert len(state.records) == 1


def test_round_snapshot_is_stored_and_matches_hash():
    ledger = BlockLedger()
    specs = _specs(1)
    snapshot = torch.tensor([0.25, 0.5, 0.75], dtype=torch.float64)
    state = ledger.begin_round(
        0,
        specs,
        tensor_content_hash(snapshot),
        0,
        snapshot=snapshot,
    )
    assert state.snapshot is not None
    assert torch.equal(state.snapshot, snapshot)
    assert tensor_content_hash(state.snapshot) == state.snapshot_hash



def test_budget_blocks_later_blocks_and_fault_recovers():
    ledger = BlockLedger()
    specs = _specs(3)
    snapshot = torch.tensor([1.0, 2.0, 3.0], dtype=torch.float64)
    fired = {"count": 0}

    def block_function(spec, local_snapshot):
        return local_snapshot.square().sum() * 0.001

    def fault(stage, iteration, spec, attempt):
        if stage == "backward" and spec.sequence == 1 and fired["count"] == 0:
            fired["count"] += 1
            raise AppError("E_BLOCK", "transient", "fault")

    result = execute_block_round(
        ledger=ledger,
        round_index=0,
        parameter_version=0,
        snapshot=snapshot,
        specs=specs,
        block_function=block_function,
        buffer_cost=lambda spec: 1,
        buffer_limit_elements=1,
        max_parallel_blocks=2,
        max_retries=1,
        fault_function=fault,
    )
    assert result.budget_waits >= 1
    assert result.retries == 1
    assert ledger.rounds[0].committed_count == 3
    assert any(record.status == "failed" for record in ledger.rounds[0].records)
    assert torch.isfinite(result.gradient).all()

