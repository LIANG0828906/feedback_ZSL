from __future__ import annotations

import threading

import pytest

from probe_baker.bake import BakeEngine
from probe_baker.errors import ErrorCode, ProbeError
from probe_baker.publish import PublishedStore


def test_publish_is_atomic_and_old_snapshot_remains_readable(room_scene, small_config) -> None:
    engine = BakeEngine()
    first = engine.bake(room_scene, small_config)
    second = engine.bake(room_scene, small_config)
    store = PublishedStore()
    v1 = store.publish(first.state, version=1)
    old_query = v1.query_probe(0)
    v2 = store.publish(second.state, version=2)
    assert old_query.version == 1
    assert v1.query_probe(0).version == 1
    assert v2.query_probe(0).version == 2
    assert store.pin().version == 2
    assert store.history_versions() == (1, 2)


def test_version_and_hash_mismatch_are_stale(room_scene, small_config) -> None:
    state = BakeEngine().bake(room_scene, small_config).state
    store = PublishedStore()
    snapshot = store.publish(state)
    with pytest.raises(ProbeError) as version:
        store.query_probe(0, version=snapshot.version + 1)
    assert version.value.code == ErrorCode.VERSION_STALE
    with pytest.raises(ProbeError) as content_hash:
        store.query_probe(0, expected_hash="0" * 64)
    assert content_hash.value.code == ErrorCode.VERSION_STALE
    with pytest.raises(ProbeError) as monotonic:
        store.publish(state, version=snapshot.version)
    assert monotonic.value.code == ErrorCode.VERSION_STALE


def test_concurrent_queries_observe_one_complete_version(room_scene, small_config) -> None:
    engine = BakeEngine()
    states = [engine.bake(room_scene, small_config).state for _ in range(5)]
    store = PublishedStore()
    store.publish(states[0], version=1)
    stop = threading.Event()
    observations: list[tuple[int, str, int]] = []

    def reader() -> None:
        while not stop.is_set():
            try:
                result = store.query_probes()
            except ProbeError:
                continue
            observations.append(
                (
                    int(result["version"]),
                    str(result["content_hash"]),
                    int(len(result["probe_ids"])),
                )
            )

    thread = threading.Thread(target=reader)
    thread.start()
    for version, state in enumerate(states[1:], start=2):
        store.publish(state, version=version)
    stop.set()
    thread.join(timeout=5.0)
    assert not thread.is_alive()
    assert observations
    expected_counts = {state.grid.count for state in states}
    assert {item[2] for item in observations} == expected_counts
    for version, content_hash, _ in observations:
        snap = store.snapshot_for_version(version)
        assert snap.content_hash == content_hash

