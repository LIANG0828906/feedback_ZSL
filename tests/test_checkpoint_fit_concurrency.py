from __future__ import annotations

import json
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest
import torch

from inverse_render.checkpoint import CheckpointManager
from inverse_render.errors import ErrorCode, ToolError
from inverse_render.fit import FitConfig, FitService, run_fit
from inverse_render.graph import build_dependency_graph
from inverse_render.hashing import tensor_map_hash
from inverse_render.params import ParameterStore, load_group_specs
from inverse_render.rendering import full_render
from inverse_render.scene import load_scene
from inverse_render.snapshot import SnapshotPublisher


def _manager(sample: Path, state: Path) -> CheckpointManager:
    scene = load_scene(sample / "scene.json")
    groups = load_group_specs(scene, sample / "groups.json")
    graph = build_dependency_graph(scene, groups)
    return CheckpointManager(
        state,
        scene_hash=scene.scene_hash(),
        expected_graph_version=graph.version,
        expected_group_order=[group.id for group in groups],
        expected_group_parameters={
            group.id: group.parameters for group in groups
        },
    )


def test_checkpoint_chain_resume_and_no_repeated_steps(sample_root: Path, tmp_path: Path) -> None:
    sample = sample_root / "coupled_small"
    state = tmp_path / "state"
    first = run_fit(
        sample / "scene.json",
        sample / "groups.json",
        state,
        FitConfig(steps=3, learning_rate=0.02, schedule="rotate", fresh=True),
    )
    assert first.completed_step == 3
    assert first.steps_executed == 3
    manager = _manager(sample, state)
    resume = manager.load()
    assert resume is not None
    assert resume.step == 3
    assert [entry["step"] for entry in resume.history] == [0, 1, 2, 3]
    scene = load_scene(sample / "scene.json")
    groups = load_group_specs(scene, sample / "groups.json")
    restored_store = ParameterStore.from_checkpoint(scene, groups, resume.groups)
    restored_nodes, _ = full_render(restored_store, scene)
    for node in ("intersections", "visibility", "shadow_factor", "lighting", "loss"):
        assert tensor_map_hash(restored_nodes[node]) == resume.cache_hashes[node]

    trace_lines = [json.loads(line) for line in (state / "trace.jsonl").read_text().splitlines()]
    assert [line["step"] for line in trace_lines] == [0, 1, 2, 3]
    expected_trace_keys = [
        "step",
        "modified_groups",
        "affected_nodes",
        "reused_nodes",
        "recomputed_nodes",
        "loss",
        "gradient_norm",
        "content_hash",
        "graph_version",
        "degraded",
        "degraded_reason",
        "stale_nodes",
        "ui_badge",
    ]
    for record in trace_lines:
        assert list(record) == expected_trace_keys
        assert record["ui_badge"] == "OK"

    second = run_fit(
        sample / "scene.json",
        sample / "groups.json",
        state,
        FitConfig(steps=5, learning_rate=0.02, schedule="rotate", resume=True),
    )
    assert second.resumed_from == 3
    assert second.steps_executed == 2
    assert second.completed_step == 5
    resume_after = _manager(sample, state).load()
    assert resume_after is not None
    assert [entry["step"] for entry in resume_after.history] == [0, 1, 2, 3, 4, 5]
    trace_steps = [
        json.loads(line)["step"]
        for line in (state / "trace.jsonl").read_text().splitlines()
    ]
    assert trace_steps == [0, 1, 2, 3, 4, 5]


def test_identical_runs_are_byte_deterministic(sample_root: Path, tmp_path: Path) -> None:
    sample = sample_root / "coupled_small"
    outputs: list[tuple[bytes, bytes]] = []
    for run in ("a", "b"):
        state = tmp_path / run
        run_fit(
            sample / "scene.json",
            sample / "groups.json",
            state,
            FitConfig(steps=3, learning_rate=0.02, schedule="rotate", fresh=True),
        )
        outputs.append(
            (
                (state / "checkpoint.json").read_bytes(),
                (state / "trace.jsonl").read_bytes(),
            )
        )
    assert outputs[0] == outputs[1]


