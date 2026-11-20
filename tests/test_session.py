from __future__ import annotations

import pytest

from broadcast_loudness.errors import BusyError
from broadcast_loudness.session import SessionManager


def test_one_active_session_per_material(make_audio) -> None:
    audio, params = make_audio()
    manager = SessionManager()
    first = manager.open_session(audio, params)
    with pytest.raises(BusyError) as error:
        manager.open_session(audio, params)
    assert error.value.code == "E_BUSY"
    assert manager.active_count == 1
    last = first.close()
    assert manager.active_count == 0
    assert last.version == first.snapshot().version
    second = manager.open_session(audio, params)
    assert second is not first
    second.close()


def test_pause_seek_restart_preserve_last_snapshot(make_audio) -> None:
    audio, params = make_audio(duration=3.0)
    manager = SessionManager()
    session = manager.open_session(audio, params)
    session.play()
    session.advance(2 * 48_000)
    committed = session.snapshot()
    session.pause()
    session.seek(1_000)
    session.restart()
    assert session.snapshot() is committed
    assert session.position_sample == 0
    assert session.view().current_block_index == 0
    session.close()
