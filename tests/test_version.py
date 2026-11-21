from __future__ import annotations

import threading

import numpy as np
import pytest

from lightprobe.bake import Baker
from lightprobe.errors import ProbeError
from lightprobe.version import VersionedStore

from conftest import fast_config


def test_publish_query_and_stale_errors(room):
    result = Baker().bake(room, fast_config(room))
    store = VersionedStore()
    published = store.publish(result.state)
    query = store.query([3.1, 1.2, 3.4], expected_version=published.version)
    assert query["version"] == published.version
    assert len(query["probes"]) == 8
    assert {item["version"] for item in query["probes"]} == {published.version}
    with pytest.raises(ProbeError) as caught:
        store.query([3.0, 1.0, 3.0], expected_version=999)
    assert caught.value.code == "E_VERSION_STALE"


def test_published_version_is_insulated_from_caller_mutation(room):
    result = Baker().bake(room, fast_config(room))
    store = VersionedStore()
    published = store.publish(result.state)
    published_hash = published.content_hash
    probe_id = result.state.grid.probe_ids[0]
    result.state.records.pop(probe_id)
    assert store.current.version == published.version
    assert store.current.content_hash == published_hash
    assert probe_id in store.current.state.records
    with pytest.raises((TypeError, AttributeError)):
        store.current.state.records.pop(probe_id)  # type: ignore[union-attr]
    with pytest.raises(ProbeError) as caught:
        store.query([3.0, 1.0, 3.0], expected_content_hash="not-the-hash")
    assert caught.value.code == "E_VERSION_STALE"


def test_query_never_mixes_versions_during_publish(room):
    baker = Baker()
    first = baker.bake(room, fast_config(room))
    lamp = room.occluder("street_lamp")
    center = lamp.center
    changed = room.with_updates(
        {
            "occluders": {
                "street_lamp": {
                    "center": [center[0] + 0.5, center[1], center[2] - 0.5]
                }
            }
        }
    )
    second = baker.bake(changed, fast_config(changed), base_state=first.state)
    store = VersionedStore()
    store.publish(first.state)
    errors: list[Exception] = []

    def reader() -> None:
        for index in range(80):
            try:
                result = store.query(
                    [2.5 + index % 3, 1.0, 3.0 + index % 2], normal=[0, 1, 0]
                )
                versions = {item["version"] for item in result["probes"]}
                assert versions == {result["version"]}
            except Exception as exc:  # pragma: no cover - assertion capture
                errors.append(exc)

    thread = threading.Thread(target=reader)
    thread.start()
    store.publish(second.state)
    thread.join(timeout=10)
    assert not thread.is_alive()
    assert not errors
    assert store.current.version == 2


def test_incomplete_neighborhood_returns_probe_ids(room):
    result = Baker().bake(room, fast_config(room))
    first_probe = result.state.grid.probe_ids[0]
    records = dict(result.state.records)
    records.pop(first_probe)
    broken = type(result.state)(**{**result.state.__dict__, "records": records})
    store = VersionedStore()
    store.publish(broken)
    with pytest.raises(ProbeError) as caught:
        store.query([0.0, 0.0, 0.0])
    assert caught.value.code == "E_INTERP_FAILED"
    assert first_probe in caught.value.details["probe_ids"]

