from __future__ import annotations

from pathlib import Path

import pytest

from audio_sync.errors import E_BUSY, E_VERSION_STALE, SyncError
from audio_sync.models import SyncConfig
from audio_sync.service import SyncRequest, SyncService
from audio_sync.store import ResultStore


def _request(dataset, config=None):
    return SyncRequest(
        wav_path=dataset["wav"],
        markers_path=dataset["markers"],
        config=config or SyncConfig(segment_seconds=2.0),
    )


def test_busy_cancel_and_recover(drift_dataset, tmp_path) -> None:
    service = SyncService(tmp_path)
    first = service.submit(_request(drift_dataset))
    with pytest.raises(SyncError) as busy:
        service.submit(_request(drift_dataset))
    assert busy.value.code == E_BUSY
    state = service.wait(first.job_id, timeout=30.0)
    assert state.status == "succeeded"
    snapshot = service.get_result(state.result_version, state.result_hash)
    assert snapshot.version == 1
    with pytest.raises(SyncError) as stale:
        service.get_result(version=999)
    assert stale.value.code == E_VERSION_STALE

    second = service.submit(
        _request(
            drift_dataset,
            SyncConfig(segment_seconds=2.0, interpolation="windowed_sinc"),
        )
    )
    service.cancel(second.job_id)
    cancelled = service.wait(second.job_id, timeout=30.0)
    assert cancelled.status == "cancelled"
    assert service.get_result().version == 1

    recovered = SyncService(tmp_path)
    assert recovered.get_result().version == 1
    assert recovered.get_result().content_hash == snapshot.content_hash


def test_store_rejects_hash_mismatch(drift_dataset, tmp_path) -> None:
    service = SyncService(tmp_path)
    state = service.wait(service.submit(_request(drift_dataset)).job_id, timeout=30.0)
    assert state.status == "succeeded"
    snapshot = service.get_result()
    store = ResultStore(tmp_path)
    with pytest.raises(SyncError) as stale:
        store.get_result(content_hash_value="0" * 64)
    assert stale.value.code == E_VERSION_STALE
    assert store.get_result(version=snapshot.version).content_hash == snapshot.content_hash
    assert Path(tmp_path, "reports", "latest.json").exists()

