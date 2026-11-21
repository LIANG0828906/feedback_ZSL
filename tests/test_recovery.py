from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from haptic_studio.errors import ErrorCode, HapticError
from haptic_studio.models import DeviceConfig, WaveSequence
from haptic_studio.persistence import SnapshotState, SnapshotStore
from haptic_studio.session import HapticSession


def test_snapshot_roundtrip_without_pending(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    data_dir = tmp_path / "runtime"
    session = HapticSession.create(sequence, config, data_dir)
    changed = DeviceConfig.from_mapping(
        {**config.to_dict(), "allocation_ratio": [0.61, 0.39]}
    )
    latest = session.recompute(sequence, new_config=changed)
    reopened = HapticSession.open(data_dir)
    assert reopened.latest_publication.publication_version == latest.publication_version
    assert reopened.latest_publication.version_hash == latest.version_hash
    assert reopened.recovery_report.status == "clean"
    assert not list(data_dir.glob(".session.snapshot.json.*.tmp"))
    verification = reopened.verify_historical(2)
    assert verification["match"]


def test_recover_snapshot_written_before_first_publication(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    data_dir = tmp_path / "runtime"
    session = HapticSession.create(sequence, config, data_dir)
    store = SnapshotStore(data_dir)
    state = store.load()
    store.save(
        SnapshotState(
            config_versions=state.config_versions,
            publications=(),
            pending=None,
        )
    )
    reopened = HapticSession.open(
        data_dir, sequence=sequence, config=config
    )
    assert reopened.recovery_report.status == "resumed_empty"
    assert reopened.latest_publication.publication_version == 1
    assert reopened.latest_publication.signal_hash == session.latest_publication.signal_hash


def test_empty_snapshot_without_sequence_is_recovery_invalid(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    data_dir = tmp_path / "runtime"
    HapticSession.create(sequence, config, data_dir)
    store = SnapshotStore(data_dir)
    state = store.load()
    store.save(
        SnapshotState(
            config_versions=state.config_versions,
            publications=(),
            pending=None,
        )
    )
    with pytest.raises(HapticError) as caught:
        HapticSession.open(data_dir)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
    assert caught.value.version == 1
    assert caught.value.field == "publications"


def test_crash_recovery_demo_resumes_checkpointed_segments(tmp_path: Path) -> None:
    output = tmp_path / "crash"
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "haptic_studio",
            "crash-recover",
            "--out",
            str(output),
            "--segments",
            "9",
            "--crash-after",
            "2",
        ],
        check=False,
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert completed.returncode == 0, completed.stderr
    payload = json.loads((output / "crash_recovery.json").read_text(encoding="utf-8"))
    assert payload["child_exit_code"] in (-9, 137)
    assert payload["recovery"]["status"] == "resumed_pending"
    assert payload["recovery"]["restored_checkpoint_segments"] == [0, 1]
    assert payload["recovery"]["recomputed_segments"] == [2, 3, 4, 5, 6, 7, 8]
    assert payload["verify_latest"]["match"]


def test_snapshot_missing_field_reports_first_broken_version(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    data_dir = tmp_path / "runtime"
    session = HapticSession.create(sequence, config, data_dir)
    changed = DeviceConfig.from_mapping({**config.to_dict(), "limiter": "soft"})
    session.recompute(sequence, new_config=changed)
    snapshot_path = data_dir / "session.snapshot.json"
    payload = json.loads(snapshot_path.read_text(encoding="utf-8"))
    payload["publications"][1].pop("recomputed_segments")
    snapshot_path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(HapticError) as caught:
        HapticSession.open(data_dir)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
    assert caught.value.version == 2
    assert caught.value.field == "recomputed_segments"


def test_snapshot_hash_chain_break_is_rejected(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    data_dir = tmp_path / "runtime"
    session = HapticSession.create(sequence, config, data_dir)
    changed = DeviceConfig.from_mapping({**config.to_dict(), "limiter": "soft"})
    session.recompute(sequence, new_config=changed)
    snapshot_path = data_dir / "session.snapshot.json"
    payload = json.loads(snapshot_path.read_text(encoding="utf-8"))
    payload["publications"][1]["parent_version_hash"] = "0" * 64
    snapshot_path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(HapticError) as caught:
        HapticSession.open(data_dir)
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
    assert caught.value.version == 2
