from spectral_studio import samplegen
from spectral_studio.engine import MaterialLibrary
from spectral_studio.versioning import MaterialVersion, Release


def test_conservative_recompute_when_impact_unprovable(tmp_path):
    library = MaterialLibrary(snapshot_path=str(tmp_path / "snap.json"))
    for doc in samplegen.generate_samples()[0]:
        library.add_material(doc)
    release = library.submit([])

    # Simulate a legacy/foreign version whose dependency factors are
    # missing: the engine cannot prove it is unaffected and must
    # conservatively recompute it, annotating the reason.
    mid = sorted(release.versions)[0]
    old = release.versions[mid]
    legacy = MaterialVersion(
        material_id=mid, version=old.version, params=old.params,
        dep_factors={}, encoded=old.encoded,
        intermediates=old.intermediates, prev_hash=old.prev_hash)
    versions = dict(release.versions)
    versions[mid] = legacy
    library._release = Release(release.release_id, versions,
                               release.env_config, release.ui_state,
                               release.prev_chain_hash)
    library._history[mid][-1] = legacy

    new_release = library.submit([])  # no parameter change at all
    entry = library.trace[-1]
    assert entry["recomputed_count"] == 1
    assert entry["reasons"][mid] == "conservative:dep-factors-missing"
    assert new_release.versions[mid].version == old.version + 1
