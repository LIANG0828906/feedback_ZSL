from __future__ import annotations

from dataclasses import replace

import pytest

from spectral_editor.errors import BusinessError, E_VERSION_CONFLICT
from spectral_editor.models import IlluminantSpec, TransitionRequest


def test_initial_publication_has_version_one_for_every_material(library):
    snapshot = library.current_snapshot
    assert snapshot.generation == 1
    assert len(snapshot.publication_chain) == 1
    assert all(history.current().version == 1 for history in snapshot.histories.values())


def test_illuminant_change_recomputes_only_spectral_versions(library):
    base = library.current_snapshot
    changed_config = replace(
        base.pipeline,
        illuminant=IlluminantSpec("Blackbody-5000K", 5000.0),
    )
    result = library.run_transition_sync(
        TransitionRequest("illuminant", pipeline=changed_config)
    )
    assert result.recomputed_count == 6
    assert result.reused_count == 2
    assert set(result.affected_material_ids) == {
        material_id
        for material_id, history in base.histories.items()
        if base.current_record(material_id).params["kind"] == "spectral"
    }
    for material_id, version in result.reused_versions.items():
        old_record = base.current_record(material_id)
        new_record = library.current_snapshot.current_record(material_id)
        assert version == 1
        assert new_record.content_hash == old_record.content_hash
        assert new_record.chain_hash == old_record.chain_hash


def test_white_point_change_recomputes_all_materials(library):
    base = library.current_snapshot
    changed_config = replace(
        base.pipeline,
        white_point_name="D50",
        white_point_xyz=(0.96422, 1.0, 0.82521),
    )
    result = library.run_transition_sync(
        TransitionRequest("white-point", pipeline=changed_config)
    )
    assert result.recomputed_count == 8
    assert result.reused_count == 0
    assert all(value == 2 for value in result.affected_versions.values())


def test_single_material_edit_reuses_seven_cached_versions(library):
    material_id = "spectral-oxide-red"
    current = library.current_snapshot.current_record(material_id)
    descriptor = {
        "id": material_id,
        "kind": "spectral",
        "name": "Oxide Red",
        "targets": ["srgb", "display-p3"],
        "truth_source": "unit-test-edit",
        "spectrum": {
            "wavelengths": current.params["spectrum"]["original_wavelengths_nm"],
            "interval_nm": current.params["spectrum"]["original_interval_nm"],
            "values": [
                max(0.0, min(1.0, value * 0.9))
                for value in current.params["spectrum"]["original_values"]
            ],
        },
    }
    params = library.material_update_params(material_id, descriptor)
    result = library.run_transition_sync(
        TransitionRequest(
            "edit-one",
            material_update={material_id: params},
        )
    )
    assert result.recomputed_count == 1
    assert result.reused_count == 7
    assert result.affected_versions == {material_id: 2}
    assert result.max_channel_deltas[material_id]["srgb"] > 0.0


def test_rollback_is_a_new_version_and_does_not_delete_history(library):
    material_id = "spectral-oxide-red"
    base = library.current_snapshot
    original = base.current_record(material_id)
    next_params = library.material_update_params(
        material_id,
        {
            "id": material_id,
            "kind": "spectral",
            "name": "Oxide Red",
            "targets": ["srgb", "display-p3"],
            "truth_source": "unit-test-edit",
            "spectrum": {
                "wavelengths": original.params["spectrum"]["original_wavelengths_nm"],
                "interval_nm": original.params["spectrum"]["original_interval_nm"],
                "values": [
                    max(0.0, min(1.0, value * 0.8))
                    for value in original.params["spectrum"]["original_values"]
                ],
            },
        },
    )
    edited = library.run_transition_sync(
        TransitionRequest("edit", material_update={material_id: next_params})
    )
    current = library.current_snapshot.current_record(material_id)
    rolled_back = library.run_transition_sync(
        TransitionRequest(
            "rollback",
            rollback={
                "material_id": material_id,
                "target_version": 1,
                "expected_head_hash": current.content_hash,
            },
        )
    )
    snapshot = library.current_snapshot
    history = snapshot.histories[material_id]
    assert [record.version for record in history.versions] == [1, 2, 3]
    assert rolled_back.affected_versions == {material_id: 3}
    assert history.get(2).content_hash == edited.materials[material_id]["content_hash"]
    assert history.get(2).parent_version == 1
    assert history.get(3).parent_version == 2
    assert snapshot.current_record(material_id).params == original.params


def test_hash_conflict_preserves_entire_sequence(library):
    material_id = "spectral-oxide-red"
    before = library.current_snapshot
    with pytest.raises(BusinessError) as caught:
        library.run_transition_sync(
            TransitionRequest(
                "bad-rollback",
                rollback={
                    "material_id": material_id,
                    "target_version": 1,
                    "expected_head_hash": "not-the-head",
                },
            )
        )
    assert caught.value.code == E_VERSION_CONFLICT
    after = library.current_snapshot
    assert after.generation == before.generation
    assert after.publication_hash == before.publication_hash
    assert after.histories == before.histories


def test_version_diff_reports_changed_factors(library):
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
    material_id = "spectral-oxide-red"
    diff = library.diff_versions(material_id, 1, 2)
    assert diff["changed_dependency_factors"] == ["illuminant"]
    assert diff["to_version"] == 2
    assert diff["max_channel_deltas"]["srgb"] > 0.0