def test_checkpoint_rejections_locate_first_anomalous_step(sample_root: Path, tmp_path: Path) -> None:
    sample = sample_root / "coupled_small"
    state = tmp_path / "state"
    run_fit(
        sample / "scene.json",
        sample / "groups.json",
        state,
        FitConfig(steps=2, learning_rate=0.02, schedule="rotate", fresh=True),
    )
    checkpoint_path = state / "checkpoint.json"
    original = checkpoint_path.read_bytes()

    document = json.loads(original)
    del document["cumulative_loss"]
    checkpoint_path.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(ToolError) as caught:
        _manager(sample, state).load()
    assert caught.value.code == ErrorCode.E_CKPT_INVALID
    assert caught.value.details["step"] == 2
    assert caught.value.details["field"] == "cumulative_loss"

    document = json.loads(original)
    document["history"][1]["payload_hash"] = "0" * 64
    checkpoint_path.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(ToolError) as caught:
        _manager(sample, state).load()
    assert caught.value.code == ErrorCode.E_CKPT_INVALID
    assert caught.value.details["step"] == 1

    checkpoint_path.write_bytes(original)
    document = json.loads(original)
    del document["groups"]["material"]["material.primary.albedo"]
    checkpoint_path.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(ToolError) as caught:
        _manager(sample, state).load()
    assert caught.value.code == ErrorCode.E_CKPT_INVALID
    assert caught.value.details["step"] == 2
    assert caught.value.details["group"] == "material"

    checkpoint_path.write_bytes(original)
    (state / "checkpoint.json.tmp").write_bytes(b"{partial")
    valid = _manager(sample, state).load()
    assert valid is not None and valid.step == 2

    checkpoint_path.write_bytes(b'{"step":2,')
    with pytest.raises(ToolError) as caught:
        _manager(sample, state).load()
    assert caught.value.code == ErrorCode.E_CKPT_INVALID
    assert caught.value.details["step"] == 2


def test_stale_degradation_is_written_to_trace(sample_root: Path, tmp_path: Path) -> None:
    sample = sample_root / "coupled_small"
    state = tmp_path / "state"

    def inject_external_edit(step: int, store) -> None:
        if step == 1:
            with torch.no_grad():
                store.parameters["camera.position"].add_(0.013)

    run_fit(
        sample / "scene.json",
        sample / "groups.json",
        state,
        FitConfig(
            steps=2,
            learning_rate=0.01,
            schedule="material",
            fresh=True,
            step_hook=inject_external_edit,
        ),
    )
    trace = [json.loads(line) for line in (state / "trace.jsonl").read_text().splitlines()]
    step_one = trace[1]
    assert step_one["modified_groups"] == ["material"]
    assert step_one["degraded"] is True
    assert step_one["degraded_reason"] == "upstream_content_changed_without_invalidation"
    assert "intersections" in step_one["stale_nodes"]
    assert step_one["ui_badge"] == "FULL_RECOMPUTE"
    assert step_one["reused_nodes"] == 0
    assert step_one["recomputed_nodes"] == 5


