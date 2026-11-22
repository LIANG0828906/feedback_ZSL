from __future__ import annotations

import json

from diffrender.compare import assert_reports_equal
from diffrender.checkpoint import CheckpointStore
from diffrender.optimize import OptimizationRunner, RunConfig


def test_checkpoint_hash_and_version_invalidation_recomputes(dataset, tmp_path):
    task = dataset.task("single")
    config = RunConfig(
        iterations=4,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=2,
        clip_enabled=True,
    )
    store_dir = tmp_path / "checkpoints"
    runner = OptimizationRunner(task, config, checkpoint_dir=store_dir)
    runner.run_to(4)
    store = CheckpointStore(store_dir)
    valid = store.load(
        expected_config_fingerprint=runner.config_fingerprint,
        expected_dataset_fingerprint=task.source_fingerprint,
    )
    assert valid.valid
    payload = json.loads(store.path.read_text(encoding="utf-8"))
    assert payload["config"]["clip_enabled"] is True
    assert payload["parameter_version"] == 4

    payload["loss_history"][0] += 1.0
    store.path.write_text(
        json.dumps(payload, ensure_ascii=False, sort_keys=True),
        encoding="utf-8",
    )
    invalid = store.load(
        expected_config_fingerprint=runner.config_fingerprint,
        expected_dataset_fingerprint=task.source_fingerprint,
    )
    assert not invalid.valid
    assert invalid.reason == "content_hash_mismatch"
    assert invalid.requires_full_recompute

    resumed = OptimizationRunner(task, config, checkpoint_dir=store_dir)
    load_info = resumed.load_checkpoint()
    assert load_info["action"] == "full_recompute"
    resumed.run_to(4)
    reference = OptimizationRunner(task, config)
    reference.run_to(4)
    assert_reports_equal(
        reference.report().to_dict(),
        resumed.report().to_dict(),
        compare_gradient_history=True,
        label="checkpoint fallback",
    )


def test_checkpoint_config_version_mismatch_is_global(dataset, tmp_path):
    task = dataset.task("single")
    config = RunConfig(
        iterations=2,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=1,
        clip_enabled=True,
    )
    runner = OptimizationRunner(task, config, checkpoint_dir=tmp_path)
    runner.run_to(2)
    other = RunConfig(
        iterations=2,
        learning_rate=0.006,
        mode="exact",
        checkpoint_every=1,
        clip_enabled=False,
    )
    other_runner = OptimizationRunner(task, other)
    result = CheckpointStore(tmp_path).load(
        expected_config_fingerprint=other_runner.config_fingerprint,
        expected_dataset_fingerprint=task.source_fingerprint,
    )
    assert not result.valid
    assert result.reason == "version_mismatch:config_fingerprint"
    assert result.payload is None

