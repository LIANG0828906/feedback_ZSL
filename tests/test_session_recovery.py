from __future__ import annotations

import json
from pathlib import Path

import pytest

from repair_app.engine import SessionEngine
from repair_app.errors import AppError
from repair_app.locking import SessionLock


def test_partial_process_recover_and_resume(stereo_session) -> None:
    engine, session_path, output_path = stereo_session
    first = engine.process(max_new_blocks=3)
    assert first["status"] == "stopped"
    assert first["committed_blocks"] == 3
    assert output_path.exists()
    recovered = engine.recover()
    assert recovered["recovered_blocks"] == 3
    assert recovered["remaining_blocks"] == 29
    events: list[dict] = []
    resumed = engine.process(trace=events.append)
    assert resumed["status"] == "completed"
    assert resumed["reused_blocks"] == 3
    assert resumed["processed_blocks"] == 29
    reused = [event for event in events if event.get("status") == "reused"]
    assert [event["index"] for event in reused] == [0, 1, 2]


def test_missing_session_is_recovery_invalid(tmp_path: Path) -> None:
    engine = SessionEngine(tmp_path / "missing.json")
    with pytest.raises(AppError) as error:
        engine.recover()
    assert error.value.code == "E_RECOVERY_INVALID"


def test_truncated_output_is_recovery_invalid(stereo_session) -> None:
    engine, _, output_path = stereo_session
    engine.process(max_new_blocks=2)
    with open(output_path, "r+b") as handle:
        handle.truncate(output_path.stat().st_size - 7)
    with pytest.raises(AppError) as error:
        engine.recover()
    assert error.value.code == "E_RECOVERY_INVALID"


def test_session_integrity_tamper_is_session_mismatch(stereo_session) -> None:
    engine, session_path, _ = stereo_session
    payload = json.loads(session_path.read_text(encoding="utf-8"))
    payload["intervals"][0]["gain_db"] = 99.0
    session_path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(AppError) as error:
        engine.status()
    assert error.value.code == "E_SESSION_MISMATCH"


def test_block_cache_tamper_is_session_mismatch(stereo_session) -> None:
    engine, _, _ = stereo_session
    engine.process(max_new_blocks=2)
    snapshot = engine.store.load()
    cache_path = snapshot.cache_path(0)
    payload = bytearray(cache_path.read_bytes())
    payload[0] ^= 0xFF
    cache_path.write_bytes(bytes(payload))
    with pytest.raises(AppError) as error:
        engine.status()
    assert error.value.code == "E_SESSION_MISMATCH"


def test_second_processor_returns_busy(stereo_session) -> None:
    engine, _, _ = stereo_session
    with SessionLock(engine.store.lock_path):
        with pytest.raises(AppError) as error:
            engine.process()
    assert error.value.code == "E_BUSY"
    assert engine.process()["status"] == "completed"


def test_output_can_be_used_to_restore_missing_cache(stereo_session) -> None:
    engine, _, _ = stereo_session
    engine.process()
    snapshot = engine.store.load()
    cache_path = snapshot.cache_path(0)
    cache_path.unlink()
    recovered = engine.recover()
    assert recovered["complete"] is True
    assert cache_path.exists()

