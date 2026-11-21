from __future__ import annotations

import threading

import pytest

from lightprobe.bake import Baker
from lightprobe.errors import ProbeError
from lightprobe.snapshot import SnapshotWriter
from lightprobe.util import atomic_write_json

from conftest import fast_config


def test_cancellation_responds_at_chunk_checkpoint(room, tmp_path, monkeypatch):
    cancel_event = threading.Event()
    original = SnapshotWriter.commit_chunk

    def cancel_after_first_chunk(self, records):
        result = original(self, records)
        if self._chunk_index == 1:
            cancel_event.set()
        return result

    monkeypatch.setattr(SnapshotWriter, "commit_chunk", cancel_after_first_chunk)
    snapshot = tmp_path / "cancelled"
    with pytest.raises(ProbeError) as caught:
        Baker().bake(
            room,
            fast_config(room, chunk_size=1),
            snapshot_dir=snapshot,
            target_version=2,
            run_id="cancel-test",
            cancel_event=cancel_event,
        )
    assert caught.value.code == "E_CANCELLED"
    assert (snapshot / "meta.json").exists()
    assert list(snapshot.glob("chunk_*.json"))
    assert not list(snapshot.glob("*.tmp-*"))

