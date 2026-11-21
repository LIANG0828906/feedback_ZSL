import json

import pytest

from spectral_studio import samplegen
from spectral_studio.canon import canonical_json
from spectral_studio.engine import MaterialLibrary
from spectral_studio.errors import ErrorCode, SpectralStudioError
from spectral_studio.persistence import load_snapshot, save_snapshot
from spectral_studio.versioning import (MaterialVersion, Release,
                                        STATUS_INCOMPLETE)


def _library(tmp_path):
    library = MaterialLibrary(snapshot_path=str(tmp_path / "snapshot.json"))
    for doc in samplegen.generate_samples()[0]:
        library.add_material(doc)
    library.submit([], ui_state={"panel": "materials"})
    return library


def test_snapshot_roundtrip_restores_state(tmp_path):
    library = _library(tmp_path)
    library.submit([{"op": "set_env", "white_point": "D50"}],
                   ui_state={"panel": "whitepoint"})
    recovered = MaterialLibrary.recover(str(tmp_path / "snapshot.json"))
    assert recovered.current_release.chain_hash == \
        library.current_release.chain_hash
    assert recovered.ui_state["panel"] == "whitepoint"
    for mid, version in recovered.current_release.versions.items():
        original = library.current_release.versions[mid]
        assert version.content_hash == original.content_hash
        assert version.encoded == original.encoded
    # History is fully rebuilt: rollback still works after recovery.
    mid = sorted(recovered.current_release.versions)[0]
    assert len(recovered.history_of(mid)) == 2


def test_broken_hash_chain_locates_first_broken_version(tmp_path):
    library = _library(tmp_path)
    path = tmp_path / "snapshot.json"
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    mid = sorted(data["history"])[0]
    data["history"][mid][0]["encoded"]["srgb"] = [0.0, 0.0, 0.0]
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(canonical_json(data))
    with pytest.raises(SpectralStudioError) as exc:
        load_snapshot(str(path))
    assert exc.value.code == ErrorCode.E_RECOVERY_INVALID
    assert exc.value.detail["material_id"] == mid
    assert exc.value.detail["version"] == 1


def test_missing_version_field_aborts(tmp_path):
    library = _library(tmp_path)
    path = tmp_path / "snapshot.json"
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    mid = sorted(data["history"])[0]
    del data["history"][mid][0]["dep_factors"]
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(canonical_json(data))
    with pytest.raises(SpectralStudioError) as exc:
        load_snapshot(str(path))
    assert exc.value.code == ErrorCode.E_RECOVERY_INVALID
    assert exc.value.detail["missing_field"] == "dep_factors"


def test_recovery_recomputes_only_incomplete_versions(tmp_path):
    library = _library(tmp_path)
    path = tmp_path / "snapshot.json"
    release = library.current_release
    mid = sorted(release.versions)[0]
    old_v = release.versions[mid]
    tentative = dict(release.versions)
    tentative[mid] = MaterialVersion(
        material_id=mid, version=old_v.version + 1, params=old_v.params,
        dep_factors=library.env.dependency_factors(), encoded={},
        intermediates={}, prev_hash=old_v.content_hash,
        status=STATUS_INCOMPLETE,
        recompute_reason="material-params-changed")
    library._history[mid].append(tentative[mid])
    journal = Release(release.release_id + 1, tentative, release.env_config,
                      release.ui_state, release.chain_hash)
    save_snapshot(library, str(path), release=journal)
    library._history[mid].pop()

    recovered = MaterialLibrary.recover(str(path))
    entry = recovered.trace[-1]
    assert entry["event"] == "recovery-recompute"
    assert entry["recomputed"] == [mid]
    new_v = recovered.current_release.versions[mid]
    assert new_v.version == old_v.version + 1
    assert new_v.encoded  # recomputed, no longer empty
    # Untouched materials keep their published versions and hashes.
    for other, version in recovered.current_release.versions.items():
        if other != mid:
            assert version.content_hash == release.versions[other].content_hash
