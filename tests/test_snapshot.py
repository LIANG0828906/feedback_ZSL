from __future__ import annotations

import json
from pathlib import Path

import pytest

from streamforge import Pipeline
from streamforge.errors import BusinessError, ErrorCode
from streamforge.models import sha256_json
from tests.conftest import read_json, write_json


def test_snapshot_restore_reuses_consistent_chain(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    snapshot = first.snapshot_path
    first_snapshot_bytes = snapshot.read_bytes()
    original_peak = first.snapshot_document["memory"]["peak_bytes"]
    second = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run(restore_snapshot=snapshot)
    assert second.restored
    assert second.status == "complete"
    assert set(second.reused_tiers) == {"low", "mid", "high"}
    assert second.recomputed_tiers == []
    assert second.snapshot_document["memory"]["peak_bytes"] == original_peak
    assert snapshot.read_bytes() == first_snapshot_bytes


def test_snapshot_restore_recomputes_only_changed_tier(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    first_hashes = {
        tier_id: [artifact.content_hash for artifact in artifacts]
        for tier_id, artifacts in first.artifacts.items()
    }
    changed_config, changed_budget = config_factory(
        tier_updates={"high": {"video_qp": 31}},
        config_version="demo-v1-changed-high",
    )
    second = Pipeline(
        config_path=changed_config,
        budget_path=changed_budget,
        output_root=output,
    ).run(restore_snapshot=first.snapshot_path)
    assert "high" in second.recomputed_tiers
    assert set(second.reused_tiers) == {"low", "mid"}
    second_hashes = {
        tier_id: [artifact.content_hash for artifact in artifacts]
        for tier_id, artifacts in second.artifacts.items()
    }
    assert second_hashes["low"] == first_hashes["low"]
    assert second_hashes["mid"] == first_hashes["mid"]
    assert second_hashes["high"] != first_hashes["high"]


def test_snapshot_boundary_change_invalidates_all_tiers(
    config_factory, sample_bundle, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    from streamforge.samples import generate_samples

    longer_root = tmp_path / "longer_samples"
    longer = generate_samples(longer_root, seed=20261121, duration_ms=1600)
    changed_config, changed_budget = config_factory(
        source_audio=longer["audio"],
        source_video=longer["video"],
        config_version="demo-v1-longer",
    )
    second = Pipeline(
        config_path=changed_config,
        budget_path=changed_budget,
        output_root=output,
    ).run(restore_snapshot=first.snapshot_path)
    assert len(second.timeline.boundaries) == 4
    assert set(second.recomputed_tiers) == {"low", "mid", "high"}


def test_snapshot_tamper_and_missing_chain_are_rejected(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    tampered = tmp_path / "tampered.json"
    document = read_json(first.snapshot_path)
    document["memory"]["peak_bytes"] += 1
    write_json(tampered, document)
    with pytest.raises(BusinessError) as caught:
        Pipeline(
            config_path=config, budget_path=budget, output_root=output
        ).run(restore_snapshot=tampered)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID

    (output / "high" / "slice-00000.fslice").unlink()
    with pytest.raises(BusinessError) as caught:
        Pipeline(
            config_path=config, budget_path=budget, output_root=output
        ).run(restore_snapshot=first.snapshot_path)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID


def test_missing_snapshot_field_is_recovery_invalid(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    document = read_json(first.snapshot_path)
    del document["scheduler"]["waiting_queue"]
    bad = tmp_path / "missing.json"
    write_json(bad, document)
    with pytest.raises(BusinessError) as caught:
        Pipeline(
            config_path=config, budget_path=budget, output_root=output
        ).run(restore_snapshot=bad)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID


def test_complete_but_short_snapshot_chain_is_recovery_invalid(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    document = read_json(first.snapshot_path)
    document["tiers"]["low"]["completed"].pop()
    document["snapshot_hash"] = sha256_json(
        {key: value for key, value in document.items() if key != "snapshot_hash"}
    )
    short = tmp_path / "short.json"
    write_json(short, document)
    with pytest.raises(BusinessError) as caught:
        Pipeline(
            config_path=config, budget_path=budget, output_root=output
        ).run(restore_snapshot=short)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
