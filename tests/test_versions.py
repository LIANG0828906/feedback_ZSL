from __future__ import annotations

from pathlib import Path

import pytest

from haptic_studio.errors import ErrorCode, HapticError
from haptic_studio.models import DeviceConfig, WaveSequence
from haptic_studio.session import HapticSession
from haptic_studio.versions import ConfigVersionStore


def test_config_version_store_is_append_only(
    config: DeviceConfig,
) -> None:
    store = ConfigVersionStore(initial=config)
    second_config = DeviceConfig.from_mapping(
        {**config.to_dict(), "allocation_ratio": [0.7, 0.3]}
    )
    second = store.commit(second_config)
    rollback = store.rollback(1)
    assert [item.version for item in store.versions] == [1, 2, 3]
    assert rollback.version == 3
    assert rollback.source_version == 1
    assert rollback.config.content_hash == config.content_hash
    assert rollback.parent_hash == second.version_hash
    diff = store.diff(1, 2)
    assert diff["changes"][0]["field"] == "allocation_ratio"


def test_stale_handle_and_historical_query(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = HapticSession.create(sequence, config, tmp_path / "runtime")
    old_handle = session.issue_handle()
    old_query = session.preview(old_handle)
    changed = DeviceConfig.from_mapping(
        {**config.to_dict(), "allocation_ratio": [0.65, 0.35]}
    )
    new_published = session.recompute(sequence, new_config=changed)
    with pytest.raises(HapticError) as caught:
        session.preview(old_handle)
    assert caught.value.code == ErrorCode.STALE_HANDLE
    historical = session.preview_version(old_query.hit_version)
    assert historical.hit_version == old_query.hit_version
    assert historical.signal_hash == old_query.signal_hash
    assert historical.published.version_hash == old_query.version_hash
    fresh = session.preview(session.issue_handle())
    assert fresh.hit_version == new_published.publication_version
    assert fresh.signal_hash == new_published.signal_hash
    assert fresh.left.tobytes() == new_published.sampling.left_bytes
    assert fresh.right.tobytes() == new_published.sampling.right_bytes


def test_rollback_is_new_publication_and_historical_hashes_verify(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = HapticSession.create(sequence, config, tmp_path / "runtime")
    changed = DeviceConfig.from_mapping(
        {
            **config.to_dict(),
            "bit_depth": 16,
            "allocation_ratio": [0.2, 0.8],
            "polarity": [-1, 1],
        }
    )
    session.recompute(sequence, new_config=changed)
    rollback = session.rollback_config(1)
    assert rollback.publication_version == 3
    assert rollback.config_version == 3
    assert rollback.config.content_hash == config.content_hash
    assert session.config_history()[2].source_version == 1
    assert len(session.publication_history()) == 3
    for version in (1, 2, 3):
        verification = session.verify_historical(version)
        assert verification["match"], verification
    diff = session.diff_publications(1, 2)
    assert diff["config_diff"]["changes"]
    assert diff["signal_hash_changed"]


def test_invalid_config_does_not_publish_or_commit(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = HapticSession.create(sequence, config, tmp_path / "runtime")
    before_publication = session.latest_publication.publication_version
    before_configs = len(session.config_history())
    with pytest.raises(HapticError) as caught:
        bad = DeviceConfig.from_mapping(
            {**config.to_dict(), "sample_rate": 64, "bit_depth": 8}
        )
        session.recompute(sequence, new_config=bad)
    assert caught.value.code == ErrorCode.WAVE_INVALID
    assert caught.value.segment_index == 0
    assert caught.value.field == "frequency_hz"
    assert session.latest_publication.publication_version == before_publication
    assert len(session.config_history()) == before_configs
