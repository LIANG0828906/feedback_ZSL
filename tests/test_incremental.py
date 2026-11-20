from __future__ import annotations

from pathlib import Path

from repair_app.audio import sha256_file
from repair_app.engine import SessionEngine
from repair_app.generator import generate_sample_file
from repair_app.models import Interval


def _build_session(tmp_path: Path, name: str) -> tuple[SessionEngine, Path]:
    input_path = tmp_path / "input.wav"
    if not input_path.exists():
        generate_sample_file(
            input_path, sample_rate=8_000, channels=1, seconds=1.0, seed=321
        )
    session_path = tmp_path / f"{name}.json"
    output_path = tmp_path / f"{name}.wav"
    engine = SessionEngine(session_path)
    engine.create(
        input_path=input_path,
        output_path=output_path,
        block_size=500,
        intervals=[Interval(1, 1_000, 3_000, gain_db=2.0, fade_in_ms=0, fade_out_ms=0)],
    )
    engine.process()
    return engine, output_path


def test_parameter_change_invalidates_only_intersecting_blocks(tmp_path: Path) -> None:
    engine, _ = _build_session(tmp_path, "incremental")
    before = {key: dict(value) for key, value in engine.store.load().completed.items()}
    update = engine.update_chain({"noise_gate": {"threshold_db": -50.0}})
    assert update["dirty_blocks"] == [2, 3, 4, 5]
    events: list[dict] = []
    report = engine.process(trace=events.append)
    assert report["processed_blocks"] == 4
    assert report["reused_blocks"] == 12
    after = {key: dict(value) for key, value in engine.store.load().completed.items()}
    for index in range(16):
        if index in {2, 3, 4, 5}:
            assert after[str(index)]["config_sha256"] != before[str(index)]["config_sha256"]
        else:
            assert after[str(index)] == before[str(index)]
    statuses = {event["index"]: event["status"] for event in events if event["event"] == "block"}
    assert all(statuses[index] == "reused" for index in range(16) if index not in {2, 3, 4, 5})
    assert all(statuses[index] == "processed" for index in {2, 3, 4, 5})


def test_interval_move_invalidates_old_and_new_blocks(tmp_path: Path) -> None:
    engine, _ = _build_session(tmp_path, "move")
    update = engine.update_intervals(
        [Interval(1, 5_000, 7_000, gain_db=2.0, fade_in_ms=0, fade_out_ms=0)]
    )
    assert update["dirty_blocks"] == [2, 3, 4, 5, 10, 11, 12, 13]
    report = engine.process()
    assert report["processed_blocks"] == 8
    assert report["reused_blocks"] == 8


def test_deterministic_replay_produces_byte_identical_wav(tmp_path: Path) -> None:
    first_engine, first_output = _build_session(tmp_path, "first")
    second_engine, second_output = _build_session(tmp_path, "second")
    assert sha256_file(first_output) == sha256_file(second_output)
    assert first_engine.store.load().completed == second_engine.store.load().completed

