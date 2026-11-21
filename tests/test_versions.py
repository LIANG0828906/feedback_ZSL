import pytest

from loudmeter.errors import StaleHandleError, VersionStaleError
from loudmeter.versions import VersionStore


def _manifest(tag):
    return {"asset_id": "a", "tag": tag, "blocks": [{"index": 0}],
            "gating": {"integrated_lufs": -24.0}}


def test_publish_and_query_hash():
    store = VersionStore()
    h1 = store.publish(_manifest("v1"))
    r = store.query(h1)
    assert r.version_hash
    assert r.manifest["tag"] == "v1"
    # querying with the hash actually hit succeeds
    store.query(h1, expected_version_hash=r.version_hash)


def test_old_version_readable_while_new_published():
    store = VersionStore()
    h1 = store.publish(_manifest("v1"))
    h2 = store.publish(_manifest("v2"))
    assert store.query(h1).manifest["tag"] == "v1"
    assert store.query(h2).manifest["tag"] == "v2"
    assert store.query_current().manifest["tag"] == "v2"


def test_version_stale_on_hash_mismatch():
    store = VersionStore()
    h1 = store.publish(_manifest("v1"))
    with pytest.raises(VersionStaleError):
        store.query(h1, expected_version_hash="0" * 64)


def test_stale_handle_after_retire():
    store = VersionStore()
    h1 = store.publish(_manifest("v1"))
    store.retire(h1)
    with pytest.raises(StaleHandleError):
        store.query(h1)


def test_manifest_immutable():
    store = VersionStore()
    h1 = store.publish(_manifest("v1"))
    r = store.query(h1)
    with pytest.raises(TypeError):
        r.manifest["tag"] = "mutated"
