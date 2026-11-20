from __future__ import annotations

import json
import os
import subprocess
import sys
from dataclasses import replace
from pathlib import Path

import pytest

from spectral_editor.canonical import write_json_atomic
from spectral_editor.errors import BusinessError, E_RECOVERY_INVALID
from spectral_editor.library import load_library
from spectral_editor.models import IlluminantSpec, TransitionRequest
from spectral_editor.persistence import (
    load_snapshot,
    save_recovery_anchor,
    save_snapshot,
)
from spectral_editor.versioning import publication_hash


def _read(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def _write(path: Path, document):
    write_json_atomic(path, document)


def test_restore_rebuilds_versions_hashes_and_ui_state(library):
    material_id = "spectral-oxide-red"
    library.update_ui_state(
        selected_material=material_id,
        selected_version=1,
        timeline_material=material_id,
    )
    snapshot_path = library.snapshot_fixture_path
    before = library.current_snapshot
    restored, pending = load_library(
        snapshot_path,
        trace_path=snapshot_path.parent.parent / "trace.jsonl",
        pending_path=snapshot_path.parent / "pending.json",
    )
    try:
        assert pending is None
        assert restored.current_snapshot.generation == before.generation
        assert restored.current_snapshot.publication_hash == before.publication_hash
        assert restored.current_snapshot.histories == before.histories
        assert restored.current_snapshot.ui_state["selected_material"] == material_id
    finally:
        restored.close()


def test_missing_snapshot_field_aborts_before_any_library_is_returned(library):
    snapshot_path = library.snapshot_fixture_path
    document = _read(snapshot_path)
    del document["ui_state"]
    _write(snapshot_path, document)
    with pytest.raises(BusinessError) as caught:
        load_library(snapshot_path)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.path == "snapshot.ui_state"


def test_content_hash_tamper_is_rejected(library):
    snapshot_path = library.snapshot_fixture_path
    document = _read(snapshot_path)
    document["materials"]["spectral-oxide-red"]["versions"][0]["colors"]["srgb"][0] = 0.123456
    _write(snapshot_path, document)
    with pytest.raises(BusinessError) as caught:
        load_snapshot(snapshot_path)
    assert caught.value.code == E_RECOVERY_INVALID
    assert "content hash mismatch" in caught.value.message


def test_broken_publication_hash_chain_is_rejected(library):
    snapshot_path = library.snapshot_fixture_path
    document = _read(snapshot_path)
    document["publication_chain"][0]["previous_publication_hash"] = "broken"
    _write(snapshot_path, document)
    with pytest.raises(BusinessError) as caught:
        load_snapshot(snapshot_path)
    assert caught.value.code == E_RECOVERY_INVALID
    assert "chain is broken" in caught.value.message


def test_material_version_rollback_across_publications_is_rejected(library):
    base = library.current_snapshot
    library.run_transition_sync(
        TransitionRequest(
            "illuminant",
            pipeline=replace(
                base.pipeline,
                illuminant=IlluminantSpec("Blackbody-5000K", 5000.0),
            ),
        )
    )
    library.run_transition_sync(
        TransitionRequest(
            "white-point",
            pipeline=replace(
                library.current_snapshot.pipeline,
                white_point_name="D50",
                white_point_xyz=(0.96422, 1.0, 0.82521),
            ),
        )
    )
    snapshot_path = library.snapshot_fixture_path
    document = _read(snapshot_path)
    material_id = "spectral-oxide-red"
    first_entry = document["publication_chain"][0]
    last_entry = document["publication_chain"][-1]
    last_entry["material_versions"][material_id] = first_entry["material_versions"][material_id]
    last_entry["record_hashes"][material_id] = first_entry["record_hashes"][material_id]
    last_entry["chain_hashes"][material_id] = first_entry["chain_hashes"][material_id]
    last_entry["publication_hash"] = publication_hash(
        generation=last_entry["generation"],
        previous_publication_hash=last_entry["previous_publication_hash"],
        pipeline_hash=last_entry["pipeline_hash"],
        material_versions=last_entry["material_versions"],
        record_hashes=last_entry["record_hashes"],
        chain_hashes=last_entry["chain_hashes"],
    )
    document["publication_hash"] = last_entry["publication_hash"]
    _write(snapshot_path, document)
    (snapshot_path.parent / "recovery_anchor.json").unlink(missing_ok=True)
    with pytest.raises(BusinessError) as caught:
        load_snapshot(snapshot_path)
    assert caught.value.code == E_RECOVERY_INVALID
    assert "rolled back" in caught.value.message


def test_recovery_anchor_rejects_replaced_older_snapshot(library):
    snapshot_path = library.snapshot_fixture_path
    old_bytes = snapshot_path.read_bytes()
    base = library.current_snapshot
    library.run_transition_sync(
        TransitionRequest(
            "white-point",
            pipeline=replace(
                base.pipeline,
                white_point_name="D50",
                white_point_xyz=(0.96422, 1.0, 0.82521),
            ),
        )
    )
    snapshot_path.write_bytes(old_bytes)
    with pytest.raises(BusinessError) as caught:
        load_snapshot(snapshot_path)
    assert caught.value.code == E_RECOVERY_INVALID
    assert "recovery anchor" in caught.value.message


def test_forced_termination_with_pending_journal_resumes_only_unfinished_versions(
    library, tmp_path
):
    snapshot_path = library.snapshot_fixture_path
    before = library.current_snapshot
    script = f"""
import os
from dataclasses import replace
from spectral_editor.library import load_library
from spectral_editor.models import IlluminantSpec, TransitionRequest
library, pending = load_library({str(snapshot_path)!r})
assert pending is None
config = replace(library.current_snapshot.pipeline,
                 illuminant=IlluminantSpec('Blackbody-5000K', 5000.0))
library.write_pending(TransitionRequest('forced-kill', pipeline=config))
os._exit(37)
"""
    completed = subprocess.run(
        [sys.executable, "-c", script],
        cwd=str(Path(__file__).resolve().parents[1]),
        env={**os.environ, "PYTHONPATH": str(Path(__file__).resolve().parents[1])},
        check=False,
    )
    assert completed.returncode == 37
    library_after_kill, pending = load_library(snapshot_path)
    try:
        assert library_after_kill.current_snapshot.generation == before.generation
        assert pending is not None
        assert pending.base_generation == before.generation
        assert pending.base_publication_hash == before.publication_hash
        result = library_after_kill.resume_pending(pending)
        assert result.generation == before.generation + 1
        assert result.recomputed_count == 6
        assert result.reused_count == 2
        for material_id, version in result.reused_versions.items():
            assert (
                library_after_kill.current_snapshot.current_record(material_id).content_hash
                == before.current_record(material_id).content_hash
            )
    finally:
        library_after_kill.close()


def test_committed_snapshot_with_stale_pending_is_recognized_as_completed(library):
    snapshot_path = library.snapshot_fixture_path
    before = library.current_snapshot
    request = TransitionRequest(
        "committed-before-pending-clear",
        pipeline=replace(
            before.pipeline,
            illuminant=IlluminantSpec("Blackbody-5000K", 5000.0),
        ),
    )
    library.write_pending(request)
    candidate = library.prepare_transition(request)
    save_snapshot(snapshot_path, candidate.snapshot)
    save_recovery_anchor(snapshot_path, candidate.snapshot)
    restored, pending = load_library(snapshot_path)
    try:
        assert restored.current_snapshot.generation == before.generation + 1
        assert restored.current_snapshot.publication_hash == candidate.snapshot.publication_hash
        assert pending is None
        assert not (snapshot_path.parent / "pending.json").exists()
    finally:
        restored.close()


def test_restore_uses_last_atomic_snapshot_not_partial_temp_file(library, tmp_path):
    snapshot_path = library.snapshot_fixture_path
    temp_file = snapshot_path.parent / ".snapshot.json.interrupted.tmp"
    temp_file.write_text('{"schema": "bad"', encoding="utf-8")
    restored = load_snapshot(snapshot_path)
    assert restored.generation == library.current_snapshot.generation
