from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from lightprobe.bake import Baker
from lightprobe.errors import ProbeError
from lightprobe.snapshot import load_snapshot
from lightprobe.util import canonical_json, sha256_json

from conftest import ROOT, fast_config


def test_snapshot_roundtrip_reuses_all_committed_probes(room, tmp_path):
    config = fast_config(room)
    uninterrupted = Baker().bake(room, config)
    snapshot = tmp_path / "snapshot"
    written = Baker().bake(
        room,
        config,
        snapshot_dir=snapshot,
        target_version=4,
        run_id="roundtrip",
    )
    recovered = load_snapshot(snapshot)
    assert recovered.chunk_count == 5
    assert len(recovered.records) == written.report.probe_count
    assert recovered.metadata["target_version"] == 4
    resumed = Baker().bake(
        room,
        config,
        recovered=recovered,
        snapshot_dir=tmp_path / "resumed",
        target_version=4,
        run_id="roundtrip",
    )
    assert resumed.report.recomputed_count == 0
    assert resumed.report.reused_count == written.report.probe_count
    assert resumed.state.content_hash() == uninterrupted.state.content_hash()


def test_corrupt_chunk_stops_recovery(room, tmp_path):
    config = fast_config(room)
    snapshot = tmp_path / "snapshot"
    Baker().bake(
        room, config, snapshot_dir=snapshot, target_version=2, run_id="corrupt"
    )
    chunk_path = snapshot / "chunk_000000.json"
    payload = json.loads(chunk_path.read_text(encoding="utf-8"))
    payload["records"][0]["coefficients"][0][0] += 10.0
    chunk_path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(ProbeError) as caught:
        load_snapshot(snapshot)
    assert caught.value.code == "E_RECOVERY_INVALID"


def test_missing_chunk_field_stops_recovery(room, tmp_path):
    config = fast_config(room)
    snapshot = tmp_path / "snapshot"
    Baker().bake(
        room, config, snapshot_dir=snapshot, target_version=2, run_id="missing"
    )
    chunk_path = snapshot / "chunk_000000.json"
    payload = json.loads(chunk_path.read_text(encoding="utf-8"))
    del payload["records"][0]["visibility_fraction"]
    unsigned = dict(payload)
    unsigned.pop("chunk_hash")
    payload["chunk_hash"] = sha256_json(unsigned)
    chunk_path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(ProbeError) as caught:
        load_snapshot(snapshot)
    assert caught.value.code == "E_RECOVERY_INVALID"
    assert "missing fields" in caught.value.message


def test_forced_termination_recovers_only_committed_chunks(room, tmp_path):
    scene_path = tmp_path / "room.json"
    scene_path.write_text(json.dumps(room.to_dict()), encoding="utf-8")
    snapshot = tmp_path / "snapshot"
    code = (
        "import sys\n"
        "from lightprobe.scene import load_scene\n"
        "from lightprobe.bake import Baker\n"
        "from lightprobe.bake_types import BakeConfig\n"
        "scene=load_scene(sys.argv[1])\n"
        "config=BakeConfig.from_scene(scene, validation_probe_limit=8, "
        "validation_direction_count=64, chunk_size=16)\n"
        "Baker().bake(scene, config, snapshot_dir=sys.argv[2], target_version=5, "
        "run_id='kill-test', crash_after_chunks=2)\n"
    )
    crashed = subprocess.run(
        [sys.executable, "-c", code, str(scene_path), str(snapshot)],
        cwd=ROOT,
        check=False,
    )
    assert crashed.returncode == 137
    partial = load_snapshot(snapshot)
    assert partial.chunk_count == 2
    assert 0 < len(partial.records) < room_grid_count()

    config = fast_config(room)
    resumed = Baker().bake(
        room,
        config,
        recovered=partial,
        snapshot_dir=tmp_path / "resumed",
        target_version=5,
        run_id="kill-test",
    )
    uninterrupted = Baker().bake(room, config, target_version=5)
    assert resumed.report.reused_count == len(partial.records)
    assert resumed.report.recomputed_count == len(uninterrupted.state.records) - len(partial.records)
    assert resumed.state.content_hash() == uninterrupted.state.content_hash()
    assert not list(snapshot.glob("*.tmp-*"))


def room_grid_count() -> int:
    from lightprobe.grid import build_grid
    from lightprobe.samples import room_scene
    from lightprobe.scene import load_scene_dict

    return build_grid(load_scene_dict(room_scene()), 2.0).probe_count

