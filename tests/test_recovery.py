from __future__ import annotations

import json

import pytest

from inverse_imaging.checkpoint import CheckpointManager
from inverse_imaging.engine import FittingConfig, FittingSession
from inverse_imaging.errors import E_RECOVERY_INVALID, AppError
from inverse_imaging.params import GROUPS, canonical_json_bytes, sha256_bytes


def _config(directory, steps=4, terminate_after=None):
    return FittingConfig(
        steps=steps,
        learning_rate=0.01,
        active_groups=GROUPS,
        chunk_size=3,
        parallel=2,
        verify_incremental=True,
        checkpoint_dir=str(directory),
        termination_after_step=terminate_after,
    )


def test_forced_termination_resume_matches_uninterrupted(scene, tmp_path) -> None:
    uninterrupted_dir = tmp_path / "uninterrupted"
    interrupted_dir = tmp_path / "interrupted"

    uninterrupted = FittingSession(scene, _config(uninterrupted_dir)).run_sync()
    first = FittingSession(
        scene, _config(interrupted_dir, terminate_after=1)
    ).run_sync()
    resumed = FittingSession(scene, _config(interrupted_dir)).run_sync()

    assert uninterrupted.status == "completed"
    assert first.status == "terminated"
    assert resumed.status == "completed"
    assert (uninterrupted_dir / "trajectory.jsonl").read_bytes() == (
        interrupted_dir / "trajectory.jsonl"
    ).read_bytes()
    left = {
        path.name: path.read_bytes()
        for path in sorted((uninterrupted_dir / "checkpoints").iterdir())
    }
    right = {
        path.name: path.read_bytes()
        for path in sorted((interrupted_dir / "checkpoints").iterdir())
    }
    assert left == right


def test_recovery_does_not_repeat_committed_step(scene, tmp_path) -> None:
    checkpoint_dir = tmp_path / "chain"
    first = FittingSession(
        scene, _config(checkpoint_dir, terminate_after=0)
    ).run_sync()
    latest = CheckpointManager(checkpoint_dir, scene.scene_id).load_latest()
    assert first.status == "terminated"
    assert latest.step == 0
    before = (checkpoint_dir / "trajectory.jsonl").read_text().count("\n")
    resumed = FittingSession(scene, _config(checkpoint_dir)).run_sync()
    assert resumed.status == "completed"
    after = (checkpoint_dir / "trajectory.jsonl").read_text().count("\n")
    assert after == 4
    assert before == 1


def test_broken_hash_chain_is_rejected(scene, tmp_path) -> None:
    checkpoint_dir = tmp_path / "broken"
    FittingSession(scene, _config(checkpoint_dir, terminate_after=1)).run_sync()
    manager = CheckpointManager(checkpoint_dir, scene.scene_id)
    path = checkpoint_dir / "checkpoints" / "checkpoint-00000001.json"
    payload = json.loads(path.read_text())
    payload.pop("self_hash")
    payload["prev_hash"] = "0" * 64
    payload["self_hash"] = sha256_bytes(canonical_json_bytes(payload))
    path.write_text(json.dumps(payload))
    with pytest.raises(AppError) as error:
        manager.load_latest()
    assert error.value.code == E_RECOVERY_INVALID
    assert error.value.field == "prev_hash"


def test_content_tamper_is_rejected(scene, tmp_path) -> None:
    checkpoint_dir = tmp_path / "tamper"
    FittingSession(scene, _config(checkpoint_dir, terminate_after=0)).run_sync()
    manager = CheckpointManager(checkpoint_dir, scene.scene_id)
    path = checkpoint_dir / "checkpoints" / "checkpoint-00000000.json"
    payload = json.loads(path.read_text())
    payload["cumulative_loss"] += 1.0
    path.write_text(json.dumps(payload))
    with pytest.raises(AppError) as error:
        manager.load_latest()
    assert error.value.code == E_RECOVERY_INVALID
    assert error.value.field == "self_hash"