def test_forced_termination_and_disk_resume(sample_root: Path, tmp_path: Path) -> None:
    sample = sample_root / "coupled_small"
    state = tmp_path / "state"
    base = [
        sys.executable,
        "-m",
        "inverse_render.cli",
        "fit",
        "--scene",
        str(sample / "scene.json"),
        "--groups",
        str(sample / "groups.json"),
        "--state-dir",
        str(state),
        "--steps",
        "4",
        "--lr",
        "0.02",
        "--schedule",
        "rotate",
    ]
    crashed = subprocess.run(
        base + ["--fresh", "--crash-after", "2"],
        text=True,
        capture_output=True,
        check=False,
    )
    assert crashed.returncode == 137
    crashed_document = json.loads((state / "checkpoint.json").read_text())
    assert [entry["step"] for entry in crashed_document["history"]] == [0, 1, 2]
    assert [json.loads(line)["step"] for line in (state / "trace.jsonl").read_text().splitlines()] == [
        0,
        1,
        2,
    ]

    resumed = subprocess.run(base, text=True, capture_output=True, check=False)
    assert resumed.returncode == 0, resumed.stderr
    payload = json.loads(resumed.stdout)
    assert payload["resumed_from"] == 2
    assert payload["steps_executed"] == 2
    assert payload["completed_step"] == 4
    document = json.loads((state / "checkpoint.json").read_text())
    assert [entry["step"] for entry in document["history"]] == [0, 1, 2, 3, 4]
    trace_steps = [
        json.loads(line)["step"] for line in (state / "trace.jsonl").read_text().splitlines()
    ]
    assert trace_steps == [0, 1, 2, 3, 4]

    direct_state = tmp_path / "direct"
    direct = subprocess.run(
        [
            sys.executable,
            "-m",
            "inverse_render.cli",
            "fit",
            "--scene",
            str(sample / "scene.json"),
            "--groups",
            str(sample / "groups.json"),
            "--state-dir",
            str(direct_state),
            "--steps",
            "4",
            "--lr",
            "0.02",
            "--schedule",
            "rotate",
            "--fresh",
        ],
        text=True,
        capture_output=True,
        check=False,
    )
    assert direct.returncode == 0, direct.stderr
    assert (state / "checkpoint.json").read_bytes() == (
        direct_state / "checkpoint.json"
    ).read_bytes()
    assert (state / "trace.jsonl").read_bytes() == (
        direct_state / "trace.jsonl"
    ).read_bytes()


def test_busy_and_parallel_queries_with_stale_handles(sample_root: Path, tmp_path: Path) -> None:
    sample = sample_root / "coupled_small"
    state = tmp_path / "state"
    first = FitService(sample / "scene.json", sample / "groups.json", state)
    first.start(FitConfig(steps=200, learning_rate=0.001, schedule="rotate", fresh=True))
    second = FitService(sample / "scene.json", sample / "groups.json", tmp_path / "other")
    with pytest.raises(ToolError) as caught:
        second.start(FitConfig(steps=1))
    assert caught.value.code == ErrorCode.E_BUSY

    early = None
    deadline = time.time() + 5.0
    while time.time() < deadline:
        try:
            early = first.query()
            if early.step >= 1:
                break
        except ToolError as exc:
            assert exc.code == ErrorCode.E_SNAPSHOT_STALE
        time.sleep(0.005)
    assert early is not None
    assert early.step >= 1
    assert early.groups
    first.cancel()
    result = first.wait(timeout=10.0)
    assert result.status == "cancelled"
    assert not (state / "checkpoint.json.tmp").exists()
    latest = first.query()
    assert latest.step == result.completed_step
    with pytest.raises(ToolError) as caught:
        first.query(early.handle)
    assert caught.value.code == ErrorCode.E_SNAPSHOT_STALE


def test_snapshot_unpublished_and_replaced_handles_are_stale() -> None:
    publisher = SnapshotPublisher()
    common = {
        "last_loss": 1.0,
        "cumulative_loss": 1.0,
        "graph_version": "graph",
        "cache_hashes": {"loss": "0" * 64},
        "checkpoint_hash": "1" * 64,
    }
    first = publisher.publish(step=1, groups={"g": {"x": 1.0}}, **common)
    prepared = publisher.prepare(step=2, groups={"g": {"x": 2.0}}, **common)
    with pytest.raises(ToolError) as caught:
        publisher.query(prepared.handle)
    assert caught.value.code == ErrorCode.E_SNAPSHOT_STALE
    assert publisher.query(first.handle).step == 1
    second = publisher.publish(prepared)
    assert publisher.query(second.handle).step == 2
    with pytest.raises(ToolError) as caught:
        publisher.query(first.handle)
    assert caught.value.code == ErrorCode.E_SNAPSHOT_STALE

