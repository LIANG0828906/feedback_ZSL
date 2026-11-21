from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

from studio_calibrator.config import load_config, parse_config
from studio_calibrator.engine import AnalysisEngine, RecomputeManager
from studio_calibrator.snapshot import SnapshotStore
from studio_calibrator.util import canonical_json_bytes


def _manager(sample: Path, root: Path) -> RecomputeManager:
    return RecomputeManager(
        AnalysisEngine(sample / "manifest.json", sample / "geometry.json"),
        SnapshotStore(root),
    )


def test_target_change_reuses_unaffected_nodes(
    sample: Path, baseline: dict, tmp_path: Path
) -> None:
    manager = _manager(sample, tmp_path / "snapshots_target")
    base = manager.recompute(load_config(sample / "config.json"), tmp_path / "trace_1.jsonl")
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["target_suppression_db"] = 13.0
    changed = manager.recompute(parse_config(raw), tmp_path / "trace_2.jsonl")
    assert changed.incremental["changed_inputs"] == ["target_suppression_db"]
    stats = changed.incremental["cache_stats"]
    assert stats["alignment"] == {"cache_hits": 36, "recomputed": 0}
    assert stats["coupling"] == {"cache_hits": 216, "recomputed": 0}
    assert stats["route_matrix"] == {"cache_hits": 6, "recomputed": 0}
    assert stats["phase"]["cache_hits"] >= 30
    assert stats["budget"] == {"cache_hits": 0, "recomputed": 6}
    assert stats["calibration"] == {"cache_hits": 0, "recomputed": 6}
    assert changed.coupling_diff["compared"] == 216
    assert changed.coupling_diff["max_deviation_score"] == 0.0
    assert changed.content_hash_value != base.content_hash_value


def test_boundary_change_recomputes_only_affected_bands(
    sample: Path, tmp_path: Path
) -> None:
    manager = _manager(sample, tmp_path / "snapshots_band")
    manager.recompute(load_config(sample / "config.json"), tmp_path / "trace_1.jsonl")
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["bands"][2]["high_hz"] = 1800.0
    raw["bands"][3]["low_hz"] = 1800.0
    changed = manager.recompute(parse_config(raw), tmp_path / "trace_2.jsonl")
    plan = changed.incremental["nodes"]
    assert plan["coupling"]["recomputed_bands"] == ["B2", "B3"]
    assert plan["coupling"]["cache_hits"] == ["B0", "B1", "B4", "B5"]
    assert changed.incremental["cache_stats"]["coupling"] == {
        "cache_hits": 144,
        "recomputed": 72,
    }
    assert plan["phase"]["recomputed_bands"] == [
        "B0",
        "B1",
        "B2",
        "B3",
        "B4",
        "B5",
    ]


def test_snapshot_restore_reproduces_route_and_calibration_bytes(
    baseline: dict,
) -> None:
    store = SnapshotStore(baseline["snapshot_dir"])
    first = store.load_current()
    second = SnapshotStore(baseline["snapshot_dir"]).load_current()
    assert first["snapshot_id"] == second["snapshot_id"]
    assert first["route_matrix_bytes"] == second["route_matrix_bytes"]
    assert first["calibration_bytes"] == second["calibration_bytes"]
    assert first["route_matrix_bytes"] == canonical_json_bytes(
        first["state"]["route_matrix"]
    )
    assert first["calibration_bytes"] == canonical_json_bytes(
        first["state"]["calibration"]
    )
    assert first["metadata"]["state_hash"] == baseline["state"].content_hash_value


def test_sigkill_before_commit_restores_only_committed_snapshot(
    sample: Path, baseline: dict, tmp_path: Path
) -> None:
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["target_suppression_db"] = 13.0
    config_path = tmp_path / "changed.json"
    config_path.write_text(
        json.dumps(raw, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )
    env = dict(os.environ)
    env["STUDIO_CALIBRATOR_TEST_CRASH_AT"] = "before-commit"
    command = [
        sys.executable,
        "-m",
        "studio_calibrator.cli",
        "diagnose",
        "--manifest",
        str(sample / "manifest.json"),
        "--geometry",
        str(sample / "geometry.json"),
        "--config",
        str(config_path),
        "--snapshot-dir",
        str(baseline["snapshot_dir"]),
        "--trace",
        str(tmp_path / "crashed_trace.jsonl"),
        "--out",
        str(tmp_path / "crashed.json"),
    ]
    result = subprocess.run(command, cwd=Path.cwd(), env=env, check=False)
    assert result.returncode < 0
    restored = SnapshotStore(baseline["snapshot_dir"]).load_current()
    assert restored["snapshot_id"] == baseline["snapshot"]["snapshot_id"]
    assert (
        restored["metadata"]["state_hash"] == baseline["state"].content_hash_value
    )
    committed = SnapshotStore(baseline["snapshot_dir"]).committed_ids()
    assert baseline["snapshot"]["snapshot_id"] in committed
